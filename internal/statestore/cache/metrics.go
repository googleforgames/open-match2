// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package cache

import (
	"context"
	"sync/atomic"

	"github.com/sirupsen/logrus"
	otelmetrics "go.opentelemetry.io/otel/metric"
)

const metricsNamePrefix = "om_core.cache."

var (
	otelLogger = logrus.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "replicationQueue",
	})

	// Explicit sub-millisecond to multi-second bucket boundaries (in milliseconds)
	// for cache latency histograms.
	cacheDurationHistogramBucketsMs = []float64{
		0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000,
	}

	// Expiration metrics.
	otelCacheExpirationCycleDuration    otelmetrics.Float64Histogram
	otelCacheAssignmentsExpiredPerCycle otelmetrics.Int64Histogram
	otelCacheInactivesExpiredPerCycle   otelmetrics.Int64Histogram
	otelCacheTicketsExpiredPerCycle     otelmetrics.Int64Histogram

	// Outgoing replication queue metrics.
	otelCacheOutgoingUpdatesPerPoll        otelmetrics.Int64Histogram
	otelCacheOutgoingQueueTimeouts         otelmetrics.Int64Counter
	otelCacheOutgoingThresholdReachedCount otelmetrics.Int64Counter
	otelCacheOutgoingQueueWaitDuration     otelmetrics.Float64Histogram
	otelCacheOutgoingSendDuration          otelmetrics.Float64Histogram

	// Incoming replication queue metrics.
	otelCacheIncomingPerPoll            otelmetrics.Int64Histogram
	otelCacheIncomingEmptyTimeouts      otelmetrics.Int64Counter
	otelCacheIncomingProcessingTimeouts otelmetrics.Int64Counter
	otelCacheIncomingFullPolls          otelmetrics.Int64Counter
	otelCacheIncomingPollDuration       otelmetrics.Float64Histogram
	otelCacheIncomingPollWaitDuration   otelmetrics.Float64Histogram
	otelCacheIncomingQueueWaitDuration  otelmetrics.Float64Histogram
	otelCacheIncomingApplyDuration      otelmetrics.Float64Histogram
	otelCacheIncomingApplySleepDuration otelmetrics.Float64Histogram
	otelCacheReplicationLag             otelmetrics.Float64Histogram

	// Metric variable declarations are global to the module, so they can be
	// accessed directly in the module code.
	// The live counters that the metrics observe.
	ActiveCount          int64
	TicketCount          int64
	InactiveCount        int64
	AssignmentCount      int64
	IncomingBacklogCount int64
	OutgoingBacklogCount int64

	// Observable counters. These aren't updated in real-time, and
	// only get recorded with a callback function, when observed.
	otelCacheActiveTickets      otelmetrics.Int64ObservableUpDownCounter
	otelCacheInactiveTickets    otelmetrics.Int64ObservableUpDownCounter
	otelCacheAssignments        otelmetrics.Int64ObservableUpDownCounter
	otelCacheIncomingBacklog    otelmetrics.Int64ObservableUpDownCounter
	otelCacheOutgoingBacklog    otelmetrics.Int64ObservableUpDownCounter
	packageCallbackRegistration otelmetrics.Registration
)

//nolint:cyclop,funlen // Cyclop linter sees each metric initialization as +1 cyclomatic complexity.
func RegisterMetrics(meterPointer *otelmetrics.Meter) {
	if meterPointer == nil {
		return
	}
	meter := *meterPointer
	var err error

	otelCacheActiveTickets, err = meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"tickets.active",
		otelmetrics.WithDescription("Active tickets that can appear in pools"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheInactiveTickets, err = meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"tickets.inactive",
		otelmetrics.WithDescription("Inactive tickets that won't appear in pools"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheAssignments, err = meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"assignments",
		otelmetrics.WithDescription("Assignments in Open Match cache"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingBacklog, err = meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"incoming.backlog",
		otelmetrics.WithDescription("Number of incoming replication updates buffered and waiting to be applied to the local cache"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheOutgoingBacklog, err = meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"outgoing.backlog",
		otelmetrics.WithDescription("Number of outgoing replication updates queued and waiting to be sent to state storage"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheExpirationCycleDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"expiration.duration",
		otelmetrics.WithDescription("Duration of cache expiration logic"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheAssignmentsExpiredPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"assignment.expirations",
		otelmetrics.WithDescription("Number of assignments expired per cache expiration cycle"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheOutgoingUpdatesPerPoll, err = meter.Int64Histogram(
		metricsNamePrefix+"outgoing.updates",
		otelmetrics.WithDescription("Number of outgoing replication updates per cycle"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheTicketsExpiredPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.expirations",
		otelmetrics.WithDescription("Number of tickets expired per cache expiration cycle"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingPerPoll, err = meter.Int64Histogram(
		metricsNamePrefix+"incoming.updates",
		otelmetrics.WithDescription("Number of incoming replication updates per poll"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheInactivesExpiredPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.inactive.expirations",
		otelmetrics.WithDescription("Number of inactive tickets expired per cache expiration cycle"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheOutgoingQueueTimeouts, err = meter.Int64Counter(
		metricsNamePrefix+"outgoing.timeouts",
		otelmetrics.WithDescription("Number of times the outgoing replication queue waited OM_CACHE_OUT_WAIT_TIMEOUT_MS and did not reach OM_CACHE_OUT_MAX_QUEUE_THRESHOLD updates to send as a batch to the replicator"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheOutgoingThresholdReachedCount, err = meter.Int64Counter(
		metricsNamePrefix+"outgoing.maxqueuethresholdreached",
		otelmetrics.WithDescription("Number of times the outgoing replication queue saw OM_CACHE_OUT_MAX_QUEUE_THRESHOLD updates in less than OM_CACHE_OUT_WAIT_TIMEOUT_MS milliseconds"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheOutgoingQueueWaitDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"outgoing.queue.wait",
		otelmetrics.WithDescription("Time an outgoing replication update spends queued and being sent before returning a response"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheOutgoingSendDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"outgoing.send.duration",
		otelmetrics.WithDescription("Duration of sending a batch of outgoing replication updates to state storage"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingEmptyTimeouts, err = meter.Int64Counter(
		metricsNamePrefix+"incoming.timeouts.empty",
		otelmetrics.WithDescription("Number of times the incoming replication queue saw no updates after waiting for OM_CACHE_IN_WAIT_TIMEOUT_MS"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingProcessingTimeouts, err = meter.Int64Counter(
		metricsNamePrefix+"incoming.timeouts.full",
		otelmetrics.WithDescription("Number of times the incoming replication queue couldn't process all pending updates in OM_CACHE_IN_MAX_APPLY_DURATION_MS"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingFullPolls, err = meter.Int64Counter(
		metricsNamePrefix+"incoming.polls.full",
		otelmetrics.WithDescription("Number of times an incoming replication poll returned the maximum OM_CACHE_IN_MAX_UPDATES_PER_POLL updates"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingPollDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"incoming.poll.duration",
		otelmetrics.WithDescription("Duration of polling state storage for incoming replication updates"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingPollWaitDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"incoming.poll.wait",
		otelmetrics.WithDescription("Post-poll pause before the next incoming replication poll (OM_CACHE_IN_POLL_WAIT_MS after a partial poll, OM_CACHE_IN_FULL_POLL_WAIT_MS after a full poll, or any remaining OM_CACHE_IN_WAIT_TIMEOUT_MS if an empty poll returned early on error)"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingQueueWaitDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"incoming.queue.wait",
		otelmetrics.WithDescription("Time the incoming poller spent pushing polled updates into the local replication channel"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingApplyDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"incoming.apply.duration",
		otelmetrics.WithDescription("Time spent applying incoming replication updates to the local cache per cycle"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingApplySleepDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"incoming.apply.sleep",
		otelmetrics.WithDescription("Time spent sleeping between incoming cache apply cycles (OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS after draining the queue, or OM_CACHE_IN_FULL_APPLY_SLEEP_MS after hitting OM_CACHE_IN_MAX_APPLY_DURATION_MS)"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheReplicationLag, err = meter.Float64Histogram(
		metricsNamePrefix+"replication.lag",
		otelmetrics.WithDescription("Time elapsed between when a state update is written to state storage and when it is applied to the local cache"),
		otelmetrics.WithUnit("ms"),
		otelmetrics.WithExplicitBucketBoundaries(cacheDurationHistogramBucketsMs...),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	activeTickets := otelCacheActiveTickets
	inactiveTickets := otelCacheInactiveTickets
	assignments := otelCacheAssignments
	incomingBacklog := otelCacheIncomingBacklog
	outgoingBacklog := otelCacheOutgoingBacklog

	// Each time metrics are sampled, get the latest counts of active/inactive
	// tickets, assignments, and incoming/outgoing queue backlogs.
	if packageCallbackRegistration != nil {
		_ = packageCallbackRegistration.Unregister()
		packageCallbackRegistration = nil
	}
	reg, err := meter.RegisterCallback(
		func(ctx context.Context, o otelmetrics.Observer) error {
			o.ObserveInt64(activeTickets, atomic.LoadInt64(&ActiveCount))
			o.ObserveInt64(inactiveTickets, atomic.LoadInt64(&InactiveCount))
			o.ObserveInt64(assignments, atomic.LoadInt64(&AssignmentCount))
			o.ObserveInt64(incomingBacklog, atomic.LoadInt64(&IncomingBacklogCount))
			o.ObserveInt64(outgoingBacklog, atomic.LoadInt64(&OutgoingBacklogCount))
			return nil
		},
		activeTickets,
		inactiveTickets,
		assignments,
		incomingBacklog,
		outgoingBacklog,
	)
	if err != nil {
		panic(err)
	}
	packageCallbackRegistration = reg
}

// RegisterMetrics registers the cache OpenTelemetry metrics on the provided
// meter and binds the observable gauge callbacks to this ReplicatedTicketCache
// instance's atomic counters.
func (tc *ReplicatedTicketCache) RegisterMetrics(meterPointer *otelmetrics.Meter) {
	if tc == nil || meterPointer == nil {
		return
	}
	RegisterMetrics(meterPointer)
	if packageCallbackRegistration != nil {
		_ = packageCallbackRegistration.Unregister()
		packageCallbackRegistration = nil
	}
	meter := *meterPointer

	activeTickets, err := meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"tickets.active",
		otelmetrics.WithDescription("Active tickets that can appear in pools"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	inactiveTickets, err := meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"tickets.inactive",
		otelmetrics.WithDescription("Inactive tickets that won't appear in pools"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	assignments, err := meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"assignments",
		otelmetrics.WithDescription("Assignments in Open Match cache"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	incomingBacklog, err := meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"incoming.backlog",
		otelmetrics.WithDescription("Number of incoming replication updates buffered and waiting to be applied to the local cache"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	outgoingBacklog, err := meter.Int64ObservableUpDownCounter(
		metricsNamePrefix+"outgoing.backlog",
		otelmetrics.WithDescription("Number of outgoing replication updates queued and waiting to be sent to state storage"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	if _, err := meter.RegisterCallback(
		func(ctx context.Context, o otelmetrics.Observer) error {
			o.ObserveInt64(activeTickets, atomic.LoadInt64(&tc.activeCount))
			o.ObserveInt64(inactiveTickets, atomic.LoadInt64(&tc.inactiveCount))
			o.ObserveInt64(assignments, atomic.LoadInt64(&tc.assignmentCount))
			o.ObserveInt64(incomingBacklog, atomic.LoadInt64(&tc.incomingBacklogCount))
			o.ObserveInt64(outgoingBacklog, atomic.LoadInt64(&tc.outgoingBacklogCount))
			return nil
		},
		activeTickets,
		inactiveTickets,
		assignments,
		incomingBacklog,
		outgoingBacklog,
	); err != nil {
		panic(err)
	}
}
