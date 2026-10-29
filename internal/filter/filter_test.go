// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package filter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"github.com/googleforgames/open-match2/v2/internal/filter/testcases"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
)

// TestMeetsCriteria validates that tickets are correctly included/excluded
// from a pool based on that pool's filters (i.e. it validates the filter
// works as expected).
func TestMeetsCriteria(t *testing.T) {
	t.Run("Included", func(t *testing.T) {
		for _, tc := range testcases.IncludedTestCases() {
			t.Run(tc.Name, func(t *testing.T) {
				if !In(tc.Pool, tc.Ticket) {
					t.Error("ticket should be included in the pool")
				}
			})
		}
	})

	t.Run("Excluded", func(t *testing.T) {
		for _, tc := range testcases.ExcludedTestCases() {
			t.Run(tc.Name, func(t *testing.T) {
				if In(tc.Pool, tc.Ticket) {
					t.Error("ticket should be excluded from the pool")
				}
			})
		}
	})
}

// TestValidPoolFilter runs the pool filter validation function with known
// valid and invalid values to make sure the validation function behaves as expected.
func TestValidPoolFilter(t *testing.T) {
	now := time.Now().UTC()
	type testcase struct {
		name string
		pool *pb.Pool
		err  error
	}
	tcs := []testcase{
		{
			name: "valid pool without creation time filter",
			pool: &pb.Pool{},
			err:  nil,
		},
		{
			name: "valid creation time filter with start and end",
			pool: &pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					Start: timestamppb.New(now),
					End:   timestamppb.New(now.Add(time.Hour)),
				},
			},
			err: nil,
		},
		{
			name: "valid creation time filter with start only",
			pool: &pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					Start: timestamppb.New(now),
				},
			},
			err: nil,
		},
		{
			name: "valid creation time filter with end only",
			pool: &pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					End: timestamppb.New(now),
				},
			},
			err: nil,
		},
		{
			name: "valid creation time filter with both start and end nil",
			pool: &pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{},
			},
			err: nil,
		},
		{
			name: "invalid creation time filter start",
			pool: &pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					Start: &timestamppb.Timestamp{Nanos: -1},
				},
			},
			err: InvalidCreationTimeFilterStartErrorStatus,
		},
		{
			name: "invalid creation time filter end",
			pool: &pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					End: &timestamppb.Timestamp{Nanos: -1},
				},
			},
			err: InvalidCreationTimeFilterEndErrorStatus,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := ValidatePoolFilters(tc.pool)
			if tc.err != nil {
				require.False(t, ok)
				require.Error(t, err)
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.True(t, ok)
			require.NoError(t, err)
		})
	}
}
