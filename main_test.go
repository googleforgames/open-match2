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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/googleforgames/open-match2/v2/internal/config"
	"github.com/googleforgames/open-match2/v2/internal/mmfauth"
	"github.com/googleforgames/open-match2/v2/internal/statestore/cache"
	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	memoryReplicator "github.com/googleforgames/open-match2/v2/internal/statestore/memory"
	redisReplicator "github.com/googleforgames/open-match2/v2/internal/statestore/redis"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/redis"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/oauth2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcMetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
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

	testMetricReader  *sdkmetric.ManualReader
	testMeterProvider *sdkmetric.MeterProvider

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

const redisUri = "redis://localhost:6379"

// TestMain sets up both the in-memory mock ticket replicator and the actual
// redis ticket replicator (using local redis and falling back to
// testcontainers) so tests can run against both types and confirm they produce
// the expected results.
func TestMain(m *testing.M) {
	logrus.SetLevel(logrus.FatalLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	os.Setenv("OM_REDIS_DIAL_MAX_BACKOFF_TIMEOUT", "10s")

	redisConfig := func(uri string) {
		if !strings.HasPrefix(uri, "redis://") {
			uri = "redis://" + uri
		}
		// Attempt to connect to locally running redis
		redisUrl, err := url.Parse(uri)
		if err != nil {
			testLogger.Fatalf("failed to parse connection string for redis: %s", err)
		}
		os.Setenv("REDISHOST", redisUrl.Hostname())
		os.Setenv("REDISPORT", redisUrl.Port())
		os.Setenv("OM_REDIS_READ_HOST", redisUrl.Hostname())
		os.Setenv("OM_REDIS_WRITE_HOST", redisUrl.Hostname())
		os.Setenv("OM_REDIS_READ_PORT", redisUrl.Port())
		os.Setenv("OM_REDIS_WRITE_PORT", redisUrl.Port())
	}
	redisConfig(redisUri)
	cfg = config.Read() // Read env vars into config

	// Configure metrics once before any background cache replication goroutines start.
	meter, otelShutdownFunc = initializeOtel()
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer shutCancel()
		_ = otelShutdownFunc(shutCtx)
	}()
	testMetricReader = sdkmetric.NewManualReader()
	testMeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetricReader))
	testMeter := testMeterProvider.Meter("open_match.core.test")
	meter = &testMeter
	registerMetrics(meter)
	cache.RegisterMetrics(meter)

	if os.Getenv("OM_SKIP_REDIS_TESTS") == "true" {
		testLogger.Infof("Skipping redis replicator setup (OM_SKIP_REDIS_TESTS=true)")
	} else {
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
		tcs["redis"] = cache.New(cfg, rr)
	}

	// Set up the memory-backed replicated ticket cache
	tcs["memory"] = cache.New(cfg, memoryReplicator.New(cfg))

	for _, v := range tcs {
		go v.OutgoingReplicationQueue(ctx)
		go v.IncomingReplicationQueue(ctx)
	}

	code := m.Run()
	cancel()
	if code != 0 {
		os.Exit(code)
	}
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
	inValidEts := map[string]func() *pb.Ticket{
		"past": func() *pb.Ticket {
			return &pb.Ticket{ExpirationTime: timestamppb.New(time.Now().Add(-1 * time.Second))}
		},
		// 1/1/1970
		"epoch": func() *pb.Ticket {
			return &pb.Ticket{ExpirationTime: &timestamppb.Timestamp{Seconds: 0, Nanos: 0}}
		},
		"exceeds_max_ttl": func() *pb.Ticket {
			return &pb.Ticket{ExpirationTime: timestamppb.New(time.Now().Add(
				time.Millisecond*time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS")) + 10*time.Second))}
		},
		// Timestamppb max allowed timestamp value + 1
		"out_of_protobuf_range": func() *pb.Ticket {
			return &pb.Ticket{ExpirationTime: &timestamppb.Timestamp{Seconds: 253402300800, Nanos: 0}}
		},
	}

	// Expected failure status for invalid expiration times
	invalidExpTimeStatus := status.New(codes.InvalidArgument,
		fmt.Sprintf("specified expiration time not between (now, now + %v)",
			cfg.GetInt("OM_CACHE_TICKET_TTL_MS")))

	// Make sure all expected invalid expiration times do fail validation correctly
	for testCaseName, makeTicket := range inValidEts {
		t.Run(fmt.Sprintf("InvalidExpTime=%v", testCaseName), func(makeTicket func() *pb.Ticket) func(t *testing.T) {
			return func(t *testing.T) {
				t.Parallel()
				_, err := validateAndMarshalIncomingTicket(&pb.CreateTicketRequest{Ticket: makeTicket()})
				// Check expected error was returned
				s := status.Convert(err)
				assert.Equal(t, invalidExpTimeStatus.Code(), s.Code())
				assert.Contains(t, s.Message(), invalidExpTimeStatus.Message())

				// Verify CreateTicket preserves codes.InvalidArgument
				_, createErr := createTicket(context.Background(), tcs["memory"], &pb.CreateTicketRequest{Ticket: makeTicket()})
				createStatus := status.Convert(createErr)
				assert.Equal(t, codes.InvalidArgument, createStatus.Code())
				assert.Contains(t, createStatus.Message(), invalidExpTimeStatus.Message())
			}
		}(makeTicket))
	}

	// valid filterable data attribute test cases
	tags := []*pb.Ticket_FilterableData{
		{},
		{Tags: []string{"bonusxp"}},
	}
	sas := []*pb.Ticket_FilterableData{
		{},
		{StringArgs: map[string]string{"class": "tank"}},
	}
	das := []*pb.Ticket_FilterableData{
		{},
		{DoubleArgs: map[string]float64{"mmr": 1200.0}},
	}
	cts := []func() *pb.Ticket_FilterableData{
		func() *pb.Ticket_FilterableData { return &pb.Ticket_FilterableData{} },
		func() *pb.Ticket_FilterableData {
			return &pb.Ticket_FilterableData{CreationTime: &timestamppb.Timestamp{Seconds: 0, Nanos: 0}}
		},
		func() *pb.Ticket_FilterableData {
			return &pb.Ticket_FilterableData{CreationTime: timestamppb.Now()}
		},
	}
	validEts := map[string]func() *pb.Ticket{
		"empty": func() *pb.Ticket { return &pb.Ticket{} },
		"now+halfMaxTTL": func() *pb.Ticket {
			return &pb.Ticket{ExpirationTime: timestamppb.New(time.Now().Add(
				(time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))) / 2))}
		},
		"now+maxTTL": func() *pb.Ticket {
			return &pb.Ticket{ExpirationTime: timestamppb.New(time.Now().Add(
				time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))))}
		},
		"now+matTTL-1": func() *pb.Ticket {
			return &pb.Ticket{ExpirationTime: timestamppb.New(time.Now().Add(
				time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS")-1)))}
		},
	}

	// Combine the ways of making empty contents to generate different empty
	// valid Attributes possibilities.
	attrFactories := []func() *pb.Ticket_FilterableData{}
	for _, tagItem := range tags {
		for _, saItem := range sas {
			for _, daItem := range das {
				for _, ctFactory := range cts {
					tCopy := tagItem
					sCopy := saItem
					dCopy := daItem
					cFactory := ctFactory
					attrFactories = append(attrFactories, func() *pb.Ticket_FilterableData {
						ct := cFactory()
						return &pb.Ticket_FilterableData{
							Tags:         tCopy.GetTags(),
							StringArgs:   sCopy.GetStringArgs(),
							DoubleArgs:   dCopy.GetDoubleArgs(),
							CreationTime: ct.GetCreationTime(),
						}
					})
				}
			}
		}
	}

	// Test that all valid combinations of expiration times, and attributes are parsed correctly.
	for eCaseName, makeExpTicket := range validEts {
		for idx, makeAttrs := range attrFactories {
			t.Run(fmt.Sprintf("ValidTicketAttrs-ExpTime=%v-FilterableData=%v", eCaseName, idx), func(eCaseName string, makeExpTicket func() *pb.Ticket, makeAttrs func() *pb.Ticket_FilterableData) func(t *testing.T) {
				return func(t *testing.T) {
					t.Parallel()
					attrs := makeAttrs()
					expTicket := makeExpTicket()
					request := &pb.CreateTicketRequest{
						Ticket: &pb.Ticket{
							ExpirationTime: expTicket.GetExpirationTime(),
							Attributes:     attrs,
						},
					}
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
					if eCaseName == "empty" {
						expectedExp := result.GetAttributes().GetCreationTime().AsTime().Add(
							time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS")))
						assert.Equal(t, expectedExp, result.GetExpirationTime().AsTime())
					}

					// Check that the ticket insertion code for the replication layer of
					// the ticket cache works correctly.
					for tcTypeName, tc := range tcs {
						freshExp := makeExpTicket()
						freshReq := &pb.CreateTicketRequest{
							Ticket: &pb.Ticket{
								ExpirationTime: freshExp.GetExpirationTime(),
								Attributes:     makeAttrs(),
							},
						}
						response, err := createTicket(context.Background(), tc, freshReq)
						require.NoError(t, err, "Error trying to replicate ticket using %v replication", tcTypeName)
						assert.NotEmpty(t, response.GetTicketId(), "%v replication didn't provide a ticket id", tcTypeName)
					}
				}
			}(eCaseName, makeExpTicket, makeAttrs))
		}
	}
}

// TestValidateTicketStateUpdates tests the validateTicketStateUpdates function
// used by the internal implementations of the gRPC functions ActivateTickets()
// and DeactivateTickets() defined in proto/v2/api.proto .
func TestValidateTicketStateUpdates(t *testing.T) {
	tooManyIds := make([]string, cfg.GetInt("OM_MAX_STATE_UPDATES_PER_CALL")+1)
	for i := range tooManyIds {
		tooManyIds[i] = validTicketIds[0]
	}

	// Table-driven test cases
	type testcase struct {
		in                   []string
		exValidIds           []string
		exInvalidDetailsKeys []string
		exError              error
	}
	tests := map[string]*testcase{
		"empty_input": {
			in:                   []string{},
			exValidIds:           nil,
			exInvalidDetailsKeys: nil,
			exError:              NoTicketIdsErr,
		},
		"valid_input": {
			in:                   slices.Clone(validTicketIds),
			exValidIds:           slices.Clone(validTicketIds),
			exInvalidDetailsKeys: nil,
			exError:              nil,
		},
		"invalid_input": {
			in:                   slices.Clone(invalidTicketIds),
			exValidIds:           nil,
			exInvalidDetailsKeys: nil,
			exError:              NoValidTicketIdsErr,
		},
		"partial_valid_input": {
			in:                   append(slices.Clone(validTicketIds), invalidTicketIds...),
			exValidIds:           slices.Clone(validTicketIds),
			exInvalidDetailsKeys: slices.Clone(invalidTicketIds),
			exError:              InvalidIdErr,
		},
		"too_many_updates": {
			in:                   tooManyIds,
			exValidIds:           nil,
			exInvalidDetailsKeys: nil,
			exError:              TooManyUpdatesErr,
		},
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
						assert.NoError(t, err)
					} else {
						// Check expected error was returned
						require.ErrorIs(t, err, thisTestCase.exError)
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
	tooManyIds := make([]string, cfg.GetInt("OM_MAX_STATE_UPDATES_PER_CALL")+1)
	for i := range tooManyIds {
		tooManyIds[i] = validTicketIds[0]
	}

	// Table-driven test cases
	type testcase struct {
		in                   []string
		exStatus             *status.Status
		exInvalidDetailsKeys []string
	}
	tests := map[string]*testcase{
		"empty_input": {
			in:                   []string{},
			exInvalidDetailsKeys: nil,
			exStatus:             status.New(codes.InvalidArgument, NoTicketIdsErr.Error()),
		},
		"valid_input": {
			in:                   slices.Clone(validTicketIds),
			exInvalidDetailsKeys: nil,
			exStatus:             status.New(codes.OK, ""),
		},
		"invalid_input": {
			in:                   slices.Clone(invalidTicketIds),
			exInvalidDetailsKeys: nil,
			exStatus:             status.New(codes.InvalidArgument, NoValidTicketIdsErr.Error()),
		},
		"partial_valid_input": {
			in:                   append(slices.Clone(validTicketIds), invalidTicketIds...),
			exInvalidDetailsKeys: slices.Clone(invalidTicketIds),
			exStatus:             status.New(codes.InvalidArgument, InvalidIdErr.Error()),
		},
		"too_many_updates": {
			in:                   tooManyIds,
			exInvalidDetailsKeys: nil,
			exStatus:             status.New(codes.InvalidArgument, TooManyUpdatesErr.Error()),
		},
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
						func(thisTestCase *testcase, abort bool, updateType string) func(t *testing.T) {
							return func(t *testing.T) {
								t.Parallel()

								// function call
								ctx, cancel := context.WithCancel(context.Background())
								defer cancel()
								if abort {
									cancel()
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
						}(thisTestCase, abort, updateType))
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
				failedId := strings.TrimPrefix(violation.GetField(), "ticket_id/")
				testLogger.Debugf("failed id: '%v'", failedId)
				failedKeys = append(failedKeys, failedId)
			}
		}
	}
	return failedKeys
}

//----------------------------------------------------------------------------------
// Test Helpers for Root Unit and Integration Tests
//----------------------------------------------------------------------------------

func newFastCoreTestConfig() *viper.Viper {
	c := config.Read()
	c.Set("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 500)
	c.Set("OM_CACHE_IN_WAIT_TIMEOUT_MS", 15)
	c.Set("OM_CACHE_IN_POLL_WAIT_MS", 5)
	c.Set("OM_CACHE_IN_FULL_POLL_WAIT_MS", 5)
	c.Set("OM_CACHE_IN_QUEUE_BUFFER_SIZE", 2000)
	c.Set("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 5)
	c.Set("OM_CACHE_IN_FULL_APPLY_SLEEP_MS", 5)
	c.Set("OM_CACHE_IN_MAX_APPLY_DURATION_MS", 200)
	c.Set("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 10)
	c.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 5)
	c.Set("OM_CACHE_OUT_QUEUE_BUFFER_SIZE", 500)
	c.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", false)
	c.Set("OM_CACHE_OUT_MAX_PACKED_KEYS", 500)
	c.Set("OM_CACHE_TICKET_TTL_MS", 600000)
	c.Set("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS", 600000)
	c.Set("OM_CACHE_EXPIRATION_INTERVAL_MS", 600000)
	c.Set("OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE", 5000)
	c.Set("OM_MATCH_TICKET_DEACTIVATION_WAIT", true)
	c.Set("OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS", 5000)
	c.Set("OM_MMF_TIMEOUT_SECS", 10)
	return c
}

var parkedPollerForever = make(chan struct{})

type trackedStateReplicator struct {
	base       store.StateReplicator
	mu         sync.Mutex
	parking    bool
	parkedOnce sync.Once
	parkedChan chan struct{}
}

func newTrackedStateReplicator(base store.StateReplicator) *trackedStateReplicator {
	return &trackedStateReplicator{
		base:       base,
		parkedChan: make(chan struct{}),
	}
}

func (tr *trackedStateReplicator) GetUpdates() []*store.StateUpdate {
	tr.mu.Lock()
	if tr.parking {
		tr.parkedOnce.Do(func() { close(tr.parkedChan) })
		tr.mu.Unlock()
		<-parkedPollerForever
		return nil
	}
	tr.mu.Unlock()
	return tr.base.GetUpdates()
}

func (tr *trackedStateReplicator) parkPoller() {
	tr.mu.Lock()
	tr.parking = true
	tr.mu.Unlock()
	<-tr.parkedChan
}

func (tr *trackedStateReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	return tr.base.SendUpdates(updates)
}

func (tr *trackedStateReplicator) GetReplIdValidator() *regexp.Regexp {
	return tr.base.GetReplIdValidator()
}

func startLocalTestCache(t *testing.T, customCfg *viper.Viper, repl store.StateReplicator) *cache.ReplicatedTicketCache {
	t.Helper()
	if customCfg == nil {
		customCfg = newFastCoreTestConfig()
	}
	if repl == nil {
		repl = memoryReplicator.New(customCfg)
	}
	tracked := newTrackedStateReplicator(repl)
	localTc := cache.New(customCfg, tracked)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		localTc.OutgoingReplicationQueue(ctx)
	}()
	go func() {
		defer wg.Done()
		localTc.IncomingReplicationQueue(ctx)
	}()
	t.Cleanup(func() {
		tracked.parkPoller()
		cancel()
		wg.Wait()
	})
	return localTc
}

var activeGlobalCacheCleanup func()

func setupGlobalTestCache(t *testing.T, customCfg *viper.Viper, repl store.StateReplicator) {
	t.Helper()
	if activeGlobalCacheCleanup != nil {
		activeGlobalCacheCleanup()
		activeGlobalCacheCleanup = nil
	}
	origCfg := cfg
	if customCfg == nil {
		customCfg = newFastCoreTestConfig()
	}
	cfg = customCfg
	if repl == nil {
		repl = memoryReplicator.New(cfg)
	}
	tracked := newTrackedStateReplicator(repl)
	tc = cache.ReplicatedTicketCache{}
	tc.Init(cfg, tracked)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		tc.OutgoingReplicationQueue(ctx)
	}()
	go func() {
		defer wg.Done()
		tc.IncomingReplicationQueue(ctx)
	}()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			tracked.parkPoller()
			cancel()
			wg.Wait()
			cfg = origCfg
		})
	}
	activeGlobalCacheCleanup = cleanup
	t.Cleanup(cleanup)
}

type testMeterSnapshot struct {
	reader   *sdkmetric.ManualReader
	baseline metricdata.ResourceMetrics
}

func setupTestMeter(t *testing.T) *testMeterSnapshot {
	t.Helper()
	var base metricdata.ResourceMetrics
	require.NoError(t, testMetricReader.Collect(context.Background(), &base))
	return &testMeterSnapshot{
		reader:   testMetricReader,
		baseline: base,
	}
}

func collectMetrics(t *testing.T, snap *testMeterSnapshot) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, snap.reader.Collect(context.Background(), &rm))
	return rm
}

func findMetricByName(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func rawSumInt64Counter(rm metricdata.ResourceMetrics, name string) int64 {
	m, ok := findMetricByName(rm, name)
	if !ok {
		return 0
	}
	sumData, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		return 0
	}
	var total int64
	for _, dp := range sumData.DataPoints {
		total += dp.Value
	}
	return total
}

func sumInt64Counter(t *testing.T, snap *testMeterSnapshot, name string) int64 {
	t.Helper()
	rm := collectMetrics(t, snap)
	return rawSumInt64Counter(rm, name) - rawSumInt64Counter(snap.baseline, name)
}

func rawSumInt64CounterByAttr(rm metricdata.ResourceMetrics, name, attrKey, attrVal string) int64 {
	m, ok := findMetricByName(rm, name)
	if !ok {
		return 0
	}
	sumData, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		return 0
	}
	var total int64
	for _, dp := range sumData.DataPoints {
		if val, found := dp.Attributes.Value(attribute.Key(attrKey)); found && val.AsString() == attrVal {
			total += dp.Value
		}
	}
	return total
}

func sumInt64CounterByAttr(t *testing.T, snap *testMeterSnapshot, name, attrKey, attrVal string) int64 {
	t.Helper()
	rm := collectMetrics(t, snap)
	return rawSumInt64CounterByAttr(rm, name, attrKey, attrVal) - rawSumInt64CounterByAttr(snap.baseline, name, attrKey, attrVal)
}

func rawSumInt64CounterByAttrs(rm metricdata.ResourceMetrics, name string, expectedAttrs map[string]string) int64 {
	m, ok := findMetricByName(rm, name)
	if !ok {
		return 0
	}
	sumData, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		return 0
	}
	var total int64
dataPointLoop:
	for _, dp := range sumData.DataPoints {
		for k, wantVal := range expectedAttrs {
			val, found := dp.Attributes.Value(attribute.Key(k))
			if !found || val.Emit() != wantVal {
				continue dataPointLoop
			}
		}
		total += dp.Value
	}
	return total
}

func sumInt64CounterByAttrs(t *testing.T, snap *testMeterSnapshot, name string, expectedAttrs map[string]string) int64 {
	t.Helper()
	rm := collectMetrics(t, snap)
	return rawSumInt64CounterByAttrs(rm, name, expectedAttrs) - rawSumInt64CounterByAttrs(snap.baseline, name, expectedAttrs)
}

func sumMatchesReceived(t *testing.T, snap *testMeterSnapshot, mmfName string, hasTickets bool) int64 {
	t.Helper()
	return sumInt64CounterByAttrs(t, snap, "om_core.match.received", map[string]string{
		"mmf.name":          mmfName,
		"match.has_tickets": strconv.FormatBool(hasTickets),
	})
}

func assertMmfFailureStatus(t *testing.T, err error, totalMmfs int, expectedFailedMmfNames []string) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error, got %T: %v", err, err)
	assert.Equal(t, codes.Internal, st.Code())
	assert.Contains(t, st.Message(), fmt.Sprintf("%d of %d matchmaking functions failed", len(expectedFailedMmfNames), totalMmfs))

	var actualFailedNames []string
	for _, detail := range st.Details() {
		info, isErrorInfo := detail.(*errdetails.ErrorInfo)
		if !isErrorInfo {
			continue
		}
		assert.Equal(t, mmfFailureErrorInfoReason, info.GetReason())
		assert.Equal(t, mmfFailureErrorInfoDomain, info.GetDomain())
		md := info.GetMetadata()
		assert.NotEmpty(t, md["grpc_code"])
		assert.NotEmpty(t, md["message"])
		assert.Contains(t, st.Message(), fmt.Sprintf("%s: %s", md["mmf_name"], md["message"]))
		actualFailedNames = append(actualFailedNames, md["mmf_name"])
	}
	assert.ElementsMatch(t, expectedFailedMmfNames, actualFailedNames)
}

func decodeMatchInvalidTicketIds(t *testing.T, match *pb.Match) ([]string, bool) {
	t.Helper()
	extAny, ok := match.GetExtensions()[MatchInvalidTicketIdsExtensionKey]
	if !ok {
		return nil, false
	}
	var listVal structpb.ListValue
	require.NoError(t, extAny.UnmarshalTo(&listVal))
	ids := make([]string, len(listVal.GetValues()))
	for idx, val := range listVal.GetValues() {
		ids[idx] = val.GetStringValue()
	}
	return ids, true
}

type selectiveDeactivateFailureReplicator struct {
	base    store.StateReplicator
	mu      sync.Mutex
	failSet map[string]struct{}
}

func newSelectiveDeactivateFailureReplicator(base store.StateReplicator) *selectiveDeactivateFailureReplicator {
	return &selectiveDeactivateFailureReplicator{
		base:    base,
		failSet: make(map[string]struct{}),
	}
}

func (r *selectiveDeactivateFailureReplicator) setFailedTicketIds(ids ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failSet = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		r.failSet[id] = struct{}{}
	}
}

func (r *selectiveDeactivateFailureReplicator) shouldFail(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.failSet[id]
	return ok
}

func (r *selectiveDeactivateFailureReplicator) GetUpdates() []*store.StateUpdate {
	return r.base.GetUpdates()
}

func (r *selectiveDeactivateFailureReplicator) GetReplIdValidator() *regexp.Regexp {
	return r.base.GetReplIdValidator()
}

func (r *selectiveDeactivateFailureReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	responses := make([]*store.StateResponse, len(updates))
	forwarded := make([]*store.StateUpdate, 0, len(updates))
	forwardedIndices := make([]int, 0, len(updates))

	for i, u := range updates {
		if u != nil && u.Cmd == store.Deactivate && r.shouldFail(u.PrimaryKey()) {
			responses[i] = &store.StateResponse{
				Result: u.PrimaryKey(),
				Err:    errors.New("simulated deactivation write failure"),
			}
			continue
		}
		forwarded = append(forwarded, u)
		forwardedIndices = append(forwardedIndices, i)
	}

	if len(forwarded) > 0 {
		baseResponses := r.base.SendUpdates(forwarded)
		for j, origIdx := range forwardedIndices {
			if j < len(baseResponses) {
				responses[origIdx] = baseResponses[j]
			}
		}
	}
	return responses
}

func rawFloat64HistogramCount(rm metricdata.ResourceMetrics, name string) uint64 {
	m, ok := findMetricByName(rm, name)
	if !ok {
		return 0
	}
	histData, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		return 0
	}
	var count uint64
	for _, dp := range histData.DataPoints {
		count += dp.Count
	}
	return count
}

func float64HistogramCount(t *testing.T, snap *testMeterSnapshot, name string) uint64 {
	t.Helper()
	rm := collectMetrics(t, snap)
	return rawFloat64HistogramCount(rm, name) - rawFloat64HistogramCount(snap.baseline, name)
}

func rawInt64HistogramStats(rm metricdata.ResourceMetrics, name string) (count uint64, sum int64) {
	m, ok := findMetricByName(rm, name)
	if !ok {
		return 0, 0
	}
	histData, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		return 0, 0
	}
	for _, dp := range histData.DataPoints {
		count += dp.Count
		sum += dp.Sum
	}
	return count, sum
}

func int64HistogramStats(t *testing.T, snap *testMeterSnapshot, name string) (count uint64, sum int64) {
	t.Helper()
	rm := collectMetrics(t, snap)
	curCount, curSum := rawInt64HistogramStats(rm, name)
	baseCount, baseSum := rawInt64HistogramStats(snap.baseline, name)
	return curCount - baseCount, curSum - baseSum
}

func createAndActivateTicketsInCache(t *testing.T, targetTc *cache.ReplicatedTicketCache, count int, tags []string, extraBytes int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, count)
	var ext map[string]*anypb.Any
	if extraBytes > 0 {
		payload, err := anypb.New(wrapperspb.Bytes(make([]byte, extraBytes)))
		require.NoError(t, err)
		ext = map[string]*anypb.Any{"payload": payload}
	}
	for i := 0; i < count; i++ {
		resp, err := createTicket(ctx, targetTc, &pb.CreateTicketRequest{
			Ticket: &pb.Ticket{
				Attributes: &pb.Ticket_FilterableData{
					Tags: tags,
				},
				Extensions: ext,
			},
		})
		require.NoError(t, err)
		require.NotEmpty(t, resp.GetTicketId())
		ids[i] = resp.GetTicketId()
	}
	_, err := activateTickets(ctx, testLogger, targetTc, &pb.ActivateTicketsRequest{TicketIds: ids})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		snap := targetTc.SnapshotActiveTickets()
		for _, id := range ids {
			if _, ok := snap[id]; !ok {
				return false
			}
		}
		return true
	}, 3*time.Second, 5*time.Millisecond)
	return ids
}

type mockInvokeMmfServerStream struct {
	grpc.ServerStream
	ctx      context.Context
	mu       sync.Mutex
	matches  []*pb.Match
	sendHook func(*pb.StreamedMmfResponse) error
}

func (m *mockInvokeMmfServerStream) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockInvokeMmfServerStream) Send(resp *pb.StreamedMmfResponse) error {
	if m.sendHook != nil {
		if err := m.sendHook(resp); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.matches = append(m.matches, resp.GetMatch())
	m.mu.Unlock()
	return nil
}

func (m *mockInvokeMmfServerStream) receivedMatches() []*pb.Match {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.matches)
}

type mockWatchAssignmentsServerStream struct {
	grpc.ServerStream
	ctx        context.Context
	mu         sync.Mutex
	responses  []*pb.StreamedWatchAssignmentsResponse
	sendNotify chan *pb.StreamedWatchAssignmentsResponse
}

func (m *mockWatchAssignmentsServerStream) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockWatchAssignmentsServerStream) Send(resp *pb.StreamedWatchAssignmentsResponse) error {
	m.mu.Lock()
	m.responses = append(m.responses, resp)
	m.mu.Unlock()
	if m.sendNotify != nil {
		select {
		case m.sendNotify <- resp:
		default:
		}
	}
	return nil
}

func (m *mockWatchAssignmentsServerStream) receivedResponses() []*pb.StreamedWatchAssignmentsResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.responses)
}

type testMmfService struct {
	pb.UnimplementedMatchMakingFunctionServiceServer
	runFunc func(pb.MatchMakingFunctionService_RunServer) error
}

func (s *testMmfService) Run(stream pb.MatchMakingFunctionService_RunServer) error {
	return s.runFunc(stream)
}

func startTestMmfGrpcServer(
	t *testing.T,
	creds credentials.TransportCredentials,
	runFunc func(pb.MatchMakingFunctionService_RunServer) error,
) (host string, port int32) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var opts []grpc.ServerOption
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
	}
	srv := grpc.NewServer(opts...)
	pb.RegisterMatchMakingFunctionServiceServer(srv, &testMmfService{runFunc: runFunc})

	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})

	tcpAddr := lis.Addr().(*net.TCPAddr)
	return "127.0.0.1", int32(tcpAddr.Port)
}

//----------------------------------------------------------------------------------
// 25 New Unit and Integration Test Scenarios (Task 10.4)
//----------------------------------------------------------------------------------

// Scenario 1: Active Snapshot Isolation
func TestActiveSnapshotIsolation(t *testing.T) {
	localTc := startLocalTestCache(t, nil, nil)
	ctx := context.Background()

	ids := createAndActivateTicketsInCache(t, localTc, 2, []string{"mode.ctf"}, 0)
	t1, t2 := ids[0], ids[1]

	// Deactivate t2 so only t1 is active at the time of snapshotting.
	_, highestReplId := updateTicketsActiveState(ctx, testLogger, localTc, []string{t2}, store.Deactivate)
	require.True(t, localTc.WaitForDeactivation(t2, highestReplId, 2*time.Second))

	snapBefore := localTc.SnapshotActiveTickets()
	diffBefore := setDifference(&localTc.Tickets, &localTc.InactiveSet)
	require.Len(t, snapBefore, 1)
	require.Contains(t, snapBefore, t1)
	require.Len(t, diffBefore, 1)
	assert.Equal(t, t1, diffBefore[0].(*pb.Ticket).GetId())

	// Subsequent mutations: create & activate t3, re-activate t2, and deactivate t1.
	t3Slice := createAndActivateTicketsInCache(t, localTc, 1, []string{"mode.ctf"}, 0)
	t3 := t3Slice[0]
	_, err := activateTickets(ctx, testLogger, localTc, &pb.ActivateTicketsRequest{TicketIds: []string{t2}})
	require.NoError(t, err)
	_, deactT1ReplId := updateTicketsActiveState(ctx, testLogger, localTc, []string{t1}, store.Deactivate)
	require.True(t, localTc.WaitForDeactivation(t1, deactT1ReplId, 2*time.Second))
	require.Eventually(t, func() bool {
		cur := localTc.SnapshotActiveTickets()
		_, hasT2 := cur[t2]
		_, hasT3 := cur[t3]
		_, hasT1 := cur[t1]
		return hasT2 && hasT3 && !hasT1
	}, 2*time.Second, 5*time.Millisecond)

	// Point-in-time snapshots taken before mutations must remain completely isolated.
	assert.Len(t, snapBefore, 1)
	assert.Contains(t, snapBefore, t1)
	assert.NotContains(t, snapBefore, t2)
	assert.NotContains(t, snapBefore, t3)
	assert.Len(t, diffBefore, 1)
	assert.Equal(t, t1, diffBefore[0].(*pb.Ticket).GetId())
}

// Scenario 2: Deactivation Watermark Waiting
func TestWaitForDeactivationWatermark(t *testing.T) {
	ctx := context.Background()
	for replType, cacheInst := range tcs {
		t.Run(replType, func(t *testing.T) {
			resp, err := createTicket(ctx, cacheInst, &pb.CreateTicketRequest{
				Ticket: &pb.Ticket{
					Attributes: &pb.Ticket_FilterableData{Tags: []string{"watermark-test"}},
				},
			})
			require.NoError(t, err)
			ticketId := resp.GetTicketId()

			errs, highestReplId := updateTicketsActiveState(ctx, testLogger, cacheInst, []string{ticketId}, store.Deactivate)
			require.Empty(t, errs)
			require.NotEmpty(t, highestReplId)

			// 1. Wait via InactiveSet + watermark.
			assert.True(t, cacheInst.WaitForDeactivation(ticketId, highestReplId, 5*time.Second))

			// 2. Wait via watermark fallback alone when ticket ID is absent from InactiveSet.
			absentTicketId := "9999999999999-999999"
			_, inInactive := cacheInst.InactiveSet.Load(absentTicketId)
			require.False(t, inInactive)
			assert.True(t, cacheInst.WaitForDeactivation(absentTicketId, highestReplId, 5*time.Second))

			// 3. Unreached future watermark and absent ticket ID times out.
			assert.False(t, cacheInst.WaitForDeactivation(absentTicketId, "9999999999999-999999", 25*time.Millisecond))
		})
	}
}

type recordingStateReplicator struct {
	base    store.StateReplicator
	mu      sync.Mutex
	updates []*store.StateUpdate
}

func (r *recordingStateReplicator) GetUpdates() []*store.StateUpdate {
	return r.base.GetUpdates()
}

func (r *recordingStateReplicator) SendUpdates(ups []*store.StateUpdate) []*store.StateResponse {
	r.mu.Lock()
	for _, u := range ups {
		if u != nil {
			cp := *u
			cp.Keys = slices.Clone(u.Keys)
			r.updates = append(r.updates, &cp)
		}
	}
	r.mu.Unlock()
	return r.base.SendUpdates(ups)
}

func (r *recordingStateReplicator) GetReplIdValidator() *regexp.Regexp {
	return r.base.GetReplIdValidator()
}

func (r *recordingStateReplicator) recordedUpdates() []*store.StateUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.updates)
}

// Scenario 3: Packed Ticket State Updates
func TestPackedTicketStateUpdates(t *testing.T) {
	packedCfg := newFastCoreTestConfig()
	packedCfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
	spyRepl := &recordingStateReplicator{base: memoryReplicator.New(packedCfg)}
	localTc := startLocalTestCache(t, packedCfg, spyRepl)

	ids := createAndActivateTicketsInCache(t, localTc, 3, []string{"packed"}, 0)

	errs, highestReplId := updateTicketsActiveState(context.Background(), testLogger, localTc, ids, store.Deactivate)
	require.Empty(t, errs)
	require.NotEmpty(t, highestReplId)

	for _, id := range ids {
		assert.True(t, localTc.WaitForDeactivation(id, highestReplId, 2*time.Second))
	}
	assert.Empty(t, localTc.SnapshotActiveTickets())

	// Verify the deactivation was sent as a single packed StateUpdate containing all 3 ticket IDs.
	var deactUpdates []*store.StateUpdate
	for _, u := range spyRepl.recordedUpdates() {
		if u.Cmd == store.Deactivate {
			deactUpdates = append(deactUpdates, u)
		}
	}
	require.Len(t, deactUpdates, 1)
	assert.Equal(t, ids, deactUpdates[0].Keys)
}

// Scenario 4: Unpacked Error Attribution
func TestUnpackedTicketStateUpdateErrorAttribution(t *testing.T) {
	unpackedCfg := newFastCoreTestConfig()
	unpackedCfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", false)

	t.Run("EmptyStateResponseResultFallsBackToEnqueuedTicketId", func(t *testing.T) {
		localTc := cache.New(unpackedCfg, memoryReplicator.New(unpackedCfg))
		ticketIds := []string{"1700000000000-1", "1700000000000-2", "1700000000000-3"}

		// Custom worker that returns an empty Result on error for ticketIds[1].
		go func() {
			for i := 0; i < len(ticketIds); i++ {
				req := <-localTc.UpRequests
				if req.Update.Key == ticketIds[1] {
					req.ResultsChan <- &store.StateResponse{
						Result: "", // Empty Result to verify fallback attribution
						Err:    errors.New("simulated storage write error"),
					}
				} else {
					req.ResultsChan <- &store.StateResponse{
						Result: fmt.Sprintf("1700000000000-%d", 10+i),
						Err:    nil,
					}
				}
			}
		}()

		errs, highestReplId := updateTicketsActiveState(context.Background(), testLogger, localTc, ticketIds, store.Deactivate)
		require.Len(t, errs, 1)
		require.Contains(t, errs, ticketIds[1])
		assert.Equal(t, codes.Internal, status.Code(errs[ticketIds[1]]))
		assert.Contains(t, errs[ticketIds[1]].Error(),
			fmt.Sprintf("Unable to update ticket %s state to inactive : simulated storage write error", ticketIds[1]))
		assert.Equal(t, "1700000000000-12", highestReplId)
	})

	t.Run("ReverseOrderCompletionAttributesToExactTicketId", func(t *testing.T) {
		localTc := cache.New(unpackedCfg, memoryReplicator.New(unpackedCfg))
		ticketIds := []string{"1700000000001-1", "1700000000001-2", "1700000000001-3"}

		// Drain all 3 requests and reply in reverse order (index 2, then 1, then 0).
		go func() {
			reqs := make([]*cache.UpdateRequest, len(ticketIds))
			for i := range reqs {
				reqs[i] = <-localTc.UpRequests
			}
			reqs[2].ResultsChan <- &store.StateResponse{Result: "1700000000001-99", Err: nil}
			reqs[1].ResultsChan <- &store.StateResponse{Result: "", Err: errors.New("reverse order failure")}
			reqs[0].ResultsChan <- &store.StateResponse{Result: "1700000000001-50", Err: nil}
		}()

		errs, highestReplId := updateTicketsActiveState(context.Background(), testLogger, localTc, ticketIds, store.Activate)
		require.Len(t, errs, 1)
		require.Contains(t, errs, ticketIds[1])
		assert.Equal(t, codes.Internal, status.Code(errs[ticketIds[1]]))
		assert.Contains(t, errs[ticketIds[1]].Error(),
			fmt.Sprintf("Unable to update ticket %s state to active : reverse order failure", ticketIds[1]))
		assert.Equal(t, "1700000000001-99", highestReplId)
	})
}

// Scenario 5: Match Deactivation Timeout Annotation
func TestWaitForMatchTicketDeactivationTimeoutAnnotation(t *testing.T) {
	ctx := context.Background()
	shortTimeoutCfg := newFastCoreTestConfig()
	shortTimeoutCfg.Set("OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS", 20)

	origCfg := cfg
	cfg = shortTimeoutCfg
	defer func() { cfg = origCfg }()

	localTc := cache.New(shortTimeoutCfg, memoryReplicator.New(shortTimeoutCfg))

	customVal, err := anypb.New(wrapperspb.String("preserve-me"))
	require.NoError(t, err)

	// 1. Timeout when annotateOnTimeout == true attaches deactivation_timeout=true and preserves extensions.
	matchTimeoutAnnotate := &pb.Match{
		Id:         "match-timeout-annotate",
		Extensions: map[string]*anypb.Any{"custom_key": customVal},
	}
	ok := waitForMatchTicketDeactivation(ctx, testLogger, localTc, matchTimeoutAnnotate, "mmf1", "prof1", "1700000000000-1", "1700000000000-99", true)
	assert.False(t, ok)
	assert.Equal(t, customVal, matchTimeoutAnnotate.GetExtensions()["custom_key"])
	require.Contains(t, matchTimeoutAnnotate.GetExtensions(), MatchDeactivationTimeoutExtensionKey)
	var bVal wrapperspb.BoolValue
	require.NoError(t, matchTimeoutAnnotate.GetExtensions()[MatchDeactivationTimeoutExtensionKey].UnmarshalTo(&bVal))
	assert.True(t, bVal.GetValue())

	// 2. Empty ticketId (all deactivation writes failed) when annotateOnTimeout == true attaches extension.
	matchWriteFail := &pb.Match{
		Id:         "match-write-fail",
		Extensions: map[string]*anypb.Any{"custom_key": customVal},
	}
	ok = waitForMatchTicketDeactivation(ctx, testLogger, localTc, matchWriteFail, "mmf1", "prof1", "", "", true)
	assert.False(t, ok)
	assert.Equal(t, customVal, matchWriteFail.GetExtensions()["custom_key"])
	require.Contains(t, matchWriteFail.GetExtensions(), MatchDeactivationTimeoutExtensionKey)

	// 3. Timeout when annotateOnTimeout == false does not mutate extensions.
	matchTimeoutNoAnnotate := &pb.Match{
		Id:         "match-timeout-no-annotate",
		Extensions: map[string]*anypb.Any{"custom_key": customVal},
	}
	ok = waitForMatchTicketDeactivation(ctx, testLogger, localTc, matchTimeoutNoAnnotate, "mmf1", "prof1", "1700000000000-1", "1700000000000-99", false)
	assert.False(t, ok)
	assert.NotContains(t, matchTimeoutNoAnnotate.GetExtensions(), MatchDeactivationTimeoutExtensionKey)

	// 4. Replication succeeds in time when annotateOnTimeout == true does not mutate extensions.
	replicatedId := "1700000000000-5"
	localTc.InactiveSet.Store(replicatedId, true)
	matchSuccess := &pb.Match{
		Id:         "match-success",
		Extensions: map[string]*anypb.Any{"custom_key": customVal},
	}
	ok = waitForMatchTicketDeactivation(ctx, testLogger, localTc, matchSuccess, "mmf1", "prof1", replicatedId, "", true)
	assert.True(t, ok)
	assert.NotContains(t, matchSuccess.GetExtensions(), MatchDeactivationTimeoutExtensionKey)
}

// Scenario 6: Chunked Request Pool Non-Aliasing
func TestGetChunkedRequestsPoolNonAliasing(t *testing.T) {
	reqProfile := &pb.Profile{
		Name: "test.profile.v1",
		Pools: map[string]*pb.Pool{
			"pool_alpha": {
				Name:              "pool_alpha",
				TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "mode.ranked"}},
			},
		},
	}

	t1 := &pb.Ticket{Id: "1700000000000-1"}
	t2 := &pb.Ticket{Id: "1700000000000-2"}
	t3 := &pb.Ticket{Id: "1700000000000-3"}

	chunkedPools := []map[string][]*pb.Ticket{
		{"pool_alpha": {t1, t2}},
		{"pool_alpha": {t3}},
	}

	chunks := getChunkedRequests(reqProfile, chunkedPools)
	require.Len(t, chunks, 2)

	// Caller's profile pool must not be mutated.
	assert.Nil(t, reqProfile.GetPools()["pool_alpha"].GetParticipants())

	// Earlier chunk's pool must be a distinct clone not overwritten by later chunks.
	poolChunk0 := chunks[0].GetProfile().GetPools()["pool_alpha"]
	poolChunk1 := chunks[1].GetProfile().GetPools()["pool_alpha"]
	require.NotNil(t, poolChunk0)
	require.NotNil(t, poolChunk1)
	assert.NotSame(t, poolChunk0, poolChunk1)

	require.Len(t, poolChunk0.GetParticipants().GetTickets(), 2)
	assert.Equal(t, "1700000000000-1", poolChunk0.GetParticipants().GetTickets()[0].GetId())
	assert.Equal(t, "1700000000000-2", poolChunk0.GetParticipants().GetTickets()[1].GetId())

	require.Len(t, poolChunk1.GetParticipants().GetTickets(), 1)
	assert.Equal(t, "1700000000000-3", poolChunk1.GetParticipants().GetTickets()[0].GetId())
}

// Scenario 7: Chunked Request Empty-Pool Preservation
func TestGetChunkedRequestsEmptyPoolPreservation(t *testing.T) {
	reqProfile := &pb.Profile{
		Name: "test.profile.empty_pools",
		Pools: map[string]*pb.Pool{
			"matched_pool": {
				Name:              "matched_pool",
				TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "has-tickets"}},
			},
			"zero_ticket_valid_pool": {
				Name:              "zero_ticket_valid_pool",
				TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "no-tickets"}},
			},
			"invalid_filter_pool": {
				Name: "invalid_filter_pool",
				CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
					Start: &timestamppb.Timestamp{Seconds: 253402300800, Nanos: 0},
				},
			},
		},
	}

	t1 := &pb.Ticket{Id: "1700000000000-1"}
	chunkedPools := []map[string][]*pb.Ticket{
		{"matched_pool": {t1}},
		{"matched_pool": nil},
	}

	chunks := getChunkedRequests(reqProfile, chunkedPools)
	require.Len(t, chunks, 2)
	for idx, ch := range chunks {
		assert.Equal(t, int32(2), ch.GetNumChunks())
		pools := ch.GetProfile().GetPools()
		require.Contains(t, pools, "matched_pool", "chunk %d missing matched_pool", idx)
		require.Contains(t, pools, "zero_ticket_valid_pool", "chunk %d missing zero_ticket_valid_pool", idx)
		assert.NotContains(t, pools, "invalid_filter_pool", "chunk %d should exclude invalid_filter_pool", idx)

		emptyPool := pools["zero_ticket_valid_pool"]
		require.NotNil(t, emptyPool.GetParticipants())
		assert.Equal(t, "zero_ticket_valid_pool_roster", emptyPool.GetParticipants().GetName())
		assert.Empty(t, emptyPool.GetParticipants().GetTickets())
	}

	// Nil and empty chunkedPools slice handling.
	for _, emptyIn := range [][]map[string][]*pb.Ticket{nil, {}} {
		res := getChunkedRequests(reqProfile, emptyIn)
		require.Len(t, res, 1)
		assert.Equal(t, int32(1), res[0].GetNumChunks())
		assert.Contains(t, res[0].GetProfile().GetPools(), "matched_pool")
		assert.Contains(t, res[0].GetProfile().GetPools(), "zero_ticket_valid_pool")
		assert.NotContains(t, res[0].GetProfile().GetPools(), "invalid_filter_pool")
	}
}

func generateTestTLSConfig(t *testing.T) (serverCreds credentials.TransportCredentials, clientTLS *tls.Config) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(derBytes)
	require.NoError(t, err)

	rootPool := x509.NewCertPool()
	rootPool.AddCert(cert)

	tlsCert := tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}), &tls.Config{
		RootCAs:    rootPool,
		MinVersion: tls.VersionTLS12,
	}
}

type fakeTokenSource struct {
	audience string
	seq      *atomic.Int64
}

func (f *fakeTokenSource) Token() (*oauth2.Token, error) {
	n := f.seq.Add(1)
	return &oauth2.Token{
		AccessToken: fmt.Sprintf("tok-%s-%d", f.audience, n),
		Expiry:      time.Now().Add(1 * time.Hour),
	}, nil
}

// Scenario 8: Per-MMF HTTPS OIDC Auth and Refresh on Rejection
func TestInvokeMatchmakingFunctionsHTTPSOIDCAuthAndRefresh(t *testing.T) {
	setupGlobalTestCache(t, nil, nil)
	serverCreds, clientTLS := generateTestTLSConfig(t)

	origTLSConfig := tlsConfig
	origTokenCache := idTokenCache
	tlsConfig = clientTLS
	defer func() {
		tlsConfig = origTLSConfig
		idTokenCache = origTokenCache
	}()

	var mu sync.Mutex
	seqByAudience := map[string]*atomic.Int64{}
	idTokenCache = mmfauth.NewIDTokenCache(func(ctx context.Context, audience string) (oauth2.TokenSource, error) {
		mu.Lock()
		defer mu.Unlock()
		seq, ok := seqByAudience[audience]
		if !ok {
			seq = &atomic.Int64{}
			seqByAudience[audience] = seq
		}
		return &fakeTokenSource{audience: audience, seq: seq}, nil
	})

	ticketIds := createAndActivateTicketsInCache(t, &tc, 2, []string{"https-auth"}, 0)
	t1, t2 := ticketIds[0], ticketIds[1]

	var mmf1Calls atomic.Int32
	var mmf1SeenTokens []string
	var mmf1Mu sync.Mutex

	_, port1 := startTestMmfGrpcServer(t, serverCreds, func(stream pb.MatchMakingFunctionService_RunServer) error {
		md, _ := grpcMetadata.FromIncomingContext(stream.Context())
		authHeaders := md.Get("authorization")
		mmf1Mu.Lock()
		if len(authHeaders) > 0 {
			mmf1SeenTokens = append(mmf1SeenTokens, authHeaders[0])
		}
		mmf1Mu.Unlock()

		for {
			_, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}

		if mmf1Calls.Add(1) == 1 {
			return status.Error(codes.Unauthenticated, "stale token rejected")
		}
		return stream.Send(&pb.StreamedMmfResponse{
			Match: &pb.Match{
				Id:      "https-match-1",
				Rosters: map[string]*pb.Roster{"r1": {Name: "r1", Tickets: []*pb.Ticket{{Id: t1}}}},
			},
		})
	})

	var mmf2SeenToken string
	var mmf2Mu sync.Mutex
	_, port2 := startTestMmfGrpcServer(t, serverCreds, func(stream pb.MatchMakingFunctionService_RunServer) error {
		md, _ := grpcMetadata.FromIncomingContext(stream.Context())
		authHeaders := md.Get("authorization")
		mmf2Mu.Lock()
		if len(authHeaders) > 0 {
			mmf2SeenToken = authHeaders[0]
		}
		mmf2Mu.Unlock()

		for {
			_, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
		return stream.Send(&pb.StreamedMmfResponse{
			Match: &pb.Match{
				Id:      "https-match-2",
				Rosters: map[string]*pb.Roster{"r2": {Name: "r2", Tickets: []*pb.Ticket{{Id: t2}}}},
			},
		})
	})

	host1 := fmt.Sprintf("https://127.0.0.1:%d", port1)
	host2 := fmt.Sprintf("https://127.0.0.1:%d", port2)

	callerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
	srv := &grpcServer{}
	err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
		Profile: &pb.Profile{
			Name: "com.example.https.profile",
			Pools: map[string]*pb.Pool{
				"p1": {Name: "p1", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "https-auth"}}},
			},
		},
		Mmfs: []*pb.MatchmakingFunctionSpec{
			{Name: "https-mmf-1", Host: host1, Port: 0, Type: pb.MatchmakingFunctionSpec_GRPC},
			{Name: "https-mmf-2", Host: host2, Port: 0, Type: pb.MatchmakingFunctionSpec_GRPC},
		},
	}, callerStream)
	require.NoError(t, err)

	matches := callerStream.receivedMatches()
	require.Len(t, matches, 2)
	matchIds := []string{matches[0].GetId(), matches[1].GetId()}
	assert.ElementsMatch(t, []string{"https-match-1", "https-match-2"}, matchIds)

	// Verify per-MMF audience tokens and single retry after refresh on MMF 1.
	assert.Equal(t, int32(2), mmf1Calls.Load())
	mmf1Mu.Lock()
	assert.Equal(t, []string{
		fmt.Sprintf("Bearer tok-%s-1", host1),
		fmt.Sprintf("Bearer tok-%s-2", host1),
	}, mmf1SeenTokens)
	mmf1Mu.Unlock()

	mmf2Mu.Lock()
	assert.Equal(t, fmt.Sprintf("Bearer tok-%s-1", host2), mmf2SeenToken)
	mmf2Mu.Unlock()

	// Both tickets must be automatically deactivated.
	assert.True(t, tc.WaitForDeactivation(t1, "", 2*time.Second))
	assert.True(t, tc.WaitForDeactivation(t2, "", 2*time.Second))
	assert.Empty(t, tc.SnapshotActiveTickets())
}

// Scenario 9: Mid-Stream MMF Error Deactivation Wait and Metrics
func TestInvokeMatchmakingFunctionsMidStreamErrorDeactivationWaitAndMetrics(t *testing.T) {
	for _, waitEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("DeactivationWait=%v", waitEnabled), func(t *testing.T) {
			localCfg := newFastCoreTestConfig()
			localCfg.Set("OM_MATCH_TICKET_DEACTIVATION_WAIT", waitEnabled)
			setupGlobalTestCache(t, localCfg, nil)
			reader := setupTestMeter(t)

			ticketIds := createAndActivateTicketsInCache(t, &tc, 3, []string{"midstream"}, 0)
			t1, t2, t3 := ticketIds[0], ticketIds[1], ticketIds[2]

			host1, port1 := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
				for {
					_, err := stream.Recv()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						return err
					}
				}
				if err := stream.Send(&pb.StreamedMmfResponse{
					Match: &pb.Match{
						Id:      "pre-crash-1",
						Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: t1}}}},
					},
				}); err != nil {
					return err
				}
				if err := stream.Send(&pb.StreamedMmfResponse{
					Match: &pb.Match{
						Id:      "pre-crash-2",
						Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: t2}}}},
					},
				}); err != nil {
					return err
				}
				return status.Error(codes.Internal, "simulated mid-stream MMF crash")
			})

			host2, port2 := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
				for {
					_, err := stream.Recv()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						return err
					}
				}
				return stream.Send(&pb.StreamedMmfResponse{
					Match: &pb.Match{
						Id:      "sibling-match-3",
						Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: t3}}}},
					},
				})
			})

			callerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
			srv := &grpcServer{}
			err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
				Profile: &pb.Profile{
					Name: "com.example.midstream.run1",
					Pools: map[string]*pb.Pool{
						"pool": {Name: "pool", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "midstream"}}},
					},
				},
				Mmfs: []*pb.MatchmakingFunctionSpec{
					{Name: "crashing-mmf", Host: "http://" + host1, Port: port1, Type: pb.MatchmakingFunctionSpec_GRPC},
					{Name: "healthy-mmf", Host: "http://" + host2, Port: port2, Type: pb.MatchmakingFunctionSpec_GRPC},
				},
			}, callerStream)
			// The mid-stream MMF crash is reported after all matches have been streamed.
			assertMmfFailureStatus(t, err, 2, []string{"crashing-mmf"})
			assert.Contains(t, err.Error(), "simulated mid-stream MMF crash")

			// Verify all 3 matches were delivered to the caller.
			matches := callerStream.receivedMatches()
			require.Len(t, matches, 3)
			assert.ElementsMatch(t,
				[]string{"pre-crash-1", "pre-crash-2", "sibling-match-3"},
				[]string{matches[0].GetId(), matches[1].GetId(), matches[2].GetId()},
			)

			// Verify all 3 matched tickets were deactivated before return.
			for _, id := range ticketIds {
				assert.True(t, tc.WaitForDeactivation(id, "", 2*time.Second))
			}

			// Verify metrics: om_core.match.received, om_core.mmf.failures, om_core.mmf.ticket.deactivations.
			assert.Equal(t, int64(2), sumInt64CounterByAttr(t, reader, "om_core.match.received", "mmf.name", "crashing-mmf"))
			assert.Equal(t, int64(1), sumInt64CounterByAttr(t, reader, "om_core.match.received", "mmf.name", "healthy-mmf"))
			assert.Equal(t, int64(1), sumInt64CounterByAttr(t, reader, "om_core.mmf.failures", "mmf.name", "crashing-mmf"))
			assert.Equal(t, int64(0), sumInt64CounterByAttr(t, reader, "om_core.mmf.failures", "mmf.name", "healthy-mmf"))
			assert.Equal(t, int64(3), sumInt64Counter(t, reader, "om_core.mmf.ticket.deactivations"))
		})
	}
}

// Scenario 10: CloseSend, Multi-Chunk Empty Pools, and Send Error Unwrapping
func TestInvokeMatchmakingFunctionsCloseSendMultiChunkAndSendEOFUnwrap(t *testing.T) {
	setupGlobalTestCache(t, nil, nil)
	reader := setupTestMeter(t)

	// Create 5 large tickets (~950 KB each => ~4.75 MB total > 4 MB maxPbSize) for "large_pool"
	// and 1 small ticket for "subset_pool".
	createAndActivateTicketsInCache(t, &tc, 5, []string{"large_pool_tag"}, 950*1000)
	smallIds := createAndActivateTicketsInCache(t, &tc, 1, []string{"subset_pool_tag"}, 0)

	var receivedChunks []*pb.ChunkedMmfRunRequest
	var chunkMu sync.Mutex

	hostOk, portOk := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
		// Read until io.EOF to prove om-core calls CloseSend().
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			chunkMu.Lock()
			receivedChunks = append(receivedChunks, chunk)
			chunkMu.Unlock()
		}
		return stream.Send(&pb.StreamedMmfResponse{
			Match: &pb.Match{
				Id:      "multichunk-match",
				Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: smallIds[0]}}}},
			},
		})
	})

	// Second MMF immediately closes/aborts stream with a gRPC status error while client sends multi-chunk > 4MB data,
	// exercising the Send() io.EOF -> Recv() error unwrapping path and recording om_core.mmf.failures.
	hostFail, portFail := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
		return status.Error(codes.ResourceExhausted, "mmf capacity exceeded during send")
	})

	callerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
	srv := &grpcServer{}
	err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
		Profile: &pb.Profile{
			Name: "com.example.multichunk.v1",
			Pools: map[string]*pb.Pool{
				"large_pool": {
					Name:              "large_pool",
					TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "large_pool_tag"}},
				},
				"subset_pool": {
					Name:              "subset_pool",
					TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "subset_pool_tag"}},
				},
				"empty_pool": {
					Name:              "empty_pool",
					TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "never_matches"}},
				},
				"invalid_pool": {
					Name: "invalid_pool",
					CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
						Start: &timestamppb.Timestamp{Seconds: 253402300800, Nanos: 0},
					},
				},
			},
		},
		Mmfs: []*pb.MatchmakingFunctionSpec{
			{Name: "chunk-reader-mmf", Host: "http://" + hostOk, Port: portOk, Type: pb.MatchmakingFunctionSpec_GRPC},
			{Name: "early-abort-mmf", Host: "http://" + hostFail, Port: portFail, Type: pb.MatchmakingFunctionSpec_GRPC},
		},
	}, callerStream)
	assertMmfFailureStatus(t, err, 2, []string{"early-abort-mmf"})
	assert.Contains(t, err.Error(), "mmf capacity exceeded during send")

	chunkMu.Lock()
	chunksCopy := slices.Clone(receivedChunks)
	chunkMu.Unlock()

	require.GreaterOrEqual(t, len(chunksCopy), 2, "expected >4MB payload to split into at least 2 chunks")
	for idx, ch := range chunksCopy {
		assert.Equal(t, int32(len(chunksCopy)), ch.GetNumChunks())
		pools := ch.GetProfile().GetPools()
		assert.Contains(t, pools, "large_pool", "chunk %d missing large_pool", idx)
		assert.Contains(t, pools, "subset_pool", "chunk %d missing subset_pool", idx)
		assert.Contains(t, pools, "empty_pool", "chunk %d missing empty_pool", idx)
		assert.NotContains(t, pools, "invalid_pool", "chunk %d must exclude invalid_pool", idx)
		require.NotNil(t, pools["empty_pool"].GetParticipants())
		assert.Empty(t, pools["empty_pool"].GetParticipants().GetTickets())
	}

	assert.Len(t, callerStream.receivedMatches(), 1)
	assert.Equal(t, int64(1), sumInt64CounterByAttr(t, reader, "om_core.mmf.failures", "mmf.name", "early-abort-mmf"))
}

type failingAssignReplicator struct {
	validator *regexp.Regexp
}

func (f *failingAssignReplicator) GetUpdates() []*store.StateUpdate {
	return nil
}

func (f *failingAssignReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	res := make([]*store.StateResponse, len(updates))
	for i, u := range updates {
		res[i] = &store.StateResponse{
			Result: u.PrimaryKey(),
			Err:    errors.New("simulated assignment storage failure"),
		}
	}
	return res
}

func (f *failingAssignReplicator) GetReplIdValidator() *regexp.Regexp {
	return f.validator
}

// Scenario 11: CreateAssignments Validation, Replication, and Metrics
func TestCreateAssignmentsValidationReplicationAndMetrics(t *testing.T) {
	setupGlobalTestCache(t, nil, nil)
	reader := setupTestMeter(t)
	srv := &grpcServer{}
	ctx := context.Background()

	// 1. Validation: nil roster
	_, err := srv.CreateAssignments(ctx, &pb.CreateAssignmentsRequest{AssignmentRoster: nil})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "roster with assignment is required")

	// 2. Validation: nil assignment
	_, err = srv.CreateAssignments(ctx, &pb.CreateAssignmentsRequest{
		AssignmentRoster: &pb.Roster{Name: "r", Assignment: nil},
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "roster with assignment is required")

	// 3. Validation: empty zero-byte assignment (&pb.Assignment{})
	_, err = srv.CreateAssignments(ctx, &pb.CreateAssignmentsRequest{
		AssignmentRoster: &pb.Roster{Name: "r", Assignment: &pb.Assignment{}},
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "roster with non-empty assignment is required")

	// 4. Validation: exceeding OM_MAX_STATE_UPDATES_PER_CALL
	manyTickets := make([]*pb.Ticket, cfg.GetInt("OM_MAX_STATE_UPDATES_PER_CALL")+1)
	for i := range manyTickets {
		manyTickets[i] = &pb.Ticket{Id: validTicketIds[0]}
	}
	_, err = srv.CreateAssignments(ctx, &pb.CreateAssignmentsRequest{
		AssignmentRoster: &pb.Roster{
			Name:       "r",
			Assignment: &pb.Assignment{Connection: "10.0.0.1:7777"},
			Tickets:    manyTickets,
		},
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), TooManyUpdatesErr.Error())

	// 5. Valid replication and om_core.ticket.assignment metric recording
	t1, t2 := "1700000000000-101", "1700000000000-102"
	_, err = srv.CreateAssignments(ctx, &pb.CreateAssignmentsRequest{
		AssignmentRoster: &pb.Roster{
			Name:       "r",
			Assignment: &pb.Assignment{Connection: "10.0.0.1:7777"},
			Tickets:    []*pb.Ticket{{Id: t1}, {Id: t2}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), sumInt64Counter(t, reader, "om_core.ticket.assignment"))
	require.Eventually(t, func() bool {
		_, ok1 := tc.Assignments.Load(t1)
		_, ok2 := tc.Assignments.Load(t2)
		return ok1 && ok2
	}, 2*time.Second, 5*time.Millisecond)

	// 6. Empty ticket ID in roster returns error and increments om_core.ticket.assignment.failures
	_, err = srv.CreateAssignments(ctx, &pb.CreateAssignmentsRequest{
		AssignmentRoster: &pb.Roster{
			Name:       "r",
			Assignment: &pb.Assignment{Connection: "10.0.0.1:7777"},
			Tickets:    []*pb.Ticket{{Id: ""}, {Id: "1700000000000-103"}},
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "Empty ticket ID in assignment request!")
	assert.Equal(t, int64(1), sumInt64Counter(t, reader, "om_core.ticket.assignment.failures"))

	// 7. "Unable to assign ticket" error propagation on replication failure
	failRepl := &failingAssignReplicator{validator: regexp.MustCompile(`^\d{13}-\d+$`)}
	setupGlobalTestCache(t, nil, failRepl)
	_, err = srv.CreateAssignments(ctx, &pb.CreateAssignmentsRequest{
		AssignmentRoster: &pb.Roster{
			Name:       "r",
			Assignment: &pb.Assignment{Connection: "10.0.0.1:7777"},
			Tickets:    []*pb.Ticket{{Id: "1700000000000-104"}},
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "Unable to assign ticket")
	assert.Equal(t, int64(2), sumInt64Counter(t, reader, "om_core.ticket.assignment.failures"))
}

// Scenario 12: WatchAssignments Delivery, Timeout, Cancellation, and Gauge Balancing
func TestWatchAssignmentsDeliveryTimeoutCancellationAndGauge(t *testing.T) {
	srv := &grpcServer{}

	t.Run("FullDeliveryAndGaugeBalancedToZero", func(t *testing.T) {
		setupGlobalTestCache(t, nil, nil)
		reader := setupTestMeter(t)

		nowMs := time.Now().UnixMilli()
		id1 := fmt.Sprintf("%013d-1", nowMs)
		id2 := fmt.Sprintf("%013d-2", nowMs)
		tc.Assignments.Store(id1, &pb.Assignment{Connection: "gs-1:7000"})
		tc.Assignments.Store(id2, &pb.Assignment{Connection: "gs-2:7000"})

		ws := &mockWatchAssignmentsServerStream{ctx: context.Background()}
		err := srv.WatchAssignments(&pb.WatchAssignmentsRequest{TicketIds: []string{id1, id2}}, ws)
		require.NoError(t, err)
		resps := ws.receivedResponses()
		require.Len(t, resps, 2)
		assert.ElementsMatch(t, []string{id1, id2}, []string{resps[0].GetId(), resps[1].GetId()})
		assert.Equal(t, int64(0), sumInt64Counter(t, reader, "om_core.ticket.assignment.watch"))
	})

	t.Run("PartialDeliveryWithTTLTimeoutBalancesGaugeAndTerminatesCleanly", func(t *testing.T) {
		shortCfg := newFastCoreTestConfig()
		shortCfg.Set("OM_CACHE_TICKET_TTL_MS", 120)
		setupGlobalTestCache(t, shortCfg, nil)
		reader := setupTestMeter(t)

		nowMs := time.Now().UnixMilli()
		id1 := fmt.Sprintf("%013d-1", nowMs)
		id2 := fmt.Sprintf("%013d-2", nowMs)
		id3 := fmt.Sprintf("%013d-3", nowMs)

		// Only id1 gets an assignment before TTL timeout.
		tc.Assignments.Store(id1, &pb.Assignment{Connection: "gs-partial:7000"})

		ws := &mockWatchAssignmentsServerStream{ctx: context.Background()}
		err := srv.WatchAssignments(&pb.WatchAssignmentsRequest{TicketIds: []string{id1, id2, id3}}, ws)
		require.NoError(t, err)

		resps := ws.receivedResponses()
		require.Len(t, resps, 1)
		assert.Equal(t, id1, resps[0].GetId())

		// Watch gauge must balance back to 0 (decremented by remaining 2 watches on timeout, never incremented).
		assert.Equal(t, int64(0), sumInt64Counter(t, reader, "om_core.ticket.assignment.watch"))

		// Storing late assignments after WatchAssignments returned must not panic on closed updateChan.
		tc.Assignments.Store(id2, &pb.Assignment{Connection: "gs-late:7000"})
		tc.Assignments.Store(id3, &pb.Assignment{Connection: "gs-late:7000"})
	})

	t.Run("ImmediateUnblockingOnCallerStreamCancellation", func(t *testing.T) {
		longCfg := newFastCoreTestConfig()
		longCfg.Set("OM_CACHE_TICKET_TTL_MS", 60000)
		setupGlobalTestCache(t, longCfg, nil)
		reader := setupTestMeter(t)

		nowMs := time.Now().UnixMilli()
		id1 := fmt.Sprintf("%013d-1", nowMs)
		id2 := fmt.Sprintf("%013d-2", nowMs)

		streamCtx, streamCancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(40 * time.Millisecond)
			streamCancel()
		}()

		startTime := time.Now()
		ws := &mockWatchAssignmentsServerStream{ctx: streamCtx}
		err := srv.WatchAssignments(&pb.WatchAssignmentsRequest{TicketIds: []string{id1, id2}}, ws)
		require.NoError(t, err)
		assert.Less(t, time.Since(startTime), 500*time.Millisecond, "WatchAssignments should unblock immediately on stream context cancellation")
		assert.Equal(t, int64(0), sumInt64Counter(t, reader, "om_core.ticket.assignment.watch"))
	})
}

// Scenario 13: Deactivation Wait Ordering and Semantics
func TestInvokeMatchmakingFunctionsDeactivationWaitOrdering(t *testing.T) {
	srv := &grpcServer{}

	t.Run("WaitTrueReplicatesDeactivationBeforeStreamingMatch", func(t *testing.T) {
		waitTrueCfg := newFastCoreTestConfig()
		waitTrueCfg.Set("OM_MATCH_TICKET_DEACTIVATION_WAIT", true)
		setupGlobalTestCache(t, waitTrueCfg, nil)

		ids := createAndActivateTicketsInCache(t, &tc, 1, []string{"wait-order"}, 0)
		tid := ids[0]

		host, port := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
			for {
				if _, err := stream.Recv(); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					return err
				}
			}
			return stream.Send(&pb.StreamedMmfResponse{
				Match: &pb.Match{
					Id:      "wait-true-match",
					Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: tid}}}},
				},
			})
		})

		var inactiveWhenSent bool
		callerStream := &mockInvokeMmfServerStream{
			ctx: context.Background(),
			sendHook: func(resp *pb.StreamedMmfResponse) error {
				_, inactiveWhenSent = tc.InactiveSet.Load(tid)
				return nil
			},
		}

		err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
			Profile: &pb.Profile{
				Name:  "com.example.order.waittrue",
				Pools: map[string]*pb.Pool{"p": {Name: "p", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "wait-order"}}}},
			},
			Mmfs: []*pb.MatchmakingFunctionSpec{{Name: "mmf", Host: "http://" + host, Port: port, Type: pb.MatchmakingFunctionSpec_GRPC}},
		}, callerStream)
		require.NoError(t, err)
		assert.True(t, inactiveWhenSent, "with OM_MATCH_TICKET_DEACTIVATION_WAIT=true, ticket must be in InactiveSet before stream.Send")
	})

	t.Run("WaitFalseStreamsBeforeDeactivationWaitAndCompletesBeforeReturn", func(t *testing.T) {
		waitFalseCfg := newFastCoreTestConfig()
		waitFalseCfg.Set("OM_MATCH_TICKET_DEACTIVATION_WAIT", false)
		// Slow outgoing batch timeout so stream.Send happens before outgoing batch flushes, while still completing fast.
		waitFalseCfg.Set("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 60)
		setupGlobalTestCache(t, waitFalseCfg, nil)

		ids := createAndActivateTicketsInCache(t, &tc, 1, []string{"wait-order"}, 0)
		tid := ids[0]

		host, port := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
			for {
				if _, err := stream.Recv(); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					return err
				}
			}
			return stream.Send(&pb.StreamedMmfResponse{
				Match: &pb.Match{
					Id:      "wait-false-match",
					Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: tid}}}},
				},
			})
		})

		var activeWhenSent bool
		callerStream := &mockInvokeMmfServerStream{
			ctx: context.Background(),
			sendHook: func(resp *pb.StreamedMmfResponse) error {
				_, isInactive := tc.InactiveSet.Load(tid)
				activeWhenSent = !isInactive
				return nil
			},
		}

		err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
			Profile: &pb.Profile{
				Name:  "com.example.order.waitfalse",
				Pools: map[string]*pb.Pool{"p": {Name: "p", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "wait-order"}}}},
			},
			Mmfs: []*pb.MatchmakingFunctionSpec{{Name: "mmf", Host: "http://" + host, Port: port, Type: pb.MatchmakingFunctionSpec_GRPC}},
		}, callerStream)
		require.NoError(t, err)
		assert.True(t, activeWhenSent, "with OM_MATCH_TICKET_DEACTIVATION_WAIT=false, match is streamed before deactivation replicates")
		_, inactiveAfterReturn := tc.InactiveSet.Load(tid)
		assert.True(t, inactiveAfterReturn, "deactivation must still complete before InvokeMatchmakingFunctions returns")
	})

	t.Run("WaitFalseSkipsDeactivationWhenCallerStreamAlreadyCancelled", func(t *testing.T) {
		waitFalseCfg := newFastCoreTestConfig()
		waitFalseCfg.Set("OM_MATCH_TICKET_DEACTIVATION_WAIT", false)
		setupGlobalTestCache(t, waitFalseCfg, nil)

		ids := createAndActivateTicketsInCache(t, &tc, 1, []string{"wait-order"}, 0)
		tid := ids[0]

		streamCtx, streamCancel := context.WithCancel(context.Background())

		host, port := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
			for {
				if _, err := stream.Recv(); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					return err
				}
			}
			// Cancel caller stream before MMF sends match.
			streamCancel()
			return stream.Send(&pb.StreamedMmfResponse{
				Match: &pb.Match{
					Id:      "cancelled-match",
					Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: tid}}}},
				},
			})
		})

		callerStream := &mockInvokeMmfServerStream{ctx: streamCtx}
		err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
			Profile: &pb.Profile{
				Name:  "com.example.order.cancelled",
				Pools: map[string]*pb.Pool{"p": {Name: "p", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "wait-order"}}}},
			},
			Mmfs: []*pb.MatchmakingFunctionSpec{{Name: "mmf", Host: "http://" + host, Port: port, Type: pb.MatchmakingFunctionSpec_GRPC}},
		}, callerStream)
		require.NoError(t, err)
		assert.Empty(t, callerStream.receivedMatches())
		_, isInactive := tc.InactiveSet.Load(tid)
		assert.False(t, isInactive, "ticket should not be deactivated when caller stream was cancelled before send")
	})
}

// Scenario 14: CreateTicket Status Preservation, Size Metric Gating, and Creation-Relative TTL
func TestCreateTicketStatusPreservationMetricGatingAndRelativeTTL(t *testing.T) {
	localTc := startLocalTestCache(t, nil, nil)
	reader := setupTestMeter(t)
	ctx := context.Background()

	// 1. Invalid expiration time preserves codes.InvalidArgument and does not record om_core.ticket.size.
	_, err := createTicket(ctx, localTc, &pb.CreateTicketRequest{
		Ticket: &pb.Ticket{
			ExpirationTime: timestamppb.New(time.Now().Add(-10 * time.Second)),
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, uint64(0), float64HistogramCount(t, reader, "om_core.ticket.size"))
	assert.Equal(t, int64(0), sumInt64Counter(t, reader, "om_core.ticket.creations"))

	// 2. Failed replication does not record om_core.ticket.size or om_core.ticket.creations.
	failTc := cache.New(newFastCoreTestConfig(), nil)
	go func() {
		req := <-failTc.UpRequests
		req.ResultsChan <- &store.StateResponse{Result: "", Err: errors.New("storage failure")}
	}()
	_, err = createTicket(ctx, failTc, &pb.CreateTicketRequest{
		Ticket: &pb.Ticket{Attributes: &pb.Ticket_FilterableData{Tags: []string{"t"}}},
	})
	require.Error(t, err)
	assert.Equal(t, uint64(0), float64HistogramCount(t, reader, "om_core.ticket.size"))
	assert.Equal(t, int64(0), sumInt64Counter(t, reader, "om_core.ticket.creations"))

	// 3. Successful replication records om_core.ticket.size and om_core.ticket.creations.
	resp, err := createTicket(ctx, localTc, &pb.CreateTicketRequest{
		Ticket: &pb.Ticket{Attributes: &pb.Ticket_FilterableData{Tags: []string{"valid"}}},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetTicketId())
	assert.Equal(t, uint64(1), float64HistogramCount(t, reader, "om_core.ticket.size"))
	assert.Equal(t, int64(1), sumInt64Counter(t, reader, "om_core.ticket.creations"))

	// 4. Default ExpirationTime is computed as CreationTime + OM_CACHE_TICKET_TTL_MS.
	customCreationTime := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Millisecond)
	raw, err := validateAndMarshalIncomingTicket(&pb.CreateTicketRequest{
		Ticket: &pb.Ticket{
			Attributes: &pb.Ticket_FilterableData{
				CreationTime: timestamppb.New(customCreationTime),
			},
		},
	})
	require.NoError(t, err)
	var unmarshaled pb.Ticket
	require.NoError(t, proto.Unmarshal(raw, &unmarshaled))
	expectedExp := customCreationTime.Add(time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS")) * time.Millisecond)
	assert.Equal(t, expectedExp, unmarshaled.GetExpirationTime().AsTime())
}

// Scenario 15: RPC Error Metric Cardinality, Non-Double-Counted Invalid IDs, and Watch Metric Spelling
func TestMetricsCardinalityInvalidIdCountingAndWatchSpelling(t *testing.T) {
	localTc := startLocalTestCache(t, nil, nil)
	reader := setupTestMeter(t)
	ctx := context.Background()

	// 1. om_core.rpc.errors omits unbounded "error" attribute.
	h := New()
	require.NotNil(t, h)
	connCtx := h.TagConn(ctx, &stats.ConnTagInfo{})
	h.HandleConn(connCtx, &stats.ConnBegin{})
	rpcCtx := h.TagRPC(connCtx, &stats.RPCTagInfo{FullMethodName: "/openmatch2.OpenMatchService/CreateTicket"})
	h.HandleRPC(rpcCtx, &stats.End{
		BeginTime: time.Now().Add(-5 * time.Millisecond),
		EndTime:   time.Now(),
		Error:     status.Error(codes.InvalidArgument, "dynamic high-cardinality error details 98765"),
	})

	rm := collectMetrics(t, reader)
	rpcErrMetric, ok := findMetricByName(rm, "om_core.rpc.errors")
	require.True(t, ok)
	rpcErrSum, ok := rpcErrMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, rpcErrSum.DataPoints, 1)
	dpAttrs := rpcErrSum.DataPoints[0].Attributes
	rpcVal, hasRpc := dpAttrs.Value("rpc")
	assert.True(t, hasRpc)
	assert.Equal(t, "CreateTicket", rpcVal.AsString())
	statusVal, hasStatus := dpAttrs.Value("grpc.status")
	assert.True(t, hasStatus)
	assert.Equal(t, "InvalidArgument", statusVal.AsString())
	_, hasErrorAttr := dpAttrs.Value("error")
	assert.False(t, hasErrorAttr, "om_core.rpc.errors must not include unbounded 'error' attribute")

	// 2. ActivateTickets and DeactivateTickets do not double-count invalid IDs in unspecified failure histograms.
	mixedIds := []string{validTicketIds[0], "bad-id-1", "bad-id-2"}
	_, _ = activateTickets(ctx, testLogger, localTc, &pb.ActivateTicketsRequest{TicketIds: mixedIds})
	_, _ = deactivateTickets(ctx, testLogger, localTc, &pb.DeactivateTicketsRequest{TicketIds: mixedIds})

	actInvCount, actInvSum := int64HistogramStats(t, reader, "om_core.ticket.activation.failures.invalid_id")
	actFailCount, actFailSum := int64HistogramStats(t, reader, "om_core.ticket.activation.failures.unspecified")
	assert.Equal(t, uint64(1), actInvCount)
	assert.Equal(t, int64(2), actInvSum)
	assert.Equal(t, uint64(1), actFailCount)
	assert.Equal(t, int64(0), actFailSum, "invalid IDs must not be double-counted in activation.failures.unspecified")

	deactInvCount, deactInvSum := int64HistogramStats(t, reader, "om_core.ticket.deactivation.failures.invalid_id")
	deactFailCount, deactFailSum := int64HistogramStats(t, reader, "om_core.ticket.deactivation.failures.unspecified")
	assert.Equal(t, uint64(1), deactInvCount)
	assert.Equal(t, int64(2), deactInvSum)
	assert.Equal(t, uint64(1), deactFailCount)
	assert.Equal(t, int64(0), deactFailSum, "invalid IDs must not be double-counted in deactivation.failures.unspecified")

	// 3. om_core.ticket.assignment.watch registered with corrected name and description.
	otelAssignmentWatches.Add(ctx, 0)
	rmAfter := collectMetrics(t, reader)
	watchMetric, foundWatch := findMetricByName(rmAfter, "om_core.ticket.assignment.watch")
	require.True(t, foundWatch)
	assert.Equal(t, "Number of tickets currently being watched for assignment", watchMetric.Description)
	_, foundMisspelled := findMetricByName(rmAfter, "om_core.ticket.assignemnt.watch")
	assert.False(t, foundMisspelled)
}

// Scenario 16: Context Cancellation and Graceful Server Shutdown
func TestContextCancellationAndGracefulServerShutdown(t *testing.T) {
	t.Run("BlockedEnqueueRespectsContextCancellation", func(t *testing.T) {
		// Unbuffered UpRequests with no reader blocks enqueuing until ctx is cancelled.
		unbufferedTc := &cache.ReplicatedTicketCache{
			Cfg:        newFastCoreTestConfig(),
			UpRequests: make(chan *cache.UpdateRequest),
		}

		ctx1, cancel1 := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel1()
		_, err := createTicket(ctx1, unbufferedTc, &pb.CreateTicketRequest{
			Ticket: &pb.Ticket{Attributes: &pb.Ticket_FilterableData{Tags: []string{"t"}}},
		})
		assert.Equal(t, codes.DeadlineExceeded, status.Code(err))

		unbufferedTc.Cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel2()
		errsPacked, _ := updateTicketsActiveState(ctx2, testLogger, unbufferedTc, []string{"1700000000000-1", "1700000000000-2"}, store.Activate)
		require.Len(t, errsPacked, 2)
		for _, e := range errsPacked {
			assert.Equal(t, codes.DeadlineExceeded, status.Code(e))
		}

		unbufferedTc.Cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", false)
		ctx3, cancel3 := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel3()
		errsUnpacked, _ := updateTicketsActiveState(ctx3, testLogger, unbufferedTc, []string{"1700000000000-1", "1700000000000-2"}, store.Deactivate)
		require.Len(t, errsUnpacked, 2)
		for _, e := range errsUnpacked {
			assert.Equal(t, codes.DeadlineExceeded, status.Code(e))
		}
	})

	t.Run("BlockedResponseWaitRespectsContextCancellation", func(t *testing.T) {
		// Buffered UpRequests accepts enqueue, but nobody writes to ResultsChan.
		bufferedTc := &cache.ReplicatedTicketCache{
			Cfg:        newFastCoreTestConfig(),
			UpRequests: make(chan *cache.UpdateRequest, 16),
		}

		ctx1, cancel1 := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel1()
		_, err := createTicket(ctx1, bufferedTc, &pb.CreateTicketRequest{
			Ticket: &pb.Ticket{Attributes: &pb.Ticket_FilterableData{Tags: []string{"t"}}},
		})
		assert.Equal(t, codes.DeadlineExceeded, status.Code(err))

		bufferedTc.Cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", true)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel2()
		errsPacked, _ := updateTicketsActiveState(ctx2, testLogger, bufferedTc, []string{"1700000000000-1", "1700000000000-2"}, store.Activate)
		require.Len(t, errsPacked, 2)
		for _, e := range errsPacked {
			assert.Equal(t, codes.DeadlineExceeded, status.Code(e))
		}

		bufferedTc.Cfg.Set("OM_CACHE_PACK_TICKET_STATE_UPDATES", false)
		ctx3, cancel3 := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel3()
		errsUnpacked, _ := updateTicketsActiveState(ctx3, testLogger, bufferedTc, []string{"1700000000000-1", "1700000000000-2"}, store.Deactivate)
		require.Len(t, errsUnpacked, 2)
		for _, e := range errsUnpacked {
			assert.Equal(t, codes.DeadlineExceeded, status.Code(e))
		}
	})

	t.Run("GracefulServerStartupAndShutdownWithin2Seconds", func(t *testing.T) {
		lGrpc, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		grpcPort := lGrpc.Addr().(*net.TCPAddr).Port
		require.NoError(t, lGrpc.Close())

		lHttp, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		httpPort := lHttp.Addr().(*net.TCPAddr).Port
		require.NoError(t, lHttp.Close())

		srvCfg := newFastCoreTestConfig()
		srvCfg.Set("OM_GRPC_PORT", strconv.Itoa(grpcPort))
		srvCfg.Set("PORT", strconv.Itoa(httpPort))

		srvCtx, srvCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			start(srvCtx, srvCfg)
			close(done)
		}()

		// Wait until both gRPC and HTTP gateway ports accept connections.
		require.Eventually(t, func() bool {
			c1, err1 := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", grpcPort), 50*time.Millisecond)
			if err1 != nil {
				return false
			}
			_ = c1.Close()
			c2, err2 := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort), 50*time.Millisecond)
			if err2 != nil {
				return false
			}
			_ = c2.Close()
			return true
		}, 3*time.Second, 15*time.Millisecond)

		shutdownStart := time.Now()
		srvCancel()

		select {
		case <-done:
			assert.Less(t, time.Since(shutdownStart), 2*time.Second)
		case <-time.After(2 * time.Second):
			t.Fatal("start() did not shut down gRPC and HTTP servers within 2 seconds")
		}
	})
}

// Scenario 17: Server Panic Recovery Interceptors
func TestServerPanicRecoveryInterceptors(t *testing.T) {
	// 1. Unary interceptor normal pass-through
	expectedResp := &pb.CreateTicketResponse{TicketId: "1700000000000-1"}
	resp, err := unaryPanicRecoveryInterceptor(
		context.Background(),
		&pb.CreateTicketRequest{},
		&grpc.UnaryServerInfo{FullMethod: "/openmatch2.OpenMatchService/CreateTicket"},
		func(ctx context.Context, req any) (any, error) {
			return expectedResp, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, expectedResp, resp)

	// 2. Unary interceptor panic recovery
	resp, err = unaryPanicRecoveryInterceptor(
		context.Background(),
		&pb.CreateTicketRequest{},
		&grpc.UnaryServerInfo{FullMethod: "/openmatch2.OpenMatchService/CreateTicket"},
		func(ctx context.Context, req any) (any, error) {
			panic("simulated unary handler panic")
		},
	)
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Equal(t, "internal server error", status.Convert(err).Message())

	// 3. Stream interceptor normal pass-through
	err = streamPanicRecoveryInterceptor(
		&grpcServer{},
		&mockInvokeMmfServerStream{ctx: context.Background()},
		&grpc.StreamServerInfo{FullMethod: "/openmatch2.OpenMatchService/InvokeMatchmakingFunctions"},
		func(srv any, ss grpc.ServerStream) error {
			return nil
		},
	)
	require.NoError(t, err)

	// 4. Stream interceptor panic recovery
	err = streamPanicRecoveryInterceptor(
		&grpcServer{},
		&mockInvokeMmfServerStream{ctx: context.Background()},
		&grpc.StreamServerInfo{FullMethod: "/openmatch2.OpenMatchService/InvokeMatchmakingFunctions"},
		func(srv any, ss grpc.ServerStream) error {
			panic("simulated stream handler panic")
		},
	)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Equal(t, "internal server error", status.Convert(err).Message())
}

// Scenario 18: Match Deactivation Timeout Extension Helper
func TestAnnotateMatchDeactivationTimeoutHelper(t *testing.T) {
	// 1. Nil match is a safe no-op.
	assert.NotPanics(t, func() {
		annotateMatchDeactivationTimeout(nil)
	})

	// 2. Match with nil Extensions map.
	mNilExt := &pb.Match{Id: "match-nil-ext", Extensions: nil}
	annotateMatchDeactivationTimeout(mNilExt)
	require.NotNil(t, mNilExt.GetExtensions())
	require.Contains(t, mNilExt.GetExtensions(), MatchDeactivationTimeoutExtensionKey)
	var bVal1 wrapperspb.BoolValue
	require.NoError(t, mNilExt.GetExtensions()[MatchDeactivationTimeoutExtensionKey].UnmarshalTo(&bVal1))
	assert.True(t, bVal1.GetValue())

	// 3. Match with pre-existing Extensions map preserves existing entries.
	existingAny, err := anypb.New(wrapperspb.String("keep-existing"))
	require.NoError(t, err)
	mExistingExt := &pb.Match{
		Id:         "match-existing-ext",
		Extensions: map[string]*anypb.Any{"existing_key": existingAny},
	}
	annotateMatchDeactivationTimeout(mExistingExt)
	assert.Equal(t, existingAny, mExistingExt.GetExtensions()["existing_key"])
	require.Contains(t, mExistingExt.GetExtensions(), MatchDeactivationTimeoutExtensionKey)
	var bVal2 wrapperspb.BoolValue
	require.NoError(t, mExistingExt.GetExtensions()[MatchDeactivationTimeoutExtensionKey].UnmarshalTo(&bVal2))
	assert.True(t, bVal2.GetValue())
}

// Scenario 19: InvokeMMFs Validation, 45-Ticket Multi-Chunk Splitting, Fan-Out Errors, and Fan-In Drain
func TestInvokeMatchmakingFunctionsValidationMultiChunkFanOutAndDrain(t *testing.T) {
	setupGlobalTestCache(t, nil, nil)
	reader := setupTestMeter(t)
	srv := &grpcServer{}

	// 1. Nil Profile & Nil MMFs validation
	err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{Profile: nil}, &mockInvokeMmfServerStream{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "profile is required")

	err = srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
		Profile: &pb.Profile{Name: "p"},
		Mmfs:    nil,
	}, &mockInvokeMmfServerStream{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "list of mmfs to invoke is required")

	// 2. 45-ticket multi-chunk splitting (> 4 MB) and reassembly, plus REST & bad-URI fan-out errors,
	// plus returning and counting matches with zero or only invalid ticket IDs.
	ticketIds := createAndActivateTicketsInCache(t, &tc, 45, []string{"split45"}, 100*1000)

	var reassembledIds []string
	var numChunksSeen int32
	var reassembleMu sync.Mutex

	host, port := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
		for {
			chunk, recvErr := stream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				return recvErr
			}
			reassembleMu.Lock()
			numChunksSeen = chunk.GetNumChunks()
			for _, tk := range chunk.GetProfile().GetPools()["pool45"].GetParticipants().GetTickets() {
				reassembledIds = append(reassembledIds, tk.GetId())
			}
			reassembleMu.Unlock()
		}

		// Stream a match with 0 tickets, a match with only invalid ticket IDs, and 3 valid matches.
		_ = stream.Send(&pb.StreamedMmfResponse{
			Match: &pb.Match{Id: "empty-match", Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: nil}}},
		})
		_ = stream.Send(&pb.StreamedMmfResponse{
			Match: &pb.Match{Id: "invalid-ids-match", Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: ""}, {Id: "not-valid-id"}}}}},
		})
		for idx := 0; idx < 3; idx++ {
			if sendErr := stream.Send(&pb.StreamedMmfResponse{
				Match: &pb.Match{
					Id:      fmt.Sprintf("valid-match-%d", idx),
					Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: ticketIds[idx]}}}},
				},
			}); sendErr != nil {
				return sendErr
			}
		}
		return nil
	})

	// 3. Fan-in drain when caller stream.Send fails on the first valid match.
	var sendAttempts atomic.Int32
	failingCallerStream := &mockInvokeMmfServerStream{
		ctx: context.Background(),
		sendHook: func(resp *pb.StreamedMmfResponse) error {
			if sendAttempts.Add(1) == 1 {
				return errors.New("caller disconnected mid-stream")
			}
			return nil
		},
	}

	err = srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
		Profile: &pb.Profile{
			Name: "com.example.split45.v1",
			Pools: map[string]*pb.Pool{
				"pool45": {Name: "pool45", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "split45"}}},
			},
		},
		Mmfs: []*pb.MatchmakingFunctionSpec{
			{Name: "valid-grpc-mmf", Host: "http://" + host, Port: port, Type: pb.MatchmakingFunctionSpec_GRPC},
			{Name: "rest-nyi-mmf", Host: "http://127.0.0.1", Port: 8080, Type: pb.MatchmakingFunctionSpec_REST},
			{Name: "bad-uri-mmf", Host: "ftp://invalid-scheme", Port: 21, Type: pb.MatchmakingFunctionSpec_GRPC},
		},
	}, failingCallerStream)
	assertMmfFailureStatus(t, err, 3, []string{"rest-nyi-mmf", "bad-uri-mmf"})

	reassembleMu.Lock()
	assert.Greater(t, numChunksSeen, int32(1), "45 tickets at 100KB each must split across multiple chunks")
	assert.ElementsMatch(t, ticketIds, reassembledIds)
	reassembleMu.Unlock()

	assert.Equal(t, int64(1), sumInt64CounterByAttr(t, reader, "om_core.mmf.failures", "mmf.name", "rest-nyi-mmf"))
	assert.Equal(t, int64(1), sumInt64CounterByAttr(t, reader, "om_core.mmf.failures", "mmf.name", "bad-uri-mmf"))
	// Both the empty match and the invalid-IDs-only match are now returned and counted with match.has_tickets=false.
	assert.Equal(t, int64(3), sumMatchesReceived(t, reader, "valid-grpc-mmf", true))
	assert.Equal(t, int64(2), sumMatchesReceived(t, reader, "valid-grpc-mmf", false))
}

// Scenario 20: Exhaustive 23-Case MMF URL Parser Table and Scheme-Less Invocation
func TestParseMmfUrlTableAndSchemelessInvocation(t *testing.T) {
	type urlTestCase struct {
		name       string
		spec       *pb.MatchmakingFunctionSpec
		wantScheme string
		wantHost   string
		wantErrSub string
	}

	cases := []urlTestCase{
		// 12 Valid URL forms
		{
			name:       "case_01_http_localhost_with_port",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://localhost", Port: 50090},
			wantScheme: "http",
			wantHost:   "localhost:50090",
		},
		{
			name:       "case_02_https_cloud_run_with_443",
			spec:       &pb.MatchmakingFunctionSpec{Host: "https://mmf-service-xyz.a.run.app", Port: 443},
			wantScheme: "https",
			wantHost:   "mmf-service-xyz.a.run.app:443",
		},
		{
			name:       "case_03_https_cloud_run_default_port_0",
			spec:       &pb.MatchmakingFunctionSpec{Host: "https://mmf-service-xyz.a.run.app", Port: 0},
			wantScheme: "https",
			wantHost:   "mmf-service-xyz.a.run.app:443",
		},
		{
			name:       "case_04_http_k8s_svc_default_port_0",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://mmf-service.default.svc", Port: 0},
			wantScheme: "http",
			wantHost:   "mmf-service.default.svc:80",
		},
		{
			name:       "case_05_schemeless_ipv4_with_port",
			spec:       &pb.MatchmakingFunctionSpec{Host: "10.24.1.15", Port: 50090},
			wantScheme: "http",
			wantHost:   "10.24.1.15:50090",
		},
		{
			name:       "case_06_schemeless_k8s_fqdn_with_port",
			spec:       &pb.MatchmakingFunctionSpec{Host: "mmf.default.svc.cluster.local", Port: 50090},
			wantScheme: "http",
			wantHost:   "mmf.default.svc.cluster.local:50090",
		},
		{
			name:       "case_07_schemeless_k8s_fqdn_embedded_port",
			spec:       &pb.MatchmakingFunctionSpec{Host: "mmf.default.svc.cluster.local:50090", Port: 0},
			wantScheme: "http",
			wantHost:   "mmf.default.svc.cluster.local:50090",
		},
		{
			name:       "case_08_spec_port_overrides_embedded_port",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://127.0.0.1:50090", Port: 50091},
			wantScheme: "http",
			wantHost:   "127.0.0.1:50091",
		},
		{
			name:       "case_09_unbracketed_ipv6_loopback",
			spec:       &pb.MatchmakingFunctionSpec{Host: "::1", Port: 50090},
			wantScheme: "http",
			wantHost:   "[::1]:50090",
		},
		{
			name:       "case_10_bracketed_ipv6_loopback",
			spec:       &pb.MatchmakingFunctionSpec{Host: "[::1]", Port: 50090},
			wantScheme: "http",
			wantHost:   "[::1]:50090",
		},
		{
			name:       "case_11_http_bracketed_ipv6_with_embedded_port",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://[::1]:50090", Port: 0},
			wantScheme: "http",
			wantHost:   "[::1]:50090",
		},
		{
			name:       "case_12_whitespace_trimmed_host",
			spec:       &pb.MatchmakingFunctionSpec{Host: "  http://localhost  ", Port: 8080},
			wantScheme: "http",
			wantHost:   "localhost:8080",
		},
		// 11 Invalid URL forms
		{
			name:       "case_13_nil_spec",
			spec:       nil,
			wantErrSub: "mmf spec is nil",
		},
		{
			name:       "case_14_empty_host",
			spec:       &pb.MatchmakingFunctionSpec{Host: "", Port: 50090},
			wantErrSub: "mmf host is empty",
		},
		{
			name:       "case_15_whitespace_only_host",
			spec:       &pb.MatchmakingFunctionSpec{Host: "   ", Port: 50090},
			wantErrSub: "mmf host is empty",
		},
		{
			name:       "case_16_unsupported_ftp_scheme",
			spec:       &pb.MatchmakingFunctionSpec{Host: "ftp://localhost", Port: 21},
			wantErrSub: "unsupported mmf scheme",
		},
		{
			name:       "case_17_unsupported_grpc_scheme",
			spec:       &pb.MatchmakingFunctionSpec{Host: "grpc://localhost", Port: 50090},
			wantErrSub: "unsupported mmf scheme",
		},
		{
			name:       "case_18_http_missing_hostname",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://", Port: 50090},
			wantErrSub: "missing hostname",
		},
		{
			name:       "case_19_https_missing_hostname",
			spec:       &pb.MatchmakingFunctionSpec{Host: "https://", Port: 443},
			wantErrSub: "missing hostname",
		},
		{
			name:       "case_20_http_port_only_missing_hostname",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://:50090", Port: 0},
			wantErrSub: "missing hostname",
		},
		{
			name:       "case_21_negative_port_out_of_range",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://localhost", Port: -1},
			wantErrSub: "out of range",
		},
		{
			name:       "case_22_port_above_65535_out_of_range",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://localhost", Port: 70000},
			wantErrSub: "out of range",
		},
		{
			name:       "case_23_embedded_port_out_of_range",
			spec:       &pb.MatchmakingFunctionSpec{Host: "http://localhost:99999", Port: 0},
			wantErrSub: "invalid port",
		},
	}

	for _, tcItem := range cases {
		t.Run(tcItem.name, func(t *testing.T) {
			u, err := parseMmfUrl(tcItem.spec)
			if tcItem.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tcItem.wantErrSub)
				assert.Nil(t, u)
			} else {
				require.NoError(t, err)
				require.NotNil(t, u)
				assert.Equal(t, tcItem.wantScheme, u.Scheme)
				assert.Equal(t, tcItem.wantHost, u.Host)
			}
		})
	}

	// End-to-end InvokeMatchmakingFunctions with scheme-less host ("127.0.0.1") and embedded port with Port=0.
	t.Run("EndToEndSchemelessHostAndEmbeddedPortInvocation", func(t *testing.T) {
		setupGlobalTestCache(t, nil, nil)
		ids := createAndActivateTicketsInCache(t, &tc, 2, []string{"schemeless"}, 0)

		host, port := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
			for {
				if _, err := stream.Recv(); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					return err
				}
			}
			return stream.Send(&pb.StreamedMmfResponse{
				Match: &pb.Match{
					Id:      "schemeless-match",
					Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: ids[0]}}}},
				},
			})
		})

		callerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
		srv := &grpcServer{}
		err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
			Profile: &pb.Profile{
				Name:  "com.example.schemeless.v1",
				Pools: map[string]*pb.Pool{"p": {Name: "p", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "schemeless"}}}},
			},
			Mmfs: []*pb.MatchmakingFunctionSpec{
				{Name: "schemeless-ip", Host: host, Port: port, Type: pb.MatchmakingFunctionSpec_GRPC},
				{Name: "schemeless-embedded-port", Host: fmt.Sprintf("%s:%d", host, port), Port: 0, Type: pb.MatchmakingFunctionSpec_GRPC},
			},
		}, callerStream)
		require.NoError(t, err)
		assert.Len(t, callerStream.receivedMatches(), 2)
	})
}

// Scenario 21: OTel Collector Sidecar Dockerfile and Config Verification
func TestOTelCollectorSidecarDockerfileAndConfig(t *testing.T) {
	dockerfileBytes, err := os.ReadFile("deploy/cloud_run/otel-collector-sidecar/Dockerfile")
	require.NoError(t, err)
	dockerfile := string(dockerfileBytes)

	assert.Contains(t, dockerfile, "/etc/otelcol-google/config.yaml")
	assert.Contains(t, dockerfile, "/etc/otelcol-contrib/config.yaml")
	for _, line := range strings.Split(dockerfile, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "ENTRYPOINT") {
			assert.Contains(t, trimmed, "--config", "Dockerfile must not specify a bare ENTRYPOINT without --config")
		}
	}

	configBytes, err := os.ReadFile("deploy/cloud_run/otel-collector-sidecar/config.yaml")
	require.NoError(t, err)
	configContent := string(configBytes)
	assert.Contains(t, configContent, "googlemanagedprometheus")
}

type delayedCatchUpReplicator struct {
	mu        sync.Mutex
	calls     int
	firstWave []*store.StateUpdate
	delay     time.Duration
	validator *regexp.Regexp
}

func (d *delayedCatchUpReplicator) GetUpdates() []*store.StateUpdate {
	d.mu.Lock()
	d.calls++
	callNum := d.calls
	d.mu.Unlock()

	if callNum == 1 {
		time.Sleep(d.delay)
		return d.firstWave
	}
	time.Sleep(15 * time.Millisecond)
	return nil
}

func (d *delayedCatchUpReplicator) SendUpdates(ups []*store.StateUpdate) []*store.StateResponse {
	res := make([]*store.StateResponse, len(ups))
	nowMs := time.Now().UnixMilli()
	for i := range ups {
		res[i] = &store.StateResponse{Result: fmt.Sprintf("%013d-%d", nowMs, i+1)}
	}
	return res
}

func (d *delayedCatchUpReplicator) GetReplIdValidator() *regexp.Regexp {
	return d.validator
}

// Scenario 22: Cold-Start Catch-Up Barrier in InvokeMatchmakingFunctions
func TestInvokeMatchmakingFunctionsColdStartCatchUpBarrier(t *testing.T) {
	srv := &grpcServer{}

	t.Run("WaitsForCatchUpBeforeSnapshottingActiveTickets", func(t *testing.T) {
		rawTicket, err := proto.Marshal(&pb.Ticket{
			Attributes: &pb.Ticket_FilterableData{
				Tags:         []string{"coldstart"},
				CreationTime: timestamppb.Now(),
			},
			ExpirationTime: timestamppb.New(time.Now().Add(5 * time.Minute)),
		})
		require.NoError(t, err)

		tid := fmt.Sprintf("%013d-1", time.Now().UnixMilli())
		delayedRepl := &delayedCatchUpReplicator{
			delay:     60 * time.Millisecond,
			validator: regexp.MustCompile(`^\d{13}-\d+$`),
			firstWave: []*store.StateUpdate{
				{Cmd: store.Ticket, Key: tid, Value: string(rawTicket), ReplId: tid},
			},
		}
		setupGlobalTestCache(t, nil, delayedRepl)
		require.False(t, tc.IsCaughtUp())

		var snapshotCountSeenByMmf int
		host, port := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
			for {
				ch, recvErr := stream.Recv()
				if errors.Is(recvErr, io.EOF) {
					break
				}
				if recvErr != nil {
					return recvErr
				}
				snapshotCountSeenByMmf += len(ch.GetProfile().GetPools()["p"].GetParticipants().GetTickets())
			}
			return nil
		})

		callerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
		err = srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
			Profile: &pb.Profile{
				Name:  "com.example.coldstart.ok",
				Pools: map[string]*pb.Pool{"p": {Name: "p", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "coldstart"}}}},
			},
			Mmfs: []*pb.MatchmakingFunctionSpec{{Name: "mmf", Host: "http://" + host, Port: port, Type: pb.MatchmakingFunctionSpec_GRPC}},
		}, callerStream)
		require.NoError(t, err)
		assert.True(t, tc.IsCaughtUp())
		assert.Equal(t, 1, snapshotCountSeenByMmf, "InvokeMatchmakingFunctions must wait for catch-up before snapshotting active tickets")
	})

	t.Run("ReturnsUnavailableOnCatchUpTimeoutOrCallerStreamCancel", func(t *testing.T) {
		origCfg := cfg
		cfg = newFastCoreTestConfig()
		defer func() { cfg = origCfg }()

		// Unstarted cache whose caughtUpChan never closes.
		tc = cache.ReplicatedTicketCache{}
		tc.Init(cfg, memoryReplicator.New(cfg))
		require.False(t, tc.IsCaughtUp())

		streamCtx, streamCancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
		defer streamCancel()

		err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
			Profile: &pb.Profile{Name: "com.example.coldstart.timeout"},
			Mmfs:    []*pb.MatchmakingFunctionSpec{},
		}, &mockInvokeMmfServerStream{ctx: streamCtx})
		require.Error(t, err)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.Contains(t, err.Error(), "initial ticket cache replication catch-up did not complete in time")
	})
}

// Scenario 23: Empty and Invalid-Ticket Matches Returned to the Caller
func TestEmptyAndInvalidTicketMatchesReturnedToCallerAndMidStreamAuth(t *testing.T) {
	setupGlobalTestCache(t, nil, nil)
	reader := setupTestMeter(t)
	srv := &grpcServer{}

	// 1. Diagnostic empty match and invalid-only match are returned to the caller and counted with match.has_tickets=false.
	hostDiag, portDiag := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
		for {
			if _, err := stream.Recv(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
		}
		_ = stream.Send(&pb.StreamedMmfResponse{Match: &pb.Match{Id: "diag-empty", Rosters: nil}})
		_ = stream.Send(&pb.StreamedMmfResponse{
			Match: &pb.Match{Id: "diag-bad-id", Rosters: map[string]*pb.Roster{"r": {Name: "r", Tickets: []*pb.Ticket{{Id: "bad-id"}}}}},
		})
		return nil
	})

	callerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
	err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
		Profile: &pb.Profile{Name: "com.example.diag.v1"},
		Mmfs:    []*pb.MatchmakingFunctionSpec{{Name: "diag-mmf", Host: "http://" + hostDiag, Port: portDiag, Type: pb.MatchmakingFunctionSpec_GRPC}},
	}, callerStream)
	require.NoError(t, err)

	diagMatches := callerStream.receivedMatches()
	require.Len(t, diagMatches, 2)
	diagById := make(map[string]*pb.Match, len(diagMatches))
	for _, m := range diagMatches {
		require.NotNil(t, m)
		diagById[m.GetId()] = m
	}
	require.Contains(t, diagById, "diag-empty")
	require.Contains(t, diagById, "diag-bad-id")

	_, hasEmptyInvalidExt := decodeMatchInvalidTicketIds(t, diagById["diag-empty"])
	assert.False(t, hasEmptyInvalidExt)
	badIds, hasBadInvalidExt := decodeMatchInvalidTicketIds(t, diagById["diag-bad-id"])
	require.True(t, hasBadInvalidExt)
	assert.Equal(t, []string{"bad-id"}, badIds)

	assert.Equal(t, int64(2), sumMatchesReceived(t, reader, "diag-mmf", false))
	assert.Equal(t, int64(0), sumMatchesReceived(t, reader, "diag-mmf", true))

	// 2. Mid-stream codes.Unauthenticated after an empty match must NOT re-trigger initial HTTPS OIDC token refresh.
	serverCreds, clientTLS := generateTestTLSConfig(t)
	origTLSConfig := tlsConfig
	origTokenCache := idTokenCache
	tlsConfig = clientTLS
	defer func() {
		tlsConfig = origTLSConfig
		idTokenCache = origTokenCache
	}()

	var tokenFactoryCalls atomic.Int32
	idTokenCache = mmfauth.NewIDTokenCache(func(ctx context.Context, audience string) (oauth2.TokenSource, error) {
		tokenFactoryCalls.Add(1)
		seq := &atomic.Int64{}
		return &fakeTokenSource{audience: audience, seq: seq}, nil
	})

	var httpsServerCalls atomic.Int32
	_, portHttps := startTestMmfGrpcServer(t, serverCreds, func(stream pb.MatchMakingFunctionService_RunServer) error {
		httpsServerCalls.Add(1)
		for {
			if _, err := stream.Recv(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
		}
		// Send an empty match first (setting receivedAnyResponse = true), then return Unauthenticated.
		if err := stream.Send(&pb.StreamedMmfResponse{Match: &pb.Match{Id: "empty-before-unauth"}}); err != nil {
			return err
		}
		return status.Error(codes.Unauthenticated, "mid-stream unauthenticated after response")
	})

	httpsCallerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
	err = srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
		Profile: &pb.Profile{Name: "com.example.diag.https"},
		Mmfs: []*pb.MatchmakingFunctionSpec{
			{Name: "midstream-unauth-mmf", Host: fmt.Sprintf("https://127.0.0.1:%d", portHttps), Port: 0, Type: pb.MatchmakingFunctionSpec_GRPC},
		},
	}, httpsCallerStream)
	assertMmfFailureStatus(t, err, 1, []string{"midstream-unauth-mmf"})

	httpsMatches := httpsCallerStream.receivedMatches()
	require.Len(t, httpsMatches, 1)
	assert.Equal(t, []string{"empty-before-unauth"}, []string{httpsMatches[0].GetId()})

	assert.Equal(t, int32(1), httpsServerCalls.Load(), "mid-stream Unauthenticated after an empty response must not retry")
	assert.Equal(t, int32(1), tokenFactoryCalls.Load(), "mid-stream Unauthenticated after an empty response must not refresh token")
	assert.Equal(t, int64(1), sumInt64CounterByAttr(t, reader, "om_core.mmf.failures", "mmf.name", "midstream-unauth-mmf"))
	assert.Equal(t, int64(1), sumMatchesReceived(t, reader, "midstream-unauth-mmf", false))
	assert.Equal(t, int64(0), sumMatchesReceived(t, reader, "midstream-unauth-mmf", true))
}

// Scenario 23b: Every MMF Response Returned, in Both Deactivation Modes
func TestEveryMmfResponseReturnedInBothDeactivationModes(t *testing.T) {
	for _, waitEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("DeactivationWait=%v", waitEnabled), func(t *testing.T) {
			localCfg := newFastCoreTestConfig()
			localCfg.Set("OM_MATCH_TICKET_DEACTIVATION_WAIT", waitEnabled)
			setupGlobalTestCache(t, localCfg, nil)
			reader := setupTestMeter(t)

			ids := createAndActivateTicketsInCache(t, &tc, 2, []string{"every-response"}, 0)
			ticketA, ticketB := ids[0], ids[1]

			host, port := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
				for {
					if _, err := stream.Recv(); errors.Is(err, io.EOF) {
						break
					} else if err != nil {
						return err
					}
				}
				responses := []*pb.StreamedMmfResponse{
					{Match: &pb.Match{Id: "no-tickets"}},
					{Match: nil},
					{Match: &pb.Match{
						Id: "only-invalid",
						Rosters: map[string]*pb.Roster{
							"r": {Name: "r", Tickets: []*pb.Ticket{{Id: "bad-1"}, {Id: ""}}},
						},
					}},
					{Match: &pb.Match{
						Id: "mixed",
						Rosters: map[string]*pb.Roster{
							"r": {Name: "r", Tickets: []*pb.Ticket{{Id: ticketA}, {Id: "bad-2"}}},
						},
					}},
					{Match: &pb.Match{
						Id: "valid",
						Rosters: map[string]*pb.Roster{
							"r": {Name: "r", Tickets: []*pb.Ticket{{Id: ticketB}}},
						},
					}},
				}
				for _, resp := range responses {
					if err := stream.Send(resp); err != nil {
						return err
					}
				}
				return nil
			})

			callerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
			srv := &grpcServer{}
			err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
				Profile: &pb.Profile{
					Name: "com.example.everyresponse.v1",
					Pools: map[string]*pb.Pool{
						"p": {Name: "p", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "every-response"}}},
					},
				},
				Mmfs: []*pb.MatchmakingFunctionSpec{
					{Name: "every-response-mmf", Host: "http://" + host, Port: port, Type: pb.MatchmakingFunctionSpec_GRPC},
				},
			}, callerStream)
			require.NoError(t, err)

			matches := callerStream.receivedMatches()
			require.Len(t, matches, 5)

			var nilCount int
			byId := make(map[string]*pb.Match, 4)
			for _, m := range matches {
				if m == nil {
					nilCount++
					continue
				}
				byId[m.GetId()] = m
			}
			assert.Equal(t, 1, nilCount)
			require.Len(t, byId, 4)
			require.Contains(t, byId, "no-tickets")
			require.Contains(t, byId, "only-invalid")
			require.Contains(t, byId, "mixed")
			require.Contains(t, byId, "valid")

			_, hasInvalidNoTickets := decodeMatchInvalidTicketIds(t, byId["no-tickets"])
			assert.False(t, hasInvalidNoTickets)
			_, hasInvalidValid := decodeMatchInvalidTicketIds(t, byId["valid"])
			assert.False(t, hasInvalidValid)

			onlyInvalidIds, hasOnlyInvalid := decodeMatchInvalidTicketIds(t, byId["only-invalid"])
			require.True(t, hasOnlyInvalid)
			assert.Equal(t, []string{"bad-1", ""}, onlyInvalidIds)

			mixedInvalidIds, hasMixedInvalid := decodeMatchInvalidTicketIds(t, byId["mixed"])
			require.True(t, hasMixedInvalid)
			assert.Equal(t, []string{"bad-2"}, mixedInvalidIds)

			assert.True(t, tc.WaitForDeactivation(ticketA, "", 2*time.Second))
			assert.True(t, tc.WaitForDeactivation(ticketB, "", 2*time.Second))
			assert.Equal(t, int64(2), sumInt64Counter(t, reader, "om_core.mmf.ticket.deactivations"))
			assert.Equal(t, int64(2), sumMatchesReceived(t, reader, "every-response-mmf", true))
			assert.Equal(t, int64(3), sumMatchesReceived(t, reader, "every-response-mmf", false))
		})
	}
}

// Scenario 23c: Failed Deactivation Writes Reported Only When Waiting for Deactivation
func TestFailedDeactivationWritesReportedOnlyWhenWaitingForDeactivation(t *testing.T) {
	for _, waitEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("DeactivationWait=%v", waitEnabled), func(t *testing.T) {
			localCfg := newFastCoreTestConfig()
			localCfg.Set("OM_MATCH_TICKET_DEACTIVATION_WAIT", waitEnabled)
			failRepl := newSelectiveDeactivateFailureReplicator(memoryReplicator.New(localCfg))
			setupGlobalTestCache(t, localCfg, failRepl)

			ids := createAndActivateTicketsInCache(t, &tc, 3, []string{"deact-fail"}, 0)
			failRepl.setFailedTicketIds(ids[1])

			host, port := startTestMmfGrpcServer(t, nil, func(stream pb.MatchMakingFunctionService_RunServer) error {
				for {
					if _, err := stream.Recv(); errors.Is(err, io.EOF) {
						break
					} else if err != nil {
						return err
					}
				}
				return stream.Send(&pb.StreamedMmfResponse{
					Match: &pb.Match{
						Id: "partially-deactivated",
						Rosters: map[string]*pb.Roster{
							"r": {
								Name:    "r",
								Tickets: []*pb.Ticket{{Id: ids[0]}, {Id: ids[1]}, {Id: ids[2]}},
							},
						},
					},
				})
			})

			callerStream := &mockInvokeMmfServerStream{ctx: context.Background()}
			srv := &grpcServer{}
			err := srv.InvokeMatchmakingFunctions(&pb.MmfRequest{
				Profile: &pb.Profile{
					Name: "com.example.deactfail.v1",
					Pools: map[string]*pb.Pool{
						"p": {Name: "p", TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "deact-fail"}}},
					},
				},
				Mmfs: []*pb.MatchmakingFunctionSpec{
					{Name: "deact-fail-mmf", Host: "http://" + host, Port: port, Type: pb.MatchmakingFunctionSpec_GRPC},
				},
			}, callerStream)
			require.NoError(t, err)

			matches := callerStream.receivedMatches()
			require.Len(t, matches, 1)
			m := matches[0]
			require.NotNil(t, m)

			if !waitEnabled {
				assert.NotContains(t, m.GetExtensions(), MatchDeactivationFailedTicketIdsExtensionKey)
			} else {
				require.Contains(t, m.GetExtensions(), MatchDeactivationFailedTicketIdsExtensionKey)
				var listVal structpb.ListValue
				require.NoError(t, m.GetExtensions()[MatchDeactivationFailedTicketIdsExtensionKey].UnmarshalTo(&listVal))
				failedIds := make([]string, len(listVal.GetValues()))
				for idx, v := range listVal.GetValues() {
					failedIds[idx] = v.GetStringValue()
				}
				assert.Equal(t, []string{ids[1]}, failedIds)
				assert.NotContains(t, m.GetExtensions(), MatchDeactivationTimeoutExtensionKey)
				assert.NotContains(t, m.GetExtensions(), MatchInvalidTicketIdsExtensionKey)
			}
		})
	}
}

func mustBytesAny(t *testing.T, data []byte) *anypb.Any {
	t.Helper()
	wrapped, err := anypb.New(wrapperspb.Bytes(data))
	require.NoError(t, err)
	return wrapped
}

// Scenario 23d: MMF Request Chunks Respect the 4 MiB Soft Limit
func TestMmfRequestChunksRespectFourMiBSoftLimit(t *testing.T) {
	sampleTickets := []*pb.Ticket{
		{Id: "1-1"},
		{
			Id:         "1730000000000-123",
			Attributes: &pb.Ticket_FilterableData{Tags: []string{"a", "b"}},
		},
		{
			Id:         "x",
			Extensions: map[string]*anypb.Any{"big": mustBytesAny(t, make([]byte, 300))},
		},
	}
	for _, tk := range sampleTickets {
		oneTicketRoster := &pb.Roster{Name: "r", Tickets: []*pb.Ticket{tk}}
		twoTicketRoster := &pb.Roster{Name: "r", Tickets: []*pb.Ticket{tk, tk}}
		assert.Equal(t, proto.Size(twoTicketRoster)-proto.Size(oneTicketRoster), encodedRosterTicketSize(tk))
	}

	profile := &pb.Profile{
		Name: "com.example.softlimit",
		Pools: map[string]*pb.Pool{
			"everyone":       {Name: "everyone"},
			"everyone-again": {Name: "everyone-again"},
			"nobody": {
				Name:              "nobody",
				TagPresentFilters: []*pb.Pool_TagPresentFilter{{Tag: "no-such-tag"}},
			},
		},
	}
	validPools := map[string][]*pb.Ticket{
		"everyone":       {},
		"everyone-again": {},
		"nobody":         {},
	}

	const totalTickets = 600_000
	activeSnapshot := make(map[any]any, totalTickets)
	for idx := 0; idx < totalTickets; idx++ {
		id := strconv.Itoa(idx/1000) + "-" + strconv.Itoa(idx%1000)
		activeSnapshot[id] = &pb.Ticket{Id: id}
	}

	packedChunks := packPoolTicketChunks(profile, validPools, activeSnapshot)
	chunkRequests := getChunkedRequests(profile, packedChunks)

	require.Greater(t, len(chunkRequests), 1)

	var everyoneTotal, everyoneAgainTotal, nobodyTotal int
	for idx, chunk := range chunkRequests {
		chunkBytes := proto.Size(chunk)
		assert.LessOrEqual(t, chunkBytes, mmfChunkSoftLimitBytes, "chunk %d exceeded 4 MiB soft limit", idx)
		if idx < len(chunkRequests)-1 {
			assert.Greater(t, chunkBytes, mmfChunkFillTargetBytes-1000, "non-final chunk %d was not filled close to target", idx)
		}

		pools := chunk.GetProfile().GetPools()
		require.Len(t, pools, 3)
		everyoneTotal += len(pools["everyone"].GetParticipants().GetTickets())
		everyoneAgainTotal += len(pools["everyone-again"].GetParticipants().GetTickets())
		nobodyTotal += len(pools["nobody"].GetParticipants().GetTickets())
	}

	assert.Equal(t, totalTickets, everyoneTotal)
	assert.Equal(t, totalTickets, everyoneAgainTotal)
	assert.Equal(t, 0, nobodyTotal)
}

// Scenario 24: OpenTelemetry Service Version Verification
func TestOpenTelemetryServiceVersionAndInitializer(t *testing.T) {
	assert.Equal(t, "2.1.0", serviceVersion)

	m, shutdown := initializeOtel()
	require.NotNil(t, m)
	require.NotNil(t, shutdown)
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shutCancel()
	_ = shutdown(shutCtx)
}

// Scenario 25: Cloud Run Deployment Manifest Flags Verification
func TestCloudRunDeploymentManifestFlags(t *testing.T) {
	// 1. ci.yaml
	ciBytes, err := os.ReadFile("ci.yaml")
	require.NoError(t, err)
	ciContent := string(ciBytes)
	assert.Contains(t, ciContent, "--no-cpu-throttling")
	assert.Contains(t, ciContent, "--execution-environment")
	assert.Contains(t, ciContent, "gen2")
	assert.Contains(t, ciContent, "--min-instances")
	assert.Contains(t, ciContent, "\"2\"")
	assert.Contains(t, ciContent, "--no-allow-unauthenticated")
	assert.Contains(t, ciContent, "--ingress")

	// 2. deploy/cloud_run/cloudbuild.yaml
	cbBytes, err := os.ReadFile("deploy/cloud_run/cloudbuild.yaml")
	require.NoError(t, err)
	cbContent := string(cbBytes)
	assert.Contains(t, cbContent, "--no-cpu-throttling")
	assert.Contains(t, cbContent, "--execution-environment")
	assert.Contains(t, cbContent, "gen2")
	assert.Contains(t, cbContent, "--min-instances")
	assert.Contains(t, cbContent, "\"2\"")
	assert.Contains(t, cbContent, "--no-allow-unauthenticated")
	assert.Contains(t, cbContent, "--ingress")
	assert.Contains(t, cbContent, "internal")

	// 3. deploy/cloud_run/service.yaml
	svcBytes, err := os.ReadFile("deploy/cloud_run/service.yaml")
	require.NoError(t, err)
	svcContent := string(svcBytes)
	assert.Contains(t, svcContent, "run.googleapis.com/cpu-throttling: 'false'")
	assert.Contains(t, svcContent, "run.googleapis.com/execution-environment: gen2")
	assert.Contains(t, svcContent, "autoscaling.knative.dev/minScale: '2'")
	assert.Contains(t, svcContent, "run.googleapis.com/ingress: internal")

	// 4. .agents/skills/open_match2/SKILL.md
	skillBytes, err := os.ReadFile(".agents/skills/open_match2/SKILL.md")
	require.NoError(t, err)
	skillContent := string(skillBytes)
	assert.Contains(t, skillContent, "--no-cpu-throttling")
	assert.Contains(t, skillContent, "run.googleapis.com/cpu-throttling: 'false'")
	assert.Contains(t, skillContent, "gen2")
	assert.Contains(t, skillContent, "autoscaling.knative.dev/minScale: '2'")
	assert.Contains(t, skillContent, "run.googleapis.com/ingress: internal")
}
