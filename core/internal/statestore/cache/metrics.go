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

	"github.com/sirupsen/logrus"
	otelmetrics "go.opentelemetry.io/otel/metric"
)

const metricsNamePrefix = "om_core.cache."

var (
	otelLogger = logrus.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "replicationQueue",
	})

	// Expiration metrics.
	otelCacheExpirationCycleDuration    otelmetrics.Float64Histogram
	otelCacheAssignmentsExpiredPerCycle otelmetrics.Int64Histogram
	otelCacheInactivesExpiredPerCycle   otelmetrics.Int64Histogram
	otelCacheTicketsExpiredPerCycle     otelmetrics.Int64Histogram

	// Outgoing replication queue metrics.
	otelCacheOutgoingUpdatesPerPoll        otelmetrics.Int64Histogram
	otelCacheOutgoingQueueTimeouts         otelmetrics.Int64Counter
	otelCacheOutgoingThresholdReachedCount otelmetrics.Int64Counter

	// Incoming replication queue metrics.
	otelCacheIncomingPerPoll            otelmetrics.Int64Histogram
	otelCacheIncomingEmptyTimeouts      otelmetrics.Int64Counter
	otelCacheIncomingProcessingTimeouts otelmetrics.Int64Counter

	// Metric variable declarations are global to the module, so they can be
	// accessed directly in the module code.
	// The live counters that the metrics observe.
	TicketCount     int64
	InactiveCount   int64
	AssignmentCount int64

	// Observable counters. These aren't updated in real-time, and
	// only get recorded with a callback function, when observed.
	otelCacheActiveTickets   otelmetrics.Int64ObservableUpDownCounter
	otelCacheInactiveTickets otelmetrics.Int64ObservableUpDownCounter
	otelCacheAssignments     otelmetrics.Int64ObservableUpDownCounter
)

//nolint:cyclop // Cyclop linter sees each metric initialization as +1 cyclomatic complexity for some reason.
func RegisterMetrics(meterPointer *otelmetrics.Meter) {
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

	otelCacheExpirationCycleDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"expiration.duration",
		otelmetrics.WithDescription("Duration of cache expiration logic"),
		otelmetrics.WithUnit("ms"),
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

	otelCacheIncomingEmptyTimeouts, err = meter.Int64Counter(
		metricsNamePrefix+"incoming.timeouts.empty",
		otelmetrics.WithDescription("Number of times the incoming replication queue saw no updates after waiting for OM_CACHE_IN_WAIT_TIMEOUT_MS"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCacheIncomingProcessingTimeouts, err = meter.Int64Counter(
		metricsNamePrefix+"incoming.timeouts.full",
		otelmetrics.WithDescription("Number of times the incoming replication queue couldn't process all pending updates in 500ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// Each time metrics are sampled, get the latest counts of active/inactive
	// tickets and assignments.
	if _, err := meter.RegisterCallback(
		func(ctx context.Context, o otelmetrics.Observer) error {
			o.ObserveInt64(otelCacheActiveTickets, TicketCount-InactiveCount)
			o.ObserveInt64(otelCacheInactiveTickets, InactiveCount)
			o.ObserveInt64(otelCacheAssignments, AssignmentCount)
			return nil
		},
		otelCacheActiveTickets, otelCacheInactiveTickets, otelCacheAssignments,
	); err != nil {
		panic(err)
	}
}
