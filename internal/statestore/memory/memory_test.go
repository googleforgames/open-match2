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
package memoryReplicator

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildTestConfig(waitTimeoutMs, maxUpdatesPerPoll, queueBufferSize int) *viper.Viper {
	cfg := viper.New()
	cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", waitTimeoutMs)
	cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", maxUpdatesPerPoll)
	cfg.Set("OM_CACHE_IN_QUEUE_BUFFER_SIZE", queueBufferSize)
	return cfg
}

func TestMemoryReplicator_RoundTripSendAndGet(t *testing.T) {
	rc := New(buildTestConfig(100, 100, 100))

	inputs := []*store.StateUpdate{
		{Cmd: store.Ticket, Value: "serialized-ticket-payload"},
		{Cmd: store.Activate, Key: "ticket-1"},
		{Cmd: store.Activate, Keys: []string{"ticket-2", "ticket-3", "ticket-4"}},
		{Cmd: store.Deactivate, Key: "ticket-5"},
		{Cmd: store.Deactivate, Keys: []string{"ticket-6", "ticket-7"}},
		{Cmd: store.Assign, Key: "ticket-1", Value: "10.0.0.1:7777"},
	}

	responses := rc.SendUpdates(inputs)
	require.Len(t, responses, len(inputs))
	for i, resp := range responses {
		require.NotNil(t, resp, "response %d must be non-nil", i)
		require.NoError(t, resp.Err, "response %d unexpected error", i)
		assert.Regexp(t, rc.GetReplIdValidator(), resp.Result)
	}

	updates := rc.GetUpdates()
	require.Len(t, updates, len(inputs))

	// 0: Ticket
	assert.Equal(t, store.Ticket, updates[0].Cmd)
	assert.Equal(t, responses[0].Result, updates[0].Key)
	assert.Nil(t, updates[0].Keys)
	assert.Equal(t, "serialized-ticket-payload", updates[0].Value)
	assert.Equal(t, responses[0].Result, updates[0].ReplId)

	// 1: Unpacked Activate
	assert.Equal(t, store.Activate, updates[1].Cmd)
	assert.Equal(t, "ticket-1", updates[1].Key)
	assert.Nil(t, updates[1].Keys)
	assert.Empty(t, updates[1].Value)
	assert.Equal(t, responses[1].Result, updates[1].ReplId)

	// 2: Packed Activate
	assert.Equal(t, store.Activate, updates[2].Cmd)
	assert.Equal(t, "ticket-2", updates[2].Key)
	assert.Equal(t, []string{"ticket-2", "ticket-3", "ticket-4"}, updates[2].Keys)
	assert.Empty(t, updates[2].Value)
	assert.Equal(t, responses[2].Result, updates[2].ReplId)

	// 3: Unpacked Deactivate
	assert.Equal(t, store.Deactivate, updates[3].Cmd)
	assert.Equal(t, "ticket-5", updates[3].Key)
	assert.Nil(t, updates[3].Keys)
	assert.Empty(t, updates[3].Value)
	assert.Equal(t, responses[3].Result, updates[3].ReplId)

	// 4: Packed Deactivate
	assert.Equal(t, store.Deactivate, updates[4].Cmd)
	assert.Equal(t, "ticket-6", updates[4].Key)
	assert.Equal(t, []string{"ticket-6", "ticket-7"}, updates[4].Keys)
	assert.Empty(t, updates[4].Value)
	assert.Equal(t, responses[4].Result, updates[4].ReplId)

	// 5: Assign
	assert.Equal(t, store.Assign, updates[5].Cmd)
	assert.Equal(t, "ticket-1", updates[5].Key)
	assert.Nil(t, updates[5].Keys)
	assert.Equal(t, "10.0.0.1:7777", updates[5].Value)
	assert.Equal(t, responses[5].Result, updates[5].ReplId)
}

func TestMemoryReplicator_ValidationErrorsAndPrimaryKeyContract(t *testing.T) {
	rc := New(buildTestConfig(30, 100, 100))

	cases := []struct {
		name           string
		input          *store.StateUpdate
		expectedErr    error
		expectedResult string
	}{
		{
			name:           "ticket missing data with caller key",
			input:          &store.StateUpdate{Cmd: store.Ticket, Key: "caller-ticket-id", Value: ""},
			expectedErr:    NoTicketDataErr,
			expectedResult: "caller-ticket-id",
		},
		{
			name:           "activate unpacked missing key",
			input:          &store.StateUpdate{Cmd: store.Activate, Key: ""},
			expectedErr:    NoTicketKeyErr,
			expectedResult: "",
		},
		{
			name:           "activate packed all empty keys with fallback key",
			input:          &store.StateUpdate{Cmd: store.Activate, Key: "fallback-act-key", Keys: []string{"", ""}},
			expectedErr:    NoTicketKeyErr,
			expectedResult: "fallback-act-key",
		},
		{
			name:           "deactivate unpacked missing key",
			input:          &store.StateUpdate{Cmd: store.Deactivate},
			expectedErr:    NoTicketKeyErr,
			expectedResult: "",
		},
		{
			name:           "deactivate packed all empty keys",
			input:          &store.StateUpdate{Cmd: store.Deactivate, Keys: []string{"", ""}},
			expectedErr:    NoTicketKeyErr,
			expectedResult: "",
		},
		{
			name:           "assign missing key",
			input:          &store.StateUpdate{Cmd: store.Assign, Key: "", Value: "10.0.0.1:7777"},
			expectedErr:    NoTicketKeyErr,
			expectedResult: "",
		},
		{
			name:           "assign missing assignment value",
			input:          &store.StateUpdate{Cmd: store.Assign, Key: "assign-key-1", Value: ""},
			expectedErr:    NoAssignmentErr,
			expectedResult: "assign-key-1",
		},
		{
			name:           "invalid command with single key",
			input:          &store.StateUpdate{Cmd: 42, Key: "bad-cmd-key"},
			expectedErr:    InvalidInputErr,
			expectedResult: "bad-cmd-key",
		},
		{
			name:           "invalid command with packed keys",
			input:          &store.StateUpdate{Cmd: -1, Keys: []string{"", "packed-primary"}},
			expectedErr:    InvalidInputErr,
			expectedResult: "packed-primary",
		},
	}

	batch := make([]*store.StateUpdate, len(cases))
	for i, tc := range cases {
		batch[i] = tc.input
	}

	responses := rc.SendUpdates(batch)
	require.Len(t, responses, len(cases))

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, responses[i])
			assert.ErrorIs(t, responses[i].Err, tc.expectedErr)
			assert.Equal(t, tc.expectedResult, responses[i].Result)
			assert.Equal(t, tc.input.PrimaryKey(), responses[i].Result)
		})
	}

	// Confirm no invalid updates were enqueued.
	assert.Empty(t, rc.GetUpdates())
}

func TestMemoryReplicator_InputNonMutation(t *testing.T) {
	rc := New(buildTestConfig(100, 100, 100))

	ticketKeys := []string{"tk-1", "tk-2"}
	ticketInput := &store.StateUpdate{
		Cmd:    store.Ticket,
		Key:    "caller-ticket-key",
		Keys:   ticketKeys,
		Value:  "ticket-bytes",
		ReplId: "caller-repl-1",
	}

	activateKeys := []string{"", "act-1", "act-2"}
	activateInput := &store.StateUpdate{
		Cmd:    store.Activate,
		Key:    "caller-act-key",
		Keys:   activateKeys,
		Value:  "caller-act-val",
		ReplId: "caller-repl-2",
	}

	deactivateKeys := []string{"deact-only"}
	deactivateInput := &store.StateUpdate{
		Cmd:    store.Deactivate,
		Key:    "caller-deact-key",
		Keys:   deactivateKeys,
		Value:  "caller-deact-val",
		ReplId: "caller-repl-3",
	}

	assignKeys := []string{"assign-extra"}
	assignInput := &store.StateUpdate{
		Cmd:    store.Assign,
		Key:    "assign-key",
		Keys:   assignKeys,
		Value:  "127.0.0.1:9000",
		ReplId: "caller-repl-4",
	}

	res := rc.SendUpdates([]*store.StateUpdate{
		ticketInput,
		activateInput,
		deactivateInput,
		assignInput,
	})
	require.Len(t, res, 4)
	for _, r := range res {
		require.NoError(t, r.Err)
	}

	// Verify caller structs and backing slices were not mutated in place.
	assert.Equal(t, "caller-ticket-key", ticketInput.Key)
	assert.Equal(t, []string{"tk-1", "tk-2"}, ticketInput.Keys)
	assert.Equal(t, "ticket-bytes", ticketInput.Value)
	assert.Equal(t, "caller-repl-1", ticketInput.ReplId)

	assert.Equal(t, "caller-act-key", activateInput.Key)
	assert.Equal(t, []string{"", "act-1", "act-2"}, activateInput.Keys)
	assert.Equal(t, "caller-act-val", activateInput.Value)
	assert.Equal(t, "caller-repl-2", activateInput.ReplId)

	assert.Equal(t, "caller-deact-key", deactivateInput.Key)
	assert.Equal(t, []string{"deact-only"}, deactivateInput.Keys)
	assert.Equal(t, "caller-deact-val", deactivateInput.Value)
	assert.Equal(t, "caller-repl-3", deactivateInput.ReplId)

	assert.Equal(t, "assign-key", assignInput.Key)
	assert.Equal(t, []string{"assign-extra"}, assignInput.Keys)
	assert.Equal(t, "127.0.0.1:9000", assignInput.Value)
	assert.Equal(t, "caller-repl-4", assignInput.ReplId)

	// Mutate caller's backing slice after SendUpdates and verify queued update is isolated.
	activateKeys[1] = "mutated-after-send"
	drained := rc.GetUpdates()
	require.Len(t, drained, 4)
	assert.Equal(t, []string{"act-1", "act-2"}, drained[1].Keys)
}

func TestMemoryReplicator_FieldNormalizationParityWithRedis(t *testing.T) {
	rc := New(buildTestConfig(100, 100, 100))

	inputs := []*store.StateUpdate{
		// Ticket with extraneous Key and Keys
		{
			Cmd:   store.Ticket,
			Key:   "extraneous-ticket-key",
			Keys:  []string{"extra-1", "extra-2"},
			Value: "binary-ticket-data",
		},
		// Unpacked Activate with extraneous Value
		{
			Cmd:   store.Activate,
			Key:   "single-act",
			Value: "extraneous-value",
		},
		// Packed Activate with 1 valid key and empty strings + extraneous Key/Value
		{
			Cmd:   store.Activate,
			Key:   "ignored-key",
			Keys:  []string{"", "only-valid-act", ""},
			Value: "extraneous-value",
		},
		// Packed Deactivate with empty strings, duplicates, and extraneous Key/Value
		{
			Cmd:   store.Deactivate,
			Key:   "ignored-deact-key",
			Keys:  []string{"", "d-1", "", "d-2", "d-1"},
			Value: "extraneous-value",
		},
		// Assign with extraneous Keys
		{
			Cmd:   store.Assign,
			Key:   "assign-ticket",
			Keys:  []string{"extra-assign-key"},
			Value: "game-server:7000",
		},
	}

	res := rc.SendUpdates(inputs)
	require.Len(t, res, len(inputs))
	for _, r := range res {
		require.NoError(t, r.Err)
	}

	updates := rc.GetUpdates()
	require.Len(t, updates, len(inputs))

	// Ticket: Key overwritten with generated ReplId, Keys nil, Value preserved.
	assert.Equal(t, res[0].Result, updates[0].Key)
	assert.Nil(t, updates[0].Keys)
	assert.Equal(t, "binary-ticket-data", updates[0].Value)

	// Unpacked Activate: Value stripped, Keys nil.
	assert.Equal(t, "single-act", updates[1].Key)
	assert.Nil(t, updates[1].Keys)
	assert.Empty(t, updates[1].Value)

	// Single-valid-key packed Activate: normalized to single Key with nil Keys, Value stripped.
	assert.Equal(t, "only-valid-act", updates[2].Key)
	assert.Nil(t, updates[2].Keys)
	assert.Empty(t, updates[2].Value)

	// Multi-key packed Deactivate: empty strings filtered, duplicates preserved, Key is first valid key, Value stripped.
	assert.Equal(t, "d-1", updates[3].Key)
	assert.Equal(t, []string{"d-1", "d-2", "d-1"}, updates[3].Keys)
	assert.Empty(t, updates[3].Value)

	// Assign: Keys stripped, Key and Value preserved.
	assert.Equal(t, "assign-ticket", updates[4].Key)
	assert.Nil(t, updates[4].Keys)
	assert.Equal(t, "game-server:7000", updates[4].Value)
}

func TestMemoryReplicator_ConcurrentStrictOrderingAndClockRegression(t *testing.T) {
	const (
		goroutines = 12
		perWorker  = 100
		total      = goroutines * perWorker
	)

	rc := New(buildTestConfig(100, total+100, total+100))

	// Phase 1: Seed last timestamp 5 seconds into the future to simulate clock regression
	// while 12 concurrent goroutines send 100 updates each (1,200 updates total).
	rc.mu.Lock()
	rc.replTS = time.Now().Add(5 * time.Second)
	rc.replCount = 0
	rc.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		workerID := g
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				resp := rc.SendUpdates([]*store.StateUpdate{
					{
						Cmd: store.Activate,
						Key: fmt.Sprintf("ticket-%d-%d", workerID, i),
					},
				})
				require.Len(t, resp, 1)
				require.NoError(t, resp[0].Err)
			}
		}()
	}
	wg.Wait()

	drained := rc.GetUpdates()
	require.Len(t, drained, total)
	for i, u := range drained {
		assert.Regexp(t, rc.GetReplIdValidator(), u.ReplId)
		if i > 0 {
			assert.Equal(t, -1, store.CompareReplIds(drained[i-1].ReplId, u.ReplId),
				"expected strictly increasing ReplIds at index %d: %s vs %s", i, drained[i-1].ReplId, u.ReplId)
		}
	}

	// Phase 2: Rewind replicator's last timestamp 1 second into the past, send one additional
	// Activate update, and verify its replication ID resets its sequence suffix to "-0".
	rc.mu.Lock()
	rc.replTS = time.Now().Add(-1 * time.Second)
	rc.mu.Unlock()

	postResp := rc.SendUpdates([]*store.StateUpdate{
		{Cmd: store.Activate, Key: "post-rewind-ticket"},
	})
	require.Len(t, postResp, 1)
	require.NoError(t, postResp[0].Err)
	assert.True(t, strings.HasSuffix(postResp[0].Result, "-0"),
		"expected replication ID suffix -0 after timestamp advance, got %s", postResp[0].Result)

	postUpdates := rc.GetUpdates()
	require.Len(t, postUpdates, 1)
	assert.True(t, strings.HasSuffix(postUpdates[0].ReplId, "-0"),
		"expected drained update ReplId suffix -0, got %s", postUpdates[0].ReplId)
}

func TestMemoryReplicator_FullChannelNonBlockingOverflowAndRollback(t *testing.T) {
	const capacity = 4
	rc := New(buildTestConfig(50, capacity, capacity))

	// Pin timestamp into the future so sequence numbers increment deterministically.
	pinnedTS := time.Now().Add(5 * time.Second)
	rc.mu.Lock()
	rc.replTS = pinnedTS
	rc.replCount = 0
	rc.mu.Unlock()

	for i := 1; i <= capacity; i++ {
		res := rc.SendUpdates([]*store.StateUpdate{
			{Cmd: store.Activate, Key: fmt.Sprintf("fill-%d", i)},
		})
		require.Len(t, res, 1)
		require.NoError(t, res[0].Err)
		assert.True(t, strings.HasSuffix(res[0].Result, fmt.Sprintf("-%d", i)),
			"expected sequence -%d, got %s", i, res[0].Result)
	}

	// Next SendUpdates call must fail immediately without blocking and roll back the sequence counter.
	start := time.Now()
	overflowRes := rc.SendUpdates([]*store.StateUpdate{
		{Cmd: store.Activate, Key: "overflow-ticket"},
	})
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 50*time.Millisecond, "SendUpdates on a full channel must not block")
	require.Len(t, overflowRes, 1)
	assert.ErrorIs(t, overflowRes[0].Err, FullReplicationQueueErr)
	assert.Equal(t, "overflow-ticket", overflowRes[0].Result)

	// Drain the channel and verify the next send receives the consecutive sequence number (-5).
	drained := rc.GetUpdates()
	require.Len(t, drained, capacity)

	afterDrainRes := rc.SendUpdates([]*store.StateUpdate{
		{Cmd: store.Activate, Key: "after-drain-ticket"},
	})
	require.Len(t, afterDrainRes, 1)
	require.NoError(t, afterDrainRes[0].Err)
	assert.True(t, strings.HasSuffix(afterDrainRes[0].Result, fmt.Sprintf("-%d", capacity+1)),
		"expected consecutive sequence -%d after rollback and drain, got %s", capacity+1, afterDrainRes[0].Result)
}

func TestMemoryReplicator_GetUpdatesTimeoutAndBatching(t *testing.T) {
	t.Run("returns empty non-nil slice after wait timeout when idle", func(t *testing.T) {
		const waitMs = 40
		rc := New(buildTestConfig(waitMs, 10, 10))

		start := time.Now()
		updates := rc.GetUpdates()
		elapsed := time.Since(start)

		require.NotNil(t, updates)
		assert.Empty(t, updates)
		assert.GreaterOrEqual(t, elapsed, 30*time.Millisecond)
	})

	t.Run("returns immediately without waiting for timeout when updates are buffered", func(t *testing.T) {
		rc := New(buildTestConfig(2000, 10, 20))

		res := rc.SendUpdates([]*store.StateUpdate{
			{Cmd: store.Activate, Key: "immediate-1"},
			{Cmd: store.Activate, Key: "immediate-2"},
		})
		require.Len(t, res, 2)

		start := time.Now()
		updates := rc.GetUpdates()
		elapsed := time.Since(start)

		require.Len(t, updates, 2)
		assert.Equal(t, "immediate-1", updates[0].Key)
		assert.Equal(t, "immediate-2", updates[1].Key)
		assert.Less(t, elapsed, 250*time.Millisecond)
	})

	t.Run("caps each poll batch at OM_CACHE_IN_MAX_UPDATES_PER_POLL", func(t *testing.T) {
		const maxPerPoll = 3
		const totalUpdates = 7
		rc := New(buildTestConfig(500, maxPerPoll, 20))

		batch := make([]*store.StateUpdate, totalUpdates)
		for i := 0; i < totalUpdates; i++ {
			batch[i] = &store.StateUpdate{
				Cmd: store.Deactivate,
				Key: fmt.Sprintf("batch-key-%d", i),
			}
		}
		res := rc.SendUpdates(batch)
		require.Len(t, res, totalUpdates)

		poll1 := rc.GetUpdates()
		require.Len(t, poll1, 3)
		assert.Equal(t, "batch-key-0", poll1[0].Key)
		assert.Equal(t, "batch-key-1", poll1[1].Key)
		assert.Equal(t, "batch-key-2", poll1[2].Key)

		poll2 := rc.GetUpdates()
		require.Len(t, poll2, 3)
		assert.Equal(t, "batch-key-3", poll2[0].Key)
		assert.Equal(t, "batch-key-4", poll2[1].Key)
		assert.Equal(t, "batch-key-5", poll2[2].Key)

		poll3 := rc.GetUpdates()
		require.Len(t, poll3, 1)
		assert.Equal(t, "batch-key-6", poll3[0].Key)
	})
}
