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
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/redis"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"github.com/googleforgames/open-match2/internal/config"
	"github.com/googleforgames/open-match2/internal/statestore/cache"
	memoryReplicator "github.com/googleforgames/open-match2/internal/statestore/memory"
	redisReplicator "github.com/googleforgames/open-match2/internal/statestore/redis"
	pb "github.com/googleforgames/open-match2/pkg/pb"
)

var (
	testLogger = logrus.WithFields(logrus.Fields{
		"app":       "open_match_test_suite",
		"component": "core",
	})

	// One global instance for the local ticket cache. Everything reads and
	// writes to this one instance which contains concurrent-safe data
	// structures where necessary.
	tcs = map[string]*cache.ReplicatedTicketCache{}

	// Ticket ids are using the redis stream ID format.
	// https://redis.io/docs/latest/develop/data-types/streams/#entry-ids
	// The first number is a unix timestamp at millisecond precision (13 digits
	// as of 2024). The second number is the 'sequence number', basically a
	// counter of how many entries have happened in a given unix timestamp.
	invalidTicketIds = []string{
		"",                             // empty string
		"0",                            // no separation of timestamp from sequence number
		"-1",                           // no timestamp
		"0-",                           // no sequence number
		"123456789012-1",               // invalid timestamp (13 characters required to parse as a Unix timestamp in milliseconds)
		"12345678901234-1234567890123", // timestamp at greater than millisecond precision
	}
	validTicketIds = []string{
		"1234567890123-1",             // very low sequence number
		"1234567890123-1234567890123", // very high sequence number
	}
)

const redisUri = "localhost:6379"

// TestMain sets up both the in-memory mock ticket replicator and the actual
// redis ticket replicator (using local redis and falling back to
// testcontainers) so tests can run against both types and confirm they produce
// the expected results.
func TestMain(m *testing.M) {
	logrus.SetLevel(logrus.FatalLevel)
	logrus.SetLevel(logrus.DebugLevel)
	logrus.SetLevel(logrus.TraceLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	os.Setenv("OM_REDIS_DIAL_MAX_BACKOFF_TIMEOUT", "10s")

	redisConfig := func(uri string) {
		// Attempt to connect to locally running redis
		redisUrl, err := url.Parse(uri)
		if err != nil {
			testLogger.Fatalf("failed to parse connection string for redis: %s", err)
		}
		os.Setenv("REDISHOST", redisUrl.Hostname())
		os.Setenv("REDISPORT", redisUrl.Port())
	}
	redisConfig(redisUri)
	cfg = config.Read() // Read env vars into config

	// Configure metrics. The tests don't actually utilize the metrics at all,
	// but unfortunately OTEL metrics cause panics when you try to record
	// metrics without initializing them first.
	meter, otelShutdownFunc = initializeOtel()
	defer otelShutdownFunc(ctx) //nolint:errcheck
	registerMetrics(meter)
	cache.RegisterMetrics(meter)

	rr, err := redisReplicator.New(cfg)
	if err == nil {
		testLogger.Infof("Connection to local redis returned no error")
	} else {
		// do a redis testcontainer instead
		testLogger.Infof("Unable to connect to local redis; falling back to testcontainer")
		redisContainer, err := redis.RunContainer(ctx,
			// Latest Redis containers fail with
			//	"# Fatal: Can't initialize Background Jobs. Error message: Operation not permitted"
			// https://discuss.circleci.com/t/redis-fatal-cant-initialize-background-jobs/48376/8
			//testcontainers.WithImage("redis:latest"))
			//
			// This is the latest version that works in my limited testing.
			testcontainers.WithImage("redis:7.0.10"))
		if err != nil {
			log.Fatalf("failed to start redis container: %s", err)
		}
		uri, err := redisContainer.ConnectionString(ctx)
		if err != nil {
			log.Fatalf("failed to get connection string for redis container: %s", err)
		}
		testLogger.Infof("got connection string for redis container: %s", err)
		redisConfig(uri)
		cfg = config.Read() // Read latest REDISPORT/REDISHOST env vars into config
		rr, err = redisReplicator.New(cfg)
		if err != nil {
			log.Fatalf("failed to connect to redis testcontainer: %s", err)
		}
	}

	// Set up the redis-backed replicated ticket cache
	tcs["redis"] = &cache.ReplicatedTicketCache{
		Cfg:        cfg,
		UpRequests: make(chan *cache.UpdateRequest),
		Replicator: rr,
	}

	// Set up the memory-backed replicated ticket cache
	tcs["memory"] = &cache.ReplicatedTicketCache{
		Cfg:        cfg,
		UpRequests: make(chan *cache.UpdateRequest),
		Replicator: memoryReplicator.New(cfg),
	}

	for _, v := range tcs {
		go v.OutgoingReplicationQueue(ctx)
		go v.IncomingReplicationQueue(ctx)
	}

	m.Run()
}

// TestCreateTicket tests the createTicket() function, which is the internal
// implementation of the gRPC CreateTicket() function defined in
// proto/v2/api.proto .
//
// This does combinatorial creation of tickets with every valid attribute we
// want to test, which makes it fail complexity linter.
//
//nolint:gocognit,cyclop
func TestCreateTicket(t *testing.T) {
	// expiration times that should fail validation
	inValidEts := map[string]*pb.Ticket{
		// Now() will always fail, because by the time you send the timestamp to
		// the validation function, time will have advanced past it.
		"now": &pb.Ticket{ExpirationTime: timestamppb.Now()},
		// 1/1/1970
		"epoch": &pb.Ticket{ExpirationTime: &timestamppb.Timestamp{Seconds: 0, Nanos: 0}},
		// Timestamppb max allowed timestamp value
		"last_possible": &pb.Ticket{ExpirationTime: &timestamppb.Timestamp{Seconds: 253402300800, Nanos: 0}},
	}

	// Expected failure status for invalid expiration times
	invalidExpTimeStatus := status.New(codes.InvalidArgument,
		fmt.Sprintf("specified expiration time not between (now, now + %v)",
			cfg.GetInt("OM_CACHE_TICKET_TTL_MS")))

	// Make sure all expected invalid expiration times do fail validation correctly
	for testCaseName, thisTestCase := range inValidEts {
		t.Run(fmt.Sprintf("InvalidExpTime=%v", testCaseName), func() func(t *testing.T) {
			return func(t *testing.T) {
				t.Parallel()
				_, err := validateAndMarshalIncomingTicket(&pb.CreateTicketRequest{Ticket: thisTestCase})
				// Check expected error was returned
				s := status.Convert(err)
				assert.Equal(t, invalidExpTimeStatus.Code(), s.Code())
				assert.Contains(t, s.Message(), invalidExpTimeStatus.Message())
			}
		}())
	}

	// valid filterable data attribute test cases
	tags := []*pb.Ticket_FilterableData{
		&pb.Ticket_FilterableData{},
		&pb.Ticket_FilterableData{Tags: []string{"bonusxp"}},
	}
	sas := []*pb.Ticket_FilterableData{
		&pb.Ticket_FilterableData{},
		&pb.Ticket_FilterableData{StringArgs: map[string]string{"class": "tank"}},
	}
	das := []*pb.Ticket_FilterableData{
		&pb.Ticket_FilterableData{},
		&pb.Ticket_FilterableData{DoubleArgs: map[string]float64{"mmr": 1200.0}},
	}
	cts := []*pb.Ticket_FilterableData{
		&pb.Ticket_FilterableData{},
		&pb.Ticket_FilterableData{CreationTime: &timestamppb.Timestamp{Seconds: 0, Nanos: 0}},
		&pb.Ticket_FilterableData{CreationTime: timestamppb.Now()},
	}
	validEts := map[string]*pb.Ticket{
		"empty": &pb.Ticket{},
		"now+halfMaxTTL": &pb.Ticket{ExpirationTime: timestamppb.New(time.Now().Add(
			(time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))) / 2))},
		"now+maxTTL": &pb.Ticket{ExpirationTime: timestamppb.New(time.Now().Add(
			time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))))},
		"now+matTTL-1": &pb.Ticket{ExpirationTime: timestamppb.New(time.Now().Add(
			time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS")-1)))},
	}

	// Combine the ways of making empty contents to generate different empty
	// valid Attributes possibilities.
	attrs := []*pb.Ticket_FilterableData{}
	for _, tags := range tags {
		for _, sas := range sas {
			for _, das := range das {
				for _, ct := range cts {
					attrs = append(attrs, &pb.Ticket_FilterableData{
						Tags:         tags.GetTags(),
						StringArgs:   sas.GetStringArgs(),
						DoubleArgs:   das.GetDoubleArgs(),
						CreationTime: ct.GetCreationTime(),
					})
				}
			}
		}
	}

	// Test that all valid combinations of expiration times, and attributes are parsed correctly.
	for eCaseName, testExpirationTime := range validEts {
		for _, testAttrs := range attrs {
			t.Run(fmt.Sprintf("ValidTicketAttrs-ExpTime=%v-FilterableData=%v", eCaseName, testAttrs), func(attrs *pb.Ticket_FilterableData) func(t *testing.T) {
				return func(t *testing.T) {
					t.Parallel()
					request := &pb.CreateTicketRequest{
						Ticket: &pb.Ticket{ExpirationTime: testExpirationTime.GetExpirationTime(),
							Attributes: attrs}}
					result := &pb.Ticket{}

					// First, check that the ticket processing works correctly
					marshalledTicket, err := validateAndMarshalIncomingTicket(request)
					require.NoError(t, err)
					proto.Unmarshal(marshalledTicket, result) //nolint:errcheck
					// Filterable Data Attributes being copied correctly to the new
					// ticket?
					if len(attrs.GetTags()) > 0 {
						assert.Equal(t, attrs.GetTags(),
							result.GetAttributes().GetTags())
					}
					if len(attrs.GetStringArgs()) > 0 {
						assert.Equal(t,
							attrs.GetStringArgs(),
							result.GetAttributes().GetStringArgs())
					}
					if len(attrs.GetDoubleArgs()) > 0 {
						assert.Equal(t, attrs.GetDoubleArgs(),
							result.GetAttributes().GetDoubleArgs())
					}
					if attrs.GetCreationTime().IsValid() {
						// Note: have to use AsTime() to convert
						// timestamppb.Timestamp to time.Time because
						// timestamppb.Timestamp has a DoNotCompare pragma set in
						// its golang object definition
						assert.Equal(t, attrs.GetCreationTime().AsTime(),
							result.GetAttributes().GetCreationTime().AsTime())
					}

					// Creation and expiration time being generated correctly?
					assert.True(t, result.GetAttributes().GetCreationTime().IsValid())
					assert.True(t, result.GetExpirationTime().IsValid())

					// Check that the ticket insertion code for the replication layer of
					// the ticket cache works correctly.
					for tcTypeName, tc := range tcs {
						response, err := createTicket(context.Background(), tc, request)
						require.NoError(t, err, "Error trying to replicate ticket using %v replication", tcTypeName)
						assert.NotEmpty(t, response.GetTicketId(), "%v replication didn't provide a ticket id", tcTypeName)
					}
				}
			}(testAttrs))
		}
	}
}

// TestValidateTicketStateUpdates tests the validateTicketStateUpdates function
// used by the internal implementations of the gRPC functions ActivateTickets()
// and DeactivateTickets() defined in proto/v2/api.proto .
func TestValidateTicketStateUpdates(t *testing.T) {

	// Table-driven test cases
	type testcase struct {
		in                   []string
		exValidIds           []string
		exInvalidDetailsKeys []string
		exError              error
	}
	tests := map[string]*testcase{
		//testcase{name:, in:, exValidIds:, exInvalidDetails:, exError:},
		"empty_input": &testcase{
			in:                   []string{},
			exValidIds:           nil,
			exInvalidDetailsKeys: nil,
			exError:              NoTicketIdsErr},
		"valid_input": &testcase{
			in:                   validTicketIds,
			exValidIds:           validTicketIds,
			exInvalidDetailsKeys: nil,
			exError:              nil},
		"invalid_input": &testcase{
			in:                   invalidTicketIds,
			exValidIds:           nil,
			exInvalidDetailsKeys: nil,
			exError:              NoValidTicketIdsErr},
		"partial_valid_input": &testcase{
			in:                   append(validTicketIds, invalidTicketIds...),
			exValidIds:           validTicketIds,
			exInvalidDetailsKeys: invalidTicketIds,
			exError:              InvalidIdErr},
	}

	// Validate that all types of ticket cache replication produce the same results.
	for replType, tc := range tcs {
		// Run all tests in the table.
		for testName, thisTestCase := range tests {
			t.Run(fmt.Sprintf("ValidateTicketStateUpdates-%v-%v", replType, testName), func(thisTestCase *testcase) func(t *testing.T) {
				return func(t *testing.T) {
					t.Parallel()

					// function call
					valid, invalid, err := validateTicketStateUpdates(testLogger, tc.Replicator.GetReplIdValidator(), thisTestCase.in)

					// Check that expected list of valid ids was returned
					assert.ElementsMatch(t, valid, thisTestCase.exValidIds)

					// Expected no error
					if thisTestCase.exError == nil {
						assert.Empty(t, err)
					} else {
						// Check expected error was returned
						require.EqualError(t, err, thisTestCase.exError.Error())
					}

					// Get all keys from the invalid id details map
					invalidIds := []string{}
					for k, v := range invalid {
						// expected invalid ticket id error
						require.EqualError(t, v, InvalidIdErr.Error())
						invalidIds = append(invalidIds, k)
					}

					// Check that map of invalid ids errors contained every key we expected
					assert.ElementsMatch(t, invalidIds, thisTestCase.exInvalidDetailsKeys)
				}
			}(thisTestCase))
		}
	}
}

// TestActivateDeactivateTickets tests only that the activateTickets() and
// deactivateTickets() function calls successfully perform their validation
// tasks. It does not validate that the actual ticket state updates are
// successful (that is a job for unit tests of the state storage layer and e2e
// tests that are generating real ticket IDs).
//
// activateTickets() and deactivateTickets() are the internal implementations of the gRPC functions ActivateTickets() and DeactivateTickets() defined in
// proto/v2/api.proto .
//
// This loops over every testcase and runs it against activation and
// deactivation validation codepaths, which makes it fail the complexity linter.
//
//nolint:gocognit
func TestActivateDeactivateTickets(t *testing.T) {
	// Table-driven test cases
	type testcase struct {
		in                   []string
		exStatus             *status.Status
		exInvalidDetailsKeys []string
	}
	tests := map[string]*testcase{
		"empty_input": &testcase{
			in:                   []string{},
			exInvalidDetailsKeys: nil,
			exStatus:             status.New(codes.InvalidArgument, NoTicketIdsErr.Error())},
		"valid_input": &testcase{
			in:                   validTicketIds,
			exInvalidDetailsKeys: nil,
			exStatus:             status.New(codes.OK, "")},
		"invalid_input": &testcase{
			in:                   invalidTicketIds,
			exInvalidDetailsKeys: nil,
			exStatus:             status.New(codes.InvalidArgument, NoValidTicketIdsErr.Error())},
		"partial_valid_input": &testcase{
			in:                   append(validTicketIds, invalidTicketIds...),
			exInvalidDetailsKeys: invalidTicketIds,
			exStatus:             status.New(codes.InvalidArgument, InvalidIdErr.Error())},
	}

	// Validate that all types of ticket cache replication produce the same results.
	for replType, tc := range tcs {

		// Run all tests in the table.
		for testName, thisTestCase := range tests {

			// Check that each test in the table also completes even if context
			// is cancelled (these operations do not support cancellation)
			for _, abort := range []bool{false, true} {
				abortTitleString := ""
				if abort {
					abortTitleString = "[context_cancelled]"
				}

				// Check that both kinds of state update (activation and deactivation) give the expected results.
				for _, updateType := range []string{"Activate", "Deactivate"} {
					t.Run(fmt.Sprintf("%vTicket-%v-%v%v", updateType, replType, testName, abortTitleString),
						func(thisTestCase *testcase) func(t *testing.T) {
							return func(t *testing.T) {
								t.Parallel()

								// function call
								ctx, cancel := context.WithCancel(context.Background())
								defer cancel()
								if abort {
									go func() {
										time.Sleep(25 * time.Millisecond)
										cancel() // test that the function works properly when context is cancelled
									}()
								}

								var err error
								if updateType == "Activate" {
									// Activation is successful if ticketid does NOT appear in the inactive set
									_, err = activateTickets(ctx, testLogger, tc, &pb.ActivateTicketsRequest{TicketIds: thisTestCase.in})
								} else if updateType == "Deactivate" {
									// Activation is successful if ticketid does appear in the inactive set
									_, err = deactivateTickets(ctx, testLogger, tc, &pb.DeactivateTicketsRequest{TicketIds: thisTestCase.in})
								}

								// Check expected error was returned
								s := status.Convert(err)
								assert.Equal(t, thisTestCase.exStatus.Code(), s.Code())
								assert.Contains(t, s.Message(), thisTestCase.exStatus.Message())

								// Get all keys from the invalid id details map of the status BadRequest.FieldViolations array
								// and make sure it contains all expected keys.
								deets := s.Details()
								if len(deets) > 0 {
									assert.ElementsMatch(t, getStatusDetailBrvKeys(s), thisTestCase.exInvalidDetailsKeys)
								}

							}
						}(thisTestCase))
				}
			}
		}
	}
}

// Parse the grpc Status object and return the ticket ids (i.e. 'keys') of all
// the BadRequest.FieldViolation detail messages.
func getStatusDetailBrvKeys(s *status.Status) []string {
	failedKeys := []string{}
	// invalid IDs are returned in the details field of the grpc Status message type
	for _, detail := range s.Details() {
		switch failure := detail.(type) {
		case *errdetails.BadRequest:
			// There should be one violation for each invalid ID
			for _, violation := range failure.GetFieldViolations() {
				// The 'Field' fields of the FieldViolations message contains the string 'ticket_id/<ticketId>'
				testLogger.Debugf("failed id: '%v'", strings.Split(violation.GetField(), "/")[1])
				failedKeys = append(failedKeys, strings.Split(violation.GetField(), "/")[1])
			}
		}
	}
	return failedKeys
}
