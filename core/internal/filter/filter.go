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
// Package filter defines which tickets pass which filters.
package filter

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "open-match.dev/open-match2/pkg/pb"
)

var (
	InvalidCreationTimeFilterStartErrorStatus = status.Error(codes.InvalidArgument, ".invalid creation time filter start value")
	InvalidCreationTimeFilterEndErrorStatus   = status.Error(codes.InvalidArgument, ".invalid creation time filter end value")
)

// ValidatePoolFilters checks the fields in the filters to verify they are
// set to valid values.
func ValidatePoolFilters(pool *pb.Pool) (valid bool, err error) {
	valid = true
	// The Start and End times in the creation time filter are user input and should be validated.
	if pool.GetCreationTimeRangeFilter() != nil {
		if pool.GetCreationTimeRangeFilter().GetStart() != nil &&
			!pool.GetCreationTimeRangeFilter().GetStart().IsValid() {
			return false, fmt.Errorf("%w", InvalidCreationTimeFilterStartErrorStatus)
		}
		if pool.GetCreationTimeRangeFilter().GetEnd() != nil &&
			!pool.GetCreationTimeRangeFilter().GetEnd().IsValid() {
			return false, fmt.Errorf("%w", InvalidCreationTimeFilterEndErrorStatus)
		}
	}

	return valid, nil
}

// In returns true if the Ticket meets all the criteria for this Pool's Filter.
//
//nolint:gocognit,cyclop
func In(pool *pb.Pool, ticket *pb.Ticket) bool {
	filterableFields := ticket.GetAttributes()

	// CreateTime is validated by Open Match on ticket ingestion and hence
	// expected to be valid.
	crTime := filterableFields.GetCreationTime().AsTime()
	crTimeFilter := pool.GetCreationTimeRangeFilter()

	if crTimeFilter != nil { //nolint:nestif

		// Converting from timestamppb to time.Time for comparison.
		// filtering is in the performance critical path, so
		// doing those conversions only once per field is probably (?) faster.
		crTimeFilterEnd := crTimeFilter.GetEnd().AsTime()
		crTimeFilterStart := crTimeFilter.GetStart().AsTime()

		// Created before the filter Start time
		if crTime.Before(crTimeFilterStart) {
			return false
		}
		// Created after the filter End time
		if crTime.After(crTimeFilterEnd) {
			return false
		}
		// Created on the filter Start timestamp
		if crTimeFilter.GetBounds() == pb.Pool_EXCLUDE_START || crTimeFilter.GetBounds() == pb.Pool_EXCLUDE_BOTH {
			if crTime == crTimeFilterStart {
				return false
			}
		}
		// Created on the filter End timestamp
		if crTimeFilter.GetBounds() == pb.Pool_EXCLUDE_END || crTimeFilter.GetBounds() == pb.Pool_EXCLUDE_BOTH {
			if crTime == crTimeFilterEnd {
				return false
			}
		}
	}

	for _, filter := range pool.GetDoubleRangeFilters() {
		v, ok := filterableFields.GetDoubleArgs()[filter.GetDoubleArg()]
		if !ok {
			return false
		}

		switch filter.GetBounds() {
		case pb.Pool_EXCLUDE_NONE:
			if !(v >= filter.GetMinimum() && v <= filter.GetMaximum()) {
				return false
			}
		case pb.Pool_EXCLUDE_MIN:
			if !(v > filter.GetMinimum() && v <= filter.GetMaximum()) {
				return false
			}
		case pb.Pool_EXCLUDE_MAX:
			if !(v >= filter.GetMinimum() && v < filter.GetMaximum()) {
				return false
			}
		case pb.Pool_EXCLUDE_BOTH:
			if !(v > filter.GetMinimum() && v < filter.GetMaximum()) {
				return false
			}
		}

	}

	for _, f := range pool.GetStringEqualsFilters() {
		v, ok := filterableFields.GetStringArgs()[f.GetStringArg()]
		if !ok {
			return false
		}
		if f.GetValue() != v {
			return false
		}
	}

	ticketTags := filterableFields.GetTags()
tagFilterTest:
	for _, f := range pool.GetTagPresentFilters() {
		for _, v := range ticketTags {
			if v == f.GetTag() {
				continue tagFilterTest
			}
		}
		return false
	}

	return true
}
