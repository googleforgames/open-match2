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
package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReplIdTimestampMs(t *testing.T) {
	validCases := []struct {
		name   string
		replId string
		want   int64
	}{
		{
			name:   "standard_13_digit_ms_id",
			replId: "1700000000000-0",
			want:   1700000000000,
		},
		{
			name:   "zero_timestamp_and_sequence",
			replId: "0-0",
			want:   0,
		},
		{
			name:   "multi_digit_sequence",
			replId: "1700000000123-45678",
			want:   1700000000123,
		},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReplIdTimestampMs(tc.replId)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	invalidInputs := []string{
		"",
		"1700000000000",
		"1700000000000-",
		"-0",
		"-",
		"1700000000000-0-1",
		"abc-0",
		"1700000000000-xyz",
		"-1700000000000-0",
		"1700000000000--1",
		"+1700000000000-0",
		"1700000000000-+0",
		"+1700000000000-+1",
	}

	for _, input := range invalidInputs {
		t.Run("invalid_"+input, func(t *testing.T) {
			got, err := ParseReplIdTimestampMs(input)
			require.Error(t, err)
			assert.ErrorContains(t, err, "invalid replication ID")
			assert.Equal(t, int64(0), got)
		})
	}
}

func TestCompareReplIds(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want int
	}{
		{
			name: "both_empty",
			a:    "",
			b:    "",
			want: 0,
		},
		{
			name: "left_empty",
			a:    "",
			b:    "1700000000000-0",
			want: -1,
		},
		{
			name: "right_empty",
			a:    "1700000000000-0",
			b:    "",
			want: 1,
		},
		{
			name: "equal_ids",
			a:    "1700000000000-5",
			b:    "1700000000000-5",
			want: 0,
		},
		{
			name: "numeric_sequence_2_less_than_10",
			a:    "1700000000000-2",
			b:    "1700000000000-10",
			want: -1,
		},
		{
			name: "numeric_sequence_10_greater_than_2",
			a:    "1700000000000-10",
			b:    "1700000000000-2",
			want: 1,
		},
		{
			name: "different_timestamps_less",
			a:    "1700000000000-99",
			b:    "1700000000001-0",
			want: -1,
		},
		{
			name: "different_timestamps_greater",
			a:    "1700000000001-0",
			b:    "1700000000000-99",
			want: 1,
		},
		{
			name: "zero_padded_timestamps_equal",
			a:    "0000000000100-5",
			b:    "100-5",
			want: 0,
		},
		{
			name: "zero_padded_timestamps_less",
			a:    "0000000000099-5",
			b:    "100-0",
			want: -1,
		},
		{
			name: "fallback_lexicographical_less",
			a:    "nonstandard-a",
			b:    "nonstandard-b",
			want: -1,
		},
		{
			name: "fallback_lexicographical_greater",
			a:    "nonstandard-b",
			b:    "nonstandard-a",
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CompareReplIds(tc.a, tc.b))
		})
	}
}

func TestSentinelErrors(t *testing.T) {
	require.NotNil(t, NoTicketKeyErr)
	require.NotNil(t, NoTicketDataErr)
	require.NotNil(t, NoAssignmentErr)
	require.NotNil(t, InvalidInputErr)

	assert.Equal(t, "Missing ticket key", NoTicketKeyErr.Error())
	assert.Equal(t, "No ticket data", NoTicketDataErr.Error())
	assert.Equal(t, "Missing assignment", NoAssignmentErr.Error())
	assert.Equal(t, "Invalid input", InvalidInputErr.Error())

	sentinels := []error{NoTicketKeyErr, NoTicketDataErr, NoAssignmentErr, InvalidInputErr}
	for i := 0; i < len(sentinels); i++ {
		for j := i + 1; j < len(sentinels); j++ {
			assert.NotErrorIs(t, sentinels[i], sentinels[j])
		}
	}
}

func TestStateUpdatePrimaryKey(t *testing.T) {
	var nilUpdate *StateUpdate
	assert.Equal(t, "", nilUpdate.PrimaryKey())

	withKey := &StateUpdate{
		Key:  "primary-ticket",
		Keys: []string{"other-1", "other-2"},
	}
	assert.Equal(t, "primary-ticket", withKey.PrimaryKey())

	withKeys := &StateUpdate{
		Keys: []string{"packed-1", "packed-2"},
	}
	assert.Equal(t, "packed-1", withKeys.PrimaryKey())

	withLeadingEmptyKeys := &StateUpdate{
		Keys: []string{"", "", "packed-3", "packed-4"},
	}
	assert.Equal(t, "packed-3", withLeadingEmptyKeys.PrimaryKey())

	allEmpty := &StateUpdate{
		Keys: []string{"", ""},
	}
	assert.Equal(t, "", allEmpty.PrimaryKey())

	bothEmpty := &StateUpdate{}
	assert.Equal(t, "", bothEmpty.PrimaryKey())
}

func TestStateUpdateString(t *testing.T) {
	cases := []struct {
		name   string
		update StateUpdate
		want   string
	}{
		{
			name: "ticket_binary_payload_summarized_by_byte_length",
			update: StateUpdate{
				Cmd:    Ticket,
				Key:    "1700000000000-0",
				Value:  "\x08\x01\x12\x04test",
				ReplId: "1700000000000-0",
			},
			want: "{Cmd:0 Key:1700000000000-0 Keys: Value:<8 bytes> ReplId:1700000000000-0}",
		},
		{
			name: "unpacked_activate",
			update: StateUpdate{
				Cmd:    Activate,
				Key:    "1700000000000-0",
				ReplId: "1700000000001-0",
			},
			want: "{Cmd:1 Key:1700000000000-0 Keys: Value: ReplId:1700000000001-0}",
		},
		{
			name: "packed_deactivate_3_keys",
			update: StateUpdate{
				Cmd:    Deactivate,
				Key:    "t1",
				Keys:   []string{"t1", "t2", "t3"},
				ReplId: "1700000000002-0",
			},
			want: "{Cmd:2 Key:t1 Keys:[t1 t2 t3] Value: ReplId:1700000000002-0}",
		},
		{
			name: "packed_deactivate_5_keys_truncated",
			update: StateUpdate{
				Cmd:    Deactivate,
				Key:    "t1",
				Keys:   []string{"t1", "t2", "t3", "t4", "t5"},
				ReplId: "1700000000003-0",
			},
			want: "{Cmd:2 Key:t1 Keys:[t1 t2 t3 ...] Value: ReplId:1700000000003-0}",
		},
		{
			name: "assign_command",
			update: StateUpdate{
				Cmd:    Assign,
				Key:    "1700000000000-0",
				Value:  "10.0.0.1:7777",
				ReplId: "1700000000004-0",
			},
			want: "{Cmd:3 Key:1700000000000-0 Keys: Value:10.0.0.1:7777 ReplId:1700000000004-0}",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.update.String())
		})
	}
}
