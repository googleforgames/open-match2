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
	"fmt"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/googleforgames/open-match2/v2/internal/config"
	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	memoryReplicator "github.com/googleforgames/open-match2/v2/internal/statestore/memory"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelmetrics "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	testMetricReader  *sdkmetric.ManualReader
	testMeterProvider *sdkmetric.MeterProvider
	testMeter         otelmetrics.Meter
	replIdRegex       = regexp.MustCompile(`^\d{13}-\d+$`)
)

func TestMain(m *testing.M) {
	testMetricReader = sdkmetric.NewManualReader()
	testMeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetricReader))
	testMeter = testMeterProvider.Meter("open-match-cache-test")
	RegisterMetrics(&testMeter)
	os.Exit(m.Run())
}

// scriptedPollReplicator is a fake/spy StateReplicator that returns scripted
// update batches on each GetUpdates call and records call timestamps.
type scriptedPollReplicator struct {
	mu          sync.Mutex
	batches     [][]*store.StateUpdate
	pollTimes   []time.Time
	pollNotify  chan int
	sendHandler func([]*store.StateUpdate) []*store.StateResponse
}

func (r *scriptedPollReplicator) GetUpdates() []*store.StateUpdate {
	r.mu.Lock()
	idx := len(r.pollTimes)
	r.pollTimes = append(r.pollTimes, time.Now())
	var out []*store.StateUpdate
	if idx < len(r.batches) {
		out = r.batches[idx]
	} else {
		out = []*store.StateUpdate{}
	}
	notify := r.pollNotify
	r.mu.Unlock()

	if notify != nil {
		select {
		case notify <- idx + 1:
		default:
		}
	}
	return out
}

func (r *scriptedPollReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	r.mu.Lock()
	fn := r.sendHandler
	r.mu.Unlock()
	if fn != nil {
		return fn(updates)
	}
	res := make([]*store.StateResponse, len(updates))
	ts := time.Now().UnixMilli()
	for i := range updates {
		res[i] = &store.StateResponse{Result: fmt.Sprintf("%013d-%d", ts, i)}
	}
	return res
}

func (r *scriptedPollReplicator) GetReplIdValidator() *regexp.Regexp {
	return replIdRegex
}

func (r *scriptedPollReplicator) pollHistory() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]time.Time, len(r.pollTimes))
	copy(cp, r.pollTimes)
	return cp
}

// spyReplicator wraps a StateReplicator (or generates monotonic IDs when base is nil)
// and records every batch of updates passed to SendUpdates.
type spyReplicator struct {
	mu          sync.Mutex
	base        store.StateReplicator
	sentBatches [][]*store.StateUpdate
	seq         int64
}

func (s *spyReplicator) GetUpdates() []*store.StateUpdate {
	if s.base != nil {
		return s.base.GetUpdates()
	}
	return nil
}

func (s *spyReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	copiedBatch := make([]*store.StateUpdate, len(updates))
	for i, u := range updates {
		if u != nil {
			cp := *u
			if len(u.Keys) > 0 {
				cp.Keys = append([]string(nil), u.Keys...)
			}
			copiedBatch[i] = &cp
		}
	}

	s.mu.Lock()
	s.sentBatches = append(s.sentBatches, copiedBatch)
	s.mu.Unlock()

	if s.base != nil {
		return s.base.SendUpdates(updates)
	}

	res := make([]*store.StateResponse, len(updates))
	nowMs := time.Now().UnixMilli()
	for i := range updates {
		id := atomic.AddInt64(&s.seq, 1)
		res[i] = &store.StateResponse{
			Result: fmt.Sprintf("%013d-%d", nowMs, id),
		}
	}
	return res
}

func (s *spyReplicator) GetReplIdValidator() *regexp.Regexp {
	if s.base != nil {
		return s.base.GetReplIdValidator()
	}
	return replIdRegex
}

func (s *spyReplicator) batches() [][]*store.StateUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]*store.StateUpdate, len(s.sentBatches))
	copy(out, s.sentBatches)
	return out
}

// faultInjectingReplicator injects nil results or truncated response slices.
type faultInjectingReplicator struct {
	responseFunc func([]*store.StateUpdate) []*store.StateResponse
}

func (f *faultInjectingReplicator) GetUpdates() []*store.StateUpdate {
	return nil
}

func (f *faultInjectingReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	if f.responseFunc != nil {
		return f.responseFunc(updates)
	}
	return nil
}

func (f *faultInjectingReplicator) GetReplIdValidator() *regexp.Regexp {
	return replIdRegex
}

func newFastTestConfig() *viper.Viper {
	cfg := config.Read()
	cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 100)
	cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 20)
	cfg.Set("OM_CACHE_IN_POLL_WAIT_MS", 5)
	cfg.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 5)
	cfg.Set("OM_CACHE_IN_QUEUE_BUFFER_SIZE", 2000)
	cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 5)
	cfg.Set("OM_CACHE_IN_FULL_APPLY_SLEEP_MS", 5)
	cfg.Set("OM_CACHE_IN_MAX_APPLY_DURATION_MS", 200)
	cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 10)
	cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 10)
	cfg.Set("OM_CACHE_OUT_QUEUE_BUFFER_SIZE", 500)
	cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", false)
	cfg.Set("OM_CACHE_OUT_MAX_PACKED_KEYS", 500)
	cfg.Set("OM_CACHE_TICKET_TTL_MS", 600000)
	cfg.Set("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS", 600000)
	cfg.Set("OM_CACHE_EXPIRATION_INTERVAL_MS", 1000)
	cfg.Set("OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE", 5000)
	return cfg
}

func marshalTestTicket(t *testing.T, expTime *timestamppb.Timestamp) string {
	t.Helper()
	tk := &pb.Ticket{
		ExpirationTime: expTime,
		Attributes: &pb.Ticket_FilterableData{
			CreationTime: timestamppb.Now(),
			Tags:         []string{"test-tag"},
		},
	}
	raw, err := proto.Marshal(tk)
	require.NoError(t, err)
	return string(raw)
}

func baselineSetDifference(tc *ReplicatedTicketCache) map[any]any {
	diff := make(map[any]any)
	tc.Tickets.Range(func(k, v any) bool {
		if _, inactive := tc.InactiveSet.Load(k); !inactive {
			diff[k] = v
		}
		return true
	})
	return diff
}

// 1. Deactivation Watermark and InactiveSet Wakeup
func TestDeactivationWatermarkAndInactiveSetWakeup(t *testing.T) {
	cfg := newFastTestConfig()
	tc := New(cfg, nil)

	// Empty inputs return false immediately.
	assert.False(t, tc.WaitForDeactivation("", "", 50*time.Millisecond))
	assert.Empty(t, tc.AppliedReplId())
	assert.False(t, tc.HasAppliedReplId(""))
	assert.False(t, tc.HasAppliedReplId("1700000000000-0"))

	// Zero or negative timeout returns false when condition is not met.
	assert.False(t, tc.WaitForDeactivation("missing-ticket", "1700000000000-0", 0))
	assert.False(t, tc.WaitForDeactivation("missing-ticket", "", -1*time.Millisecond))

	// Immediate wakeup via InactiveSet when targetReplId is empty.
	tc.applyIncomingUpdate(logger, store.StateUpdate{
		Cmd: store.Deactivate,
		Key: "1700000000001-0",
	})
	assert.True(t, tc.WaitForDeactivation("1700000000001-0", "", 0))
	// When targetReplId is also required but not yet applied, returns false on zero timeout.
	assert.False(t, tc.WaitForDeactivation("1700000000001-0", "1700000000050-0", 0))

	// Monotonic watermark comparison via AppliedReplId and HasAppliedReplId.
	tc.advanceWatermark("1700000000010-5")
	assert.Equal(t, "1700000000010-5", tc.AppliedReplId())
	assert.True(t, tc.HasAppliedReplId("1700000000009-99"))
	assert.True(t, tc.HasAppliedReplId("1700000000010-4"))
	assert.True(t, tc.HasAppliedReplId("1700000000010-5"))
	assert.False(t, tc.HasAppliedReplId("1700000000010-6"))
	assert.False(t, tc.HasAppliedReplId("1700000000010-12"))

	// Older replication ID or empty batch replication ID must not regress the watermark.
	tc.advanceWatermark("1700000000010-2")
	assert.Equal(t, "1700000000010-5", tc.AppliedReplId())
	tc.advanceWatermark("")
	assert.Equal(t, "1700000000010-5", tc.AppliedReplId())

	// Asynchronous broadcast wakeup when watermark advances.
	var wg sync.WaitGroup
	wg.Add(2)
	var okBoth, okWatermarkOnly atomic.Bool

	go func() {
		defer wg.Done()
		okBoth.Store(tc.WaitForDeactivation("1700000000020-0", "1700000000020-1", 2*time.Second))
	}()
	go func() {
		defer wg.Done()
		okWatermarkOnly.Store(tc.WaitForDeactivation("", "1700000000020-1", 2*time.Second))
	}()

	time.Sleep(15 * time.Millisecond)
	tc.applyIncomingUpdate(logger, store.StateUpdate{
		Cmd:    store.Deactivate,
		Key:    "1700000000020-0",
		ReplId: "1700000000020-1",
	})
	tc.advanceWatermark("1700000000020-1")

	wg.Wait()
	assert.True(t, okBoth.Load())
	assert.True(t, okWatermarkOnly.Load())

	// Timeout expiration when neither InactiveSet nor targetReplId is reached.
	start := time.Now()
	assert.False(t, tc.WaitForDeactivation("never-arrives", "9999999999999-0", 35*time.Millisecond))
	assert.GreaterOrEqual(t, time.Since(start), 25*time.Millisecond)
}

// 2. End-to-End Memory Replication Watermark
func TestEndToEndMemoryReplicationWatermark(t *testing.T) {
	cfg := newFastTestConfig()
	memRep := memoryReplicator.New(cfg)
	tc := New(cfg, memRep)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go tc.OutgoingReplicationQueue(ctx)
	go tc.IncomingReplicationQueue(ctx)

	// Create a ticket through OutgoingReplicationQueue.
	createResChan := make(chan *store.StateResponse, 1)
	tc.UpRequests <- &UpdateRequest{
		Ctx:         ctx,
		ResultsChan: createResChan,
		EnqueuedAt:  time.Now(),
		Update: store.StateUpdate{
			Cmd:   store.Ticket,
			Value: marshalTestTicket(t, nil),
		},
	}

	var createResp *store.StateResponse
	select {
	case createResp = <-createResChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ticket creation response")
	}
	require.NotNil(t, createResp)
	require.NoError(t, createResp.Err)
	ticketId := createResp.Result
	require.NotEmpty(t, ticketId)

	// Deactivate the ticket through OutgoingReplicationQueue.
	deactResChan := make(chan *store.StateResponse, 1)
	tc.UpRequests <- &UpdateRequest{
		Ctx:         ctx,
		ResultsChan: deactResChan,
		EnqueuedAt:  time.Now(),
		Update: store.StateUpdate{
			Cmd: store.Deactivate,
			Key: ticketId,
		},
	}

	var deactResp *store.StateResponse
	select {
	case deactResp = <-deactResChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for deactivation response")
	}
	require.NotNil(t, deactResp)
	require.NoError(t, deactResp.Err)
	require.NotEmpty(t, deactResp.Result)

	// Wait for end-to-end replication via WaitForDeactivation.
	replicated := tc.WaitForDeactivation(ticketId, deactResp.Result, 2*time.Second)
	assert.True(t, replicated)
	assert.True(t, tc.HasAppliedReplId(deactResp.Result))
	_, inInactive := tc.InactiveSet.Load(ticketId)
	assert.True(t, inInactive)
	_, inActive := tc.ActiveTickets.Load(ticketId)
	assert.False(t, inActive)
}

// 3. Full Poll Adaptive Wait
func TestFullPollAdaptiveWait(t *testing.T) {
	cfg := newFastTestConfig()
	cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 4)
	cfg.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 15)
	cfg.Set("OM_CACHE_IN_POLL_WAIT_MS", 2000)
	cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 2000)
	cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 5)

	nowMs := time.Now().UnixMilli()
	batch1 := []*store.StateUpdate{
		{Cmd: store.Deactivate, Key: fmt.Sprintf("%013d-1", nowMs), ReplId: fmt.Sprintf("%013d-1", nowMs)},
		{Cmd: store.Deactivate, Key: fmt.Sprintf("%013d-2", nowMs), ReplId: fmt.Sprintf("%013d-2", nowMs)},
		{Cmd: store.Deactivate, Key: fmt.Sprintf("%013d-3", nowMs), ReplId: fmt.Sprintf("%013d-3", nowMs)},
		{Cmd: store.Deactivate, Key: fmt.Sprintf("%013d-4", nowMs), ReplId: fmt.Sprintf("%013d-4", nowMs)},
	}
	batch2 := []*store.StateUpdate{
		{Cmd: store.Deactivate, Key: fmt.Sprintf("%013d-5", nowMs), ReplId: fmt.Sprintf("%013d-5", nowMs)},
	}

	fakeRep := &scriptedPollReplicator{
		batches:    [][]*store.StateUpdate{batch1, batch2},
		pollNotify: make(chan int, 10),
	}
	tc := New(cfg, fakeRep)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tc.IncomingReplicationQueue(ctx)

	// Wait until at least 2 polls have completed.
	for {
		select {
		case count := <-fakeRep.pollNotify:
			if count >= 2 {
				history := fakeRep.pollHistory()
				require.GreaterOrEqual(t, len(history), 2)
				gap := history[1].Sub(history[0])
				assert.GreaterOrEqual(t, gap, 10*time.Millisecond)
				assert.Less(t, gap, 300*time.Millisecond, "full poll should pause for OM_CACHE_IN_FULL_POLL_WAIT_MS (15ms), not 2000ms")
				return
			}
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for second poll after full poll")
		}
	}
}

// 4. Partial Poll Adaptive Wait
func TestPartialPollAdaptiveWait(t *testing.T) {
	cfg := newFastTestConfig()
	cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 50)
	cfg.Set("OM_CACHE_IN_POLL_WAIT_MS", 15)
	cfg.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 2000)
	cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 2000)
	cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 5)

	nowMs := time.Now().UnixMilli()
	batch1 := []*store.StateUpdate{
		{Cmd: store.Deactivate, Key: fmt.Sprintf("%013d-1", nowMs), ReplId: fmt.Sprintf("%013d-1", nowMs)},
		{Cmd: store.Deactivate, Key: fmt.Sprintf("%013d-2", nowMs), ReplId: fmt.Sprintf("%013d-2", nowMs)},
	}
	batch2 := []*store.StateUpdate{
		{Cmd: store.Deactivate, Key: fmt.Sprintf("%013d-3", nowMs), ReplId: fmt.Sprintf("%013d-3", nowMs)},
	}

	fakeRep := &scriptedPollReplicator{
		batches:    [][]*store.StateUpdate{batch1, batch2},
		pollNotify: make(chan int, 10),
	}
	tc := New(cfg, fakeRep)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tc.IncomingReplicationQueue(ctx)

	for {
		select {
		case count := <-fakeRep.pollNotify:
			if count >= 2 {
				history := fakeRep.pollHistory()
				require.GreaterOrEqual(t, len(history), 2)
				gap := history[1].Sub(history[0])
				assert.GreaterOrEqual(t, gap, 10*time.Millisecond)
				assert.Less(t, gap, 300*time.Millisecond, "partial poll should pause for OM_CACHE_IN_POLL_WAIT_MS (15ms), not 2000ms")
				return
			}
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for second poll after partial poll")
		}
	}
}

// 5. Early Empty Poll Throttling
func TestEarlyEmptyPollThrottling(t *testing.T) {
	cfg := newFastTestConfig()
	cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 80)

	fakeRep := &scriptedPollReplicator{
		batches:    nil, // Always returns empty slice immediately.
		pollNotify: make(chan int, 10),
	}
	tc := New(cfg, fakeRep)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tc.IncomingReplicationQueue(ctx)

	for {
		select {
		case count := <-fakeRep.pollNotify:
			if count >= 2 {
				history := fakeRep.pollHistory()
				require.GreaterOrEqual(t, len(history), 2)
				gap := history[1].Sub(history[0])
				assert.GreaterOrEqual(t, gap, 65*time.Millisecond, "early empty poll must be throttled to remaining OM_CACHE_IN_WAIT_TIMEOUT_MS")
				return
			}
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for throttled second empty poll")
		}
	}
}

// 6. Packed Ticket State Updates
func TestPackedTicketStateUpdates(t *testing.T) {
	cfg := newFastTestConfig()
	cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
	memRep := memoryReplicator.New(cfg)
	tc := New(cfg, memRep)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go tc.OutgoingReplicationQueue(ctx)
	go tc.IncomingReplicationQueue(ctx)

	ticketPayload := marshalTestTicket(t, nil)
	ticketIds := make([]string, 4)
	var lastCreateReplId string
	for i := 0; i < 4; i++ {
		resCh := make(chan *store.StateResponse, 1)
		tc.UpRequests <- &UpdateRequest{
			Ctx:         ctx,
			ResultsChan: resCh,
			EnqueuedAt:  time.Now(),
			Update:      store.StateUpdate{Cmd: store.Ticket, Value: ticketPayload},
		}
		resp := <-resCh
		require.NoError(t, resp.Err)
		ticketIds[i] = resp.Result
		lastCreateReplId = resp.Result
	}
	require.True(t, tc.WaitForDeactivation("", lastCreateReplId, 2*time.Second))
	assert.Len(t, tc.SnapshotActiveTickets(), 4)
	assert.Equal(t, int64(4), tc.ActiveCount())

	// Packed Deactivate for first 3 tickets.
	deactCh := make(chan *store.StateResponse, 1)
	tc.UpRequests <- &UpdateRequest{
		Ctx:         ctx,
		ResultsChan: deactCh,
		EnqueuedAt:  time.Now(),
		Update: store.StateUpdate{
			Cmd:  store.Deactivate,
			Keys: []string{ticketIds[0], ticketIds[1], ticketIds[2]},
		},
	}
	deactResp := <-deactCh
	require.NoError(t, deactResp.Err)
	require.True(t, tc.WaitForDeactivation(ticketIds[2], deactResp.Result, 2*time.Second))

	snapAfterDeact := tc.SnapshotActiveTickets()
	assert.Len(t, snapAfterDeact, 1)
	assert.Contains(t, snapAfterDeact, ticketIds[3])
	for _, id := range ticketIds[:3] {
		_, inInactive := tc.InactiveSet.Load(id)
		assert.True(t, inInactive)
	}
	assert.Equal(t, int64(1), tc.ActiveCount())
	assert.Equal(t, int64(3), tc.InactiveCount())

	// Packed Activate for ticketIds[0] and ticketIds[2].
	actCh := make(chan *store.StateResponse, 1)
	tc.UpRequests <- &UpdateRequest{
		Ctx:         ctx,
		ResultsChan: actCh,
		EnqueuedAt:  time.Now(),
		Update: store.StateUpdate{
			Cmd:  store.Activate,
			Keys: []string{ticketIds[0], ticketIds[2]},
		},
	}
	actResp := <-actCh
	require.NoError(t, actResp.Err)
	require.True(t, tc.WaitForDeactivation("", actResp.Result, 2*time.Second))

	snapAfterAct := tc.SnapshotActiveTickets()
	assert.Len(t, snapAfterAct, 3)
	assert.Contains(t, snapAfterAct, ticketIds[0])
	assert.Contains(t, snapAfterAct, ticketIds[2])
	assert.Contains(t, snapAfterAct, ticketIds[3])
	_, stillInactive := tc.InactiveSet.Load(ticketIds[1])
	assert.True(t, stillInactive)
	assert.Equal(t, int64(3), tc.ActiveCount())
	assert.Equal(t, int64(1), tc.InactiveCount())
}

// 7. Outgoing Request Coalescing Rules
func TestOutgoingRequestCoalescingRules(t *testing.T) {
	t.Run("contiguous_request_coalescing_and_key_deduplication", func(t *testing.T) {
		reqs := []*UpdateRequest{
			{Update: store.StateUpdate{Cmd: store.Deactivate, Key: "k1"}},
			{Update: store.StateUpdate{Cmd: store.Deactivate, Keys: []string{"k2", "k1", "k3"}}},
			{Update: store.StateUpdate{Cmd: store.Deactivate, Key: "k3"}},
			{Update: store.StateUpdate{Cmd: store.Deactivate, Keys: []string{"", "k4", "k2"}}},
		}
		updates, groups := coalesceOutgoingRequests(reqs, 500)
		require.Len(t, updates, 1)
		assert.Equal(t, store.Deactivate, updates[0].Cmd)
		assert.Equal(t, "k1", updates[0].Key)
		assert.Equal(t, []string{"k1", "k2", "k3", "k4"}, updates[0].Keys)
		for i := range reqs {
			assert.Equal(t, []int{0}, groups[i])
		}
	})

	t.Run("causal_run_breaking_on_interleaved_commands", func(t *testing.T) {
		reqs := []*UpdateRequest{
			{Update: store.StateUpdate{Cmd: store.Deactivate, Key: "d1"}},
			{Update: store.StateUpdate{Cmd: store.Deactivate, Key: "d2"}},
			{Update: store.StateUpdate{Cmd: store.Activate, Key: "a1"}},
			{Update: store.StateUpdate{Cmd: store.Activate, Key: "a2"}},
			{Update: store.StateUpdate{Cmd: store.Ticket, Value: "ticket-bytes"}},
			{Update: store.StateUpdate{Cmd: store.Deactivate, Key: "d3"}},
			{Update: store.StateUpdate{Cmd: store.Assign, Key: "as1", Value: "10.0.0.1:7000"}},
			{Update: store.StateUpdate{Cmd: store.Deactivate, Key: "d4"}},
		}
		updates, groups := coalesceOutgoingRequests(reqs, 500)
		require.Len(t, updates, 6)

		assert.Equal(t, store.Deactivate, updates[0].Cmd)
		assert.Equal(t, []string{"d1", "d2"}, updates[0].Keys)
		assert.Equal(t, []int{0}, groups[0])
		assert.Equal(t, []int{0}, groups[1])

		assert.Equal(t, store.Activate, updates[1].Cmd)
		assert.Equal(t, []string{"a1", "a2"}, updates[1].Keys)
		assert.Equal(t, []int{1}, groups[2])
		assert.Equal(t, []int{1}, groups[3])

		assert.Equal(t, store.Ticket, updates[2].Cmd)
		assert.Equal(t, "ticket-bytes", updates[2].Value)
		assert.Equal(t, []int{2}, groups[4])

		assert.Equal(t, store.Deactivate, updates[3].Cmd)
		assert.Equal(t, "d3", updates[3].Key)
		assert.Nil(t, updates[3].Keys)
		assert.Equal(t, []int{3}, groups[5])

		assert.Equal(t, store.Assign, updates[4].Cmd)
		assert.Equal(t, "as1", updates[4].Key)
		assert.Equal(t, []int{4}, groups[6])

		assert.Equal(t, store.Deactivate, updates[5].Cmd)
		assert.Equal(t, "d4", updates[5].Key)
		assert.Nil(t, updates[5].Keys)
		assert.Equal(t, []int{5}, groups[7])
	})

	t.Run("empty_key_isolation", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
		cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 3)
		cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 500)
		memRep := memoryReplicator.New(cfg)
		spy := &spyReplicator{base: memRep}
		tc := New(cfg, spy)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch1 := make(chan *store.StateResponse, 1)
		chEmpty := make(chan *store.StateResponse, 1)
		ch2 := make(chan *store.StateResponse, 1)

		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: ch1, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "valid-1"}}
		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: chEmpty, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "", Keys: []string{"", ""}}}
		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: ch2, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "valid-2"}}

		go tc.OutgoingReplicationQueue(ctx)

		r1 := <-ch1
		rEmpty := <-chEmpty
		r2 := <-ch2

		assert.NoError(t, r1.Err)
		assert.NotEmpty(t, r1.Result)
		assert.ErrorIs(t, rEmpty.Err, store.NoTicketKeyErr)
		assert.NoError(t, r2.Err)
		assert.NotEmpty(t, r2.Result)

		batches := spy.batches()
		require.Len(t, batches, 1)
		require.Len(t, batches[0], 3)
	})

	t.Run("max_packed_keys_splitting", func(t *testing.T) {
		reqs := []*UpdateRequest{
			{Update: store.StateUpdate{Cmd: store.Deactivate, Keys: []string{"k1", "k2"}}},
			{Update: store.StateUpdate{Cmd: store.Deactivate, Keys: []string{"k3", "k4", "k5"}}},
			{Update: store.StateUpdate{Cmd: store.Deactivate, Keys: []string{"k6", "k7"}}},
		}
		updates, groups := coalesceOutgoingRequests(reqs, 3)
		require.Len(t, updates, 3)
		assert.Equal(t, []string{"k1", "k2", "k3"}, updates[0].Keys)
		assert.Equal(t, []string{"k4", "k5", "k6"}, updates[1].Keys)
		assert.Equal(t, "k7", updates[2].Key)
		assert.Nil(t, updates[2].Keys)

		assert.Equal(t, []int{0}, groups[0])
		assert.Equal(t, []int{0, 1}, groups[1])
		assert.Equal(t, []int{1, 2}, groups[2])
	})
}

// 8. Cross-Caller Outgoing Coalescing
func TestCrossCallerOutgoingCoalescing(t *testing.T) {
	const numCallers = 10
	cfg := newFastTestConfig()
	cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
	cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", numCallers)
	cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 5000)
	cfg.Set("OM_CACHE_OUT_MAX_PACKED_KEYS", 500)

	memRep := memoryReplicator.New(cfg)
	spy := &spyReplicator{base: memRep}
	tc := New(cfg, spy)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go tc.OutgoingReplicationQueue(ctx)

	results := make([]*store.StateResponse, numCallers)
	var wg sync.WaitGroup
	wg.Add(numCallers)
	for i := 0; i < numCallers; i++ {
		callerIdx := i
		go func() {
			defer wg.Done()
			resCh := make(chan *store.StateResponse, 1)
			tc.UpRequests <- &UpdateRequest{
				Ctx:         ctx,
				ResultsChan: resCh,
				EnqueuedAt:  time.Now(),
				Update: store.StateUpdate{
					Cmd: store.Deactivate,
					Key: fmt.Sprintf("1700000000000-%d", callerIdx),
				},
			}
			results[callerIdx] = <-resCh
		}()
	}
	wg.Wait()

	batches := spy.batches()
	require.Len(t, batches, 1, "10 concurrent Deactivate callers must be coalesced into 1 SendUpdates call")
	require.Len(t, batches[0], 1, "batch must contain 1 packed StateUpdate")
	assert.Equal(t, store.Deactivate, batches[0][0].Cmd)
	assert.Len(t, batches[0][0].Keys, numCallers)

	expectedReplId := results[0].Result
	require.NotEmpty(t, expectedReplId)
	for i, r := range results {
		require.NotNil(t, r, "caller %d response must be non-nil", i)
		require.NoError(t, r.Err, "caller %d unexpected error", i)
		assert.Equal(t, expectedReplId, r.Result, "all coalesced callers must receive the same replication ID")
	}
}

// 9. Materialized ActiveTickets Equivalence
func TestMaterializedActiveTicketsEquivalence(t *testing.T) {
	cfg := newFastTestConfig()
	cfg.Set("OM_CACHE_TICKET_TTL_MS", 10000)
	tc := New(cfg, nil)

	nowMs := time.Now().UnixMilli()
	expiredId := fmt.Sprintf("%013d-1", nowMs-20000)
	t2Id := fmt.Sprintf("%013d-2", nowMs-1000)
	t3Id := fmt.Sprintf("%013d-3", nowMs-1000)
	t4CustomExpId := fmt.Sprintf("%013d-4", nowMs-1000)
	preDeactId := fmt.Sprintf("%013d-5", nowMs-1000)

	// Step 1: Create tickets (including one with custom past ExpirationTime).
	for _, id := range []string{expiredId, t2Id, t3Id} {
		tc.applyIncomingUpdate(logger, store.StateUpdate{
			Cmd:   store.Ticket,
			Key:   id,
			Value: marshalTestTicket(t, nil),
		})
	}
	tc.applyIncomingUpdate(logger, store.StateUpdate{
		Cmd:   store.Ticket,
		Key:   t4CustomExpId,
		Value: marshalTestTicket(t, timestamppb.New(time.UnixMilli(nowMs-500))),
	})
	assert.Equal(t, baselineSetDifference(tc), tc.SnapshotActiveTickets())
	assert.Equal(t, int64(len(tc.SnapshotActiveTickets())), tc.ActiveCount())

	// Step 2: Pre-deactivate a ticket ID before its creation arrives.
	tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: preDeactId})
	tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Ticket, Key: preDeactId, Value: marshalTestTicket(t, nil)})
	assert.Equal(t, baselineSetDifference(tc), tc.SnapshotActiveTickets())
	assert.NotContains(t, tc.SnapshotActiveTickets(), preDeactId)

	// Step 3: Activate a non-existent ticket ID.
	tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Activate, Key: "non-existent-id"})
	assert.Equal(t, baselineSetDifference(tc), tc.SnapshotActiveTickets())
	assert.NotContains(t, tc.SnapshotActiveTickets(), "non-existent-id")

	// Step 4: Packed deactivation.
	tc.applyIncomingUpdate(logger, store.StateUpdate{
		Cmd:  store.Deactivate,
		Keys: []string{expiredId, t2Id},
	})
	assert.Equal(t, baselineSetDifference(tc), tc.SnapshotActiveTickets())
	assert.Equal(t, int64(len(tc.SnapshotActiveTickets())), tc.ActiveCount())

	// Step 5: Packed activation / re-activation (including non-existent ID).
	tc.applyIncomingUpdate(logger, store.StateUpdate{
		Cmd:  store.Activate,
		Keys: []string{t2Id, preDeactId, "another-ghost-id"},
	})
	assert.Equal(t, baselineSetDifference(tc), tc.SnapshotActiveTickets())
	assert.Equal(t, int64(len(tc.SnapshotActiveTickets())), tc.ActiveCount())

	// Step 6: Run TTL expiration sweep (evicts expiredId from InactiveSet/Tickets and t4CustomExpId via custom ExpirationTime).
	tc.expireCacheEntries(context.Background())
	assert.Equal(t, baselineSetDifference(tc), tc.SnapshotActiveTickets())
	assert.Equal(t, int64(len(tc.SnapshotActiveTickets())), tc.ActiveCount())
	assert.NotContains(t, tc.SnapshotActiveTickets(), expiredId)
	assert.NotContains(t, tc.SnapshotActiveTickets(), t4CustomExpId)
	assert.Contains(t, tc.SnapshotActiveTickets(), t2Id)
	assert.Contains(t, tc.SnapshotActiveTickets(), t3Id)
	assert.Contains(t, tc.SnapshotActiveTickets(), preDeactId)
}

// 10. Min-Heap Expiration Ordering, Re-Activated Ticket Safety, Idempotency, and Chunk Bounds
func TestMinHeapExpirationOrderingSafetyIdempotencyAndChunkBounds(t *testing.T) {
	t.Run("out_of_order_deactivation_custom_expiration_and_reactivated_ticket_safety", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_TICKET_TTL_MS", 10000)
		cfg.Set("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS", 10000)
		tc := New(cfg, nil)

		nowMs := time.Now().UnixMilli()
		freshInactiveId := fmt.Sprintf("%013d-1", nowMs-2000)
		expiredInactiveId := fmt.Sprintf("%013d-2", nowMs-20000)
		reactivatedOldId := fmt.Sprintf("%013d-3", nowMs-25000)
		customExpActiveId := fmt.Sprintf("%013d-4", nowMs-1000)

		for _, id := range []string{freshInactiveId, expiredInactiveId, reactivatedOldId} {
			tc.applyIncomingUpdate(logger, store.StateUpdate{
				Cmd:   store.Ticket,
				Key:   id,
				Value: marshalTestTicket(t, nil),
			})
		}
		tc.applyIncomingUpdate(logger, store.StateUpdate{
			Cmd:   store.Ticket,
			Key:   customExpActiveId,
			Value: marshalTestTicket(t, timestamppb.New(time.UnixMilli(nowMs-500))),
		})

		// Deactivate out of creation-timestamp order: fresh ticket first, then older expired tickets.
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: freshInactiveId})
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: expiredInactiveId})
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: reactivatedOldId})

		// Re-activate reactivatedOldId before expiration sweep runs; its stale entry remains in inactiveHeap.
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Activate, Key: reactivatedOldId})

		tc.expireCacheEntries(context.Background())

		// expiredInactiveId and customExpActiveId must be evicted.
		_, hasExpired := tc.Tickets.Load(expiredInactiveId)
		assert.False(t, hasExpired)
		_, hasExpiredInactive := tc.InactiveSet.Load(expiredInactiveId)
		assert.False(t, hasExpiredInactive)

		_, hasCustomExp := tc.Tickets.Load(customExpActiveId)
		assert.False(t, hasCustomExp)
		_, hasCustomExpActive := tc.ActiveTickets.Load(customExpActiveId)
		assert.False(t, hasCustomExpActive)

		// reactivatedOldId must NOT be evicted when its stale inactiveHeap entry pops.
		_, hasReactivatedTicket := tc.Tickets.Load(reactivatedOldId)
		assert.True(t, hasReactivatedTicket, "re-activated ticket must not be evicted by stale inactive heap entry")
		_, hasReactivatedActive := tc.ActiveTickets.Load(reactivatedOldId)
		assert.True(t, hasReactivatedActive, "re-activated ticket must remain in ActiveTickets")

		// freshInactiveId must remain in Tickets and InactiveSet.
		_, hasFresh := tc.Tickets.Load(freshInactiveId)
		assert.True(t, hasFresh)
		_, hasFreshInactive := tc.InactiveSet.Load(freshInactiveId)
		assert.True(t, hasFreshInactive)

		assert.Equal(t, int64(2), tc.TicketCount())
		assert.Equal(t, int64(1), tc.ActiveCount())
		assert.Equal(t, int64(1), tc.InactiveCount())
	})

	t.Run("duplicate_update_idempotency_on_gauges", func(t *testing.T) {
		cfg := newFastTestConfig()
		tc := New(cfg, nil)

		id := fmt.Sprintf("%013d-1", time.Now().UnixMilli())
		payload := marshalTestTicket(t, nil)

		// Duplicate ticket creation updates.
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Ticket, Key: id, Value: payload})
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Ticket, Key: id, Value: payload})
		assert.Equal(t, int64(1), tc.TicketCount())
		assert.Equal(t, int64(1), tc.ActiveCount())

		// Duplicate deactivate updates.
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: id})
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: id})
		assert.Equal(t, int64(1), tc.TicketCount())
		assert.Equal(t, int64(0), tc.ActiveCount())
		assert.Equal(t, int64(1), tc.InactiveCount())

		// Duplicate activate updates.
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Activate, Key: id})
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Activate, Key: id})
		assert.Equal(t, int64(1), tc.TicketCount())
		assert.Equal(t, int64(1), tc.ActiveCount())
		assert.Equal(t, int64(0), tc.InactiveCount())

		// Duplicate assignment updates.
		assignBytes, err := proto.Marshal(&pb.Assignment{Connection: "10.0.0.1:7777"})
		require.NoError(t, err)
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Assign, Key: id, Value: string(assignBytes)})
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Assign, Key: id, Value: string(assignBytes)})
		assert.Equal(t, int64(1), tc.AssignmentCount())
	})

	t.Run("max_deletes_per_cycle_bounding", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_TICKET_TTL_MS", 1000)
		cfg.Set("OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE", 6)
		tc := New(cfg, nil)

		expiredBaseMs := time.Now().UnixMilli() - 15000
		payload := marshalTestTicket(t, nil)
		for i := 0; i < 15; i++ {
			id := fmt.Sprintf("%013d-%d", expiredBaseMs+int64(i), i)
			tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Ticket, Key: id, Value: payload})
			tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: id})
		}
		assert.Equal(t, int64(15), tc.TicketCount())
		assert.Equal(t, int64(15), tc.InactiveCount())

		tc.expireCacheEntries(context.Background())
		assert.Equal(t, int64(9), tc.TicketCount())
		assert.Equal(t, int64(9), tc.InactiveCount())

		tc.expireCacheEntries(context.Background())
		assert.Equal(t, int64(3), tc.TicketCount())
		assert.Equal(t, int64(3), tc.InactiveCount())

		tc.expireCacheEntries(context.Background())
		assert.Equal(t, int64(0), tc.TicketCount())
		assert.Equal(t, int64(0), tc.InactiveCount())
	})

	t.Run("multi_chunk_expiration_across_300_expired_tickets", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_TICKET_TTL_MS", 1000)
		cfg.Set("OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE", 5000)
		tc := New(cfg, nil)

		expiredBaseMs := time.Now().UnixMilli() - 20000
		payload := marshalTestTicket(t, nil)
		for i := 0; i < 300; i++ {
			id := fmt.Sprintf("%013d-%d", expiredBaseMs+int64(i), i)
			tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Ticket, Key: id, Value: payload})
			tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: id})
		}
		assert.Equal(t, int64(300), tc.TicketCount())
		assert.Equal(t, int64(300), tc.InactiveCount())

		// 300 > expirationChunkOpsLimit (256), exercising multi-chunk mutex release/re-acquire.
		tc.expireCacheEntries(context.Background())
		assert.Equal(t, int64(0), tc.TicketCount())
		assert.Equal(t, int64(0), tc.InactiveCount())
		assert.Equal(t, int64(0), tc.ActiveCount())
	})
}

// 11. Defensive Outgoing Result Synthesis
func TestDefensiveOutgoingResultSynthesis(t *testing.T) {
	t.Run("unpacked_nil_result", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", false)
		cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 1)
		rep := &faultInjectingReplicator{
			responseFunc: func(updates []*store.StateUpdate) []*store.StateResponse {
				return []*store.StateResponse{nil}
			},
		}
		tc := New(cfg, rep)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go tc.OutgoingReplicationQueue(ctx)

		resCh := make(chan *store.StateResponse, 1)
		tc.UpRequests <- &UpdateRequest{
			Ctx:         ctx,
			ResultsChan: resCh,
			Update:      store.StateUpdate{Cmd: store.Deactivate, Key: "t-unpacked-nil"},
		}
		resp := <-resCh
		require.NotNil(t, resp)
		assert.Equal(t, "t-unpacked-nil", resp.Result)
		require.Error(t, resp.Err)
		assert.Equal(t, "replicator returned nil result for update", resp.Err.Error())
	})

	t.Run("unpacked_fewer_results_than_requested", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", false)
		cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 2)
		cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 500)
		rep := &faultInjectingReplicator{
			responseFunc: func(updates []*store.StateUpdate) []*store.StateResponse {
				return []*store.StateResponse{{Result: "1700000000000-0"}}
			},
		}
		tc := New(cfg, rep)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch1 := make(chan *store.StateResponse, 1)
		ch2 := make(chan *store.StateResponse, 1)
		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: ch1, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "t-unpacked-1"}}
		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: ch2, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "t-unpacked-2"}}

		go tc.OutgoingReplicationQueue(ctx)

		r1 := <-ch1
		r2 := <-ch2
		require.NotNil(t, r1)
		assert.NoError(t, r1.Err)
		require.NotNil(t, r2)
		assert.Equal(t, "t-unpacked-2", r2.Result)
		require.Error(t, r2.Err)
		assert.Equal(t, "replicator returned fewer results than requested", r2.Err.Error())
	})

	t.Run("packed_nil_result", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
		cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 2)
		cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 500)
		rep := &faultInjectingReplicator{
			responseFunc: func(updates []*store.StateUpdate) []*store.StateResponse {
				return []*store.StateResponse{nil}
			},
		}
		tc := New(cfg, rep)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch1 := make(chan *store.StateResponse, 1)
		ch2 := make(chan *store.StateResponse, 1)
		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: ch1, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "t-packed-nil-1"}}
		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: ch2, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "t-packed-nil-2"}}

		go tc.OutgoingReplicationQueue(ctx)

		r1 := <-ch1
		r2 := <-ch2
		require.NotNil(t, r1)
		assert.Equal(t, "t-packed-nil-1", r1.Result)
		require.Error(t, r1.Err)
		assert.Equal(t, "replicator returned nil result for update", r1.Err.Error())

		require.NotNil(t, r2)
		assert.Equal(t, "t-packed-nil-2", r2.Result)
		require.Error(t, r2.Err)
		assert.Equal(t, "replicator returned nil result for update", r2.Err.Error())
	})

	t.Run("packed_fewer_results_than_requested", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
		cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 2)
		cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 500)
		rep := &faultInjectingReplicator{
			responseFunc: func(updates []*store.StateUpdate) []*store.StateResponse {
				return []*store.StateResponse{}
			},
		}
		tc := New(cfg, rep)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch1 := make(chan *store.StateResponse, 1)
		ch2 := make(chan *store.StateResponse, 1)
		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: ch1, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "t-packed-few-1"}}
		tc.UpRequests <- &UpdateRequest{Ctx: ctx, ResultsChan: ch2, Update: store.StateUpdate{Cmd: store.Deactivate, Key: "t-packed-few-2"}}

		go tc.OutgoingReplicationQueue(ctx)

		r1 := <-ch1
		r2 := <-ch2
		require.NotNil(t, r1)
		assert.Equal(t, "t-packed-few-1", r1.Result)
		require.Error(t, r1.Err)
		assert.Equal(t, "replicator returned fewer results than requested", r1.Err.Error())

		require.NotNil(t, r2)
		assert.Equal(t, "t-packed-few-2", r2.Result)
		require.Error(t, r2.Err)
		assert.Equal(t, "replicator returned fewer results than requested", r2.Err.Error())
	})
}

// 12. Active Ticket Gauge Accuracy and Per-Instance Isolation
func TestActiveTicketGaugeAccuracyAndPerInstanceIsolation(t *testing.T) {
	t.Run("active_count_never_negative_when_inactive_exceeds_tickets", func(t *testing.T) {
		cfg := newFastTestConfig()
		tc := New(cfg, nil)

		// Deactivate 5 non-existent ticket IDs so InactiveCount (5) > TicketCount (0).
		for i := 0; i < 5; i++ {
			tc.applyIncomingUpdate(logger, store.StateUpdate{
				Cmd: store.Deactivate,
				Key: fmt.Sprintf("1700000000000-%d", i),
			})
		}
		assert.Equal(t, int64(0), tc.TicketCount())
		assert.Equal(t, int64(5), tc.InactiveCount())
		assert.Equal(t, int64(0), tc.ActiveCount())
		assert.Equal(t, int64(0), atomic.LoadInt64(&ActiveCount))

		// Create 2 active tickets; ActiveCount must be 2 (not 2 - 5 = -3).
		for i := 10; i < 12; i++ {
			tc.applyIncomingUpdate(logger, store.StateUpdate{
				Cmd:   store.Ticket,
				Key:   fmt.Sprintf("1700000000000-%d", i),
				Value: marshalTestTicket(t, nil),
			})
		}
		assert.Equal(t, int64(2), tc.TicketCount())
		assert.Equal(t, int64(5), tc.InactiveCount())
		assert.Equal(t, int64(2), tc.ActiveCount())
	})

	t.Run("gauges_update_immediately_on_apply", func(t *testing.T) {
		cfg := newFastTestConfig()
		tc := New(cfg, nil)

		id := "1700000000100-1"
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Ticket, Key: id, Value: marshalTestTicket(t, nil)})
		assert.Equal(t, int64(1), tc.ActiveCount())
		assert.Equal(t, int64(1), tc.TicketCount())
		assert.Equal(t, int64(1), atomic.LoadInt64(&ActiveCount))
		assert.Equal(t, int64(1), atomic.LoadInt64(&TicketCount))

		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: id})
		assert.Equal(t, int64(0), tc.ActiveCount())
		assert.Equal(t, int64(1), tc.InactiveCount())
		assert.Equal(t, int64(0), atomic.LoadInt64(&ActiveCount))
		assert.Equal(t, int64(1), atomic.LoadInt64(&InactiveCount))

		assignRaw, err := proto.Marshal(&pb.Assignment{Connection: "10.0.0.1:9000"})
		require.NoError(t, err)
		tc.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Assign, Key: id, Value: string(assignRaw)})
		assert.Equal(t, int64(1), tc.AssignmentCount())
		assert.Equal(t, int64(1), atomic.LoadInt64(&AssignmentCount))
	})

	t.Run("per_instance_register_metrics_isolation", func(t *testing.T) {
		// Allow any background poller timers from prior subtests to exit before re-binding metric globals.
		time.Sleep(50 * time.Millisecond)

		cfg := newFastTestConfig()
		tc1 := New(cfg, nil)
		tc2 := New(cfg, nil)

		reader1 := sdkmetric.NewManualReader()
		mp1 := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader1))
		m1 := mp1.Meter("cache-instance-1")
		tc1.RegisterMetrics(&m1)

		reader2 := sdkmetric.NewManualReader()
		mp2 := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader2))
		m2 := mp2.Meter("cache-instance-2")
		tc2.RegisterMetrics(&m2)

		// Populate tc1 with 3 active tickets and 1 inactive ticket.
		for i := 0; i < 4; i++ {
			tc1.applyIncomingUpdate(logger, store.StateUpdate{
				Cmd:   store.Ticket,
				Key:   fmt.Sprintf("1700000001000-%d", i),
				Value: marshalTestTicket(t, nil),
			})
		}
		tc1.applyIncomingUpdate(logger, store.StateUpdate{Cmd: store.Deactivate, Key: "1700000001000-0"})

		// Populate tc2 with 1 active ticket and 2 inactive tickets (mutating globals after tc1).
		for i := 0; i < 3; i++ {
			tc2.applyIncomingUpdate(logger, store.StateUpdate{
				Cmd:   store.Ticket,
				Key:   fmt.Sprintf("1700000002000-%d", i),
				Value: marshalTestTicket(t, nil),
			})
		}
		tc2.applyIncomingUpdate(logger, store.StateUpdate{
			Cmd:  store.Deactivate,
			Keys: []string{"1700000002000-0", "1700000002000-1"},
		})

		// Re-registering package metrics on testMeter keeps global testMeter bound for later tests.
		defer RegisterMetrics(&testMeter)

		findGaugeValue := func(rm metricdata.ResourceMetrics, name string) (int64, bool) {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name == name {
						if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
							return sum.DataPoints[len(sum.DataPoints)-1].Value, true
						}
					}
				}
			}
			return 0, false
		}

		var rm1, rm2 metricdata.ResourceMetrics
		require.NoError(t, reader1.Collect(context.Background(), &rm1))
		require.NoError(t, reader2.Collect(context.Background(), &rm2))

		act1, ok := findGaugeValue(rm1, "om_core.cache.tickets.active")
		require.True(t, ok)
		inact1, ok := findGaugeValue(rm1, "om_core.cache.tickets.inactive")
		require.True(t, ok)
		assert.Equal(t, int64(3), act1)
		assert.Equal(t, int64(1), inact1)

		act2, ok := findGaugeValue(rm2, "om_core.cache.tickets.active")
		require.True(t, ok)
		inact2, ok := findGaugeValue(rm2, "om_core.cache.tickets.inactive")
		require.True(t, ok)
		assert.Equal(t, int64(1), act2)
		assert.Equal(t, int64(2), inact2)
	})
}

// 13. Graceful Shutdown and Outgoing Queue Drain on Context Cancellation
func TestGracefulShutdownAndOutgoingQueueDrain(t *testing.T) {
	t.Run("clean_shutdown_within_250ms_on_context_cancellation", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 5000)
		cfg.Set("OM_CACHE_IN_POLL_WAIT_MS", 5000)
		cfg.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 5000)
		cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 5000)
		cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 5000)
		cfg.Set("OM_CACHE_EXPIRATION_INTERVAL_MS", 5000)

		fakeRep := &scriptedPollReplicator{}
		tc := New(cfg, fakeRep)

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			tc.OutgoingReplicationQueue(ctx)
		}()
		go func() {
			defer wg.Done()
			tc.IncomingReplicationQueue(ctx)
		}()

		time.Sleep(20 * time.Millisecond)
		start := time.Now()
		cancel()

		doneCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(doneCh)
		}()

		select {
		case <-doneCh:
			assert.Less(t, time.Since(start), 250*time.Millisecond)
		case <-time.After(250 * time.Millisecond):
			t.Fatal("OutgoingReplicationQueue and IncomingReplicationQueue did not exit within 250ms")
		}
	})

	t.Run("flushes_pre_queued_up_requests_on_cancellation_before_exit", func(t *testing.T) {
		const totalQueued = 7
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 3)
		cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 5000)

		spy := &spyReplicator{}
		tc := New(cfg, spy)

		ctx, cancel := context.WithCancel(context.Background())
		resChans := make([]chan *store.StateResponse, totalQueued)
		for i := 0; i < totalQueued; i++ {
			resChans[i] = make(chan *store.StateResponse, 1)
			tc.UpRequests <- &UpdateRequest{
				Ctx:         ctx,
				ResultsChan: resChans[i],
				EnqueuedAt:  time.Now(),
				Update: store.StateUpdate{
					Cmd: store.Deactivate,
					Key: fmt.Sprintf("1700000000000-%d", i),
				},
			}
		}

		// Cancel context immediately so OutgoingReplicationQueue must drain all 7 items across multiple iterations before returning.
		cancel()

		done := make(chan struct{})
		go func() {
			tc.OutgoingReplicationQueue(ctx)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("OutgoingReplicationQueue did not finish draining pre-queued requests on cancelled context")
		}

		for i, ch := range resChans {
			select {
			case resp := <-ch:
				require.NotNil(t, resp, "request %d response must be non-nil", i)
				assert.NoError(t, resp.Err)
				assert.NotEmpty(t, resp.Result)
			default:
				t.Fatalf("request %d did not receive a flushed response", i)
			}
		}
		assert.Equal(t, int64(0), tc.OutgoingBacklogCount())
	})

	t.Run("unblocks_incoming_poller_when_local_channel_is_full", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_IN_QUEUE_BUFFER_SIZE", 2)
		cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 10)
		cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 10000)

		// Poll 1 returns 6 updates, while local channel capacity is 2 and applier is asleep for 10s.
		batch := make([]*store.StateUpdate, 6)
		for i := 0; i < 6; i++ {
			batch[i] = &store.StateUpdate{
				Cmd:    store.Deactivate,
				Key:    fmt.Sprintf("1700000000000-%d", i),
				ReplId: fmt.Sprintf("1700000000000-%d", i),
			}
		}
		fakeRep := &scriptedPollReplicator{
			batches:    [][]*store.StateUpdate{batch},
			pollNotify: make(chan int, 4),
		}
		tc := New(cfg, fakeRep)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			tc.IncomingReplicationQueue(ctx)
			close(done)
		}()

		// Wait until poll 1 has started and filled the 2-element channel.
		<-fakeRep.pollNotify
		require.Eventually(t, func() bool {
			return tc.IncomingBacklogCount() == 2
		}, 500*time.Millisecond, 5*time.Millisecond)

		start := time.Now()
		cancel()

		select {
		case <-done:
			assert.Less(t, time.Since(start), 250*time.Millisecond)
		case <-time.After(250 * time.Millisecond):
			t.Fatal("IncomingReplicationQueue blocked on full local channel did not exit within 250ms")
		}
	})
}

// 14. Cold-Start Initial Catch-Up Barrier
func TestColdStartInitialCatchUpBarrier(t *testing.T) {
	t.Run("immediate_catch_up_on_empty_store", func(t *testing.T) {
		cfg := newFastTestConfig()
		fakeRep := &scriptedPollReplicator{
			batches: nil, // Empty store on first poll.
		}
		tc := New(cfg, fakeRep)
		assert.False(t, tc.IsCaughtUp())

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go tc.IncomingReplicationQueue(ctx)

		waitCtx, waitCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer waitCancel()
		require.NoError(t, tc.WaitForCatchUp(waitCtx))
		assert.True(t, tc.IsCaughtUp())
	})

	t.Run("multi_poll_backlog_catch_up_wakes_applier_immediately", func(t *testing.T) {
		cfg := newFastTestConfig()
		cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 3)
		cfg.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 5)
		cfg.Set("OM_CACHE_IN_POLL_WAIT_MS", 5)
		// Configure a 5,000ms applier sleep to verify catchUpWake wakes the applier immediately.
		cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 5000)

		payload := marshalTestTicket(t, nil)
		nowMs := time.Now().UnixMilli()
		makeTicketUpdate := func(seq int) *store.StateUpdate {
			id := fmt.Sprintf("%013d-%d", nowMs, seq)
			return &store.StateUpdate{Cmd: store.Ticket, Key: id, Value: payload, ReplId: id}
		}

		// 2 full polls of 3 updates + 1 partial poll of 1 update = 7 updates total.
		fakeRep := &scriptedPollReplicator{
			batches: [][]*store.StateUpdate{
				{makeTicketUpdate(1), makeTicketUpdate(2), makeTicketUpdate(3)},
				{makeTicketUpdate(4), makeTicketUpdate(5), makeTicketUpdate(6)},
				{makeTicketUpdate(7)},
			},
		}
		tc := New(cfg, fakeRep)

		ctx, cancel := context.WithCancel(context.Background())
		inDone := make(chan struct{})

		start := time.Now()
		go func() {
			tc.IncomingReplicationQueue(ctx)
			close(inDone)
		}()
		defer func() {
			cancel()
			<-inDone
		}()

		waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
		defer waitCancel()
		require.NoError(t, tc.WaitForCatchUp(waitCtx))
		elapsed := time.Since(start)

		assert.Less(t, elapsed, 500*time.Millisecond, "catchUpWake must wake applier without waiting 5000ms")
		assert.True(t, tc.IsCaughtUp())
		assert.Equal(t, int64(7), tc.ActiveCount())
		assert.Len(t, tc.SnapshotActiveTickets(), 7)
	})

	t.Run("wait_for_catch_up_context_cancellation", func(t *testing.T) {
		cfg := newFastTestConfig()
		tc := New(cfg, nil)
		assert.False(t, tc.IsCaughtUp())

		waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer waitCancel()
		err := tc.WaitForCatchUp(waitCtx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.False(t, tc.IsCaughtUp())

		// Verify markCaughtUp and re-initialization via Init reset the barrier cleanly.
		tc.markCaughtUp()
		assert.True(t, tc.IsCaughtUp())
		tc.Init(cfg, nil)
		assert.False(t, tc.IsCaughtUp())
	})
}

// 15. Sub-Millisecond Histogram Bucket Boundaries
func TestSubMillisecondHistogramBucketBoundaries(t *testing.T) {
	expectedBuckets := []float64{
		0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000,
	}
	require.Len(t, cacheDurationHistogramBucketsMs, 17)
	assert.Equal(t, expectedBuckets, cacheDurationHistogramBucketsMs)

	ctx := context.Background()

	var beforeRM metricdata.ResourceMetrics
	require.NoError(t, testMetricReader.Collect(ctx, &beforeRM))
	beforeBucket2 := make(map[string]uint64)
	for _, sm := range beforeRM.ScopeMetrics {
		for _, m := range sm.Metrics {
			if histData, ok := m.Data.(metricdata.Histogram[float64]); ok && len(histData.DataPoints) > 0 {
				dp := histData.DataPoints[0]
				if len(dp.BucketCounts) > 2 {
					beforeBucket2[m.Name] = dp.BucketCounts[2]
				}
			}
		}
	}

	subMsObservation := durationMs(150 * time.Microsecond) // 0.15ms -> bucket (0.1, 0.25] at index 2
	require.InDelta(t, 0.15, subMsObservation, 1e-6)

	otelCacheExpirationCycleDuration.Record(ctx, subMsObservation)
	otelCacheOutgoingQueueWaitDuration.Record(ctx, subMsObservation)
	otelCacheOutgoingSendDuration.Record(ctx, subMsObservation)
	otelCacheIncomingPollDuration.Record(ctx, subMsObservation)
	otelCacheIncomingPollWaitDuration.Record(ctx, subMsObservation)
	otelCacheIncomingQueueWaitDuration.Record(ctx, subMsObservation)
	otelCacheIncomingApplyDuration.Record(ctx, subMsObservation)
	otelCacheIncomingApplySleepDuration.Record(ctx, subMsObservation)
	otelCacheReplicationLag.Record(ctx, subMsObservation)

	var rm metricdata.ResourceMetrics
	require.NoError(t, testMetricReader.Collect(ctx, &rm))

	expectedHistograms := []string{
		"om_core.cache.expiration.duration",
		"om_core.cache.outgoing.queue.wait",
		"om_core.cache.outgoing.send.duration",
		"om_core.cache.incoming.poll.duration",
		"om_core.cache.incoming.poll.wait",
		"om_core.cache.incoming.queue.wait",
		"om_core.cache.incoming.apply.duration",
		"om_core.cache.incoming.apply.sleep",
		"om_core.cache.replication.lag",
	}

	found := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found[m.Name] = m
		}
	}

	for _, histName := range expectedHistograms {
		m, ok := found[histName]
		require.True(t, ok, "expected histogram %s to be registered and collected", histName)
		assert.Equal(t, "ms", m.Unit, "histogram %s must have unit ms", histName)

		histData, ok := m.Data.(metricdata.Histogram[float64])
		require.True(t, ok, "metric %s must be a Float64Histogram", histName)
		require.NotEmpty(t, histData.DataPoints, "histogram %s must have at least one data point", histName)

		dp := histData.DataPoints[0]
		assert.Equal(t, expectedBuckets, dp.Bounds, "histogram %s must use the 17-element sub-ms bucket boundaries", histName)
		// 0.15ms falls into Bounds[1] (0.1) < 0.15 <= Bounds[2] (0.25), which is BucketCounts[2].
		require.Greater(t, len(dp.BucketCounts), 2)
		assert.GreaterOrEqual(t, dp.BucketCounts[2]-beforeBucket2[histName], uint64(1),
			"histogram %s must record 0.15ms into bucket (0.1, 0.25]", histName)
	}
}
