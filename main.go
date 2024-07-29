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
//
// Tries to use the guidelines at https://cloud.google.com/apis/design for the gRPC API where possible.
//
// TODO: permissive deadlines for all RPC calls
//
//nolint:staticcheck
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"maps"
	"math"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpcMetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/googleforgames/open-match2/pkg/pb"

	"github.com/googleforgames/open-match2/internal/filter"
	"github.com/googleforgames/open-match2/internal/logging"
	"github.com/googleforgames/open-match2/internal/statestore/cache"
	store "github.com/googleforgames/open-match2/internal/statestore/datatypes"
	memoryReplicator "github.com/googleforgames/open-match2/internal/statestore/memory"
	redisReplicator "github.com/googleforgames/open-match2/internal/statestore/redis"

	"github.com/googleforgames/open-match2/internal/config"

	"github.com/davecgh/go-spew/spew"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Required by protobuf compiler's golang gRPC auto-generated code.
type grpcServer struct {
	pb.UnimplementedOpenMatchServiceServer
}

var (
	InvalidIdErr        = errors.New("Invalid ticket id")
	NoTicketIdsErr      = errors.New("No Ticket IDs in update request")
	NoValidTicketIdsErr = errors.New("No Valid Ticket IDs in update request")
	TooManyUpdatesErr   = errors.New("Too many ticket state updates requested in a single call")

	logger = logrus.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "core",
	})
	cfg *viper.Viper = nil

	// One global instance for the local ticket cache. Everything reads and
	// writes to this one instance which contains concurrent-safe data
	// structures where necessary.
	tc               cache.ReplicatedTicketCache
	tlsConfig        *tls.Config
	meter            *metric.Meter
	otelShutdownFunc func(context.Context) error
)

func main() {
	// Read configuration env vars, and configure logging
	cfg = config.Read()
	logging.ConfigureLogging(cfg)

	// Make a parent context that gets cancelled on SIGINT
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Read in the system root certificates in case an MMF is using TLS (which will
	// always be the case if your MMF is deployed to Google Cloud Run)
	if tlsConfig == nil {
		var err error
		systemRootCa, err := x509.SystemCertPool()
		// Turn off linting to avoid "G402: TLS MinVersion too low". Setting a
		// min version can cause compatibility problems for anyone who tries to
		// run OM on something other than Cloud Run.
		tlsConfig = &tls.Config{RootCAs: systemRootCa} //nolint:gosec
		if err != nil {
			logger.Fatalf("gRPC TLS: failed to get root certs")
		}
	}

	// Configure metrics
	meter, otelShutdownFunc = initializeOtel()
	defer otelShutdownFunc(ctx) //nolint:errcheck
	registerMetrics(meter)
	cache.RegisterMetrics(meter)

	// Set up the replicated ticket cache.
	// fields using sync.Map come ready to use and don't need initialization
	tc.Cfg = cfg
	tc.UpRequests = make(chan *cache.UpdateRequest)
	switch cfg.GetString("OM_STATE_STORAGE_TYPE") {
	case "redis":
		// Default: use redis
		rr, err := redisReplicator.New(cfg)
		if err != nil {
			logger.Fatal(fmt.Errorf("Redis Failure; exiting: %w", err).Error())
		}
		tc.Replicator = rr
	case "memory":
		// NOT RECOMMENDED FOR PRODUCTION
		//
		// Store statage in local memory only. Every copy of om-core spun up
		// using this configuration is an island which does not send or receive
		// any updates to/from any other instances.
		// Useful for debugging, local development, etc.
		logger.Warnf("OM_STATE_STORAGE_TYPE configuration variable set to 'memory'. NOT RECOMMENDED FOR PRODUCTION")
		tc.Replicator = memoryReplicator.New(cfg)
	}

	// These goroutines send and receive cache updates from state storage
	go tc.OutgoingReplicationQueue(ctx)
	go tc.IncomingReplicationQueue(ctx)

	// Start the gRPC server
	start(cfg)

	// Dump the final state of the cache to the log for debugging.
	if cfg.GetBool("OM_VERBOSE") {
		spew.Dump(syncMapDump(&tc.Tickets))
		spew.Dump(syncMapDump(&tc.InactiveSet))
		spew.Dump(syncMapDump(&tc.Assignments))
	}
	logger.Info("Application stopped successfully.")
	logger.Infof("Final state of local cache: %v tickets, %v active, %v inactive, %v assignments", len(syncMapDump(&tc.Tickets)), len(setDifference(&tc.Tickets, &tc.InactiveSet)), len(syncMapDump(&tc.InactiveSet)), len(syncMapDump(&tc.Assignments)))

}

//----------------------------------------------------------------------------------
// CreateTicket()
//----------------------------------------------------------------------------------

// CreateTicket generates an event to update the ticket state storage, adding a
// new ticket. The ticket's id will be generated by the state storage and
// returned asynchronously.  This request hangs until it can return the ticket
// id. When a ticket is created, it starts off as inactive and must be
// activated with the ActivateTickets call. This ensures that no ticket that
// was not successfully replicated and returned to the om-core client is ever
// put in the pool.
func (s *grpcServer) CreateTicket(parentCtx context.Context, req *pb.CreateTicketRequest) (*pb.CreateTicketResponse, error) {
	return createTicket(parentCtx, &tc, req)
}

// createTicket is the package-internal implementation of the grpc server
// CreateTicket function. Basically, this exists as a separate function only so
// that tests can run against it without having to stand up a grpc server.
func createTicket(parentCtx context.Context, tc *cache.ReplicatedTicketCache, req *pb.CreateTicketRequest) (*pb.CreateTicketResponse, error) {
	rpcName := "CreateTicket"

	// Add structured logging fields for logs output from this function
	logger := logger.WithFields(logrus.Fields{
		"stage": "handling",
		"rpc":   rpcName,
	})

	// Validate request and update fields to ensure a valid ticket, then
	// Marshal ticket into storage format
	ticketPb, err := validateAndMarshalIncomingTicket(req)
	if ticketPb == nil || err != nil {
		err = status.Error(codes.Internal,
			fmt.Errorf("CreateTicket failed to marshal the provided ticket to the state storage format: %w", err).Error())
		logger.Error(err)
		return &pb.CreateTicketResponse{}, fmt.Errorf("%w", err)
	}

	// Record the size of the ticket in kb
	otelTicketSize.Record(parentCtx, float64(len(ticketPb))/1024.0)

	// Make a results return channel
	rChan := make(chan *store.StateResponse)

	// Queue our ticket creation cache update request to be replicated to all
	// om-core instances.
	tc.UpRequests <- &cache.UpdateRequest{
		// This command (writing a ticket to the cache) is replicated to all
		// other om-core instances using the batch writing async goroutine
		// tc.OutgoingReplicationQueue() and the update itself is applied to
		// the local ticket cache in the update-processing async goroutine
		// tc.IncomingReplicationQueue().
		ResultsChan: rChan,
		Ctx:         parentCtx,
		Update: store.StateUpdate{
			Cmd:   store.Ticket,
			Value: string(ticketPb[:]),
		},
	}

	// Get the results
	results := <-rChan

	return &pb.CreateTicketResponse{TicketId: results.Result}, results.Err
}

// validateAndMarshalIncomingTicket is the package-internal implementation to
// validate expiration time and creation time input to the CreateTicket
// function and generate replacement timestamps as necessary.
//
// Basically, this exists as a separate function only so that tests can run
// against it without having to stand up a grpc server.
//
// A note about testing and input validation: this implementation and its test
// suite are designed around how this function is executed in the live
// production code path: e.g. This function is only ever called by the
// OpenMatchServiceServer grpc server's CreateTicket function, which is assumed
// to sit behind the grpc-gateway reverse-proxy
// (https://github.com/grpc-ecosystem/grpc-gateway).  That upstream function
// call chain will fail if the JSON pb.CreateTicketRequest is invalid, so this
// function does not need to do any input validation that would be caught by
// those upstream functions. (Basically, if your request is so malformed that
// the JSON can't be marshalled to the protobuf CreateTicketRequest message,
// your input will never reach this function.)
func validateAndMarshalIncomingTicket(req *pb.CreateTicketRequest) ([]byte, error) {
	rpcName := "CreateTicket"
	// Add structured logging fields for logs output from this function
	logger := logger.WithFields(logrus.Fields{
		"stage": "validation",
		"rpc":   rpcName,
	})

	// Input validation
	if req.GetTicket().GetId() != "" {
		logger.Warnf("CreateTicket request included a ticketid '%v'. "+
			"Open Match assigns Ticket IDs on creation"+
			", see documentation for more details.",
			req.GetTicket().GetId())
	}

	// Set creation timestamp if request didn't provide one or specified one
	// that's in the future.
	//
	// Note: om-core will not complain about a creation time so old that the
	// expiration time has already passed and the ticket is immediately
	// expired!
	crTime := req.GetTicket().GetAttributes().GetCreationTime()

	switch {
	case crTime == nil:
		// This is our expected code path: user provides an empty creation
		// time and om-core fills it in. (Thus why this logs at debug logging
		// level while all other creation time validation failures are warnings)
		logger.Trace("No CreationTime provided; using current time")
		crTime = timestamppb.Now()
	case !crTime.IsValid():
		logger.Warn("CreationTime provided is invalid; replacing with current time")
		crTime = timestamppb.Now()
	case crTime.AsTime().After(time.Now()):
		logger.Warn("CreationTime provided is in the future; replacing with current time")
		crTime = timestamppb.Now()
	}

	// Set expiration timestamp if request didn't provide a valid one.
	//
	// Matchmakers can request a shorter expiration time than the default, but
	// not a longer one. The default expiration time is also the maximum allowed:
	// now + the configured ticket TTL
	exTime := req.GetTicket().GetExpirationTime()
	maxExTime := time.Now().Add(time.Millisecond *
		time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS")))

	switch {
	case exTime == nil:
		// This is our expected code path: user provides an empty expiration
		// time and om-core fills it in. (Thus why this logs at debug logging
		// level while all other recoverable exp time validations failures are warnings)
		logger.Tracef("ExpirationTime provided is missing; "+
			"replacing with current time + OM_CACHE_TICKET_TTL_MS (%v)",
			cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))
		exTime = timestamppb.New(maxExTime)
	case !exTime.IsValid():
		fallthrough
	case exTime.AsTime().After(maxExTime):
		fallthrough
	case exTime.AsTime().Before(time.Now()):
		// This combination of creation time + expiration time isn't valid.
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument,
			fmt.Sprintf("specified expiration time not between (now, now + %v)",
				cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))))
	}

	// Update ticket with new creation/expiration time
	return proto.Marshal(&pb.Ticket{ //nolint:wrapcheck
		ExpirationTime: exTime,
		Extensions:     req.GetTicket().GetExtensions(),
		Attributes: &pb.Ticket_FilterableData{
			Tags:         req.GetTicket().GetAttributes().GetTags(),
			StringArgs:   req.GetTicket().GetAttributes().GetStringArgs(),
			DoubleArgs:   req.GetTicket().GetAttributes().GetDoubleArgs(),
			CreationTime: crTime,
		},
	})

}

//----------------------------------------------------------------------------------
// ActivateTicket()
// DeactivateTicket()
//----------------------------------------------------------------------------------

// DeactivatesTicket is a lazy deletion process: it adds the provided ticketIDs to the inactive list,
// which prevents them from appearing in player pools, and the tickets are deleted when they expire.
//
// Receiving one or more invalid ticket ids in the pb.DeactivateTicketsRequest
// does not stop processing of valid ticket ids in the same request. In the
// event of a partially successful update, the grpc Status Details can be
// checked for more information about which updates failed, and why.
func (s *grpcServer) DeactivateTickets(parentCtx context.Context, req *pb.DeactivateTicketsRequest) (*pb.DeactivateTicketsResponse, error) {
	logger := logger.WithFields(logrus.Fields{
		"rpc": "DeactivateTickets",
	})
	return deactivateTickets(parentCtx, logger, &tc, req)
}

// deactivateTicket is the package-internal implementation of the grpc server
// DeactivateTicket function. Basically, this exists as a separate function only so
// that tests can run against it without having to stand up a grpc server.
func deactivateTickets(parentCtx context.Context, logger *logrus.Entry, tc *cache.ReplicatedTicketCache, req *pb.DeactivateTicketsRequest) (*pb.DeactivateTicketsResponse, error) {

	// This function does not allow for cancelling once the request is received.
	ctx := context.WithoutCancel(parentCtx)

	// Validate input list of ticket ids against state storage id format before
	// trying to process them. If only some of the ids are valid, the
	// invalidIdErrorDetails will be used to populate error details to return
	// to the gRPC client.
	validTicketIds, invalidIdErrorDetails, err := validateTicketStateUpdates(logger,
		tc.Replicator.GetReplIdValidator(), req.GetTicketIds())
	if err != nil && len(validTicketIds) == 0 {
		// Nothing valid to process.
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument, err.Error()))
	}

	// Send the ticket activation updates.
	updateStateErrDetails := updateTicketsActiveState(ctx, logger, tc, validTicketIds, store.Deactivate)

	// Attach error details we found while validating ticket IDs and
	// replicating the updates.
	maps.Copy(updateStateErrDetails, invalidIdErrorDetails)
	if len(updateStateErrDetails) > 0 {
		err = addErrorDetails(updateStateErrDetails, status.New(codes.InvalidArgument, err.Error()))
	}

	// Record metrics
	if meter != nil {
		otelDeactivationsPerCall.Record(ctx, int64(len(validTicketIds)))
		otelInvalidIdsPerDeactivateCall.Record(ctx, int64(len(invalidIdErrorDetails)))
		otelFailedIdsPerDeactivateCall.Record(ctx, int64(len(updateStateErrDetails)))
	}

	return &pb.DeactivateTicketsResponse{}, err
}

// ActivateTickets accepts a list of ticketids to activate, validates the
// input, and generates replication updates for each activation event.
//
// Receiving one or more invalid ticket ids in the pb.ActivateTicketsRequest
// does not stop processing of valid ticket ids in the same request. In the
// event of a partially successful update, the grpc Status Details can be
// checked for more information about which updates failed, and why.
func (s *grpcServer) ActivateTickets(parentCtx context.Context, req *pb.ActivateTicketsRequest) (*pb.ActivateTicketsResponse, error) {
	// Structured logging fields for this function.
	logger := logger.WithFields(logrus.Fields{
		"rpc": "ActivateTickets",
	})
	return activateTickets(parentCtx, logger, &tc, req)
}

// activateTicket is the package-internal implementation of the grpc server
// ActivateTicket function. Basically, this exists as a separate function only so
// that tests can run against it without having to stand up a grpc server.
func activateTickets(parentCtx context.Context, logger *logrus.Entry, tc *cache.ReplicatedTicketCache, req *pb.ActivateTicketsRequest) (*pb.ActivateTicketsResponse, error) {

	// This function does not allow for cancelling once the request is received.
	ctx := context.WithoutCancel(parentCtx)

	// Validate input list of ticket ids against state storage id format before
	// trying to process them. If only some of the ids are valid, the
	// invalidIdErrorDetails will be used to populate error details to return
	// to the gRPC client.
	validTicketIds, invalidIdErrorDetails, err := validateTicketStateUpdates(logger,
		tc.Replicator.GetReplIdValidator(), req.GetTicketIds())
	if err != nil && len(validTicketIds) == 0 {
		// Nothing valid to process.
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument, err.Error()))
	}

	// Send the ticket activation updates.
	updateStateErrDetails := updateTicketsActiveState(ctx, logger, tc, validTicketIds, store.Activate)

	// Attach error details we found while validating ticket IDs and
	// replicating the updates.
	maps.Copy(updateStateErrDetails, invalidIdErrorDetails)
	if len(updateStateErrDetails) > 0 {
		err = addErrorDetails(updateStateErrDetails, status.New(codes.InvalidArgument, err.Error()))
	}

	// Record metrics
	if meter != nil {
		otelActivationsPerCall.Record(ctx, int64(len(validTicketIds)))
		otelInvalidIdsPerActivateCall.Record(ctx, int64(len(invalidIdErrorDetails)))
		otelFailedIdsPerActivateCall.Record(ctx, int64(len(updateStateErrDetails)))
	}

	return &pb.ActivateTicketsResponse{}, err
}

// validateTicketStateUpdates takes a list of ticket IDs to validate, and a
// regex used to validate those IDs.  Since we want to process any valid ticket
// IDs in requests that contain multiple IDs, this function doesn't just exit
// when it finds an invalid ID.  It returns a list of valid IDs, a map of IDs
// to failures (errors) for those IDs that are invalid, and an error.
func validateTicketStateUpdates(logger *logrus.Entry, idValidator *regexp.Regexp, ticketIds []string) (validIds []string, invalidIdErrorDetails map[string]error, err error) {
	// Initialize map to prevent 'panic: assignment to entry in nil map' on first assignment
	invalidIdErrorDetails = map[string]error{}

	// Structured logging
	logger = logger.WithFields(logrus.Fields{
		"stage": "validation",
	})

	// Validate number of requested updates
	numReqUpdates := len(ticketIds)
	if numReqUpdates > cfg.GetInt("OM_MAX_STATE_UPDATES_PER_CALL") {
		return nil, nil, fmt.Errorf("%w (configured maximum %v, requested %v)",
			TooManyUpdatesErr, cfg.GetInt("OM_MAX_STATE_UPDATES_PER_CALL"), numReqUpdates)
	}
	if numReqUpdates == 0 {
		return nil, nil, NoTicketIdsErr
	}

	// Validate ids of requested updates
	if numReqUpdates > 0 {
		// check each id in the input list, making separate lists of those that are
		// valid and those that are invalid.
		for _, id := range ticketIds {
			if idValidator.MatchString(id) {
				validIds = append(validIds, id)
				logger.WithFields(logrus.Fields{"ticket_id": id}).Trace("valid ticket id")
			} else {
				// Generate error details to attach to returned grpc Status
				invalidIdErrorDetails[id] = InvalidIdErr
				logger.WithFields(logrus.Fields{"ticket_id": id}).Error(InvalidIdErr)
			}
		}
	}

	if len(validIds) == 0 {
		// Don't bother returning details about every invalid ticket id if they
		// were all invalid.
		return nil, nil, NoValidTicketIdsErr
	}
	if len(invalidIdErrorDetails) > 0 {
		err = InvalidIdErr
	}

	return
}

// updateTicketsActiveState accepts a list of ticketids to (de-)activate, and
// generates cache updates for each.
//
// NOTE This function does no input validation, the calling function has to handle that.
func updateTicketsActiveState(parentCtx context.Context, logger *logrus.Entry, tc *cache.ReplicatedTicketCache, ticketIds []string, command int) map[string]error {
	// Make a human-readable version of the requested state transition, for logging
	var requestedStateAsString string
	switch command {
	case store.Deactivate:
		requestedStateAsString = "inactive"
	case store.Activate:
		requestedStateAsString = "active"
	}
	logger = logger.WithFields(logrus.Fields{
		"stage":         "handling",
		"rpc":           "updateTicketsActiveState",
		"num_updates":   len(ticketIds),
		"desired_state": requestedStateAsString,
	})

	errs := map[string]error{}

	// Generate result channel
	rChan := make(chan *store.StateResponse, len(ticketIds))
	defer close(rChan)

	// Queue the update requests
	logger.Trace("queuing updates for ticket state change")
	for _, id := range ticketIds {
		tc.UpRequests <- &cache.UpdateRequest{
			// This command (adding/removing the id to the inactive list)
			// is replicated to all other om-core instances using the batch
			// writing async goroutine tc.OutgoingReplicationQueue() and its
			// effect is applied to the local ticket cache in the update
			// processing async goroutine tc.IncomingReplicationQueue().
			ResultsChan: rChan,
			Ctx:         parentCtx,
			Update: store.StateUpdate{
				Cmd: command,
				Key: id,
			},
		}
		logger.WithFields(logrus.Fields{
			"ticket_id": id,
		}).Trace("generated request to update ticket status")
	}

	// Look through all results for errors. Since results come back on a
	// channel, we need to process all the results, even if the context has
	// been cancelled and we'll never return those results to the calling
	// client (returning from this function before processing all results
	// closes the results channel, which will cause a panic, as the state
	// storage layer isn't context-aware). This is slightly inefficient in the
	// worst-case scenario (client has quit) but the code is very simple and
	// therefore robust.
	for i := 0; i < len(ticketIds); i++ {
		r := <-rChan
		if r.Err != nil {
			// Wrap redis error and give it a gRPC internal server error status code
			// The results.result field contains the ticket id that generated the error.
			errs[r.Result] = status.Error(codes.Internal, fmt.Errorf("Unable to update ticket %v state to %v : %w", ticketIds[i], requestedStateAsString, r.Err).Error())
			logger.Error(errs[r.Result])
		}
	}
	return errs
}

//----------------------------------------------------------------------------------
// InvokeMatchmakingFunctions()
//----------------------------------------------------------------------------------

// InvokeMatchmakingFunctions loops through each Pool in the provided Profile,
// applying the filters inside and adding participating tickets to those pools.
// It then attempts to connect to every matchmaking function in the provided
// list, and send the Profile with filled Pools to each. It processes resulting
// matches from each matchmaking function asynchronously as they arrive. For
// each match it receives, it deactivates all tickets contained in the match
// and streams the match back to the InvokeMatchmakingFunctions caller.
//
// NOTE: (complexity linters disabled) The complexity here is unavoidable as we're
// using multiple nested goroutines to process lots of MMFs simultaneously
// while minimizing the time to first reponse for clients.
//
//nolint:gocognit,cyclop,gocyclo,maintidx
func (s *grpcServer) InvokeMatchmakingFunctions(req *pb.MmfRequest, stream pb.OpenMatchService_InvokeMatchmakingFunctionsServer) error {

	// input validation
	if req.GetProfile() == nil {
		return fmt.Errorf("%w", status.Error(codes.InvalidArgument, "profile is required"))
	}
	if req.GetMmfs() == nil {
		return fmt.Errorf("%w", status.Error(codes.InvalidArgument, "list of mmfs to invoke is required"))
	}

	// By convention, profile names should use reverse-DNS notation
	// https://en.wikipedia.org/wiki/Reverse_domain_name_notation This
	// helps with metric attribute cardinality, as we can record
	// profile names alongside metric readings after stripping off the
	// most-unique portion.
	profileName := req.GetProfile().GetName()
	i := strings.LastIndex(profileName, ".")
	if i > 0 {
		profileName = profileName[0:i]
	}
	logger := logger.WithFields(logrus.Fields{
		"stage":        "handling",
		"rpc":          "InvokeMatchmakingFunctions",
		"profile_name": profileName,
	})

	// Apply filters from all pools specified in this profile
	// to find the participating tickets for each pool.  Start by snapshotting
	// the state of the ticket cache to a new data structure, so we can work
	// with that snapshot without incurring a bunch of additional access
	// contention on the ticket cache itself, which will continue to be updated
	// as we process. The participants of these pools won't reflect updates to
	// the ticket cache that happen after this point.

	// Copy the ticket cache, leaving out inactive tickets.
	activeTickets := setDifference(&tc.Tickets, &tc.InactiveSet)
	// This is largely for local debugging when developiong a matchmaker against
	// OM. Assignments are considered deprecated, so OM shouldn't be
	// responsible for them in production and doesn't output OTEL metrics for
	// tracking assignment counts.
	unassignedTickets := setDifference(&tc.InactiveSet, &tc.Assignments)

	// Track the number of tickets in the cache at the moment filters are applied
	logger.Infof(" %5d tickets active, %5d tickets inactive without assignment",
		len(activeTickets), len(unassignedTickets))
	otelCachedTicketsAvailableForFilteringPerInvokeMMFCall.Record(context.Background(), int64(len(activeTickets)),
		metric.WithAttributes(attribute.String("profile.name", profileName)),
	)

	// validate pool filters before filling them
	validPools := map[string][]*pb.Ticket{}
	for name, pool := range req.GetProfile().GetPools() {
		if valid, err := filter.ValidatePoolFilters(pool); valid {
			// Initialize a clean roster for this pool
			validPools[name] = make([]*pb.Ticket, 0)
		} else {
			logger.Error("Unable to fill pool with tickets, invalid: %w", err)
		}
	}

	// Track the number of pools in requested profiles
	logger.Tracef("Tickets filtered into %v pools", len(validPools))
	otelPoolsPerProfile.Record(context.Background(), int64(len(validPools)),
		metric.WithAttributes(attribute.String("profile.name", profileName)),
	)

	// Perform filtering, and 'chunk' the pools into 4mb pieces for streaming
	// (4mb is default max pb size.)
	// 1000 instead of 1024 for a little extra headroom, we're not trying to
	// hyper-optimize here. Every chunk contains the entire profile, and a
	// portion of the tickets in that profile's pools.
	maxPbSize := 4 * 1000 * 1000
	// Figure out how big the message is with the profile populated, but all pools empty
	emptyChunkSize := proto.Size(&pb.ChunkedMmfRunRequest{Profile: req.GetProfile(), NumChunks: math.MaxInt32})
	curChunkSize := emptyChunkSize

	// Array of 'chunks', each consisting of a portion of the pools in this profile.
	// MMFs need to re-assemble the pools with a simple loop+concat over all chunks
	var chunkCount int32
	chunkedPools := make([]map[string][]*pb.Ticket, 0)
	chunkedPools = append(chunkedPools, map[string][]*pb.Ticket{})

	for _, ticket := range activeTickets {
		for name, _ := range validPools {
			// All the implementation details of filtering are in github.com/googleforgames/open-match2/internal/filter/filter.go
			if filter.In(req.GetProfile().GetPools()[name], ticket.(*pb.Ticket)) {
				ticketSize := proto.Size(ticket.(*pb.Ticket))
				// Check if this ticket will put us over the max pb size for this chunk
				if (curChunkSize + ticketSize) >= maxPbSize {
					// Start a new chunk
					curChunkSize = emptyChunkSize
					chunkCount++
					chunkedPools = append(chunkedPools, map[string][]*pb.Ticket{})
				}
				chunkedPools[chunkCount][name] = append(chunkedPools[chunkCount][name], ticket.(*pb.Ticket))
				curChunkSize += ticketSize
			}
		}
	}

	// Track how large (that is, how many chunks) the profile became once
	// we populated all the pools with tickets.
	logger.Debugf("%v pools packed into %v chunks", len(validPools), len(chunkedPools))
	otelProfileChunksPerInvokeMMFCall.Record(context.Background(), int64(len(chunkedPools)),
		metric.WithAttributes(attribute.String("profile.name", profileName)),
	)

	// Put final participant rosters into the pools.
	// Send the full profile in each streamed 'chunk', only the pools are broken
	// up to keep the pbs under the max size. This could probably be optimized
	// so we don't repeatedly send profile details in larger payloads, but this
	// implementation is 1) simpler and 2) could still be useful to the receiving
	// MMF if it somehow only got part of the chunked request.
	chunkedRequest := make([]*pb.ChunkedMmfRunRequest, len(chunkedPools))
	for chunkIndex, chunk := range chunkedPools {
		// Fill this request 'chunk' with the chunked pools we built above
		pools := make(map[string]*pb.Pool)
		profile := &pb.Profile{
			Name:       req.GetProfile().GetName(),
			Pools:      pools,
			Extensions: req.GetProfile().GetExtensions(),
		}
		for name, participantRoster := range chunk {
			profile.GetPools()[name] = &pb.Pool{
				Participants: &pb.Roster{
					Name:    name + "_roster",
					Tickets: participantRoster,
				},
			}
		}
		chunkedRequest[chunkIndex] = &pb.ChunkedMmfRunRequest{
			Profile:   profile,
			NumChunks: int32(len(chunkedPools)),
		}
	}

	// MMF Result fan-in goroutine
	// Simple fan-in channel pattern implemented as an async inline goroutine.
	// Asynchronously sends matches to InvokeMatchMakingFunction() caller as
	// they come in from the concurrently-running MMFs. Exits when all
	// MMFs are complete and matchChan is closed.
	//
	// Channel on which MMFs return their matches.
	matchChan := make(chan *pb.Match)
	go func() {
		// Local logger with a flag to indicate logs are from this goroutine.
		logger := logger.WithFields(logrus.Fields{"stage": "fan-in"})
		logger.Trace("MMF results fan-in goroutine active")

		for {
			logger.Trace("MMF results fan-in waiting")

			select {
			case match, ok := <-matchChan: // goroutine waits here for next match
				if !ok { // The channel notified us that it is closed.
					logger.Trace("ALL MMFS COMPLETE: exiting MMF results fan-in goroutine")
					return
				}

				logger.WithFields(logrus.Fields{
					"match_id": match.GetId(),
				}).Trace("streaming back match to matchmaker")

				// Send the match back to the caller.
				err := stream.Send(&pb.StreamedMmfResponse{Match: match})
				if err != nil {
					logger.Error(err)
				}
			}
		}
	}()

	// Set up grpc dial options for all MMFs.
	//
	// Our design anticipates MMFs that aren't constrained by OM - maybe you
	// want to write an MMF that does an monte-carlo simulation or is writing
	// analytics to a database, and will never return a match to the calling
	// client. Since we want to allow long-running MMFs to continue until they
	// finish processing we send them a context below that isn't cancelled when
	// the client closes the stream; but best practice dictates /some/ timeout
	// here (default 10 mins).
	var opts []grpc.DialOption
	mmfTimeout := time.Duration(cfg.GetInt("OM_MMF_TIMEOUT_SECS")) * time.Second
	opts = append(opts,
		grpc.WithConnectParams(grpc.ConnectParams{MinConnectTimeout: mmfTimeout}),
	)

	// Auth using IAM when on Google Cloud
	tokenSources := map[string]oauth2.TokenSource{}

	// Invoke each requested MMF, and put the matches they stream back into the match channel.
	// TODO: Technically this would be better as an ErrorGroup but this already works.
	// https://stackoverflow.com/questions/71246253/handle-goroutine-termination-and-error-handling-via-error-group
	var mmfwg sync.WaitGroup
	for _, mmf := range req.GetMmfs() {

		// Add this invocation to the MMF wait group.
		mmfwg.Add(1)

		// MMF fan-out goroutine
		// Call the MMF asynchronously, so all processing happens concurrently
		go func(mmf *pb.MatchmakingFunctionSpec) error {
			defer mmfwg.Done()

			// var init
			var err error

			// Parse the connection URI
			mmfUrl, err := url.Parse(fmt.Sprintf("%v:%v", mmf.GetHost(), mmf.GetPort()))
			if err != nil {
				err = status.Error(codes.Internal,
					fmt.Errorf("Failed to parse mmf host uri %v: %w", mmf.GetHost(), err).Error())
				logger.Error(err)
				otelMmfFailures.Add(context.Background(), 1, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
				))
				return fmt.Errorf("%w", err)
			}

			// Legacy REST is not implemented in OM2; error.
			if mmf.GetType() == pb.MatchmakingFunctionSpec_REST {
				otelMmfFailures.Add(context.Background(), 1, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
				))
				// OM1 allowed MMFs to use HTTP RESTful grpc implementations,
				// but its usage was incredibly low. This is where that would
				// be re-implemented if we see enough user demand.
				return status.Error(codes.Internal, fmt.Errorf("REST Mmf invocation NYI %v: %w", mmfUrl.Host, err).Error())

			} // pb.MatchmakingFunctionSpec_gRPC is default.

			// Add mmf details to all logs from this point on.
			logger := logger.WithFields(logrus.Fields{
				"mmf_name": mmf.GetName(),
				"mmf_host": mmfUrl.Host,
			})

			// Set a timeout for this API call, but use the empty
			// background context. It is fully intended that MMFs can run
			// longer than the matchmaker is willing to wait, so we want
			// them not to get cancelled when the calling matchmaker
			// cancels its context. However, best practices dictate that
			// we define /some/ timeout (default: 10 mins)
			ctx, cancel := context.WithTimeout(context.Background(), mmfTimeout)
			defer cancel()

			// TODO: Potential future optimization: cache
			// connections/clients using a pool and re-use
			var conn *grpc.ClientConn
			creds := insecure.NewCredentials()
			if mmfUrl.Scheme == "https" {
				// MMF server is running with TLS.
				// Note: TLS is transparently added to the MMF server when running on Cloud Run,
				//       see https://cloud.google.com/run/docs/container-contract#tls
				creds = credentials.NewTLS(tlsConfig)
			}
			opts = append(opts,
				grpc.WithTransportCredentials(creds),
				grpc.WithAuthority(mmfUrl.Host),
			)

			// Connect to gRPC server for this mmf.
			conn, err = grpc.NewClient(mmfUrl.Host, opts...)
			if err != nil {
				err = status.Error(codes.Internal, fmt.Errorf("Failed to dial gRPC for %v: %w", mmfUrl.Host, err).Error())
				logger.Error(err)
				otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
				))
				return fmt.Errorf("%w", err)
			}
			defer conn.Close()

			// Fetch Google Cloud IAM auth token
			var ok bool
			// Maintain a dictionary of tokenSources by MMF so we aren't recreating them every time.
			if _, ok = tokenSources[mmf.GetHost()]; !ok {
				// Create a TokenSource if none exists.
				tokenSources[mmf.GetHost()], err = idtoken.NewTokenSource(ctx, mmf.GetHost())
				if err != nil {
					err = status.Error(codes.Internal, fmt.Errorf(
						"Failed to get a source for ID tokens to contact gRPC MMF at %v: %w",
						mmfUrl.Host, err).Error())
					logger.Error(err)
					otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
						attribute.String("mmf.name", mmf.GetName()),
						attribute.String("profile.name", profileName),
					))
					return fmt.Errorf("%w", err)
				}
			}
			// Get the token from the tokenSource.
			token, err := tokenSources[mmf.GetHost()].Token()
			if err != nil {
				err = status.Error(codes.Internal, fmt.Errorf(
					"Failed to get ID token to contact gRPC MMF at %v: %w",
					mmfUrl.Host, err).Error())
				logger.Error(err)
				otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
				))
				return fmt.Errorf("%w", err)
			}

			// Add Google Cloud IAM auth token to the context.
			ctx = grpcMetadata.AppendToOutgoingContext(ctx,
				"authorization", "Bearer "+token.AccessToken)

			// Get MMF client. May be possible to re-use, but would
			// require some validation of assumptions; for now we're
			// re-creating on every call under the assumption this will allow
			// individual MMF calls to be load-balanced when an MMF is under
			// enough load to spin up additional instances. For more
			// information about the challenges of load-balancing gRPC
			// endpoints, see:
			//https://grpc.io/blog/grpc-load-balancing/
			client := pb.NewMatchMakingFunctionServiceClient(conn)
			logger.Trace("Connected to MMF")

			// Run the MMF
			var mmfStream pb.MatchMakingFunctionService_RunClient
			mmfStream, err = client.Run(ctx)
			if err != nil {
				// Example failure that will trigger this codepath:
				// "Failed to connect to MMF at localhost:50443: rpc error:
				// code = Unavailable desc = connection error: desc =
				// \"transport: authentication handshake failed: tls: first
				// record does not look like a TLS handshake\""
				logger.Error(fmt.Errorf("Failed to connect to MMF at %v: %w", mmfUrl.Host, err).Error())
				otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
				))
				return status.Error(codes.Internal, fmt.Errorf("Failed to connect to MMF at %v: %w", mmfUrl.Host, err).Error())
			}
			logger.Trace("MMF .Run() invoked, sending profile chunks")

			// Request itself is chunked if all the tickets returned in
			// ticket pools result in a total request size larger than the
			// default gRPC message size of 4mb.
			for index, chunk := range chunkedRequest {
				err = mmfStream.Send(chunk)
				if err != nil {
					logger.Error(err)
				}
				logger.Tracef("MMF request chunk %02d/%02d: %0.2fmb", index+1, len(chunkedRequest), float64(proto.Size(chunk))/float64(1024*1024))
			}

			// Make a waitgroup that lets us know when all ticket deactivations
			// are complete. (All tickets in matches returned by the MMF are
			// set to inactive by OM.)
			var tdwg sync.WaitGroup

			// i counts the number of results (which equates to matches
			// received) for metrics reporting; this loop reads streaming
			// match responses from the mmf and runs until a break
			// statement is encountered.
			var i int64
			i = 0
			for {
				// Get results from MMF
				var result *pb.StreamedMmfResponse
				result, err = mmfStream.Recv()

				// io.EOF is the error returned by grpc when the server closes a stream.
				if errors.Is(err, io.EOF) {
					logger.Tracef("MMF stream complete")
					break
				}
				if err != nil { // MMF has an error
					err = status.Error(codes.Internal, fmt.Errorf("MMF Failure: %w", err).Error())
					logger.Error(err)
					otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
						attribute.String("mmf.name", mmf.GetName()),
						attribute.String("profile.name", profileName),
					))
					return fmt.Errorf("%w", err)
				}
				i++

				// deactivate tickets & send match back to the matchmaker
				// asynchronously as streamed match results continue to be
				// processed.
				//
				// NOTE: even with the waitgroup, this implementation is
				// only waiting until the deactivations are replicated to
				// the local ticket cache. The distributed/eventually
				// consistent nature of om-core implementation means there
				// is no feasible way to wait for replication to every
				// instance.
				resultCopy := result.GetMatch()
				tdwg.Add(1)
				go func(res *pb.Match) {
					// Local logger with a flag to indicate logs are from this goroutine.
					logger := logger.WithFields(logrus.Fields{"stage": "deactivation"})
					defer tdwg.Done()

					// Get ticket ids to deactivate from all rosters in this match.
					// TODO: This section /could/ be optimized by
					// de-duplicating tickets before sending them for
					// deactivation, but we'd only expect big gains doing that
					// if there are /lots/ of tickets appearing in multiple
					// matches. Since we expect /most/ matchmakers to try and
					// minimize collisions, it's likely a premature
					// optimization to do this before we hear concrete feedback
					// that it is a performance bottleneck.
					ticketIdsToDeactivate := []string{}
					for _, roster := range res.GetRosters() {
						for _, ticket := range roster.GetTickets() {
							ticketIdsToDeactivate = append(ticketIdsToDeactivate, ticket.GetId())
						}
					}

					// Kick off deactivation
					logger.Tracef("deactivating tickets in %v", res.GetId())
					errs := updateTicketsActiveState(ctx, logger, &tc, ticketIdsToDeactivate, store.Deactivate)
					if len(errs) > 0 {
						logger.Errorf("Error deactivating match %v tickets: %v", res.GetId(), err)
					}
					otelMmfTicketDeactivations.Add(ctx, int64(min(0, len(ticketIdsToDeactivate)-len(errs))),
						metric.WithAttributes(
							attribute.String("mmf.name", mmf.GetName()),
							attribute.String("profile.name", profileName),
						),
					)

					logger.Tracef("Done requesting deactivation of tickets in %v", res.GetId())

					// Function to check if deactivation of the last ticket in
					// this match's rosters has been replicated to the local
					// cache.
					deactivationCheck := func(ticketId string) {
						// We'd never expect the deactivation to take this
						// long to replicate, but best practices require
						// /some/ timeout.
						timeout := time.After(time.Second * 60)
						select {
						case <-timeout:
							// Log the timeout and continue.
							logger.Errorf("Timeout while waiting for ticket %v deactivation to be replicated to local cache", ticketId)
							return
						default:
							// There is always /some/ replication delay, sleep
							// before first check.
							time.Sleep(100 * time.Millisecond)
							if _, replComplete := tc.InactiveSet.Load(ticketId); replComplete == true {
								logger.Tracef("deactivation of ticket %v replicated to local cache", ticketId)
								return
							}
						}
					}

					// Option 1: Wait for deactivation to complete BEFORE
					// returning the match.
					if cfg.GetBool("OM_MATCH_TICKET_DEACTIVATION_WAIT") {
						deactivationCheck(ticketIdsToDeactivate[len(ticketIdsToDeactivate)-1])
						logger.Trace("ticket deactivations complete, returning match")
					}

					// Send back the match on the channel, which is consumed
					// in the MMF Result fan-in goroutine above.
					logger.WithFields(logrus.Fields{
						"match_id": res.GetId(),
						"stage":    "queuing_for_send",
					}).Trace("sending match to InvokeMMF() output queue")
					if ctx.Err() != nil { // context cancelled
						// In this case, just exit; there's no need to
						// stream back the rest of the results.
						logger.WithFields(logrus.Fields{
							"error": ctx.Err(),
							"stage": "queuing_for_send",
						}).Warn("context cancelled")
						return
					}
					matchChan <- res

					// Option 2: Match already returned; clean up
					// goroutine when deactivation is complete.
					if !cfg.GetBool("OM_MATCH_TICKET_DEACTIVATION_WAIT") {
						deactivationCheck(ticketIdsToDeactivate[len(ticketIdsToDeactivate)-1])
						logger.Trace("ticket deactivations complete for previously returned match")
					}
					return
				}(resultCopy) // End of match return & ticket deactivation goroutine

			} // End of match stream receiving loop

			// Track matches received from mmfs
			otelMatches.Add(ctx, i, metric.WithAttributes(
				attribute.String("mmf.name", mmf.GetName()),
				attribute.String("profile.name", profileName),
			))

			// Wait for all match ticket deactivation goroutines to complete.
			tdwg.Wait()

			// io.EOF indicates the MMF server closed the stream (mmf is
			// complete), so don't log if that's the error we received (it's
			// not a failure).
			if err != nil && !errors.Is(err, io.EOF) {
				// Track matches received from mmfs
				otelMatches.Add(context.Background(), 1, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
				))
				logger.Error(err)
			}

			// All done
			logger.Trace("async call to mmf complete")
			return err //nolint:errcheck
		}(mmf) //nolint:errcheck // End of MMF fan-out goroutine.
	} // end of for loop that runs once per MMF in the requested profile.

	// Wait for all mmfs to complete (that is, all copies of the MMF fan-out
	// goroutine have exited).
	mmfwg.Wait()

	// Close matchChan, which will cause the MMF Result fan-in goroutine to exit.
	logger.Trace("MMF waitgroup done, closing matchChan")
	close(matchChan)

	return nil
}

//----------------------------------------------------------------------------------
// [DEPRECATED] CreateAssignments()
// [DEPRECATED] WatchAssignments()
//----------------------------------------------------------------------------------

// CreateAssignments should be considered deprecated, and is only provided for
// use by developers when programming against om-core. In production, having
// the matchmaker handle this functionality doesn't make for clean failure
// domains and introduces unnecessary load on the matchmaker.
// Functionally, CreateAssignments makes replication updates for each ticket in
// the provided roster, assigning it to the server provided in the roster's
// Assignment field.
func (s *grpcServer) CreateAssignments(parentCtx context.Context, req *pb.CreateAssignmentsRequest) (*pb.CreateAssignmentsResponse, error) {
	logger := logger.WithFields(logrus.Fields{
		"stage": "handling",
		"rpc":   "CreateAssignments",
	})

	// Input validation
	if req.GetAssignmentRoster() == nil || req.GetAssignmentRoster().GetAssignment() == nil {
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument, "roster with assignment is required"))
	}
	assignmentPb, err := proto.Marshal(req.GetAssignmentRoster().GetAssignment())
	if assignmentPb == nil || err != nil {
		err = errors.Wrap(err, "failed to marshal the assignment protobuf")
		logger.Errorf("Error: %v", err)
		return nil, err
	}

	rChan := make(chan *store.StateResponse, len(req.GetAssignmentRoster().GetTickets()))
	numUpdates := 0
	for _, ticket := range req.GetAssignmentRoster().GetTickets() {
		if ticket.GetId() != "" {
			logger.Debugf("Assignment request ticket id: %v", ticket.GetId())
			tc.UpRequests <- &cache.UpdateRequest{
				// This command is replicated to all other om-core instances
				// using the batch writing async goroutine
				// tc.OutgoingReplicationQueue() and its effect is applied to the
				// local ticket cache in the update processing async goroutine
				// tc.IncomingReplicationQueue().
				Update: store.StateUpdate{
					Cmd:   store.Assign,
					Key:   ticket.GetId(),
					Value: string(assignmentPb[:]),
				},
				ResultsChan: rChan,
				Ctx:         parentCtx,
			}
			numUpdates++
		} else {
			// Note the error but continue processing
			err = status.Error(codes.Internal, "Empty ticket ID in assignment request!")
			logger.Error(err)
		}
	}

	for i := 0; i < numUpdates; i++ {
		// The results.result field contains the redis stream id (aka the replication ID)
		// We don't actually need this for anything, so just check for an error
		results := <-rChan

		if results.Err != nil {
			// Wrap redis error and give it a gRPC internal server error status code
			err = status.Error(codes.Internal, fmt.Errorf("Unable to delete ticket: %w", results.Err).Error())
			logger.Error(err)
		}
	}

	logger.Debugf("DEPRECATED CreateAssignments: %v tickets given assignment \"%v\"", numUpdates, req.GetAssignmentRoster().GetAssignment().GetConnection())

	// Record metrics
	if meter != nil {
		otelAssignmentsPerCall.Record(parentCtx, int64(numUpdates))
	}

	return &pb.CreateAssignmentsResponse{}, nil
}

// WatchAssignments should be considered deprecated, and is only provided for
// use by developers when programming against om-core. In production, having
// the matchmaker handle this functionality doesn't make for clean failure
// domains and introduces unnecessary load on the matchmaker.
// Functionally, it asynchronously watches for exactly one replication update
// containing an assignment for each of the provided ticket ids, and streams
// those assignments back to the caller.  This is rather limited functionality
// as reflected by this function's deprecated status.
func (s *grpcServer) WatchAssignments(req *pb.WatchAssignmentsRequest, stream pb.OpenMatchService_WatchAssignmentsServer) error {
	logger := logger.WithFields(logrus.Fields{
		"stage": "handling",
		"rpc":   "WatchAssignments",
	})

	logger.Debugf("ticketids to watch: %v", req.GetTicketIds())

	// var init
	newestTicketCtime := time.Now()
	updateChan := make(chan *pb.StreamedWatchAssignmentsResponse)
	defer close(updateChan)

	for _, id := range req.GetTicketIds() {
		// Launch an asynch goroutine for each ticket ID in the request that just loops,
		// checking the sync.Map for an assignment. Once one is found, it puts that in the
		// channel, then exits.
		go func(ticketId string) {
			for {
				if value, ok := tc.Assignments.Load(ticketId); ok {
					assignment := value.(*pb.Assignment)
					logger.Debugf(" watchassignments loop got assignment %v for ticketid: %v",
						assignment.GetConnection(), ticketId)
					updateChan <- &pb.StreamedWatchAssignmentsResponse{
						Assignment: assignment,
						Id:         ticketId,
					}
					return
				}
				// TODO exp bo + jitter. Not a priority since this API is
				// marked as deprecated.
				time.Sleep(1 * time.Second)
			}
		}(id)

		// Get creation timestamp from ID
		x, err := strconv.ParseInt(strings.Split(id, "-")[0], 0, 64)
		if err != nil {
			logger.Error(err)
		}
		ticketCtime := time.UnixMilli(x)

		// find the newest ticket creation time.
		if ticketCtime.After(newestTicketCtime) {
			newestTicketCtime = ticketCtime
		}
	}
	otelAssignmentWatches.Add(context.Background(), int64(len(req.GetTicketIds())))

	// loop once per result we want to get.
	// Exit when we've gotten all results or timeout is reached.
	// From newest ticket id, get the ticket creation time and deduce the max
	// time to wait from it's creation time + configured TTL
	// This is a very naive implementation. This functionality is deprecated.
	timeout := time.After(time.Until(newestTicketCtime.Add(time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS")))))
	var err error
	for i, _ := range req.GetTicketIds() {
		select {
		case thisAssignment := <-updateChan:
			otelAssignmentWatches.Add(context.Background(), -1)
			err = stream.Send(thisAssignment)
			if err != nil {
				logger.Error(err)
			}
		case <-timeout:
			otelAssignmentWatches.Add(context.Background(), int64(len(req.GetTicketIds())-i))
			return nil
		}
	}

	return nil
}

//----------------------------------------------------------------------------------
// Helper functions
//----------------------------------------------------------------------------------

// addErrorDetails returns a new status that adds error details for invalid
// ticketid input arguments to the provided status.
//
// gRPC API guidelines explains more about best practices using these.
// https://cloud.google.com/apis/design
//
// 'errs' input map keys should be the ticketId that generated the error.
func addErrorDetails(errs map[string]error, currentStatus *status.Status) error {

	// Generate error details
	deets := &errdetails.BadRequest{}
	for ticketId, e := range errs {
		deets.FieldViolations = append(deets.FieldViolations, &errdetails.BadRequest_FieldViolation{
			Field:       fmt.Sprintf("ticket_id/%v", ticketId),
			Description: e.Error(),
		})
	}

	// Add error details
	var err error
	detailedStatus, err := currentStatus.WithDetails(deets)
	if err != nil {
		logger.Errorf("Unexpected error while updating tickets: %v", err)
	}
	return fmt.Errorf("%w", detailedStatus.Err())
}

// syncMapDump is a simple helper function to convert a sync.Map into a
// standard golang map. Since a standard map is not concurrent-safe, use this
// with caution. Most of the places this is used are either for debugging the
// internal state of the om-core ticket cache, or in portions of the code where
// the implementation depends on taking a 'point-in-time' snapshot of the
// ticket cache because we're sending it to another process using invokeMMF().
func syncMapDump(sm *sync.Map) map[string]interface{} {
	out := map[string]interface{}{}
	sm.Range(func(key, value interface{}) bool {
		out[fmt.Sprint(key)] = value
		return true
	})
	return out
}

// setDifference is a simple helper function that performs a difference
// operation on two sets that are using sync.Map as their underlying data type.
//
// NOTE the limitation that this is taking a 'point-in-time' snapshot of the
// two sets, as they are still being updated asynchronously in a number of
// other goroutines. The rest of the design of OM takes this into account.
func setDifference(tix *sync.Map, inactiveSet *sync.Map) (activeTickets []any) {
	inactiveTicketIds := syncMapDump(inactiveSet)
	// Copy the ticket cache, leaving out inactive tickets.
	tix.Range(func(id, ticket any) bool {
		// not ok means an error was encountered, indicating this
		// ticket is NOT inactive (meaning it IS active)
		if _, ok := inactiveTicketIds[id.(string)]; !ok {
			activeTickets = append(activeTickets, ticket)
		}
		return true
	})
	return
}
