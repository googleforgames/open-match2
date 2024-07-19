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
	pb "open-match.dev/pkg/pb/v2"
)

// TestCase defines a single filtering test case to run.
type TestCase struct {
	Name   string
	Ticket *pb.Ticket
	Pool   *pb.Pool
}

// IncludedTestCases returns a list of test cases where using the given filter,
// the ticket is included in the result.
func IncludedTestCases() []TestCase {
	return []TestCase{
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

		multipleFilters(false, true, true, true),
		multipleFilters(true, false, true, true),
		multipleFilters(true, true, false, true),
		multipleFilters(true, true, true, false),
	}
}

func simpleDoubleRange(name string, value, min, max float64, exclude pb.Pool_FilterBounds) TestCase {
	return TestCase{
		"Double range " + name,
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
