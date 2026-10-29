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
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/googleforgames/open-match2/v2/internal/config"
	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// simulatedLatencyReplicator models an event-sourced state replicator (such as
// Redis Streams) with configurable network round-trip latency on SendUpdates
// and GetUpdates.
type simulatedLatencyReplicator struct {
	cfg         *viper.Viper
	sendLatency time.Duration
	pollLatency time.Duration
	stream      chan *store.StateUpdate
	validator   *regexp.Regexp

	mu     sync.Mutex
	lastMs int64
	seq    int64
}

func newSimulatedLatencyReplicator(cfg *viper.Viper, sendLatency, pollLatency time.Duration, bufferSize int) *simulatedLatencyReplicator {
	if bufferSize <= 0 {
		bufferSize = 50000
	}
	return &simulatedLatencyReplicator{
		cfg:         cfg,
		sendLatency: sendLatency,
		pollLatency: pollLatency,
		stream:      make(chan *store.StateUpdate, bufferSize),
		validator:   regexp.MustCompile(`^\d{13}-\d+$`),
	}
}

func (r *simulatedLatencyReplicator) nextReplIdLocked() string {
	nowMs := time.Now().UnixMilli()
	if nowMs <= r.lastMs {
		r.seq++
	} else {
		r.lastMs = nowMs
		r.seq = 0
	}
	return fmt.Sprintf("%013d-%d", r.lastMs, r.seq)
}

func (r *simulatedLatencyReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	if r.sendLatency > 0 {
		time.Sleep(r.sendLatency)
	}

	out := make([]*store.StateResponse, len(updates))
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, up := range updates {
		if up == nil {
			out[i] = &store.StateResponse{Err: store.InvalidInputErr}
			continue
		}
		replId := r.nextReplIdLocked()
		cp := &store.StateUpdate{
			Cmd:    up.Cmd,
			Key:    up.Key,
			Value:  up.Value,
			ReplId: replId,
		}
		if up.Cmd == store.Ticket && cp.Key == "" {
			cp.Key = replId
		}
		if len(up.Keys) > 0 {
			cp.Keys = make([]string, len(up.Keys))
			copy(cp.Keys, up.Keys)
			if cp.Key == "" {
				cp.Key = cp.Keys[0]
			}
		}
		r.stream <- cp
		out[i] = &store.StateResponse{Result: replId}
	}
	return out
}

func (r *simulatedLatencyReplicator) GetUpdates() []*store.StateUpdate {
	if r.pollLatency > 0 {
		time.Sleep(r.pollLatency)
	}

	maxPerPoll := 10000
	waitMs := 20
	if r.cfg != nil {
		if v := r.cfg.GetInt("OM_CACHE_IN_MAX_UPDATES_PER_POLL"); v > 0 {
			maxPerPoll = v
		}
		if r.cfg.IsSet("OM_CACHE_IN_WAIT_TIMEOUT_MS") {
			waitMs = r.cfg.GetInt("OM_CACHE_IN_WAIT_TIMEOUT_MS")
		}
	}

	out := make([]*store.StateUpdate, 0)
	timer := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
	defer timer.Stop()

	select {
	case first := <-r.stream:
		out = append(out, first)
	case <-timer.C:
		return out
	}

	for len(out) < maxPerPoll {
		select {
		case next := <-r.stream:
			out = append(out, next)
		default:
			return out
		}
	}
	return out
}

func (r *simulatedLatencyReplicator) GetReplIdValidator() *regexp.Regexp {
	return r.validator
}

func makeSerializedTicketPayload() string {
	raw, err := proto.Marshal(&pb.Ticket{
		Attributes: &pb.Ticket_FilterableData{
			CreationTime: timestamppb.Now(),
			Tags:         []string{"bench-pool"},
			DoubleArgs:   map[string]float64{"mmr": 1500},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// 1. Cache Expiration Benchmark: Benchmark expireCacheEntries at 10,000 and 50,000 tickets.
func BenchmarkExpireCacheEntries(b *testing.B) {
	payload := makeSerializedTicketPayload()

	for _, totalTickets := range []int{10000, 50000} {
		b.Run(fmt.Sprintf("tickets_%d", totalTickets), func(b *testing.B) {
			cfg := config.Read()
			cfg.Set("OM_CACHE_TICKET_TTL_MS", 600000)
			cfg.Set("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS", 600000)
			cfg.Set("OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE", 5000)

			tc := New(cfg, nil)
			nowMs := time.Now().UnixMilli()

			// Populate totalTickets through the production applyIncomingUpdate path
			// (half active, half inactive with future expiration deadlines so steady-state
			// min-heap peek and chunk sweep overhead is measured without rebuilding 50k maps per op).
			for i := 0; i < totalTickets; i++ {
				id := fmt.Sprintf("%013d-%d", nowMs+int64(i%1000), i)
				tc.applyIncomingUpdate(logger, store.StateUpdate{
					Cmd:   store.Ticket,
					Key:   id,
					Value: payload,
				})
				if i%2 == 0 {
					tc.applyIncomingUpdate(logger, store.StateUpdate{
						Cmd: store.Deactivate,
						Key: id,
					})
				}
			}

			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tc.expireCacheEntries(ctx)
			}
		})
	}
}

// 2. Active Tickets Snapshot Benchmark: Benchmark SnapshotActiveTickets with 50,000 total tickets and 25,000 active tickets.
func BenchmarkSnapshotActiveTickets(b *testing.B) {
	const (
		totalTickets  = 50000
		activeTickets = 25000
	)
	cfg := config.Read()
	tc := New(cfg, nil)
	payload := makeSerializedTicketPayload()
	nowMs := time.Now().UnixMilli()

	for i := 0; i < totalTickets; i++ {
		id := fmt.Sprintf("%013d-%d", nowMs, i)
		tc.applyIncomingUpdate(logger, store.StateUpdate{
			Cmd:   store.Ticket,
			Key:   id,
			Value: payload,
		})
		if i >= activeTickets {
			tc.applyIncomingUpdate(logger, store.StateUpdate{
				Cmd: store.Deactivate,
				Key: id,
			})
		}
	}

	if tc.ActiveCount() != activeTickets {
		b.Fatalf("expected %d active tickets, got %d", activeTickets, tc.ActiveCount())
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap := tc.SnapshotActiveTickets()
		if len(snap) != activeTickets {
			b.Fatalf("expected snapshot size %d, got %d", activeTickets, len(snap))
		}
	}
}

// 3. Replication Throughput Benchmark: Benchmark end-to-end replication throughput comparing unpacked vs. packed modes.
func BenchmarkReplicationThroughput(b *testing.B) {
	const batchSize = 20

	for _, packed := range []bool{false, true} {
		modeName := "unpacked"
		if packed {
			modeName = "packed"
		}
		b.Run(modeName, func(b *testing.B) {
			cfg := config.Read()
			cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", packed)
			cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", batchSize)
			cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 5)
			cfg.Set("OM_CACHE_OUT_QUEUE_BUFFER_SIZE", 2000)
			cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 10000)
			cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 10)
			cfg.Set("OM_CACHE_IN_POLL_WAIT_MS", 1)
			cfg.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 1)
			cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 1)
			cfg.Set("OM_CACHE_IN_FULL_APPLY_SLEEP_MS", 1)
			cfg.Set("OM_CACHE_IN_QUEUE_BUFFER_SIZE", 50000)

			simRep := newSimulatedLatencyReplicator(cfg, 100*time.Microsecond, 100*time.Microsecond, 100000)
			tc := New(cfg, simRep)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			go tc.OutgoingReplicationQueue(ctx)
			go tc.IncomingReplicationQueue(ctx)

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				resChans := make([]chan *store.StateResponse, batchSize)
				lastTicketId := ""
				for j := 0; j < batchSize; j++ {
					resChans[j] = make(chan *store.StateResponse, 1)
					tId := fmt.Sprintf("1700000000000-%d", i*batchSize+j)
					lastTicketId = tId
					tc.UpRequests <- &UpdateRequest{
						Ctx:         ctx,
						ResultsChan: resChans[j],
						EnqueuedAt:  time.Now(),
						Update: store.StateUpdate{
							Cmd: store.Deactivate,
							Key: tId,
						},
					}
				}

				var lastReplId string
				for j := 0; j < batchSize; j++ {
					resp := <-resChans[j]
					if resp == nil || resp.Err != nil {
						b.Fatalf("unexpected replication error: %v", resp)
					}
					lastReplId = resp.Result
				}

				if !tc.WaitForDeactivation(lastTicketId, lastReplId, 2*time.Second) {
					b.Fatalf("WaitForDeactivation timed out for ticket %s replId %s", lastTicketId, lastReplId)
				}
			}
		})
	}
}

// 4. Synthetic Burst Replication Stress Test
func TestSyntheticBurstReplicationStress(t *testing.T) {
	t.Run("a_25_concurrent_matches_deactivating_40_tickets_each", func(t *testing.T) {
		const (
			numMatches      = 25
			ticketsPerMatch = 40
			totalTickets    = numMatches * ticketsPerMatch
		)

		cfg := config.Read()
		cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
		cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 50)
		cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 5)
		cfg.Set("OM_CACHE_OUT_QUEUE_BUFFER_SIZE", 2000)
		cfg.Set("OM_CACHE_OUT_MAX_PACKED_KEYS", 500)
		cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 5000)
		cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 15)
		cfg.Set("OM_CACHE_IN_POLL_WAIT_MS", 2)
		cfg.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 2)
		cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 2)
		cfg.Set("OM_CACHE_IN_FULL_APPLY_SLEEP_MS", 2)
		cfg.Set("OM_CACHE_IN_QUEUE_BUFFER_SIZE", 20000)
		cfg.Set("OM_CACHE_TICKET_TTL_MS", 600000)

		simRep := newSimulatedLatencyReplicator(cfg, 500*time.Microsecond, 500*time.Microsecond, 20000)
		tc := New(cfg, simRep)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go tc.OutgoingReplicationQueue(ctx)
		go tc.IncomingReplicationQueue(ctx)

		payload := makeSerializedTicketPayload()
		nowMs := time.Now().UnixMilli()
		matchTickets := make([][]string, numMatches)

		for m := 0; m < numMatches; m++ {
			matchTickets[m] = make([]string, ticketsPerMatch)
			for k := 0; k < ticketsPerMatch; k++ {
				seq := m*ticketsPerMatch + k + 1
				id := fmt.Sprintf("%013d-%d", nowMs, seq)
				matchTickets[m][k] = id
				tc.applyIncomingUpdate(logger, store.StateUpdate{
					Cmd:   store.Ticket,
					Key:   id,
					Value: payload,
				})
			}
		}

		require.Equal(t, int64(totalTickets), tc.ActiveCount())
		require.Len(t, tc.SnapshotActiveTickets(), totalTickets)

		var (
			wg            sync.WaitGroup
			waitSuccesses atomic.Int64
			deactSendErrs atomic.Int64
		)
		wg.Add(numMatches)

		for m := 0; m < numMatches; m++ {
			mIdx := m
			go func() {
				defer wg.Done()
				ids := matchTickets[mIdx]
				resCh := make(chan *store.StateResponse, 1)
				tc.UpRequests <- &UpdateRequest{
					Ctx:         ctx,
					ResultsChan: resCh,
					EnqueuedAt:  time.Now(),
					Update: store.StateUpdate{
						Cmd:  store.Deactivate,
						Keys: ids,
					},
				}
				resp := <-resCh
				if resp == nil || resp.Err != nil {
					deactSendErrs.Add(1)
					return
				}
				allOk := true
				for _, tId := range ids {
					if !tc.WaitForDeactivation(tId, resp.Result, 5*time.Second) {
						allOk = false
						break
					}
				}
				if allOk {
					waitSuccesses.Add(1)
				}
			}()
		}

		wg.Wait()
		assert.Equal(t, int64(0), deactSendErrs.Load())
		assert.Equal(t, int64(numMatches), waitSuccesses.Load())
		assert.Equal(t, int64(0), tc.ActiveCount())
		assert.Equal(t, int64(totalTickets), tc.InactiveCount())
		assert.Empty(t, tc.SnapshotActiveTickets())
	})

	t.Run("b_2000_expiring_tickets_concurrent_with_10_packed_deactivation_workers", func(t *testing.T) {
		const (
			totalExpiringTickets = 2000
			numWorkers           = 10
			ticketsPerWorker     = totalExpiringTickets / numWorkers
			chunkSize            = 20
		)

		cfg := config.Read()
		cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
		cfg.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 20)
		cfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 2)
		cfg.Set("OM_CACHE_OUT_QUEUE_BUFFER_SIZE", 2000)
		cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 5000)
		cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 10)
		cfg.Set("OM_CACHE_IN_POLL_WAIT_MS", 1)
		cfg.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 1)
		cfg.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 1)
		cfg.Set("OM_CACHE_IN_FULL_APPLY_SLEEP_MS", 1)
		cfg.Set("OM_CACHE_IN_QUEUE_BUFFER_SIZE", 20000)
		cfg.Set("OM_CACHE_TICKET_TTL_MS", 5000)
		cfg.Set("OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE", 400)
		cfg.Set("OM_CACHE_EXPIRATION_INTERVAL_MS", 5)

		simRep := newSimulatedLatencyReplicator(cfg, 100*time.Microsecond, 100*time.Microsecond, 20000)
		tc := New(cfg, simRep)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go tc.OutgoingReplicationQueue(ctx)
		go tc.IncomingReplicationQueue(ctx)

		// Seed 2,000 tickets whose creation timestamp encoded in their ID is already past OM_CACHE_TICKET_TTL_MS (5,000ms),
		// so as soon as they are deactivated and pushed onto inactiveHeap they are immediately eligible for TTL eviction.
		expiredCreationMs := time.Now().UnixMilli() - 20000
		payload := makeSerializedTicketPayload()
		allTicketIds := make([]string, totalExpiringTickets)
		for i := 0; i < totalExpiringTickets; i++ {
			id := fmt.Sprintf("%013d-%d", expiredCreationMs+int64(i), i)
			allTicketIds[i] = id
			tc.applyIncomingUpdate(logger, store.StateUpdate{
				Cmd:   store.Ticket,
				Key:   id,
				Value: payload,
			})
		}
		require.Equal(t, int64(totalExpiringTickets), tc.ActiveCount())

		var (
			confirmedDeactivated sync.Map
			resurrectedCount     atomic.Int64
			stopReaders          = make(chan struct{})
			readerWg             sync.WaitGroup
		)

		// Launch concurrent snapshot readers to verify no confirmed-deactivated ticket ever resurrects
		// during concurrent packed deactivation and chunked min-heap TTL expiration.
		for r := 0; r < 4; r++ {
			readerWg.Add(1)
			go func() {
				defer readerWg.Done()
				for {
					select {
					case <-stopReaders:
						return
					default:
						// Capture the set of confirmed-deactivated tickets BEFORE taking the active snapshot
						// so we only check tickets whose deactivation already happened prior to the snapshot.
						deactivatedPrior := make([]string, 0, 256)
						confirmedDeactivated.Range(func(k, _ any) bool {
							deactivatedPrior = append(deactivatedPrior, k.(string))
							return true
						})
						if len(deactivatedPrior) == 0 {
							time.Sleep(500 * time.Microsecond)
							continue
						}
						snap := tc.SnapshotActiveTickets()
						for _, id := range deactivatedPrior {
							if _, inActiveSnap := snap[id]; inActiveSnap {
								resurrectedCount.Add(1)
							}
						}
					}
				}
			}()
		}

		// Also run continuous concurrent expiration sweeps alongside the background ticker.
		var expWg sync.WaitGroup
		expWg.Add(1)
		go func() {
			defer expWg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					tc.expireCacheEntries(ctx)
					time.Sleep(2 * time.Millisecond)
				}
			}
		}()

		// Launch 10 packed-deactivation workers deactivating 200 tickets each in packed batches of 20.
		var workerWg sync.WaitGroup
		workerWg.Add(numWorkers)
		for w := 0; w < numWorkers; w++ {
			wIdx := w
			go func() {
				defer workerWg.Done()
				startIdx := wIdx * ticketsPerWorker
				endIdx := startIdx + ticketsPerWorker
				workerIds := allTicketIds[startIdx:endIdx]

				for offset := 0; offset < len(workerIds); offset += chunkSize {
					chunk := workerIds[offset : offset+chunkSize]
					resCh := make(chan *store.StateResponse, 1)
					tc.UpRequests <- &UpdateRequest{
						Ctx:         ctx,
						ResultsChan: resCh,
						EnqueuedAt:  time.Now(),
						Update: store.StateUpdate{
							Cmd:  store.Deactivate,
							Keys: chunk,
						},
					}
					resp := <-resCh
					require.NotNil(t, resp)
					require.NoError(t, resp.Err)
					require.True(t, tc.WaitForDeactivation(chunk[len(chunk)-1], resp.Result, 5*time.Second))
					for _, id := range chunk {
						confirmedDeactivated.Store(id, true)
					}
				}
			}()
		}

		workerWg.Wait()
		close(stopReaders)
		readerWg.Wait()
		expWg.Wait()

		// Drain any remaining expired entries across cycles (bounded by OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE=400).
		for i := 0; i < 10 && (tc.TicketCount() > 0 || tc.InactiveCount() > 0); i++ {
			tc.expireCacheEntries(ctx)
		}

		assert.Equal(t, int64(0), resurrectedCount.Load(), "zero tickets may resurrect during concurrent deactivation and expiration")
		assert.Equal(t, int64(0), tc.ActiveCount())
		assert.Equal(t, int64(0), tc.TicketCount())
		assert.Equal(t, int64(0), tc.InactiveCount())
		assert.Empty(t, tc.SnapshotActiveTickets())
	})
}
