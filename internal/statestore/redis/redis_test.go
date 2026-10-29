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
package redisReplicator

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/googleforgames/open-match2/v2/internal/config"
	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	memoryReplicator "github.com/googleforgames/open-match2/v2/internal/statestore/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedCommand struct {
	name string
	args []interface{}
}

// statelessFakeConn records pipelined Send commands and Do calls, returning configured replies.
type statelessFakeConn struct {
	mu       sync.Mutex
	sentCmds []recordedCommand
	doCmds   []recordedCommand
	doReply  interface{}
	doErr    error
	closed   bool
}

func (c *statelessFakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *statelessFakeConn) Err() error {
	return nil
}

func (c *statelessFakeConn) Do(commandName string, args ...interface{}) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copiedArgs := make([]interface{}, len(args))
	copy(copiedArgs, args)
	c.doCmds = append(c.doCmds, recordedCommand{name: commandName, args: copiedArgs})
	return c.doReply, c.doErr
}

func (c *statelessFakeConn) Send(commandName string, args ...interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	copiedArgs := make([]interface{}, len(args))
	copy(copiedArgs, args)
	c.sentCmds = append(c.sentCmds, recordedCommand{name: commandName, args: copiedArgs})
	return nil
}

func (c *statelessFakeConn) Flush() error {
	return nil
}

func (c *statelessFakeConn) Receive() (interface{}, error) {
	return nil, nil
}

func (c *statelessFakeConn) snapshotSent() []recordedCommand {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]recordedCommand, len(c.sentCmds))
	copy(out, c.sentCmds)
	return out
}

type streamRecord struct {
	id     string
	fields []string
}

// statefulRedisStream emulates an in-memory Redis Stream supporting XADD, XTRIM MINID ~, and XREAD.
type statefulRedisStream struct {
	mu        sync.Mutex
	entries   []streamRecord
	lastTS    int64
	lastSeq   int64
	sentLog   []recordedCommand
	trimCalls []recordedCommand
}

func newStatefulRedisStream() *statefulRedisStream {
	return &statefulRedisStream{
		entries: make([]streamRecord, 0),
	}
}

func (s *statefulRedisStream) nextStreamID() string {
	nowMs := time.Now().UnixMilli()
	if nowMs <= s.lastTS {
		s.lastSeq++
	} else {
		s.lastTS = nowMs
		s.lastSeq = 0
	}
	return fmt.Sprintf("%013d-%d", s.lastTS, s.lastSeq)
}

func (s *statefulRedisStream) addRawEntry(id string, fields []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make([]string, len(fields))
	copy(copied, fields)
	s.entries = append(s.entries, streamRecord{id: id, fields: copied})
	if ts, err := store.ParseReplIdTimestampMs(id); err == nil && ts > s.lastTS {
		s.lastTS = ts
	}
}

func (s *statefulRedisStream) execute(cmd string, args []interface{}) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	upper := strings.ToUpper(cmd)
	switch upper {
	case "PING":
		return "PONG", nil
	case "XADD":
		if len(args) < 4 {
			return redis.Error("ERR wrong number of arguments for 'xadd' command"), nil
		}
		fieldArgs := args[2:]
		if len(fieldArgs)%2 != 0 {
			return redis.Error("ERR wrong number of arguments for 'xadd' command"), nil
		}
		fields := make([]string, len(fieldArgs))
		for i, a := range fieldArgs {
			fields[i] = fmt.Sprint(a)
		}
		id := s.nextStreamID()
		s.entries = append(s.entries, streamRecord{id: id, fields: fields})
		return id, nil
	case "XTRIM":
		copiedArgs := make([]interface{}, len(args))
		copy(copiedArgs, args)
		s.trimCalls = append(s.trimCalls, recordedCommand{name: "XTRIM", args: copiedArgs})
		if len(args) < 3 {
			return int64(0), nil
		}
		threshStr := fmt.Sprint(args[len(args)-1])
		threshMs, err := strconv.ParseInt(threshStr, 10, 64)
		if err != nil {
			return int64(0), err
		}
		kept := make([]streamRecord, 0, len(s.entries))
		var removed int64
		for _, entry := range s.entries {
			entryTS, parseErr := store.ParseReplIdTimestampMs(entry.id)
			if parseErr == nil && entryTS < threshMs {
				removed++
				continue
			}
			kept = append(kept, entry)
		}
		s.entries = kept
		return removed, nil
	case "XREAD":
		maxCount := 0
		lastID := "0-0"
		for i := 0; i < len(args); i++ {
			token := strings.ToUpper(fmt.Sprint(args[i]))
			if token == "COUNT" && i+1 < len(args) {
				if parsed, err := strconv.Atoi(fmt.Sprint(args[i+1])); err == nil {
					maxCount = parsed
				}
				i++
			} else if token == "STREAMS" && i+2 < len(args) {
				lastID = fmt.Sprint(args[i+2])
				break
			}
		}
		normalizedCursor := lastID
		if !strings.Contains(normalizedCursor, "-") {
			normalizedCursor += "-0"
		}

		matched := make([]interface{}, 0)
		for _, entry := range s.entries {
			if store.CompareReplIds(entry.id, normalizedCursor) > 0 {
				rawFields := make([]interface{}, len(entry.fields))
				for idx, f := range entry.fields {
					rawFields[idx] = []byte(f)
				}
				matched = append(matched, []interface{}{[]byte(entry.id), rawFields})
				if maxCount > 0 && len(matched) >= maxCount {
					break
				}
			}
		}
		if len(matched) == 0 {
			return nil, nil
		}
		return []interface{}{
			[]interface{}{
				[]byte("om-replication"),
				matched,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported command %q", cmd)
	}
}

type statefulRedisConn struct {
	stream   *statefulRedisStream
	pipeline []recordedCommand
}

func (c *statefulRedisConn) Close() error {
	return nil
}

func (c *statefulRedisConn) Err() error {
	return nil
}

func (c *statefulRedisConn) Send(commandName string, args ...interface{}) error {
	copiedArgs := make([]interface{}, len(args))
	copy(copiedArgs, args)
	rec := recordedCommand{name: commandName, args: copiedArgs}
	c.pipeline = append(c.pipeline, rec)
	c.stream.mu.Lock()
	c.stream.sentLog = append(c.stream.sentLog, rec)
	c.stream.mu.Unlock()
	return nil
}

func (c *statefulRedisConn) Do(commandName string, args ...interface{}) (interface{}, error) {
	if commandName == "" {
		replies := make([]interface{}, 0, len(c.pipeline))
		pending := c.pipeline
		c.pipeline = nil
		for _, cmd := range pending {
			rep, err := c.stream.execute(cmd.name, cmd.args)
			if err != nil {
				return nil, err
			}
			replies = append(replies, rep)
		}
		return replies, nil
	}
	return c.stream.execute(commandName, args)
}

func (c *statefulRedisConn) Flush() error {
	return nil
}

func (c *statefulRedisConn) Receive() (interface{}, error) {
	return nil, nil
}

func newTestRedisReplicatorWithConn(conn redis.Conn) *redisReplicator {
	cfg := config.Read()
	ctx, cancel := context.WithCancel(context.Background())
	pool := &redis.Pool{
		MaxIdle:   4,
		MaxActive: 4,
		Wait:      true,
		Dial: func() (redis.Conn, error) {
			return conn, nil
		},
	}
	return &redisReplicator{
		rConnPool:       pool,
		wConnPool:       pool,
		cfg:             cfg,
		replId:          "0-0",
		replIdValidator: regexp.MustCompile(`^\d{13}-\d+$`),
		ctx:             ctx,
		cancel:          cancel,
	}
}

func newTestRedisReplicatorWithStream(stream *statefulRedisStream, waitTimeoutMs, maxUpdatesPerPoll int) *redisReplicator {
	cfg := config.Read()
	cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", waitTimeoutMs)
	cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", maxUpdatesPerPoll)
	ctx, cancel := context.WithCancel(context.Background())
	pool := &redis.Pool{
		MaxIdle:   8,
		MaxActive: 8,
		Wait:      true,
		Dial: func() (redis.Conn, error) {
			return &statefulRedisConn{stream: stream}, nil
		},
	}
	return &redisReplicator{
		rConnPool:       pool,
		wConnPool:       pool,
		cfg:             cfg,
		replId:          "0-0",
		replIdValidator: regexp.MustCompile(`^\d{13}-\d+$`),
		ctx:             ctx,
		cancel:          cancel,
	}
}

// Scenario 1: Pipeline Reply Alignment with Skipped Invalid Updates
func TestSendUpdates_PipelineReplyAlignmentWithSkippedInvalidUpdates(t *testing.T) {
	t.Run("correlates pipelined replies with valid caller indices across skipped invalid items", func(t *testing.T) {
		fakeConn := &statelessFakeConn{
			doReply: []interface{}{
				"1700000000001-0", // reply for caller index 0 (Ticket)
				"1700000000001-1", // reply for caller index 2 (Activate)
				"1700000000001-2", // reply for caller index 4 (Deactivate packed)
				int64(2),          // reply for trailing XTRIM
			},
		}
		rr := newTestRedisReplicatorWithConn(fakeConn)
		defer func() { _ = rr.Close() }()

		batch := []*store.StateUpdate{
			{Cmd: store.Ticket, Value: "valid-ticket-bytes"},
			{Cmd: store.Activate, Key: ""},
			{Cmd: store.Activate, Key: "valid-act-key"},
			{Cmd: store.Assign, Key: "missing-assign-val-key", Value: ""},
			{Cmd: store.Deactivate, Keys: []string{"deact-1", "deact-2"}},
			{Cmd: 999, Key: "invalid-cmd-key"},
		}

		res := rr.SendUpdates(batch)
		require.Len(t, res, 6)

		// Sent commands should be 3 XADDs + 1 XTRIM = 4 commands total.
		sent := fakeConn.snapshotSent()
		require.Len(t, sent, 4)
		assert.Equal(t, "XADD", sent[0].name)
		assert.Equal(t, "XADD", sent[1].name)
		assert.Equal(t, "XADD", sent[2].name)
		assert.Equal(t, "XTRIM", sent[3].name)

		// Index 0: valid Ticket
		require.NoError(t, res[0].Err)
		assert.Equal(t, "1700000000001-0", res[0].Result)

		// Index 1: invalid Activate (missing key)
		assert.ErrorIs(t, res[1].Err, NoTicketKeyErr)
		assert.Equal(t, "", res[1].Result)

		// Index 2: valid Activate
		require.NoError(t, res[2].Err)
		assert.Equal(t, "1700000000001-1", res[2].Result)

		// Index 3: invalid Assign (missing assignment value)
		assert.ErrorIs(t, res[3].Err, NoAssignmentErr)
		assert.Equal(t, "missing-assign-val-key", res[3].Result)

		// Index 4: valid packed Deactivate
		require.NoError(t, res[4].Err)
		assert.Equal(t, "1700000000001-2", res[4].Result)

		// Index 5: invalid Cmd
		assert.ErrorIs(t, res[5].Err, InvalidInputErr)
		assert.Equal(t, "invalid-cmd-key", res[5].Result)
	})

	t.Run("guards against truncated or empty pipeline reply slices without panicking", func(t *testing.T) {
		// 2 valid updates sent + 1 XTRIM (expects 3 replies), but Redis returns only 2 replies (1 update + 1 XTRIM).
		fakeConn := &statelessFakeConn{
			doReply: []interface{}{
				"1700000000002-0",
				int64(0),
			},
		}
		rr := newTestRedisReplicatorWithConn(fakeConn)
		defer func() { _ = rr.Close() }()

		res := rr.SendUpdates([]*store.StateUpdate{
			{Cmd: store.Activate, Key: "first-key"},
			{Cmd: store.Activate, Key: "second-key-truncated"},
		})
		require.Len(t, res, 2)
		require.NoError(t, res[0].Err)
		assert.Equal(t, "1700000000002-0", res[0].Result)

		require.Error(t, res[1].Err)
		assert.Contains(t, res[1].Err.Error(), "missing Redis response for update")
		assert.Equal(t, "second-key-truncated", res[1].Result)
	})

	t.Run("preserves per-item validation error when batch Do fails and isolates XTRIM conversion error", func(t *testing.T) {
		batchFailErr := fmt.Errorf("connection reset by peer")
		fakeConn := &statelessFakeConn{
			doReply: nil,
			doErr:   batchFailErr,
		}
		rr := newTestRedisReplicatorWithConn(fakeConn)
		defer func() { _ = rr.Close() }()

		res := rr.SendUpdates([]*store.StateUpdate{
			{Cmd: store.Ticket, Key: "invalid-ticket", Value: ""},
			{Cmd: store.Activate, Key: "valid-key-failed-in-do"},
		})
		require.Len(t, res, 2)
		assert.ErrorIs(t, res[0].Err, NoTicketDataErr)
		assert.Equal(t, "invalid-ticket", res[0].Result)
		assert.ErrorIs(t, res[1].Err, batchFailErr)
		assert.Equal(t, "valid-key-failed-in-do", res[1].Result)

		// Also verify non-integer XTRIM reply does not clobber valid update results.
		fakeConnNonIntTrim := &statelessFakeConn{
			doReply: []interface{}{
				"1700000000003-0",
				"NOT_AN_INT",
			},
		}
		rr2 := newTestRedisReplicatorWithConn(fakeConnNonIntTrim)
		defer func() { _ = rr2.Close() }()

		res2 := rr2.SendUpdates([]*store.StateUpdate{
			{Cmd: store.Activate, Key: "valid-key"},
		})
		require.Len(t, res2, 1)
		require.NoError(t, res2[0].Err)
		assert.Equal(t, "1700000000003-0", res2[0].Result)
	})
}

// Scenario 2: Approximate XTRIM Retention Window
func TestSendUpdates_ApproximateXTrimRetentionWindow(t *testing.T) {
	stream := newStatefulRedisStream()
	rr := newTestRedisReplicatorWithStream(stream, 50, 100)
	defer func() { _ = rr.Close() }()

	const (
		ticketTTLMs     = int64(600000)
		additionalTTLMs = int64(300000)
		combinedWindow  = ticketTTLMs + additionalTTLMs // 900,000 ms
	)
	rr.cfg.Set("OM_CACHE_TICKET_TTL_MS", ticketTTLMs)
	rr.cfg.Set("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS", additionalTTLMs)

	nowMs := time.Now().UnixMilli()
	expiredID := fmt.Sprintf("%013d-0", nowMs-combinedWindow-50000)
	withinCombinedWindowID := fmt.Sprintf("%013d-0", nowMs-ticketTTLMs-100000) // older than ticketTTL, newer than combined
	recentID := fmt.Sprintf("%013d-0", nowMs-5000)

	stream.addRawEntry(expiredID, []string{"activate", "old-expired-ticket"})
	stream.addRawEntry(withinCombinedWindowID, []string{"activate", "within-combined-window-ticket"})
	stream.addRawEntry(recentID, []string{"activate", "recent-ticket"})

	beforeSendMs := time.Now().UnixMilli()
	res := rr.SendUpdates([]*store.StateUpdate{
		{Cmd: store.Activate, Key: "newly-sent-ticket"},
	})
	afterSendMs := time.Now().UnixMilli()
	require.Len(t, res, 1)
	require.NoError(t, res[0].Err)

	// Verify XTRIM command shape and threshold calculation.
	stream.mu.Lock()
	require.Len(t, stream.trimCalls, 1)
	trimArgs := stream.trimCalls[0].args
	stream.mu.Unlock()

	require.Len(t, trimArgs, 4)
	assert.Equal(t, "om-replication", trimArgs[0])
	assert.Equal(t, "MINID", trimArgs[1])
	assert.Equal(t, "~", trimArgs[2])

	threshMs, err := strconv.ParseInt(fmt.Sprint(trimArgs[3]), 10, 64)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, threshMs, beforeSendMs-combinedWindow)
	assert.LessOrEqual(t, threshMs, afterSendMs-combinedWindow)

	// Verify expiredID was evicted while withinCombinedWindowID, recentID, and the newly sent entry remain.
	updates := rr.GetUpdates()
	require.Len(t, updates, 3)
	assert.Equal(t, "within-combined-window-ticket", updates[0].Key)
	assert.Equal(t, "recent-ticket", updates[1].Key)
	assert.Equal(t, "newly-sent-ticket", updates[2].Key)
}

// Scenario 3: Packed Update Empty-Key Filtering and Odd-Length Padding
func TestSendUpdates_PackedUpdateEmptyKeyFilteringAndOddLengthPadding(t *testing.T) {
	t.Run("filters empty keys, pads even valid key count with trailing empty string, and preserves odd valid key count", func(t *testing.T) {
		fakeConn := &statelessFakeConn{
			doReply: []interface{}{
				"1700000000010-0",
				"1700000000010-1",
				int64(0),
			},
		}
		rr := newTestRedisReplicatorWithConn(fakeConn)
		defer func() { _ = rr.Close() }()

		res := rr.SendUpdates([]*store.StateUpdate{
			// 2 valid keys -> 5 args before padding -> padded with trailing "" to 6 args
			{Cmd: store.Activate, Keys: []string{"", "k1", "", "k2", ""}},
			// 3 valid keys -> 6 args -> no trailing "" padding
			{Cmd: store.Deactivate, Keys: []string{"d1", "", "d2", "d3"}},
		})
		require.Len(t, res, 2)
		require.NoError(t, res[0].Err)
		require.NoError(t, res[1].Err)

		sent := fakeConn.snapshotSent()
		require.Len(t, sent, 3) // 2 XADDs + 1 XTRIM

		assert.Equal(t, []interface{}{"om-replication", "*", "activate", "k1", "k2", ""}, sent[0].args)
		assert.Equal(t, []interface{}{"om-replication", "*", "deactivate", "d1", "d2", "d3"}, sent[1].args)
	})

	t.Run("returns NoTicketKeyErr with PrimaryKey when all packed keys are empty and skips pipeline flush", func(t *testing.T) {
		fakeConn := &statelessFakeConn{}
		rr := newTestRedisReplicatorWithConn(fakeConn)
		defer func() { _ = rr.Close() }()

		res := rr.SendUpdates([]*store.StateUpdate{
			{Cmd: store.Activate, Key: "fallback-key", Keys: []string{"", ""}},
			{Cmd: store.Deactivate, Keys: []string{""}},
		})
		require.Len(t, res, 2)
		assert.ErrorIs(t, res[0].Err, NoTicketKeyErr)
		assert.Equal(t, "fallback-key", res[0].Result)
		assert.ErrorIs(t, res[1].Err, NoTicketKeyErr)
		assert.Equal(t, "", res[1].Result)

		assert.Empty(t, fakeConn.snapshotSent(), "no Redis commands should be sent when all updates are invalid")
	})
}

// Scenario 4: Redis Command Log Formatting
func TestFormatRedisCommandForLog(t *testing.T) {
	t.Run("summarizes 4-argument ticket command payload by byte count", func(t *testing.T) {
		rawPayload := "\x08\x96\x01\x12\x0cfa-binary-pb"
		formatted := formatRedisCommandForLog("XADD", []interface{}{"om-replication", "*", "ticket", rawPayload})
		assert.Equal(t, fmt.Sprintf("XADD om-replication * ticket <%d bytes>", len(rawPayload)), formatted)
		assert.NotContains(t, formatted, "fa-binary-pb")
	})

	t.Run("strips trailing empty padding token on packed commands", func(t *testing.T) {
		formatted := formatRedisCommandForLog("XADD", []interface{}{"om-replication", "*", "activate", "t-1", "t-2", ""})
		assert.Equal(t, "XADD om-replication * activate t-1 t-2", formatted)
	})

	t.Run("truncates argument lists beyond 8 tokens with ellipsis", func(t *testing.T) {
		args := []interface{}{"om-replication", "*", "deactivate", "k1", "k2", "k3", "k4", "k5", "k6", "k7"}
		formatted := formatRedisCommandForLog("XADD", args)
		assert.Equal(t, "XADD om-replication * deactivate k1 k2 k3 k4 k5 ...", formatted)
	})
}

// Scenario 5: GetUpdates Unpacking and Unknown Command Skipping
func TestGetUpdates_UnpackingAndUnknownCommandSkipping(t *testing.T) {
	fakeConn := &statelessFakeConn{
		doReply: []interface{}{
			[]interface{}{
				[]byte("om-replication"),
				[]interface{}{
					[]interface{}{
						[]byte("1700000000100-0"),
						[]interface{}{[]byte("activate"), []byte("k1"), []byte("k2"), []byte("k3")},
					},
					[]interface{}{
						[]byte("1700000000100-1"),
						[]interface{}{[]byte("deactivate"), []byte("d1"), []byte("d2"), []byte("")},
					},
					[]interface{}{
						[]byte("1700000000100-2"),
						[]interface{}{[]byte("activate"), []byte("single-k")},
					},
					[]interface{}{
						[]byte("1700000000100-3"),
						[]interface{}{[]byte("unknown_future_cmd"), []byte("arg1")},
					},
				},
			},
		},
	}
	rr := newTestRedisReplicatorWithConn(fakeConn)
	defer func() { _ = rr.Close() }()

	updates := rr.GetUpdates()
	require.Len(t, updates, 3)

	// 0: 3-key odd-count activate
	assert.Equal(t, store.Activate, updates[0].Cmd)
	assert.Equal(t, "k1", updates[0].Key)
	assert.Equal(t, []string{"k1", "k2", "k3"}, updates[0].Keys)
	assert.Equal(t, "1700000000100-0", updates[0].ReplId)

	// 1: 2-key even-count padded deactivate (trailing "" stripped)
	assert.Equal(t, store.Deactivate, updates[1].Cmd)
	assert.Equal(t, "d1", updates[1].Key)
	assert.Equal(t, []string{"d1", "d2"}, updates[1].Keys)
	assert.Equal(t, "1700000000100-1", updates[1].ReplId)

	// 2: single-key activate
	assert.Equal(t, store.Activate, updates[2].Cmd)
	assert.Equal(t, "single-k", updates[2].Key)
	assert.Nil(t, updates[2].Keys)
	assert.Equal(t, "1700000000100-2", updates[2].ReplId)

	// Unknown command at 1700000000100-3 was skipped, but cursor advanced to 1700000000100-3.
	assert.Equal(t, "1700000000100-3", rr.replId)
}

// Scenario 6: GetUpdates Cursor Advancement Past Malformed Entries
func TestGetUpdates_CursorAdvancementPastMalformedEntries(t *testing.T) {
	// First poll contains entries with <2 field tokens and a malformed assign with <4 field tokens.
	fakeConn := &statelessFakeConn{
		doReply: []interface{}{
			[]interface{}{
				[]byte("om-replication"),
				[]interface{}{
					[]interface{}{
						[]byte("1700000000200-0"),
						[]interface{}{[]byte("activate")}, // fewer than 2 field tokens
					},
					[]interface{}{
						[]byte("1700000000200-1"),
						[]interface{}{[]byte("assign"), []byte("t-1"), []byte("connection")}, // fewer than 4 tokens for assign
					},
				},
			},
		},
	}
	rr := newTestRedisReplicatorWithConn(fakeConn)
	defer func() { _ = rr.Close() }()

	poll1 := rr.GetUpdates()
	assert.Empty(t, poll1)
	assert.Equal(t, "1700000000200-1", rr.replId, "replId cursor must advance past malformed entries")

	// Next poll delivers a valid entry after 1700000000200-1.
	fakeConn.mu.Lock()
	fakeConn.doReply = []interface{}{
		[]interface{}{
			[]byte("om-replication"),
			[]interface{}{
				[]interface{}{
					[]byte("1700000000200-2"),
					[]interface{}{[]byte("assign"), []byte("t-1"), []byte("connection"), []byte("10.0.0.1:7777")},
				},
			},
		},
	}
	fakeConn.mu.Unlock()

	poll2 := rr.GetUpdates()
	require.Len(t, poll2, 1)
	assert.Equal(t, store.Assign, poll2[0].Cmd)
	assert.Equal(t, "t-1", poll2[0].Key)
	assert.Equal(t, "10.0.0.1:7777", poll2[0].Value)
	assert.Equal(t, "1700000000200-2", rr.replId)
}

// Scenario 7: GetUpdates Cursor Preservation on Invalid Entry ID vs. Advancement on Invalid Field Array
func TestGetUpdates_CursorPreservationOnInvalidIDVsAdvancementOnInvalidFields(t *testing.T) {
	t.Run("preserves replId cursor when stream entry ID is empty or non-string", func(t *testing.T) {
		fakeConn := &statelessFakeConn{
			doReply: []interface{}{
				[]interface{}{
					[]byte("om-replication"),
					[]interface{}{
						[]interface{}{
							[]byte(""),
							[]interface{}{[]byte("activate"), []byte("k1")},
						},
						[]interface{}{
							nil,
							[]interface{}{[]byte("activate"), []byte("k2")},
						},
						[]interface{}{
							[]interface{}{"nested-non-string-id"},
							[]interface{}{[]byte("activate"), []byte("k3")},
						},
					},
				},
			},
		}
		rr := newTestRedisReplicatorWithConn(fakeConn)
		defer func() { _ = rr.Close() }()
		rr.replId = "1700000000300-0"

		updates := rr.GetUpdates()
		assert.Empty(t, updates)
		assert.Equal(t, "1700000000300-0", rr.replId, "cursor must be preserved when entry ID is invalid")
	})

	t.Run("advances replId cursor when stream entry ID is valid but field array is nil or non-slice", func(t *testing.T) {
		fakeConn := &statelessFakeConn{
			doReply: []interface{}{
				[]interface{}{
					[]byte("om-replication"),
					[]interface{}{
						[]interface{}{
							[]byte("1700000000300-1"),
							nil,
						},
						[]interface{}{
							[]byte("1700000000300-2"),
							12345,
						},
					},
				},
			},
		}
		rr := newTestRedisReplicatorWithConn(fakeConn)
		defer func() { _ = rr.Close() }()
		rr.replId = "1700000000300-0"

		updates := rr.GetUpdates()
		assert.Empty(t, updates)
		assert.Equal(t, "1700000000300-2", rr.replId, "cursor must advance past entries with valid IDs and invalid field arrays")
	})
}

// Scenario 8: Cross-Replicator Conformance Suite (8 subtests across memoryReplicator and redisReplicator)
func TestReplicatorConformanceSuite(t *testing.T) {
	backends := []struct {
		name    string
		factory func(waitTimeoutMs, maxUpdatesPerPoll int) (store.StateReplicator, func())
	}{
		{
			name: "memoryReplicator",
			factory: func(waitTimeoutMs, maxUpdatesPerPoll int) (store.StateReplicator, func()) {
				cfg := config.Read()
				cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", waitTimeoutMs)
				cfg.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", maxUpdatesPerPoll)
				cfg.Set("OM_CACHE_IN_QUEUE_BUFFER_SIZE", 1000)
				return memoryReplicator.New(cfg), func() {}
			},
		},
		{
			name: "redisReplicator",
			factory: func(waitTimeoutMs, maxUpdatesPerPoll int) (store.StateReplicator, func()) {
				stream := newStatefulRedisStream()
				rr := newTestRedisReplicatorWithStream(stream, waitTimeoutMs, maxUpdatesPerPoll)
				return rr, func() { _ = rr.Close() }
			},
		},
	}

	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			// Subtest 1: Round-trip updates
			t.Run("1_round_trip_updates", func(t *testing.T) {
				rep, cleanup := backend.factory(50, 100)
				defer cleanup()

				inputs := []*store.StateUpdate{
					{Cmd: store.Ticket, Value: "proto-ticket-payload"},
					{Cmd: store.Activate, Key: "ticket-1"},
					{Cmd: store.Activate, Keys: []string{"ticket-2", "ticket-3", "ticket-4"}},
					{Cmd: store.Deactivate, Key: "ticket-5"},
					{Cmd: store.Deactivate, Keys: []string{"ticket-6", "ticket-7"}},
					{Cmd: store.Assign, Key: "ticket-1", Value: "10.0.0.1:7777"},
				}

				res := rep.SendUpdates(inputs)
				require.Len(t, res, len(inputs))
				for i, r := range res {
					require.NoError(t, r.Err, "unexpected error at index %d", i)
					assert.Regexp(t, rep.GetReplIdValidator(), r.Result)
				}

				updates := rep.GetUpdates()
				require.Len(t, updates, len(inputs))

				assert.Equal(t, store.Ticket, updates[0].Cmd)
				assert.Equal(t, res[0].Result, updates[0].Key)
				assert.Nil(t, updates[0].Keys)
				assert.Equal(t, "proto-ticket-payload", updates[0].Value)
				assert.Equal(t, res[0].Result, updates[0].ReplId)

				assert.Equal(t, store.Activate, updates[1].Cmd)
				assert.Equal(t, "ticket-1", updates[1].Key)
				assert.Nil(t, updates[1].Keys)
				assert.Equal(t, res[1].Result, updates[1].ReplId)

				assert.Equal(t, store.Activate, updates[2].Cmd)
				assert.Equal(t, "ticket-2", updates[2].Key)
				assert.Equal(t, []string{"ticket-2", "ticket-3", "ticket-4"}, updates[2].Keys)
				assert.Equal(t, res[2].Result, updates[2].ReplId)

				assert.Equal(t, store.Deactivate, updates[3].Cmd)
				assert.Equal(t, "ticket-5", updates[3].Key)
				assert.Nil(t, updates[3].Keys)
				assert.Equal(t, res[3].Result, updates[3].ReplId)

				assert.Equal(t, store.Deactivate, updates[4].Cmd)
				assert.Equal(t, "ticket-6", updates[4].Key)
				assert.Equal(t, []string{"ticket-6", "ticket-7"}, updates[4].Keys)
				assert.Equal(t, res[4].Result, updates[4].ReplId)

				assert.Equal(t, store.Assign, updates[5].Cmd)
				assert.Equal(t, "ticket-1", updates[5].Key)
				assert.Equal(t, "10.0.0.1:7777", updates[5].Value)
				assert.Equal(t, res[5].Result, updates[5].ReplId)
			})

			// Subtest 2: Validation errors and PrimaryKey results
			t.Run("2_validation_errors_and_primary_key_results", func(t *testing.T) {
				rep, cleanup := backend.factory(20, 100)
				defer cleanup()

				inputs := []*store.StateUpdate{
					{Cmd: store.Ticket, Key: "caller-ticket", Value: ""},
					{Cmd: store.Activate, Key: ""},
					{Cmd: store.Activate, Key: "fallback-act", Keys: []string{"", ""}},
					{Cmd: store.Deactivate, Keys: []string{""}},
					{Cmd: store.Assign, Key: "", Value: "10.0.0.1:7777"},
					{Cmd: store.Assign, Key: "assign-key", Value: ""},
					{Cmd: 77, Keys: []string{"", "primary-from-keys"}},
				}

				res := rep.SendUpdates(inputs)
				require.Len(t, res, len(inputs))

				assert.ErrorIs(t, res[0].Err, store.NoTicketDataErr)
				assert.Equal(t, "caller-ticket", res[0].Result)

				assert.ErrorIs(t, res[1].Err, store.NoTicketKeyErr)
				assert.Equal(t, "", res[1].Result)

				assert.ErrorIs(t, res[2].Err, store.NoTicketKeyErr)
				assert.Equal(t, "fallback-act", res[2].Result)

				assert.ErrorIs(t, res[3].Err, store.NoTicketKeyErr)
				assert.Equal(t, "", res[3].Result)

				assert.ErrorIs(t, res[4].Err, store.NoTicketKeyErr)
				assert.Equal(t, "", res[4].Result)

				assert.ErrorIs(t, res[5].Err, store.NoAssignmentErr)
				assert.Equal(t, "assign-key", res[5].Result)

				assert.ErrorIs(t, res[6].Err, store.InvalidInputErr)
				assert.Equal(t, "primary-from-keys", res[6].Result)

				assert.Empty(t, rep.GetUpdates())
			})

			// Subtest 3: Input non-mutation
			t.Run("3_input_non_mutation", func(t *testing.T) {
				rep, cleanup := backend.factory(50, 100)
				defer cleanup()

				actKeys := []string{"", "k1", "k2"}
				actUpdate := &store.StateUpdate{
					Cmd:    store.Activate,
					Key:    "caller-key",
					Keys:   actKeys,
					Value:  "caller-val",
					ReplId: "caller-repl",
				}
				ticketUpdate := &store.StateUpdate{
					Cmd:    store.Ticket,
					Key:    "caller-ticket-key",
					Value:  "ticket-payload",
					ReplId: "caller-ticket-repl",
				}

				res := rep.SendUpdates([]*store.StateUpdate{actUpdate, ticketUpdate})
				require.Len(t, res, 2)
				require.NoError(t, res[0].Err)
				require.NoError(t, res[1].Err)

				assert.Equal(t, "caller-key", actUpdate.Key)
				assert.Equal(t, []string{"", "k1", "k2"}, actUpdate.Keys)
				assert.Equal(t, "caller-val", actUpdate.Value)
				assert.Equal(t, "caller-repl", actUpdate.ReplId)

				assert.Equal(t, "caller-ticket-key", ticketUpdate.Key)
				assert.Equal(t, "ticket-payload", ticketUpdate.Value)
				assert.Equal(t, "caller-ticket-repl", ticketUpdate.ReplId)

				actKeys[1] = "mutated-in-place"
				drained := rep.GetUpdates()
				require.Len(t, drained, 2)
				assert.Equal(t, []string{"k1", "k2"}, drained[0].Keys)
			})

			// Subtest 4: Field normalization
			t.Run("4_field_normalization", func(t *testing.T) {
				rep, cleanup := backend.factory(50, 100)
				defer cleanup()

				inputs := []*store.StateUpdate{
					{Cmd: store.Ticket, Key: "extra-key", Keys: []string{"extra-k"}, Value: "pb-bytes"},
					{Cmd: store.Activate, Key: "single-act", Value: "extra-val"},
					{Cmd: store.Activate, Key: "ignored", Keys: []string{"", "only-one-valid", ""}, Value: "extra-val"},
					{Cmd: store.Assign, Key: "assign-t", Keys: []string{"extra-k"}, Value: "127.0.0.1:7000"},
				}

				res := rep.SendUpdates(inputs)
				require.Len(t, res, len(inputs))
				for _, r := range res {
					require.NoError(t, r.Err)
				}

				updates := rep.GetUpdates()
				require.Len(t, updates, len(inputs))

				assert.Equal(t, res[0].Result, updates[0].Key)
				assert.Nil(t, updates[0].Keys)
				assert.Equal(t, "pb-bytes", updates[0].Value)

				assert.Equal(t, "single-act", updates[1].Key)
				assert.Nil(t, updates[1].Keys)
				assert.Empty(t, updates[1].Value)

				assert.Equal(t, "only-one-valid", updates[2].Key)
				assert.Nil(t, updates[2].Keys)
				assert.Empty(t, updates[2].Value)

				assert.Equal(t, "assign-t", updates[3].Key)
				assert.Nil(t, updates[3].Keys)
				assert.Equal(t, "127.0.0.1:7000", updates[3].Value)
			})

			// Subtest 5: Packed key empty-string filtering (preserving duplicate non-empty keys)
			t.Run("5_packed_key_empty_filtering_and_duplicate_preservation", func(t *testing.T) {
				rep, cleanup := backend.factory(50, 100)
				defer cleanup()

				res := rep.SendUpdates([]*store.StateUpdate{
					{Cmd: store.Deactivate, Keys: []string{"", "dup-1", "", "dup-2", "dup-1", ""}},
				})
				require.Len(t, res, 1)
				require.NoError(t, res[0].Err)

				updates := rep.GetUpdates()
				require.Len(t, updates, 1)
				assert.Equal(t, "dup-1", updates[0].Key)
				assert.Equal(t, []string{"dup-1", "dup-2", "dup-1"}, updates[0].Keys)
			})

			// Subtest 6: Odd and even packed key count parity
			t.Run("6_odd_and_even_packed_key_counts", func(t *testing.T) {
				rep, cleanup := backend.factory(50, 100)
				defer cleanup()

				res := rep.SendUpdates([]*store.StateUpdate{
					{Cmd: store.Activate, Keys: []string{"k1", "k2"}},                   // even (2)
					{Cmd: store.Activate, Keys: []string{"k1", "k2", "k3"}},             // odd (3)
					{Cmd: store.Deactivate, Keys: []string{"d1", "d2", "d3", "d4"}},     // even (4)
					{Cmd: store.Deactivate, Keys: []string{"d1", "d2", "d3", "d4", "d5"}}, // odd (5)
				})
				require.Len(t, res, 4)
				for _, r := range res {
					require.NoError(t, r.Err)
				}

				updates := rep.GetUpdates()
				require.Len(t, updates, 4)
				assert.Equal(t, []string{"k1", "k2"}, updates[0].Keys)
				assert.Equal(t, []string{"k1", "k2", "k3"}, updates[1].Keys)
				assert.Equal(t, []string{"d1", "d2", "d3", "d4"}, updates[2].Keys)
				assert.Equal(t, []string{"d1", "d2", "d3", "d4", "d5"}, updates[3].Keys)
			})

			// Subtest 7: Strictly monotonic replication IDs
			t.Run("7_strictly_monotonic_replication_ids", func(t *testing.T) {
				rep, cleanup := backend.factory(50, 100)
				defer cleanup()

				const count = 25
				batch := make([]*store.StateUpdate, count)
				for i := 0; i < count; i++ {
					batch[i] = &store.StateUpdate{
						Cmd: store.Activate,
						Key: fmt.Sprintf("mono-key-%d", i),
					}
				}
				res := rep.SendUpdates(batch)
				require.Len(t, res, count)

				updates := rep.GetUpdates()
				require.Len(t, updates, count)
				for i, u := range updates {
					assert.Regexp(t, rep.GetReplIdValidator(), u.ReplId)
					assert.Equal(t, res[i].Result, u.ReplId)
					if i > 0 {
						assert.Equal(t, -1, store.CompareReplIds(updates[i-1].ReplId, u.ReplId))
					}
				}
			})

			// Subtest 8: OM_CACHE_IN_MAX_UPDATES_PER_POLL batch size limits
			t.Run("8_max_updates_per_poll_batch_size_limits", func(t *testing.T) {
				const maxPerPoll = 3
				const total = 7
				rep, cleanup := backend.factory(50, maxPerPoll)
				defer cleanup()

				batch := make([]*store.StateUpdate, total)
				for i := 0; i < total; i++ {
					batch[i] = &store.StateUpdate{
						Cmd: store.Activate,
						Key: fmt.Sprintf("poll-limit-key-%d", i),
					}
				}
				res := rep.SendUpdates(batch)
				require.Len(t, res, total)

				poll1 := rep.GetUpdates()
				require.Len(t, poll1, 3)
				assert.Equal(t, "poll-limit-key-0", poll1[0].Key)
				assert.Equal(t, "poll-limit-key-1", poll1[1].Key)
				assert.Equal(t, "poll-limit-key-2", poll1[2].Key)

				poll2 := rep.GetUpdates()
				require.Len(t, poll2, 3)
				assert.Equal(t, "poll-limit-key-3", poll2[0].Key)
				assert.Equal(t, "poll-limit-key-4", poll2[1].Key)
				assert.Equal(t, "poll-limit-key-5", poll2[2].Key)

				poll3 := rep.GetUpdates()
				require.Len(t, poll3, 1)
				assert.Equal(t, "poll-limit-key-6", poll3[0].Key)
			})
		})
	}
}

// Scenario 9: Dial Timeout Resolution, Idempotent Close, and Failed New Goroutine Leak Check
func TestDialTimeoutsCloseAndNewLifecycle(t *testing.T) {
	// Subtest 1: Default dial timeouts (5s connect, 10s read, 5s write)
	t.Run("1_default_dial_timeouts", func(t *testing.T) {
		cfg := config.Read()
		connectTimeout, readTimeout, writeTimeout := resolveDialTimeouts(cfg)
		assert.Equal(t, 5*time.Second, connectTimeout)
		assert.Equal(t, 10*time.Second, readTimeout)
		assert.Equal(t, 5*time.Second, writeTimeout)
	})

	// Subtest 2: Fallback to OM_REDIS_POOL_IDLE_TIMEOUT when dial timeouts are non-positive
	t.Run("2_fallback_to_pool_idle_timeout", func(t *testing.T) {
		cfg := config.Read()
		cfg.Set("OM_REDIS_POOL_IDLE_TIMEOUT", 45*time.Second)
		cfg.Set("OM_REDIS_DIAL_CONNECT_TIMEOUT", 0*time.Second)
		cfg.Set("OM_REDIS_DIAL_READ_TIMEOUT", -1*time.Second)
		cfg.Set("OM_REDIS_DIAL_WRITE_TIMEOUT", 0*time.Second)

		connectTimeout, readTimeout, writeTimeout := resolveDialTimeouts(cfg)
		assert.Equal(t, 45*time.Second, connectTimeout)
		assert.Equal(t, 45*time.Second, readTimeout)
		assert.Equal(t, 45*time.Second, writeTimeout)

		// When OM_REDIS_POOL_IDLE_TIMEOUT is also non-positive, falls back to 60s.
		cfg.Set("OM_REDIS_POOL_IDLE_TIMEOUT", 0*time.Second)
		connectTimeout60, readTimeout60, writeTimeout60 := resolveDialTimeouts(cfg)
		assert.Equal(t, 60*time.Second, connectTimeout60)
		assert.Equal(t, 60*time.Second, readTimeout60)
		assert.Equal(t, 60*time.Second, writeTimeout60)
	})

	// Subtest 3: Clamping read timeout to OM_CACHE_IN_WAIT_TIMEOUT_MS + 1s
	t.Run("3_clamp_read_timeout_to_wait_timeout_plus_1s", func(t *testing.T) {
		cfg := config.Read()
		cfg.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 3000) // 3s -> min read timeout is 4s
		cfg.Set("OM_REDIS_DIAL_READ_TIMEOUT", 2*time.Second)

		_, readTimeout, _ := resolveDialTimeouts(cfg)
		assert.Equal(t, 4*time.Second, readTimeout)
	})

	// Subtest 4: Idempotent Close on an initialized replicator and nil receiver safety
	t.Run("4_idempotent_close_and_nil_receiver_safety", func(t *testing.T) {
		var nilReplicator *redisReplicator
		require.NoError(t, nilReplicator.Close())

		stream := newStatefulRedisStream()
		rr := newTestRedisReplicatorWithStream(stream, 50, 100)
		require.NoError(t, rr.Close())
		assert.ErrorIs(t, rr.ctx.Err(), context.Canceled)
		require.NoError(t, rr.Close(), "second Close call must be idempotent")
	})

	// Subtest 5: Zero leaked goroutines on failed New
	t.Run("5_zero_leaked_goroutines_on_failed_new", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		_, portStr, err := net.SplitHostPort(ln.Addr().String())
		require.NoError(t, err)
		require.NoError(t, ln.Close())

		cfg := config.Read()
		cfg.Set("REDISHOST", "127.0.0.1")
		cfg.Set("REDISPORT", portStr)
		cfg.Set("OM_REDIS_DIAL_CONNECT_TIMEOUT", 10*time.Millisecond)
		cfg.Set("OM_REDIS_DIAL_MAX_BACKOFF_TIMEOUT", 20*time.Millisecond)

		// Execute one warm-up New call first so Go's runtime singleton background
		// signal-handling goroutine is already initialized.
		warmupRR, err := New(cfg)
		require.Error(t, err)
		require.Nil(t, warmupRR)

		runtime.GC()
		time.Sleep(15 * time.Millisecond)
		baseline := runtime.NumGoroutine()

		for i := 0; i < 5; i++ {
			failedRR, dialErr := New(cfg)
			require.Error(t, dialErr)
			require.Nil(t, failedRR)
		}

		runtime.GC()
		time.Sleep(15 * time.Millisecond)
		after := runtime.NumGoroutine()
		assert.LessOrEqual(t, after, baseline, "failed New calls must not leak signal or pool goroutines")
	})
}
