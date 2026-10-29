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

// Package testcases contains lists of ticket filtering test cases.
package testcases

import (
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
)

// TestCase defines a single filtering test case to run.
type TestCase struct {
	Name   string
	Ticket *pb.Ticket
	Pool   *pb.Pool
}

var (
	refStartTime = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	refMidTime   = time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC)
	refEndTime   = time.Date(2024, 1, 1, 13, 0, 0, 0, time.UTC)
)

// IncludedTestCases returns a list of test cases where using the given filter,
// the ticket is included in the result.
func IncludedTestCases() []TestCase {
	return []TestCase{
		{
			Name:   "no filters",
			Ticket: &pb.Ticket{},
			Pool:   &pb.Pool{},
		},
		simpleDoubleRange("simpleInRange", 5, 0, 10, pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("simpleInRange", 5, 0, 10, pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("simpleInRange", 5, 0, 10, pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("simpleInRange", 5, 0, 10, pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("exactMatch", 5, 5, 5, pb.Pool_EXCLUDE_NONE),

		simpleDoubleRange("infinityMax", math.Inf(1), 0, math.Inf(1), pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("infinityMax", math.Inf(1), 0, math.Inf(1), pb.Pool_EXCLUDE_MIN),

		simpleDoubleRange("infinityMin", math.Inf(-1), math.Inf(-1), 0, pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("infinityMin", math.Inf(-1), math.Inf(-1), 0, pb.Pool_EXCLUDE_MAX),

		simpleDoubleRange("excludeNone", 0, 0, 1, pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("excludeNone", 1, 0, 1, pb.Pool_EXCLUDE_NONE),

		simpleDoubleRange("excludeMin", 1, 0, 1, pb.Pool_EXCLUDE_MIN),

		simpleDoubleRange("excludeMax", 0, 0, 1, pb.Pool_EXCLUDE_MAX),

		simpleDoubleRange("excludeBoth", 2, 0, 3, pb.Pool_EXCLUDE_BOTH),
		simpleDoubleRange("excludeBoth", 1, 0, 3, pb.Pool_EXCLUDE_BOTH),

		{
			"String equals simple positive",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					StringArgs: map[string]string{
						"field": "value",
					},
				},
			},
			&pb.Pool{
				StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
					{
						StringArg: "field",
						Value:     "value",
					},
				},
			},
		},

		{
			"TagPresent simple positive",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					Tags: []string{
						"mytag",
					},
				},
			},
			&pb.Pool{
				TagPresentFilters: []*pb.Pool_TagPresentFilter{
					{
						Tag: "mytag",
					},
				},
			},
		},

		{
			"TagPresent multiple all present",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					Tags: []string{
						"A", "B", "C",
					},
				},
			},
			&pb.Pool{
				TagPresentFilters: []*pb.Pool_TagPresentFilter{
					{
						Tag: "A",
					},
					{
						Tag: "C",
					},
					{
						Tag: "B",
					},
				},
			},
		},

		// Bounded CreationTimeRangeFilter cases (8 cases)
		boundedCreationTimeRange("inRange", refMidTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_NONE),
		boundedCreationTimeRange("inRange", refMidTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_START),
		boundedCreationTimeRange("inRange", refMidTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_END),
		boundedCreationTimeRange("inRange", refMidTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_BOTH),
		boundedCreationTimeRange("atStartBoundary", refStartTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_NONE),
		boundedCreationTimeRange("atStartBoundary", refStartTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_END),
		boundedCreationTimeRange("atEndBoundary", refEndTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_NONE),
		boundedCreationTimeRange("atEndBoundary", refEndTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_START),

		// Start-only CreationTimeRangeFilter cases (End is nil, 6 cases)
		openCreationTimeRange("startOnlyAfterStart", refMidTime, &refStartTime, nil, pb.Pool_EXCLUDE_NONE),
		openCreationTimeRange("startOnlyAfterStart", refMidTime, &refStartTime, nil, pb.Pool_EXCLUDE_START),
		openCreationTimeRange("startOnlyAfterStart", refMidTime, &refStartTime, nil, pb.Pool_EXCLUDE_END),
		openCreationTimeRange("startOnlyAfterStart", refMidTime, &refStartTime, nil, pb.Pool_EXCLUDE_BOTH),
		openCreationTimeRange("startOnlyAtStart", refStartTime, &refStartTime, nil, pb.Pool_EXCLUDE_NONE),
		openCreationTimeRange("startOnlyAtStart", refStartTime, &refStartTime, nil, pb.Pool_EXCLUDE_END),

		// End-only CreationTimeRangeFilter cases (Start is nil, 9 cases)
		openCreationTimeRange("endOnlyBeforeEnd", refMidTime, nil, &refEndTime, pb.Pool_EXCLUDE_NONE),
		openCreationTimeRange("endOnlyBeforeEnd", refMidTime, nil, &refEndTime, pb.Pool_EXCLUDE_START),
		openCreationTimeRange("endOnlyBeforeEnd", refMidTime, nil, &refEndTime, pb.Pool_EXCLUDE_END),
		openCreationTimeRange("endOnlyBeforeEnd", refMidTime, nil, &refEndTime, pb.Pool_EXCLUDE_BOTH),
		openCreationTimeRange("endOnlyAtEnd", refEndTime, nil, &refEndTime, pb.Pool_EXCLUDE_NONE),
		openCreationTimeRange("endOnlyAtEnd", refEndTime, nil, &refEndTime, pb.Pool_EXCLUDE_START),
		openCreationTimeRange("endOnlyAtUnixEpoch", time.Unix(0, 0).UTC(), nil, &refEndTime, pb.Pool_EXCLUDE_START),
		openCreationTimeRange("endOnlyAtUnixEpoch", time.Unix(0, 0).UTC(), nil, &refEndTime, pb.Pool_EXCLUDE_BOTH),
		openCreationTimeRange("endOnlyBeforeUnixEpoch", time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC), nil, &refEndTime, pb.Pool_EXCLUDE_NONE),

		// Open-ended CreationTimeRangeFilter cases (both Start and End nil, 2 cases)
		openCreationTimeRange("bothBoundsNil", refMidTime, nil, nil, pb.Pool_EXCLUDE_NONE),
		openCreationTimeRange("bothBoundsNil", refMidTime, nil, nil, pb.Pool_EXCLUDE_BOTH),

		multipleFilters(true, true, true, true),
	}
}

// ExcludedTestCases returns a list of test cases where using the given filter,
// the ticket is NOT included in the result.
func ExcludedTestCases() []TestCase {
	return []TestCase{
		{
			"DoubleRange no FilterableData Attributes",
			nil,
			&pb.Pool{
				DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
					{
						DoubleArg: "field",
						Minimum:   math.Inf(-1),
						Maximum:   math.Inf(1),
					},
				},
			},
		},
		{
			"StringEquals no FilterableData Attributes",
			nil,
			&pb.Pool{
				StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
					{
						StringArg: "field",
						Value:     "value",
					},
				},
			},
		},
		{
			"TagPresent no FilterableData Attributes",
			nil,
			&pb.Pool{
				TagPresentFilters: []*pb.Pool_TagPresentFilter{
					{
						Tag: "value",
					},
				},
			},
		},
		{
			"CreationTimeRange no FilterableData Attributes",
			nil,
			&pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					Start: timestamppb.New(refStartTime),
					End:   timestamppb.New(refEndTime),
				},
			},
		},
		{
			"CreationTimeRange nil ticket start only",
			nil,
			&pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					Start: timestamppb.New(refStartTime),
				},
			},
		},
		{
			"CreationTimeRange nil ticket end only",
			nil,
			&pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					End: timestamppb.New(refEndTime),
				},
			},
		},
		{
			"CreationTimeRange nil ticket both bounds nil",
			nil,
			&pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{},
			},
		},
		{
			"CreationTimeRange nil CreationTime bounded",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{},
			},
			&pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					Start: timestamppb.New(refStartTime),
					End:   timestamppb.New(refEndTime),
				},
			},
		},
		{
			"CreationTimeRange nil CreationTime start only",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{},
			},
			&pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					Start: timestamppb.New(refStartTime),
				},
			},
		},
		{
			"CreationTimeRange nil CreationTime end only",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{},
			},
			&pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					End: timestamppb.New(refEndTime),
				},
			},
		},
		{
			"CreationTimeRange nil CreationTime both bounds nil",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{},
			},
			&pb.Pool{
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{},
			},
		},
		{
			"Double range missing field",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					DoubleArgs: map[string]float64{
						"otherfield": 0,
					},
				},
			},
			&pb.Pool{
				DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
					{
						DoubleArg: "field",
						Minimum:   math.Inf(-1),
						Maximum:   math.Inf(1),
					},
				},
			},
		},

		simpleDoubleRange("exactMatch", 5, 5, 5, pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("exactMatch", 5, 5, 5, pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("exactMatch", 5, 5, 5, pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("valueTooLow", -1, 0, 10, pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("valueTooLow", -1, 0, 10, pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("valueTooLow", -1, 0, 10, pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("valueTooLow", -1, 0, 10, pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("valueTooHigh", 11, 0, 10, pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("valueTooHigh", 11, 0, 10, pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("valueTooHigh", 11, 0, 10, pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("valueTooHigh", 11, 0, 10, pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("minIsNan", 5, math.NaN(), 10, pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("minIsNan", 5, math.NaN(), 10, pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("minIsNan", 5, math.NaN(), 10, pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("minIsNan", 5, math.NaN(), 10, pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("maxIsNan", 5, 0, math.NaN(), pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("maxIsNan", 5, 0, math.NaN(), pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("maxIsNan", 5, 0, math.NaN(), pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("maxIsNan", 5, 0, math.NaN(), pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("minMaxAreNan", 5, math.NaN(), math.NaN(), pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("minMaxAreNan", 5, math.NaN(), math.NaN(), pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("minMaxAreNan", 5, math.NaN(), math.NaN(), pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("minMaxAreNan", 5, math.NaN(), math.NaN(), pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("valueIsNan", math.NaN(), 0, 10, pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("valueIsNan", math.NaN(), 0, 10, pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("valueIsNan", math.NaN(), 0, 10, pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("valueIsNan", math.NaN(), 0, 10, pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("valueIsNanInfRange", math.NaN(), math.Inf(-1), math.Inf(1), pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("valueIsNanInfRange", math.NaN(), math.Inf(-1), math.Inf(1), pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("valueIsNanInfRange", math.NaN(), math.Inf(-1), math.Inf(1), pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("valueIsNanInfRange", math.NaN(), math.Inf(-1), math.Inf(1), pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("infinityMax", math.Inf(1), 0, math.Inf(1), pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("infinityMax", math.Inf(1), 0, math.Inf(1), pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("infinityMin", math.Inf(-1), math.Inf(-1), 0, pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("infinityMin", math.Inf(-1), math.Inf(-1), 0, pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("allAreNan", math.NaN(), math.NaN(), math.NaN(), pb.Pool_EXCLUDE_NONE),
		simpleDoubleRange("allAreNan", math.NaN(), math.NaN(), math.NaN(), pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("allAreNan", math.NaN(), math.NaN(), math.NaN(), pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("allAreNan", math.NaN(), math.NaN(), math.NaN(), pb.Pool_EXCLUDE_BOTH),

		simpleDoubleRange("valueIsMax", 1, 0, 1, pb.Pool_EXCLUDE_MAX),
		simpleDoubleRange("valueIsMin", 0, 0, 1, pb.Pool_EXCLUDE_MIN),
		simpleDoubleRange("excludeBoth", 0, 0, 1, pb.Pool_EXCLUDE_BOTH),
		simpleDoubleRange("excludeBoth", 1, 0, 1, pb.Pool_EXCLUDE_BOTH),

		{
			"String equals simple negative", // and case sensitivity
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					StringArgs: map[string]string{
						"field": "value",
					},
				},
			},
			&pb.Pool{
				StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
					{
						StringArg: "field",
						Value:     "VALUE",
					},
				},
			},
		},

		{
			"String equals missing field",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					StringArgs: map[string]string{
						"otherfield": "othervalue",
					},
				},
			},
			&pb.Pool{
				StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
					{
						StringArg: "field",
						Value:     "value",
					},
				},
			},
		},

		{
			"TagPresent simple negative", // and case sensitivity
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					Tags: []string{
						"MYTAG",
					},
				},
			},
			&pb.Pool{
				TagPresentFilters: []*pb.Pool_TagPresentFilter{
					{
						Tag: "mytag",
					},
				},
			},
		},

		{
			"TagPresent multiple with one missing",
			&pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					Tags: []string{
						"A", "B", "C",
					},
				},
			},
			&pb.Pool{
				TagPresentFilters: []*pb.Pool_TagPresentFilter{
					{
						Tag: "A",
					},
					{
						Tag: "D",
					},
					{
						Tag: "C",
					},
				},
			},
		},

		// Bounded CreationTimeRangeFilter excluded cases (6 cases)
		boundedCreationTimeRange("beforeStart", refStartTime.Add(-time.Minute), refStartTime, refEndTime, pb.Pool_EXCLUDE_NONE),
		boundedCreationTimeRange("afterEnd", refEndTime.Add(time.Minute), refStartTime, refEndTime, pb.Pool_EXCLUDE_NONE),
		boundedCreationTimeRange("atStartExcluded", refStartTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_START),
		boundedCreationTimeRange("atStartExcluded", refStartTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_BOTH),
		boundedCreationTimeRange("atEndExcluded", refEndTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_END),
		boundedCreationTimeRange("atEndExcluded", refEndTime, refStartTime, refEndTime, pb.Pool_EXCLUDE_BOTH),

		// Start-only CreationTimeRangeFilter excluded cases (End is nil, 6 cases)
		openCreationTimeRange("startOnlyBeforeStart", refStartTime.Add(-time.Minute), &refStartTime, nil, pb.Pool_EXCLUDE_NONE),
		openCreationTimeRange("startOnlyBeforeStart", refStartTime.Add(-time.Minute), &refStartTime, nil, pb.Pool_EXCLUDE_START),
		openCreationTimeRange("startOnlyBeforeStart", refStartTime.Add(-time.Minute), &refStartTime, nil, pb.Pool_EXCLUDE_END),
		openCreationTimeRange("startOnlyBeforeStart", refStartTime.Add(-time.Minute), &refStartTime, nil, pb.Pool_EXCLUDE_BOTH),
		openCreationTimeRange("startOnlyAtStartExcluded", refStartTime, &refStartTime, nil, pb.Pool_EXCLUDE_START),
		openCreationTimeRange("startOnlyAtStartExcluded", refStartTime, &refStartTime, nil, pb.Pool_EXCLUDE_BOTH),

		// End-only CreationTimeRangeFilter excluded cases (Start is nil, 6 cases)
		openCreationTimeRange("endOnlyAfterEnd", refEndTime.Add(time.Minute), nil, &refEndTime, pb.Pool_EXCLUDE_NONE),
		openCreationTimeRange("endOnlyAfterEnd", refEndTime.Add(time.Minute), nil, &refEndTime, pb.Pool_EXCLUDE_START),
		openCreationTimeRange("endOnlyAfterEnd", refEndTime.Add(time.Minute), nil, &refEndTime, pb.Pool_EXCLUDE_END),
		openCreationTimeRange("endOnlyAfterEnd", refEndTime.Add(time.Minute), nil, &refEndTime, pb.Pool_EXCLUDE_BOTH),
		openCreationTimeRange("endOnlyAtEndExcluded", refEndTime, nil, &refEndTime, pb.Pool_EXCLUDE_END),
		openCreationTimeRange("endOnlyAtEndExcluded", refEndTime, nil, &refEndTime, pb.Pool_EXCLUDE_BOTH),

		multipleFilters(false, true, true, true),
		multipleFilters(true, false, true, true),
		multipleFilters(true, true, false, true),
		multipleFilters(true, true, true, false),
	}
}

func simpleDoubleRange(name string, value, min, max float64, exclude pb.Pool_FilterBounds) TestCase {
	return TestCase{
		fmt.Sprintf("Double range %s (val=%v, min=%v, max=%v, bounds=%s)", name, value, min, max, exclude),
		&pb.Ticket{
			Attributes: &pb.Ticket_FilterableData{
				DoubleArgs: map[string]float64{
					"field": value,
				},
			},
		},
		&pb.Pool{
			DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
				{
					DoubleArg: "field",
					Minimum:   min,
					Maximum:   max,
					Bounds:    exclude,
				},
			},
		},
	}
}

func boundedCreationTimeRange(name string, creationTime, start, end time.Time, bounds pb.Pool_FilterBounds) TestCase {
	return openCreationTimeRange(name, creationTime, &start, &end, bounds)
}

func openCreationTimeRange(name string, creationTime time.Time, start, end *time.Time, bounds pb.Pool_FilterBounds) TestCase {
	var startProto, endProto *timestamppb.Timestamp
	if start != nil {
		startProto = timestamppb.New(*start)
	}
	if end != nil {
		endProto = timestamppb.New(*end)
	}
	return TestCase{
		Name: fmt.Sprintf("CreationTimeRange %s (%s)", name, bounds),
		Ticket: &pb.Ticket{
			Attributes: &pb.Ticket_FilterableData{
				CreationTime: timestamppb.New(creationTime),
			},
		},
		Pool: &pb.Pool{
			CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
				Start:  startProto,
				End:    endProto,
				Bounds: bounds,
			},
		},
	}
}

// All filters must be matched for a ticket to be included.
// This test checks if a ticket which fails one type of filter is correctly excluded.
func multipleFilters(doubleRange, stringEquals, tagPresent, creationTimeRange bool) TestCase {
	a := float64(0) // value that produces a filter pass
	if !doubleRange {
		a = 10 // value that produces a filter rejection
	}

	b := "hi" // value that produces a filter pass
	if !stringEquals {
		b = "bye" // value that produces a filter rejection
	}

	c := "yo" // value that produces a filter pass
	if !tagPresent {
		c = "cya" // value that produces a filter rejection
	}

	// value that produces a filter pass
	d := time.Date(2009, 11, 17, 20, 34, 58, 651387237, time.UTC)
	if !creationTimeRange {
		// value that produces a filter rejection
		d = time.Date(1909, 11, 17, 20, 34, 58, 651387237, time.UTC)
	}

	return TestCase{
		fmt.Sprintf("Multiple filters: %v, %v, %v, %v", doubleRange, stringEquals, tagPresent, creationTimeRange),
		&pb.Ticket{
			Attributes: &pb.Ticket_FilterableData{
				DoubleArgs: map[string]float64{
					"a": a,
				},
				StringArgs: map[string]string{
					"b": b,
				},
				Tags:         []string{c},
				CreationTime: timestamppb.New(d),
			},
		},
		&pb.Pool{
			DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
				{
					DoubleArg: "a",
					Minimum:   -1,
					Maximum:   1,
				},
			},
			StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
				{
					StringArg: "b",
					Value:     "hi",
				},
			},
			TagPresentFilters: []*pb.Pool_TagPresentFilter{
				{
					Tag: "yo",
				},
			},
			CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
				Start: timestamppb.New(time.Date(2000, 11, 17, 20, 34, 58, 651387237, time.UTC)),
				End:   timestamppb.New(time.Date(2024, 11, 17, 20, 34, 58, 651387237, time.UTC)),
			},
		},
	}
}
