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
	"container/heap"
	"context"
	"errors"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"

	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

const expirationChunkOpsLimit = 256

var (
	logger = logrus.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "cache",
	})
)

type expirationHeapEntry struct {
	id         string
	deadlineMs int64
}

type expirationMinHeap []expirationHeapEntry

func (h expirationMinHeap) Len() int           { return len(h) }
func (h expirationMinHeap) Less(i, j int) bool { return h[i].deadlineMs < h[j].deadlineMs }
func (h expirationMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *expirationMinHeap) Push(x any) {
	*h = append(*h, x.(expirationHeapEntry))
}

func (h *expirationMinHeap) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	*h = old[:n-1]
	return entry
}

// cache.UpdateRequest is basically a wrapper around a StateUpdate (in
// statestorage/datatypes/store.go) that adds context and a channel where
// results should go. The state storage layer shouldn't need to understand the
// underlying context, or where the update request originated (which is where
// it will return). These are necessary for the gRPC server in om-core,
// however!
type UpdateRequest struct {
	Ctx context.Context
	// The update itself.
	Update store.StateUpdate
	// Return channel to confirm the write by sending back the assigned ticket ID
	ResultsChan chan *store.StateResponse
	// Timestamp when the update request was enqueued.
	EnqueuedAt time.Time
}

// The server instantiates a replicatedTicketCache on startup, and all
// ticket-reading functionality reads from this local cache. The cache also has
// the necessary data structures for replicating ticket cache changes that come
// in to this instance by it handling gRPC calls. This data structure contains
// sync.Map members, so it should not be copied after instantiation (see
// https://pkg.go.dev/sync#Map)
type ReplicatedTicketCache struct {
	// Local copies of all the state data.
	Tickets       sync.Map
	InactiveSet   sync.Map
	Assignments   sync.Map
	ActiveTickets sync.Map

	// How this replicatedTicketCache is replicated.
	Replicator store.StateReplicator
	// The queue of cache updates
	UpRequests chan *UpdateRequest

	//
	IdValidator *regexp.Regexp

	// Application config
	Cfg *viper.Viper

	// Watermark synchronization for event-driven WaitForDeactivation.
	watermarkMu     sync.RWMutex
	appliedReplId   string
	watermarkNotify chan struct{}

	// Initial startup catch-up barrier.
	catchUpOnce  sync.Once
	caughtUpChan chan struct{}

	// Cache state and deadline-ordered min-heap synchronization.
	stateMu          sync.Mutex
	inactiveHeap     expirationMinHeap
	ticketExpHeap    expirationMinHeap
	assignmentHeap   expirationMinHeap
	inInactiveHeap   map[string]struct{}
	inAssignmentHeap map[string]struct{}
	liveActive       int64
	liveTickets      int64
	liveInactive     int64
	liveAssignments  int64

	// Per-instance atomic counters for gauges and inspection.
	activeCount          int64
	ticketCount          int64
	inactiveCount        int64
	assignmentCount      int64
	incomingBacklogCount int64
	outgoingBacklogCount int64
}

// Init initializes or re-initializes the ReplicatedTicketCache with the given
// configuration and state replicator.
func (tc *ReplicatedTicketCache) Init(cfg *viper.Viper, replicator store.StateReplicator) {
	tc.Cfg = cfg
	tc.Replicator = replicator

	outBufSize := tc.cfgPositiveIntOrDefault("OM_CACHE_OUT_QUEUE_BUFFER_SIZE", 500)

	tc.stateMu.Lock()
	tc.UpRequests = make(chan *UpdateRequest, outBufSize)
	tc.inInactiveHeap = make(map[string]struct{})
	tc.inAssignmentHeap = make(map[string]struct{})
	heap.Init(&tc.inactiveHeap)
	heap.Init(&tc.ticketExpHeap)
	heap.Init(&tc.assignmentHeap)
	tc.stateMu.Unlock()

	tc.watermarkMu.Lock()
	tc.watermarkNotify = make(chan struct{})
	tc.watermarkMu.Unlock()

	tc.catchUpOnce = sync.Once{}
	tc.caughtUpChan = make(chan struct{})
}

// New constructs and initializes a new ReplicatedTicketCache.
func New(cfg *viper.Viper, replicator store.StateReplicator) *ReplicatedTicketCache {
	tc := &ReplicatedTicketCache{}
	tc.Init(cfg, replicator)
	return tc
}

func (tc *ReplicatedTicketCache) markCaughtUp() {
	tc.catchUpOnce.Do(func() {
		if tc.caughtUpChan != nil {
			close(tc.caughtUpChan)
		}
	})
}

// IsCaughtUp reports whether the initial startup replication catch-up barrier
// has completed.
func (tc *ReplicatedTicketCache) IsCaughtUp() bool {
	if tc == nil || tc.caughtUpChan == nil {
		return false
	}
	select {
	case <-tc.caughtUpChan:
		return true
	default:
		return false
	}
}

// WaitForCatchUp blocks until the initial startup replication catch-up barrier
// completes or ctx is cancelled.
func (tc *ReplicatedTicketCache) WaitForCatchUp(ctx context.Context) error {
	if tc.IsCaughtUp() {
		return nil
	}
	if tc == nil || tc.caughtUpChan == nil {
		return ctx.Err()
	}
	select {
	case <-tc.caughtUpChan:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ActiveCount returns the current number of active tickets in this cache instance.
func (tc *ReplicatedTicketCache) ActiveCount() int64 {
	return atomic.LoadInt64(&tc.activeCount)
}

// TicketCount returns the current total number of tickets in this cache instance.
func (tc *ReplicatedTicketCache) TicketCount() int64 {
	return atomic.LoadInt64(&tc.ticketCount)
}

// InactiveCount returns the current number of inactive entries in this cache instance.
func (tc *ReplicatedTicketCache) InactiveCount() int64 {
	return atomic.LoadInt64(&tc.inactiveCount)
}

// AssignmentCount returns the current number of assignments in this cache instance.
func (tc *ReplicatedTicketCache) AssignmentCount() int64 {
	return atomic.LoadInt64(&tc.assignmentCount)
}

// IncomingBacklogCount returns the current number of buffered incoming updates
// waiting to be applied to this cache instance.
func (tc *ReplicatedTicketCache) IncomingBacklogCount() int64 {
	return atomic.LoadInt64(&tc.incomingBacklogCount)
}

// OutgoingBacklogCount returns the current number of queued outgoing updates
// waiting to be sent to state storage from this cache instance.
func (tc *ReplicatedTicketCache) OutgoingBacklogCount() int64 {
	return atomic.LoadInt64(&tc.outgoingBacklogCount)
}

func (tc *ReplicatedTicketCache) updateGaugeCountsLocked() {
	if tc.liveActive < 0 {
		tc.liveActive = 0
	}
	if tc.liveTickets < 0 {
		tc.liveTickets = 0
	}
	if tc.liveInactive < 0 {
		tc.liveInactive = 0
	}
	if tc.liveAssignments < 0 {
		tc.liveAssignments = 0
	}

	atomic.StoreInt64(&tc.activeCount, tc.liveActive)
	atomic.StoreInt64(&tc.ticketCount, tc.liveTickets)
	atomic.StoreInt64(&tc.inactiveCount, tc.liveInactive)
	atomic.StoreInt64(&tc.assignmentCount, tc.liveAssignments)

	atomic.StoreInt64(&ActiveCount, tc.liveActive)
	atomic.StoreInt64(&TicketCount, tc.liveTickets)
	atomic.StoreInt64(&InactiveCount, tc.liveInactive)
	atomic.StoreInt64(&AssignmentCount, tc.liveAssignments)
}

func (tc *ReplicatedTicketCache) setIncomingBacklogCount(count int64) {
	atomic.StoreInt64(&tc.incomingBacklogCount, count)
	atomic.StoreInt64(&IncomingBacklogCount, count)
}

func (tc *ReplicatedTicketCache) setOutgoingBacklogCount(count int64) {
	atomic.StoreInt64(&tc.outgoingBacklogCount, count)
	atomic.StoreInt64(&OutgoingBacklogCount, count)
}

// SnapshotActiveTickets returns a point-in-time snapshot of all currently
// active tickets without acquiring the cache mutex.
func (tc *ReplicatedTicketCache) SnapshotActiveTickets() map[any]any {
	snapshot := make(map[any]any)
	tc.ActiveTickets.Range(func(k, v any) bool {
		snapshot[k] = v
		return true
	})
	return snapshot
}

// AppliedReplId returns the highest replication stream ID applied to this cache.
func (tc *ReplicatedTicketCache) AppliedReplId() string {
	tc.watermarkMu.RLock()
	defer tc.watermarkMu.RUnlock()
	return tc.appliedReplId
}

// HasAppliedReplId reports whether this cache has applied an update with a
// replication stream ID greater than or equal to targetReplId.
func (tc *ReplicatedTicketCache) HasAppliedReplId(targetReplId string) bool {
	if targetReplId == "" {
		return false
	}
	tc.watermarkMu.RLock()
	applied := tc.appliedReplId
	tc.watermarkMu.RUnlock()
	if applied == "" {
		return false
	}
	return store.CompareReplIds(applied, targetReplId) >= 0
}

func (tc *ReplicatedTicketCache) advanceWatermark(batchMaxReplId string) {
	tc.watermarkMu.Lock()
	if batchMaxReplId != "" && (tc.appliedReplId == "" || store.CompareReplIds(batchMaxReplId, tc.appliedReplId) > 0) {
		tc.appliedReplId = batchMaxReplId
	}
	if tc.watermarkNotify != nil {
		close(tc.watermarkNotify)
	}
	tc.watermarkNotify = make(chan struct{})
	tc.watermarkMu.Unlock()
}

func (tc *ReplicatedTicketCache) isDeactivationReplicated(ticketId, targetReplId string) bool {
	if ticketId != "" {
		if _, ok := tc.InactiveSet.Load(ticketId); ok {
			if targetReplId == "" || tc.HasAppliedReplId(targetReplId) {
				return true
			}
		}
	}
	if targetReplId != "" && tc.HasAppliedReplId(targetReplId) {
		return true
	}
	return false
}

// WaitForDeactivation waits up to timeout for ticketId to appear in InactiveSet
// and/or for targetReplId to be applied to the local cache.
func (tc *ReplicatedTicketCache) WaitForDeactivation(ticketId, targetReplId string, timeout time.Duration) bool {
	if ticketId == "" && targetReplId == "" {
		return false
	}
	if tc.isDeactivationReplicated(ticketId, targetReplId) {
		return true
	}
	if timeout <= 0 {
		return false
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		tc.watermarkMu.Lock()
		if tc.watermarkNotify == nil {
			tc.watermarkNotify = make(chan struct{})
		}
		notifyCh := tc.watermarkNotify
		tc.watermarkMu.Unlock()

		if tc.isDeactivationReplicated(ticketId, targetReplId) {
			return true
		}

		select {
		case <-notifyCh:
		case <-timer.C:
			return false
		}
	}
}

func durationMs(d time.Duration) float64 {
	ms := float64(d.Microseconds()) / 1000.0
	if ms < 0 {
		return 0
	}
	return ms
}

func (tc *ReplicatedTicketCache) cfgIntOrDefault(key string, defaultVal int) int {
	if tc == nil || tc.Cfg == nil || !tc.Cfg.IsSet(key) {
		return defaultVal
	}
	return tc.Cfg.GetInt(key)
}

func (tc *ReplicatedTicketCache) cfgPositiveIntOrDefault(key string, defaultVal int) int {
	if tc == nil || tc.Cfg == nil {
		return defaultVal
	}
	if val := tc.Cfg.GetInt(key); val > 0 {
		return val
	}
	return defaultVal
}

func extractValidKeys(u store.StateUpdate) []string {
	var keys []string
	var seen map[string]struct{}

	addKey := func(k string) {
		if k == "" {
			return
		}
		if seen == nil {
			seen = make(map[string]struct{})
		}
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}

	if u.Key != "" {
		addKey(u.Key)
	}
	for _, k := range u.Keys {
		addKey(k)
	}
	return keys
}

func coalesceOutgoingRequests(requests []*UpdateRequest, maxPackedKeys int) ([]*store.StateUpdate, [][]int) {
	if maxPackedKeys <= 0 {
		maxPackedKeys = 500
	}

	updates := make([]*store.StateUpdate, 0, len(requests))
	reqGroupIndices := make([][]int, len(requests))

	var (
		hasPending  bool
		pendingCmd  int
		pendingKeys []string
		pendingSeen map[string]struct{}
		pendingReqs []int
	)

	flushPending := func() {
		if !hasPending || len(pendingKeys) == 0 {
			hasPending = false
			pendingKeys = nil
			pendingSeen = nil
			pendingReqs = nil
			return
		}
		u := &store.StateUpdate{
			Cmd: pendingCmd,
			Key: pendingKeys[0],
		}
		if len(pendingKeys) > 1 {
			keysCopy := make([]string, len(pendingKeys))
			copy(keysCopy, pendingKeys)
			u.Keys = keysCopy
		}
		groupIdx := len(updates)
		updates = append(updates, u)
		for _, reqIdx := range pendingReqs {
			reqGroupIndices[reqIdx] = append(reqGroupIndices[reqIdx], groupIdx)
		}
		hasPending = false
		pendingKeys = nil
		pendingSeen = nil
		pendingReqs = nil
	}

	addReqToPending := func(reqIdx int) {
		if len(pendingReqs) == 0 || pendingReqs[len(pendingReqs)-1] != reqIdx {
			pendingReqs = append(pendingReqs, reqIdx)
		}
	}

	for reqIdx, req := range requests {
		if req == nil {
			continue
		}
		cmd := req.Update.Cmd
		if cmd != store.Activate && cmd != store.Deactivate {
			flushPending()
			u := req.Update
			groupIdx := len(updates)
			updates = append(updates, &u)
			reqGroupIndices[reqIdx] = append(reqGroupIndices[reqIdx], groupIdx)
			continue
		}

		validKeys := extractValidKeys(req.Update)
		if len(validKeys) == 0 {
			// Empty-key Activate/Deactivate request: isolate from valid runs.
			flushPending()
			u := req.Update
			groupIdx := len(updates)
			updates = append(updates, &u)
			reqGroupIndices[reqIdx] = append(reqGroupIndices[reqIdx], groupIdx)
			continue
		}

		if hasPending && pendingCmd != cmd {
			flushPending()
		}

		for _, k := range validKeys {
			if hasPending {
				if _, exists := pendingSeen[k]; exists {
					addReqToPending(reqIdx)
					continue
				}
				if len(pendingKeys) >= maxPackedKeys {
					flushPending()
				}
			}
			if !hasPending {
				hasPending = true
				pendingCmd = cmd
				pendingKeys = make([]string, 0, len(validKeys))
				pendingSeen = make(map[string]struct{}, len(validKeys))
				pendingReqs = make([]int, 0, 4)
			}
			pendingKeys = append(pendingKeys, k)
			pendingSeen[k] = struct{}{}
			addReqToPending(reqIdx)
		}
	}

	flushPending()
	return updates, reqGroupIndices
}

func synthesizeResponseForUpdate(results []*store.StateResponse, idx int, primaryKey string) *store.StateResponse {
	if idx < len(results) {
		if results[idx] != nil {
			return results[idx]
		}
		return &store.StateResponse{
			Result: primaryKey,
			Err:    errors.New("replicator returned nil result for update"),
		}
	}
	return &store.StateResponse{
		Result: primaryKey,
		Err:    errors.New("replicator returned fewer results than requested"),
	}
}

func deliverOutgoingResponse(ctx context.Context, req *UpdateRequest, resp *store.StateResponse) {
	if req == nil {
		return
	}
	if !req.EnqueuedAt.IsZero() && otelCacheOutgoingQueueWaitDuration != nil {
		otelCacheOutgoingQueueWaitDuration.Record(ctx, durationMs(time.Since(req.EnqueuedAt)))
	}
	if req.ResultsChan == nil {
		return
	}
	select {
	case req.ResultsChan <- resp:
	default:
		ch := req.ResultsChan
		go func() {
			ch <- resp
		}()
	}
}

// outgoingReplicationQueue is an asynchronous goroutine that runs for the
// lifetime of the server.  It processes incoming repliation events that are
// produced by gRPC handlers, and sends those events to the configured state
// storage. The updates aren't applied to the local copy of the ticket cache
// yet at this point; once the event has been successfully replicated and
// received in the incomingReplicationQueue goroutine, the update is applied to
// the local cache.
//
//nolint:gocognit,cyclop
func (tc *ReplicatedTicketCache) OutgoingReplicationQueue(ctx context.Context) {
	logger := logger.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "replicationQueue",
		"direction": "outgoing",
	})

	tc.stateMu.Lock()
	if tc.UpRequests == nil {
		outBufSize := tc.cfgPositiveIntOrDefault("OM_CACHE_OUT_QUEUE_BUFFER_SIZE", 500)
		tc.UpRequests = make(chan *UpdateRequest, outBufSize)
	}
	tc.stateMu.Unlock()

	logger.Debug("Listening for replication requests")
	pipelineRequests := make([]*UpdateRequest, 0)
	pipeline := make([]*store.StateUpdate, 0)

	for {
		pipelineRequests = pipelineRequests[:0]
		pipeline = pipeline[:0]

		maxThreshold := tc.cfgPositiveIntOrDefault("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 50)
		waitTimeoutMs := tc.cfgIntOrDefault("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 500)
		if waitTimeoutMs < 0 {
			waitTimeoutMs = 0
		}

		exec := false
		thresholdReached := false
		timedOut := false
		timer := time.NewTimer(time.Duration(waitTimeoutMs) * time.Millisecond)

		for !exec {
			select {
			case req := <-tc.UpRequests:
				if req != nil {
					pipelineRequests = append(pipelineRequests, req)
				}
				logger.Tracef(" %v requests queued for current batch", len(pipelineRequests))
				if len(pipelineRequests) >= maxThreshold {
					thresholdReached = true
					logger.Trace("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD reached")
					exec = true
				}
			case <-timer.C:
				timedOut = true
				logger.Trace("OM_CACHE_OUT_WAIT_TIMEOUT_MS reached")
				exec = true
			case <-ctx.Done():
				exec = true
			}
		}
		timer.Stop()

		if thresholdReached && len(pipelineRequests) > 0 {
			if otelCacheOutgoingThresholdReachedCount != nil {
				otelCacheOutgoingThresholdReachedCount.Add(ctx, 1)
			}
		} else if timedOut {
			if otelCacheOutgoingQueueTimeouts != nil {
				otelCacheOutgoingQueueTimeouts.Add(ctx, 1)
			}
		}

	opportunisticDrain:
		for len(pipelineRequests) < maxThreshold {
			select {
			case req := <-tc.UpRequests:
				if req != nil {
					pipelineRequests = append(pipelineRequests, req)
				}
			default:
				break opportunisticDrain
			}
		}

		tc.setOutgoingBacklogCount(int64(len(tc.UpRequests)))

		if len(pipelineRequests) > 0 {
			packEnabled := tc.Cfg != nil && tc.Cfg.GetBool("OM_CACHE_PACK_TICKET_STATE_UPDATES")
			usePacking := packEnabled && (len(pipelineRequests) > 1 || len(pipelineRequests[0].Update.Keys) > 0)

			if !usePacking {
				for _, req := range pipelineRequests {
					pipeline = append(pipeline, &req.Update)
				}
				logger.WithFields(logrus.Fields{
					"batch_update_count": len(pipeline),
				}).Trace("sending state update batch to replicator")

				if otelCacheOutgoingUpdatesPerPoll != nil {
					otelCacheOutgoingUpdatesPerPoll.Record(ctx, int64(len(pipeline)))
				}

				sendStart := time.Now()
				results := tc.Replicator.SendUpdates(pipeline)
				sendElapsed := time.Since(sendStart)
				if otelCacheOutgoingSendDuration != nil {
					otelCacheOutgoingSendDuration.Record(ctx, durationMs(sendElapsed))
				}

				tc.setOutgoingBacklogCount(int64(len(tc.UpRequests)))

				logger.WithFields(logrus.Fields{
					"result_count": len(results),
				}).Trace("state update batch results received from replicator")

				for index, req := range pipelineRequests {
					resp := synthesizeResponseForUpdate(results, index, req.Update.PrimaryKey())
					deliverOutgoingResponse(ctx, req, resp)
				}
			} else {
				maxPackedKeys := tc.cfgPositiveIntOrDefault("OM_CACHE_OUT_MAX_PACKED_KEYS", 500)
				coalescedUpdates, reqGroupIndices := coalesceOutgoingRequests(pipelineRequests, maxPackedKeys)

				logger.WithFields(logrus.Fields{
					"batch_update_count": len(coalescedUpdates),
				}).Trace("sending coalesced state update batch to replicator")

				if otelCacheOutgoingUpdatesPerPoll != nil {
					otelCacheOutgoingUpdatesPerPoll.Record(ctx, int64(len(coalescedUpdates)))
				}

				sendStart := time.Now()
				results := tc.Replicator.SendUpdates(coalescedUpdates)
				sendElapsed := time.Since(sendStart)
				if otelCacheOutgoingSendDuration != nil {
					otelCacheOutgoingSendDuration.Record(ctx, durationMs(sendElapsed))
				}

				tc.setOutgoingBacklogCount(int64(len(tc.UpRequests)))

				logger.WithFields(logrus.Fields{
					"result_count": len(results),
				}).Trace("state update batch results received from replicator")

				for reqIdx, req := range pipelineRequests {
					gIndices := reqGroupIndices[reqIdx]
					primaryKey := req.Update.PrimaryKey()
					var chosenResp *store.StateResponse
					for _, gIdx := range gIndices {
						pk := primaryKey
						if pk == "" && gIdx < len(coalescedUpdates) {
							pk = coalescedUpdates[gIdx].PrimaryKey()
						}
						r := synthesizeResponseForUpdate(results, gIdx, pk)
						chosenResp = r
						if r.Err != nil {
							break
						}
					}
					if chosenResp == nil {
						chosenResp = synthesizeResponseForUpdate(results, 0, primaryKey)
					}
					deliverOutgoingResponse(ctx, req, chosenResp)
				}
			}
		}

		if ctx.Err() != nil && len(tc.UpRequests) == 0 {
			tc.setOutgoingBacklogCount(0)
			return
		}
	}
}

// incomingReplicationQueue is an asynchronous goroutine that runs for the
// lifetime of the server. It reads all incoming replication events from the
// configured state storage and applies them to the local ticket cache.  In
// practice, this does almost all the work for every om-core gRPC handler
// /except/ InvokeMatchMakingFunction.
//
//nolint:gocognit,cyclop,maintidx
func (tc *ReplicatedTicketCache) IncomingReplicationQueue(ctx context.Context) {
	logger := logger.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "replicationQueue",
		"direction": "incoming",
	})

	tc.stateMu.Lock()
	if tc.inInactiveHeap == nil {
		tc.inInactiveHeap = make(map[string]struct{})
	}
	if tc.inAssignmentHeap == nil {
		tc.inAssignmentHeap = make(map[string]struct{})
	}
	tc.stateMu.Unlock()

	inQueueBufSize := tc.cfgPositiveIntOrDefault("OM_CACHE_IN_QUEUE_BUFFER_SIZE", 20000)
	replStream := make(chan store.StateUpdate, inQueueBufSize)
	catchUpWake := make(chan struct{}, 1)

	var (
		totalPolled   int64
		totalApplied  int64
		catchUpTarget int64 = -1
	)

	// Storage poller goroutine.
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}

			pollStart := time.Now()
			results := tc.Replicator.GetUpdates()
			pollDuration := time.Since(pollStart)

			if otelCacheIncomingPollDuration != nil {
				otelCacheIncomingPollDuration.Record(ctx, durationMs(pollDuration))
			}
			if otelCacheIncomingPerPoll != nil {
				otelCacheIncomingPerPoll.Record(ctx, int64(len(results)))
			}

			maxUpdatesPerPoll := tc.cfgPositiveIntOrDefault("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 10000)
			isFullPoll := len(results) >= maxUpdatesPerPoll
			if isFullPoll && otelCacheIncomingFullPolls != nil {
				otelCacheIncomingFullPolls.Add(ctx, 1)
			}

			if len(results) > 0 {
				pushStart := time.Now()
				pushedCount := int64(0)
				for _, curUpdate := range results {
					if curUpdate == nil {
						continue
					}
					logger.WithFields(logrus.Fields{
						"update.key":     curUpdate.Key,
						"update.command": curUpdate.Cmd,
					}).Trace("queueing incoming update from state storage")
					select {
					case replStream <- *curUpdate:
						pushedCount++
						tc.setIncomingBacklogCount(int64(len(replStream)))
					case <-ctx.Done():
						if otelCacheIncomingQueueWaitDuration != nil {
							otelCacheIncomingQueueWaitDuration.Record(ctx, durationMs(time.Since(pushStart)))
						}
						return
					}
				}
				if otelCacheIncomingQueueWaitDuration != nil {
					otelCacheIncomingQueueWaitDuration.Record(ctx, durationMs(time.Since(pushStart)))
				}
				atomic.AddInt64(&totalPolled, pushedCount)
			}

			// Cold-start catch-up target detection.
			if atomic.LoadInt64(&catchUpTarget) == -1 && !isFullPoll {
				polledNow := atomic.LoadInt64(&totalPolled)
				if atomic.CompareAndSwapInt64(&catchUpTarget, -1, polledNow) {
					if polledNow == 0 || atomic.LoadInt64(&totalApplied) >= polledNow {
						tc.markCaughtUp()
					}
				}
			}
			if len(results) > 0 && !tc.IsCaughtUp() {
				select {
				case catchUpWake <- struct{}{}:
				default:
				}
			}

			// Post-poll adaptive pause.
			if len(results) == 0 {
				if otelCacheIncomingEmptyTimeouts != nil {
					otelCacheIncomingEmptyTimeouts.Add(ctx, 1)
				}
				waitTimeoutMs := tc.cfgIntOrDefault("OM_CACHE_IN_WAIT_TIMEOUT_MS", 1500)
				waitTimeout := time.Duration(waitTimeoutMs) * time.Millisecond
				if remaining := waitTimeout - pollDuration; remaining > 0 {
					waitStart := time.Now()
					waitTimer := time.NewTimer(remaining)
					select {
					case <-waitTimer.C:
					case <-ctx.Done():
						waitTimer.Stop()
						return
					}
					if otelCacheIncomingPollWaitDuration != nil {
						otelCacheIncomingPollWaitDuration.Record(ctx, durationMs(time.Since(waitStart)))
					}
				} else if otelCacheIncomingPollWaitDuration != nil {
					otelCacheIncomingPollWaitDuration.Record(ctx, 0)
				}
			} else {
				var pauseMs int
				if isFullPoll {
					pauseMs = tc.cfgIntOrDefault("OM_CACHE_IN_FULL_POLL_WAIT_MS", 100)
				} else {
					pauseMs = tc.cfgIntOrDefault("OM_CACHE_IN_POLL_WAIT_MS", 250)
				}
				if pauseMs > 0 {
					waitStart := time.Now()
					waitTimer := time.NewTimer(time.Duration(pauseMs) * time.Millisecond)
					select {
					case <-waitTimer.C:
					case <-ctx.Done():
						waitTimer.Stop()
						return
					}
					if otelCacheIncomingPollWaitDuration != nil {
						otelCacheIncomingPollWaitDuration.Record(ctx, durationMs(time.Since(waitStart)))
					}
				} else if otelCacheIncomingPollWaitDuration != nil {
					otelCacheIncomingPollWaitDuration.Record(ctx, 0)
				}
			}
		}
	}()

	// Asynchronous cache expiration ticker goroutine.
	expIntervalMs := tc.cfgPositiveIntOrDefault("OM_CACHE_EXPIRATION_INTERVAL_MS", 1000)
	go func() {
		ticker := time.NewTicker(time.Duration(expIntervalMs) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				tc.expireCacheEntries(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Applier loop.
	hitMaxApplyDuration := false
	for {
		var sleepMs int
		if hitMaxApplyDuration {
			sleepMs = tc.cfgIntOrDefault("OM_CACHE_IN_FULL_APPLY_SLEEP_MS", 100)
		} else {
			sleepMs = tc.cfgIntOrDefault("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 500)
		}
		if sleepMs < 0 {
			sleepMs = 0
		}
		sleepDur := time.Duration(sleepMs) * time.Millisecond

		if hitMaxApplyDuration {
			sleepStart := time.Now()
			if sleepDur > 0 {
				sleepTimer := time.NewTimer(sleepDur)
				select {
				case <-sleepTimer.C:
				case <-ctx.Done():
					sleepTimer.Stop()
					return
				}
			} else if ctx.Err() != nil {
				return
			}
			if otelCacheIncomingApplySleepDuration != nil {
				otelCacheIncomingApplySleepDuration.Record(ctx, durationMs(time.Since(sleepStart)))
			}
		} else if !tc.IsCaughtUp() {
			if len(replStream) > 0 {
				if ctx.Err() != nil {
					return
				}
				if otelCacheIncomingApplySleepDuration != nil {
					otelCacheIncomingApplySleepDuration.Record(ctx, 0)
				}
			} else {
				sleepStart := time.Now()
				if sleepDur > 0 {
					sleepTimer := time.NewTimer(sleepDur)
					select {
					case <-sleepTimer.C:
					case <-catchUpWake:
						sleepTimer.Stop()
					case <-ctx.Done():
						sleepTimer.Stop()
						return
					}
				} else {
					select {
					case <-catchUpWake:
					case <-ctx.Done():
						return
					default:
					}
				}
				if otelCacheIncomingApplySleepDuration != nil {
					otelCacheIncomingApplySleepDuration.Record(ctx, durationMs(time.Since(sleepStart)))
				}
			}
		} else {
			sleepStart := time.Now()
			if sleepDur > 0 {
				sleepTimer := time.NewTimer(sleepDur)
				select {
				case <-sleepTimer.C:
				case <-ctx.Done():
					sleepTimer.Stop()
					return
				}
			} else if ctx.Err() != nil {
				return
			}
			if otelCacheIncomingApplySleepDuration != nil {
				otelCacheIncomingApplySleepDuration.Record(ctx, durationMs(time.Since(sleepStart)))
			}
		}

		maxApplyMs := tc.cfgPositiveIntOrDefault("OM_CACHE_IN_MAX_APPLY_DURATION_MS", 500)
		applyStart := time.Now()
		applyTimer := time.NewTimer(time.Duration(maxApplyMs) * time.Millisecond)
		hitMaxApplyDuration = false
		done := false
		appliedInCycle := int64(0)
		highestReplId := ""

		for !done {
			// Two-level priority select: check apply deadline timer non-blockingly first.
			select {
			case <-applyTimer.C:
				if len(replStream) > 0 {
					if otelCacheIncomingProcessingTimeouts != nil {
						otelCacheIncomingProcessingTimeouts.Add(ctx, 1)
					}
					hitMaxApplyDuration = true
					logger.Trace("lock hold timeout")
				}
				done = true
				continue
			case <-ctx.Done():
				applyTimer.Stop()
				return
			default:
			}

			select {
			case <-applyTimer.C:
				if len(replStream) > 0 {
					if otelCacheIncomingProcessingTimeouts != nil {
						otelCacheIncomingProcessingTimeouts.Add(ctx, 1)
					}
					hitMaxApplyDuration = true
					logger.Trace("lock hold timeout")
				}
				done = true
			case <-ctx.Done():
				applyTimer.Stop()
				return
			case curUpdate := <-replStream:
				tc.applyIncomingUpdate(logger, curUpdate)
				appliedInCycle++
				if curUpdate.ReplId != "" {
					if tsMs, err := store.ParseReplIdTimestampMs(curUpdate.ReplId); err == nil {
						lagMs := float64(time.Now().UnixMilli() - tsMs)
						if lagMs < 0 {
							lagMs = 0
						}
						if otelCacheReplicationLag != nil {
							otelCacheReplicationLag.Record(ctx, lagMs)
						}
					}
					if highestReplId == "" || store.CompareReplIds(curUpdate.ReplId, highestReplId) > 0 {
						highestReplId = curUpdate.ReplId
					}
				}
			default:
				logger.Trace("Incoming update queue empty")
				done = true
			}
		}
		applyTimer.Stop()

		tc.setIncomingBacklogCount(int64(len(replStream)))
		if otelCacheIncomingApplyDuration != nil {
			otelCacheIncomingApplyDuration.Record(ctx, durationMs(time.Since(applyStart)))
		}

		if appliedInCycle > 0 {
			tc.advanceWatermark(highestReplId)
			appliedNow := atomic.AddInt64(&totalApplied, appliedInCycle)
			if target := atomic.LoadInt64(&catchUpTarget); target >= 0 && appliedNow >= target {
				tc.markCaughtUp()
			}
		} else if target := atomic.LoadInt64(&catchUpTarget); target >= 0 && atomic.LoadInt64(&totalApplied) >= target {
			tc.markCaughtUp()
		}
	}
}

func (tc *ReplicatedTicketCache) applyIncomingUpdate(logger *logrus.Entry, curUpdate store.StateUpdate) {
	switch curUpdate.Cmd {
	case store.Ticket:
		ticketPb := &pb.Ticket{}
		if err := proto.Unmarshal([]byte(curUpdate.Value), ticketPb); err != nil {
			logger.Error("received ticket replication could not be unmarshalled")
		}
		ticketPb.Id = curUpdate.Key

		id := curUpdate.Key
		if id == "" {
			return
		}

		tc.stateMu.Lock()
		_, existedInTickets := tc.Tickets.Load(id)
		tc.Tickets.Store(id, ticketPb)
		if !existedInTickets {
			tc.liveTickets++
		}

		if _, inInactive := tc.InactiveSet.Load(id); !inInactive {
			_, existedInActive := tc.ActiveTickets.Load(id)
			tc.ActiveTickets.Store(id, ticketPb)
			if !existedInActive {
				tc.liveActive++
			}
		} else if _, existedInActive := tc.ActiveTickets.LoadAndDelete(id); existedInActive {
			tc.liveActive--
		}

		creationMs, err := store.ParseReplIdTimestampMs(id)
		if err != nil {
			creationMs = time.Now().UnixMilli()
		}
		if expTs := ticketPb.GetExpirationTime(); expTs != nil && expTs.IsValid() {
			expMs := expTs.AsTime().UnixMilli()
			ttlMs := int64(tc.cfgIntOrDefault("OM_CACHE_TICKET_TTL_MS", 600000))
			if expMs < creationMs+ttlMs {
				heap.Push(&tc.ticketExpHeap, expirationHeapEntry{
					id:         id,
					deadlineMs: expMs,
				})
			}
		}
		tc.updateGaugeCountsLocked()
		tc.stateMu.Unlock()
		logger.Tracef("ticket replication received: %v", id)

	case store.Activate:
		keys := extractValidKeys(curUpdate)
		if len(keys) == 0 {
			return
		}
		tc.stateMu.Lock()
		for _, id := range keys {
			if _, wasInactive := tc.InactiveSet.LoadAndDelete(id); wasInactive {
				tc.liveInactive--
			}
			if rawTicket, exists := tc.Tickets.Load(id); exists {
				if ticketPb, ok := rawTicket.(*pb.Ticket); ok && ticketPb != nil {
					_, wasActive := tc.ActiveTickets.Load(id)
					tc.ActiveTickets.Store(id, ticketPb)
					if !wasActive {
						tc.liveActive++
					}
				}
			}
		}
		tc.updateGaugeCountsLocked()
		tc.stateMu.Unlock()
		logger.Tracef("activation replication received: %v", curUpdate.Key)

	case store.Deactivate:
		keys := extractValidKeys(curUpdate)
		if len(keys) == 0 {
			return
		}
		tc.stateMu.Lock()
		if tc.inInactiveHeap == nil {
			tc.inInactiveHeap = make(map[string]struct{})
		}
		for _, id := range keys {
			if _, wasActive := tc.ActiveTickets.LoadAndDelete(id); wasActive {
				tc.liveActive--
			}
			if _, wasInactive := tc.InactiveSet.LoadOrStore(id, true); !wasInactive {
				tc.liveInactive++
			}
			if _, inHeap := tc.inInactiveHeap[id]; !inHeap {
				tc.inInactiveHeap[id] = struct{}{}
				creationMs, err := store.ParseReplIdTimestampMs(id)
				if err != nil {
					creationMs = time.Now().UnixMilli()
				}
				heap.Push(&tc.inactiveHeap, expirationHeapEntry{
					id:         id,
					deadlineMs: creationMs,
				})
			}
		}
		tc.updateGaugeCountsLocked()
		tc.stateMu.Unlock()
		logger.Tracef("deactivate replication received: %v", curUpdate.Key)

	case store.Assign:
		assignmentPb := &pb.Assignment{}
		if err := proto.Unmarshal([]byte(curUpdate.Value), assignmentPb); err != nil {
			logger.Error("received assignment replication could not be unmarshalled")
		}
		id := curUpdate.Key
		if id == "" {
			return
		}
		tc.stateMu.Lock()
		if tc.inAssignmentHeap == nil {
			tc.inAssignmentHeap = make(map[string]struct{})
		}
		_, existed := tc.Assignments.Load(id)
		tc.Assignments.Store(id, assignmentPb)
		if !existed {
			tc.liveAssignments++
		}
		if _, inHeap := tc.inAssignmentHeap[id]; !inHeap {
			tc.inAssignmentHeap[id] = struct{}{}
			heap.Push(&tc.assignmentHeap, expirationHeapEntry{
				id:         id,
				deadlineMs: time.Now().UnixMilli(),
			})
		}
		tc.updateGaugeCountsLocked()
		tc.stateMu.Unlock()
		logger.Tracef("**DEPRECATED** assign replication received %v:%v", id, assignmentPb.GetConnection())
	}
}

func (tc *ReplicatedTicketCache) expireCacheEntries(ctx context.Context) {
	exLogger := logrus.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "replicatedTicketCache",
		"operation": "expiration",
	})

	startTime := time.Now()
	nowMs := startTime.UnixMilli()
	ticketTTLMs := int64(tc.cfgIntOrDefault("OM_CACHE_TICKET_TTL_MS", 600000))
	assignAddTTLMs := int64(tc.cfgIntOrDefault("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS", 600000))
	ticketCutoffMs := nowMs - ticketTTLMs
	assignmentCutoffMs := ticketCutoffMs - assignAddTTLMs
	maxDeletesPerCycle := tc.cfgPositiveIntOrDefault("OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE", 5000)

	var (
		numTicketDeletions     int64
		numInactiveDeletions   int64
		numAssignmentDeletions int64
		totalDeleted           int
	)

	for totalDeleted < maxDeletesPerCycle {
		chunkOps := 0
		tc.stateMu.Lock()

		// Pass 1: Expire inactive tickets whose creation timestamp is at or before ticketCutoffMs.
		for tc.inactiveHeap.Len() > 0 &&
			tc.inactiveHeap[0].deadlineMs <= ticketCutoffMs &&
			chunkOps < expirationChunkOpsLimit &&
			totalDeleted < maxDeletesPerCycle {
			entry := heap.Pop(&tc.inactiveHeap).(expirationHeapEntry)
			delete(tc.inInactiveHeap, entry.id)
			chunkOps++

			// Only evict if the ticket is still in InactiveSet (not re-activated).
			// Delete from ActiveTickets and Tickets BEFORE InactiveSet so lock-free
			// readers never observe a resurrected expired ticket.
			if _, stillInactive := tc.InactiveSet.Load(entry.id); stillInactive {
				if _, wasActive := tc.ActiveTickets.LoadAndDelete(entry.id); wasActive {
					tc.liveActive--
				}
				if _, wasInTickets := tc.Tickets.LoadAndDelete(entry.id); wasInTickets {
					tc.liveTickets--
					numTicketDeletions++
				}
				if _, wasInInactive := tc.InactiveSet.LoadAndDelete(entry.id); wasInInactive {
					tc.liveInactive--
					numInactiveDeletions++
				}
				totalDeleted++
			}
		}

		// Pass 2: Expire tickets with custom ExpirationTime at or before nowMs.
		for tc.ticketExpHeap.Len() > 0 &&
			tc.ticketExpHeap[0].deadlineMs <= nowMs &&
			chunkOps < expirationChunkOpsLimit &&
			totalDeleted < maxDeletesPerCycle {
			entry := heap.Pop(&tc.ticketExpHeap).(expirationHeapEntry)
			chunkOps++

			if rawTicket, exists := tc.Tickets.Load(entry.id); exists {
				if ticketPb, ok := rawTicket.(*pb.Ticket); ok &&
					ticketPb.GetExpirationTime() != nil &&
					ticketPb.GetExpirationTime().AsTime().UnixMilli() <= nowMs {
					if _, wasActive := tc.ActiveTickets.LoadAndDelete(entry.id); wasActive {
						tc.liveActive--
					}
					if _, wasInTickets := tc.Tickets.LoadAndDelete(entry.id); wasInTickets {
						tc.liveTickets--
						numTicketDeletions++
					}
					totalDeleted++
				}
			}
		}

		// Pass 3: Expire assignments whose timestamp is at or before assignmentCutoffMs.
		for tc.assignmentHeap.Len() > 0 &&
			tc.assignmentHeap[0].deadlineMs <= assignmentCutoffMs &&
			chunkOps < expirationChunkOpsLimit &&
			totalDeleted < maxDeletesPerCycle {
			entry := heap.Pop(&tc.assignmentHeap).(expirationHeapEntry)
			delete(tc.inAssignmentHeap, entry.id)
			chunkOps++

			if _, wasInAssignments := tc.Assignments.LoadAndDelete(entry.id); wasInAssignments {
				tc.liveAssignments--
				numAssignmentDeletions++
				totalDeleted++
			}
		}

		tc.updateGaugeCountsLocked()
		tc.stateMu.Unlock()

		if chunkOps < expirationChunkOpsLimit || totalDeleted >= maxDeletesPerCycle {
			break
		}
	}

	elapsed := time.Since(startTime)
	if otelCacheExpirationCycleDuration != nil {
		otelCacheExpirationCycleDuration.Record(ctx, durationMs(elapsed))
	}
	if otelCacheTicketsExpiredPerCycle != nil {
		otelCacheTicketsExpiredPerCycle.Record(ctx, numTicketDeletions)
	}
	if otelCacheInactivesExpiredPerCycle != nil {
		otelCacheInactivesExpiredPerCycle.Record(ctx, numInactiveDeletions)
	}
	if otelCacheAssignmentsExpiredPerCycle != nil {
		otelCacheAssignmentsExpiredPerCycle.Record(ctx, numAssignmentDeletions)
	}

	if numAssignmentDeletions > 0 {
		exLogger.Tracef("Removed %v expired assignments from local cache", numAssignmentDeletions)
	}
	if numInactiveDeletions > 0 {
		exLogger.Tracef("%v ticket ids expired from the inactive list in local cache", numInactiveDeletions)
	}
	if numTicketDeletions > 0 {
		exLogger.Tracef("Removed %v expired tickets from local cache", numTicketDeletions)
	}
}
