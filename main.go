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
	"net"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"

	"github.com/googleforgames/open-match2/v2/internal/config"
	"github.com/googleforgames/open-match2/v2/internal/filter"
	"github.com/googleforgames/open-match2/v2/internal/logging"
	"github.com/googleforgames/open-match2/v2/internal/mmfauth"
	"github.com/googleforgames/open-match2/v2/internal/statestore/cache"
	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	memoryReplicator "github.com/googleforgames/open-match2/v2/internal/statestore/memory"
	redisReplicator "github.com/googleforgames/open-match2/v2/internal/statestore/redis"

	"github.com/davecgh/go-spew/spew"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// MatchDeactivationTimeoutExtensionKey is the reserved Match.Extensions key set to a
// boolean true wrapperspb.BoolValue when waiting for match ticket deactivations to
// replicate to the local cache times out or fails.
const MatchDeactivationTimeoutExtensionKey = "deactivation_timeout"

// MatchInvalidTicketIdsExtensionKey is the reserved Match.Extensions key whose value is
// a google.protobuf.Any wrapping a google.protobuf.ListValue (*structpb.ListValue) of
// string values listing every roster ticket ID in the match that failed ticket ID
// validation (om-core does not and cannot deactivate these tickets). It is set only
// when at least one roster ticket ID is invalid, and is set in both deactivation modes
// (whether OM_MATCH_TICKET_DEACTIVATION_WAIT is true or false).
const MatchInvalidTicketIdsExtensionKey = "invalid_ticket_ids"

// MatchDeactivationFailedTicketIdsExtensionKey is the reserved Match.Extensions key whose
// value is a google.protobuf.Any wrapping a google.protobuf.ListValue (*structpb.ListValue)
// of string values listing every valid roster ticket ID whose deactivation write to state
// storage failed, so the ticket may still be active and could be matched again. It is set
// only when OM_MATCH_TICKET_DEACTIVATION_WAIT is true (when false, the match is returned
// before deactivation is attempted) and only when at least one deactivation write failed.
// If every deactivation write failed, MatchDeactivationTimeoutExtensionKey is also set.
const MatchDeactivationFailedTicketIdsExtensionKey = "deactivation_failed_ticket_ids"

// mmfFailureErrorInfoReason and mmfFailureErrorInfoDomain identify the errdetails.ErrorInfo
// entries (one per failed MMF) attached to the gRPC status returned by InvokeMatchmakingFunctions
// when any invoked MMF fails.
const (
	mmfFailureErrorInfoReason = "MMF_FAILURE"
	mmfFailureErrorInfoDomain = "open-match"
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
	MMFTimeoutError     = errors.New("MMF deadline in om-core (specified in OM_MMF_TIMEOUT_SECS config var) exceeded")
	MMFsComplete        = errors.New("All MMF streams monitored by om-core have completed (io.EOF)")

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

	idTokenCache = mmfauth.NewIDTokenCache(func(ctx context.Context, audience string) (oauth2.TokenSource, error) {
		return idtoken.NewTokenSource(ctx, audience)
	})
)

func main() {
	// Read configuration env vars, and configure logging
	cfg = config.Read()
	logging.ConfigureLogging(cfg)

	// Make a parent context that gets cancelled on SIGINT or SIGTERM
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	if cfg.GetBool("OM_OTEL_SIDECAR") {
		meter, otelShutdownFunc = initializeOtel()
	} else {
		meter, otelShutdownFunc = initializeOtelWithLocalProm()
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = otelShutdownFunc(shutdownCtx)
	}()
	registerMetrics(meter)
	cache.RegisterMetrics(meter)

	// Set up the state replicator and initialize the replicated ticket cache.
	var replicator store.StateReplicator
	switch cfg.GetString("OM_STATE_STORAGE_TYPE") {
	case "redis":
		// Default: use redis
		rr, err := redisReplicator.New(cfg)
		if err != nil {
			logger.Fatalf("Redis Failure; exiting: %v", err)
		}
		defer func() {
			if closeErr := rr.Close(); closeErr != nil {
				logger.Warnf("Error closing Redis replicator: %v", closeErr)
			}
		}()
		replicator = rr
	case "memory":
		// NOT RECOMMENDED FOR PRODUCTION
		//
		// Store state in local memory only. Every copy of om-core spun up
		// using this configuration is an island which does not send or receive
		// any updates to/from any other instances.
		// Useful for debugging, local development, etc.
		logger.Warnf("OM_STATE_STORAGE_TYPE configuration variable set to 'memory'. NOT RECOMMENDED FOR PRODUCTION")
		replicator = memoryReplicator.New(cfg)
	default:
		logger.Fatalf("Unknown OM_STATE_STORAGE_TYPE: %v", cfg.GetString("OM_STATE_STORAGE_TYPE"))
	}
	tc.Init(cfg, replicator)

	// These goroutines send and receive cache updates from state storage
	go tc.OutgoingReplicationQueue(ctx)
	go tc.IncomingReplicationQueue(ctx)

	// Start the gRPC server
	start(ctx, cfg)

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
		code := status.Code(err)
		if code == codes.Unknown || code == codes.OK {
			code = codes.Internal
		}
		err = status.Error(code,
			fmt.Errorf("CreateTicket failed to marshal the provided ticket to the state storage format: %w", err).Error())
		logger.Error(err)
		return &pb.CreateTicketResponse{}, fmt.Errorf("%w", err)
	}

	// Make a 1-buffered results return channel so background workers cannot block if the caller times out.
	rChan := make(chan *store.StateResponse, 1)

	// Queue our ticket creation cache update request to be replicated to all
	// om-core instances.
	upReq := &cache.UpdateRequest{
		// This command (writing a ticket to the cache) is replicated to all
		// other om-core instances using the batch writing async goroutine
		// tc.OutgoingReplicationQueue() and the update itself is applied to
		// the local ticket cache in the update-processing async goroutine
		// tc.IncomingReplicationQueue().
		ResultsChan: rChan,
		Ctx:         parentCtx,
		EnqueuedAt:  time.Now(),
		Update: store.StateUpdate{
			Cmd:   store.Ticket,
			Value: string(ticketPb[:]),
		},
	}

	select {
	case tc.UpRequests <- upReq:
	case <-parentCtx.Done():
		return &pb.CreateTicketResponse{}, status.FromContextError(parentCtx.Err()).Err()
	}

	// Get the results
	var results *store.StateResponse
	select {
	case results = <-rChan:
	case <-parentCtx.Done():
		return &pb.CreateTicketResponse{}, status.FromContextError(parentCtx.Err()).Err()
	}

	if results.Err == nil {
		if otelTicketCreations != nil {
			otelTicketCreations.Add(parentCtx, 1)
		}
		if otelTicketSize != nil {
			otelTicketSize.Record(parentCtx, float64(len(ticketPb))/1024.0)
		}
	}

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
	// not a longer one. The default expiration time is creation time + the
	// configured ticket TTL, while the maximum allowed explicit expiration time
	// is now + the configured ticket TTL.
	ttlDuration := time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))
	exTime := req.GetTicket().GetExpirationTime()
	maxExTime := time.Now().Add(ttlDuration)

	switch {
	case exTime == nil:
		// This is our expected code path: user provides an empty expiration
		// time and om-core fills it in as CreationTime + OM_CACHE_TICKET_TTL_MS.
		logger.Tracef("ExpirationTime provided is missing; "+
			"replacing with creation time + OM_CACHE_TICKET_TTL_MS (%v)",
			cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))
		exTime = timestamppb.New(crTime.AsTime().Add(ttlDuration))
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

// DeactivateTickets is a lazy deletion process: it adds the provided ticketIDs to the inactive list,
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

// deactivateTickets is the package-internal implementation of the grpc server
// DeactivateTickets function. Basically, this exists as a separate function only so
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

	// Send the ticket deactivation updates.
	updateStateErrDetails, _ := updateTicketsActiveState(ctx, logger, tc, validTicketIds, store.Deactivate)

	// Record metrics before merging invalid ID errors into updateStateErrDetails.
	if meter != nil {
		otelDeactivationsPerCall.Record(ctx, int64(len(validTicketIds)))
		otelInvalidIdsPerDeactivateCall.Record(ctx, int64(len(invalidIdErrorDetails)))
		otelFailedIdsPerDeactivateCall.Record(ctx, int64(len(updateStateErrDetails)))
	}

	// Attach error details we found while validating ticket IDs and
	// replicating the updates.
	maps.Copy(updateStateErrDetails, invalidIdErrorDetails)
	if len(updateStateErrDetails) > 0 {
		var baseStatus *status.Status
		if err != nil {
			baseStatus = status.New(codes.InvalidArgument, err.Error())
		} else {
			baseStatus = status.New(codes.Internal, "unable to update ticket state")
		}
		err = addErrorDetails(updateStateErrDetails, baseStatus)
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

// activateTickets is the package-internal implementation of the grpc server
// ActivateTickets function. Basically, this exists as a separate function only so
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
	updateStateErrDetails, _ := updateTicketsActiveState(ctx, logger, tc, validTicketIds, store.Activate)

	// Record metrics before merging invalid ID errors into updateStateErrDetails.
	if meter != nil {
		otelActivationsPerCall.Record(ctx, int64(len(validTicketIds)))
		otelInvalidIdsPerActivateCall.Record(ctx, int64(len(invalidIdErrorDetails)))
		otelFailedIdsPerActivateCall.Record(ctx, int64(len(updateStateErrDetails)))
	}

	// Attach error details we found while validating ticket IDs and
	// replicating the updates.
	maps.Copy(updateStateErrDetails, invalidIdErrorDetails)
	if len(updateStateErrDetails) > 0 {
		var baseStatus *status.Status
		if err != nil {
			baseStatus = status.New(codes.InvalidArgument, err.Error())
		} else {
			baseStatus = status.New(codes.Internal, "unable to update ticket state")
		}
		err = addErrorDetails(updateStateErrDetails, baseStatus)
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

	traceEnabled := logger != nil && logger.Logger != nil && logger.Logger.IsLevelEnabled(logrus.TraceLevel)

	// Validate ids of requested updates
	if numReqUpdates > 0 {
		// check each id in the input list, making separate lists of those that are
		// valid and those that are invalid.
		for _, id := range ticketIds {
			if idValidator.MatchString(id) {
				validIds = append(validIds, id)
				if traceEnabled {
					logger.WithFields(logrus.Fields{"ticket_id": id}).Trace("valid ticket id")
				}
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
// generates cache updates for each, returning both a map of failed ticket IDs
// to errors and the highest replication ID assigned by state storage.
//
// NOTE This function does no input validation, the calling function has to handle that.
func updateTicketsActiveState(parentCtx context.Context, logger *logrus.Entry, tc *cache.ReplicatedTicketCache, ticketIds []string, command int) (map[string]error, string) {
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
	if len(ticketIds) == 0 {
		return errs, ""
	}

	formatUpdateErr := func(ticketId string, updateErr error) error {
		return status.Errorf(codes.Internal, "Unable to update ticket %v state to %v : %v", ticketId, requestedStateAsString, updateErr)
	}

	packUpdates := false
	if tc != nil && tc.Cfg != nil && tc.Cfg.IsSet("OM_CACHE_PACK_TICKET_STATE_UPDATES") {
		packUpdates = tc.Cfg.GetBool("OM_CACHE_PACK_TICKET_STATE_UPDATES")
	} else if cfg != nil {
		packUpdates = cfg.GetBool("OM_CACHE_PACK_TICKET_STATE_UPDATES")
	}

	// Packed path: send all ticket IDs in a single packed UpdateRequest.
	if packUpdates {
		rChan := make(chan *store.StateResponse, 1)
		packedKeys := make([]string, len(ticketIds))
		copy(packedKeys, ticketIds)

		upReq := &cache.UpdateRequest{
			ResultsChan: rChan,
			Ctx:         parentCtx,
			EnqueuedAt:  time.Now(),
			Update: store.StateUpdate{
				Cmd:  command,
				Key:  packedKeys[0],
				Keys: packedKeys,
			},
		}

		select {
		case tc.UpRequests <- upReq:
		case <-parentCtx.Done():
			ctxErr := status.FromContextError(parentCtx.Err()).Err()
			for _, id := range ticketIds {
				errs[id] = ctxErr
			}
			return errs, ""
		}

		var r *store.StateResponse
		select {
		case r = <-rChan:
		case <-parentCtx.Done():
			ctxErr := status.FromContextError(parentCtx.Err()).Err()
			for _, id := range ticketIds {
				errs[id] = ctxErr
			}
			return errs, ""
		}

		var highestReplId string
		if r.Err != nil {
			for _, id := range ticketIds {
				errs[id] = formatUpdateErr(id, r.Err)
			}
			logger.Error(errs[ticketIds[0]])
		} else if r.Result != "" {
			highestReplId = r.Result
		}
		return errs, highestReplId
	}

	// Unpacked path: allocate a distinct 1-buffered response channel per ticket ID.
	// Never close these channels on early return so background workers cannot panic on late sends.
	rChans := make([]chan *store.StateResponse, len(ticketIds))
	for i := range rChans {
		rChans[i] = make(chan *store.StateResponse, 1)
	}

	traceEnabled := logger != nil && logger.Logger != nil && logger.Logger.IsLevelEnabled(logrus.TraceLevel)
	logger.Trace("queuing updates for ticket state change")

	enqueuedIds := ticketIds
enqueueLoop:
	for i, id := range ticketIds {
		upReq := &cache.UpdateRequest{
			ResultsChan: rChans[i],
			Ctx:         parentCtx,
			EnqueuedAt:  time.Now(),
			Update: store.StateUpdate{
				Cmd: command,
				Key: id,
			},
		}
		select {
		case tc.UpRequests <- upReq:
			if traceEnabled {
				logger.WithFields(logrus.Fields{
					"ticket_id": id,
				}).Trace("generated request to update ticket status")
			}
		case <-parentCtx.Done():
			ctxErr := status.FromContextError(parentCtx.Err()).Err()
			for _, remId := range ticketIds[i:] {
				errs[remId] = ctxErr
			}
			enqueuedIds = ticketIds[:i]
			break enqueueLoop
		}
	}

	var highestReplId string
	for i := 0; i < len(enqueuedIds); i++ {
		var r *store.StateResponse
		select {
		case r = <-rChans[i]:
		case <-parentCtx.Done():
			ctxErr := status.FromContextError(parentCtx.Err()).Err()
			for _, remId := range enqueuedIds[i:] {
				errs[remId] = ctxErr
			}
			return errs, highestReplId
		}

		if r.Err != nil {
			failedId := r.Result
			if failedId == "" {
				failedId = enqueuedIds[i]
			}
			errs[failedId] = formatUpdateErr(failedId, r.Err)
			logger.Error(errs[failedId])
		} else if r.Result != "" && store.CompareReplIds(r.Result, highestReplId) > 0 {
			highestReplId = r.Result
		}
	}
	return errs, highestReplId
}

//----------------------------------------------------------------------------------
// Matchmaking Helpers (Chunking, URL Parsing, Deactivation Wait)
//----------------------------------------------------------------------------------

const (
	// mmfChunkSoftLimitBytes is the 4 MiB soft limit every message om-core sends to an MMF
	// must stay under. It is gRPC's default maximum received message size; some middleware
	// (proxies, service meshes, load balancers) enforces the same limit and silently
	// discards larger messages. Staying under it means MMFs work with default gRPC settings
	// and through that middleware without anyone raising limits.
	mmfChunkSoftLimitBytes = 4 * 1024 * 1024

	// mmfChunkFillTargetBytes is the size each chunk is filled up to. Ticket and pool bytes
	// counted are exact (measured from the generated protobuf messages), but the length
	// prefixes of enclosing messages (the profile, each pool map entry, each pool, each
	// pool's participant roster) are measured while those messages are empty and can each
	// grow by a few bytes as their varint length prefixes widen while the chunk fills. The
	// 194,304-byte difference between the target and the soft limit leaves far more room
	// than that growth can use.
	mmfChunkFillTargetBytes = 4 * 1000 * 1000
)

// poolParticipantRosterName returns the name of the roster holding a pool's tickets
// in requests sent to MMFs.
func poolParticipantRosterName(poolName string) string {
	return poolName + "_roster"
}

// encodedRosterTicketSize returns the serialized size of a single ticket inside a
// Roster's repeated tickets field (the ticket's bytes plus the field tag and length
// prefix for one element of a repeated message field). It is measured with the generated
// protobuf message rather than computed from hardcoded field numbers or wire-format
// constants, so it stays correct if the protobuf definitions change.
func encodedRosterTicketSize(ticket *pb.Ticket) int {
	return proto.Size(&pb.Roster{Tickets: []*pb.Ticket{ticket}}) - proto.Size(&pb.Roster{})
}

// packPoolTicketChunks filters the active tickets into the profile's valid pools and
// splits the result into chunks that each serialize (with the rest of the request, as
// built by getChunkedRequests) to less than mmfChunkSoftLimitBytes. Every chunk contains
// every valid pool (possibly empty) plus a portion of the pools' tickets; MMFs
// re-assemble pools with a simple loop and concatenation over all chunks.
func packPoolTicketChunks(reqProfile *pb.Profile, validPools map[string][]*pb.Ticket, activeTickets map[any]any) []map[string][]*pb.Ticket {
	emptyChunk := getChunkedRequests(reqProfile, nil)[0]
	emptyChunk.NumChunks = math.MaxInt32
	emptyChunkSize := proto.Size(emptyChunk)
	curChunkSize := emptyChunkSize

	initialChunk := make(map[string][]*pb.Ticket, len(validPools))
	maps.Copy(initialChunk, validPools)
	chunkedPools := []map[string][]*pb.Ticket{initialChunk}
	chunkIndex := 0

	profilePools := reqProfile.GetPools()
	for _, rawTicket := range activeTickets {
		t, ok := rawTicket.(*pb.Ticket)
		if !ok || t == nil {
			continue
		}
		ticketSize := -1
		for poolName := range validPools {
			if !filter.In(profilePools[poolName], t) {
				continue
			}
			if ticketSize < 0 {
				ticketSize = encodedRosterTicketSize(t)
			}
			if curChunkSize+ticketSize > mmfChunkFillTargetBytes {
				curChunkSize = emptyChunkSize
				chunkIndex++
				chunkedPools = append(chunkedPools, map[string][]*pb.Ticket{})
			}
			chunkedPools[chunkIndex][poolName] = append(chunkedPools[chunkIndex][poolName], t)
			curChunkSize += ticketSize
		}
	}

	return chunkedPools
}

func getChunkedRequests(reqProfile *pb.Profile, chunkedPools []map[string][]*pb.Ticket) []*pb.ChunkedMmfRunRequest {
	if len(chunkedPools) == 0 {
		chunkedPools = []map[string][]*pb.Ticket{{}}
	}

	chunkedRequests := make([]*pb.ChunkedMmfRunRequest, len(chunkedPools))
	for chunkIndex, chunk := range chunkedPools {
		logger.Debugf("processing chunk %v ", chunkIndex)

		pools := make(map[string]*pb.Pool)
		profile := &pb.Profile{
			Name:       reqProfile.GetName(),
			Pools:      pools,
			Extensions: reqProfile.GetExtensions(),
		}

		for name, participantRoster := range chunk {
			logger.Debugf("making chunk containing %v tickets", len(participantRoster))
			var poolCopy *pb.Pool
			if srcPool := reqProfile.GetPools()[name]; srcPool != nil {
				poolCopy = proto.Clone(srcPool).(*pb.Pool)
			} else {
				poolCopy = &pb.Pool{Name: name}
			}
			if participantRoster == nil {
				participantRoster = []*pb.Ticket{}
			}
			poolCopy.Participants = &pb.Roster{
				Name:    poolParticipantRosterName(name),
				Tickets: participantRoster,
			}
			pools[name] = poolCopy
		}

		for name, srcPool := range reqProfile.GetPools() {
			if _, exists := pools[name]; exists || srcPool == nil {
				continue
			}
			if valid, _ := filter.ValidatePoolFilters(srcPool); valid {
				poolCopy := proto.Clone(srcPool).(*pb.Pool)
				poolCopy.Participants = &pb.Roster{
					Name:    poolParticipantRosterName(name),
					Tickets: []*pb.Ticket{},
				}
				pools[name] = poolCopy
			}
		}

		chunkedRequests[chunkIndex] = &pb.ChunkedMmfRunRequest{
			Profile:   profile,
			NumChunks: int32(len(chunkedPools)),
		}
	}

	return chunkedRequests
}

func parseMmfUrl(mmf *pb.MatchmakingFunctionSpec) (*url.URL, error) {
	if mmf == nil {
		return nil, errors.New("mmf spec is nil")
	}

	rawHost := strings.TrimSpace(mmf.GetHost())
	if rawHost == "" {
		return nil, errors.New("mmf host is empty")
	}

	specPort := mmf.GetPort()
	if specPort < 0 || specPort > 65535 {
		return nil, fmt.Errorf("mmf port %d out of range [0, 65535]", specPort)
	}

	target := rawHost
	if !strings.Contains(target, "://") {
		if ip := net.ParseIP(target); ip != nil && strings.Contains(target, ":") {
			target = "[" + target + "]"
		}
		target = "http://" + target
	}

	parsedUrl, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("invalid mmf host %q: %w", rawHost, err)
	}

	scheme := strings.ToLower(parsedUrl.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("unsupported mmf scheme %q", parsedUrl.Scheme)
	}
	parsedUrl.Scheme = scheme

	hostname := parsedUrl.Hostname()
	if hostname == "" {
		return nil, errors.New("mmf host is missing hostname")
	}

	urlPort := parsedUrl.Port()
	if urlPort != "" {
		p, convErr := strconv.Atoi(urlPort)
		if convErr != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %q in mmf host", urlPort)
		}
	}

	var resolvedPort string
	switch {
	case specPort > 0:
		resolvedPort = strconv.Itoa(int(specPort))
	case urlPort != "":
		resolvedPort = urlPort
	case parsedUrl.Scheme == "https":
		resolvedPort = "443"
	default:
		resolvedPort = "80"
	}

	parsedUrl.Host = net.JoinHostPort(hostname, resolvedPort)
	return parsedUrl, nil
}

func annotateMatchDeactivationTimeout(match *pb.Match) {
	if match == nil {
		return
	}
	timeoutAny, err := anypb.New(wrapperspb.Bool(true))
	if err != nil {
		logger.Errorf("Failed to marshal deactivation_timeout extension: %v", err)
		return
	}
	if match.Extensions == nil {
		match.Extensions = make(map[string]*anypb.Any)
	}
	match.Extensions[MatchDeactivationTimeoutExtensionKey] = timeoutAny
}

func annotateMatchTicketIdList(match *pb.Match, key string, ids []string) {
	if match == nil || len(ids) == 0 {
		return
	}
	vals := make([]any, len(ids))
	for idx, id := range ids {
		vals[idx] = id
	}
	listVal, err := structpb.NewList(vals)
	if err != nil {
		logger.Errorf("Failed to build %s extension: %v", key, err)
		return
	}
	extAny, err := anypb.New(listVal)
	if err != nil {
		logger.Errorf("Failed to marshal %s extension: %v", key, err)
		return
	}
	if match.Extensions == nil {
		match.Extensions = make(map[string]*anypb.Any)
	}
	match.Extensions[key] = extAny
}

// annotateMatchInvalidTicketIds mutates match to store invalidIds under
// MatchInvalidTicketIdsExtensionKey. Because it mutates the match, it must only
// be called before the match is handed off for sending to avoid a data race.
func annotateMatchInvalidTicketIds(match *pb.Match, invalidIds []string) {
	annotateMatchTicketIdList(match, MatchInvalidTicketIdsExtensionKey, invalidIds)
}

// annotateMatchDeactivationFailedTicketIds mutates match to store failedIds under
// MatchDeactivationFailedTicketIdsExtensionKey. It is only used when
// OM_MATCH_TICKET_DEACTIVATION_WAIT is true, and because it mutates the match,
// it must only be called before the match is handed off for sending to avoid a data race.
func annotateMatchDeactivationFailedTicketIds(match *pb.Match, failedIds []string) {
	annotateMatchTicketIdList(match, MatchDeactivationFailedTicketIdsExtensionKey, failedIds)
}

type mmfFailureRecord struct {
	mmfName string
	err     error
}

func unwrapGRPCStatus(err error) *status.Status {
	var grpcStatusErr interface {
		GRPCStatus() *status.Status
	}
	if errors.As(err, &grpcStatusErr) {
		return grpcStatusErr.GRPCStatus()
	}
	return status.New(codes.Unknown, err.Error())
}

// buildMmfFailureStatus builds the aggregated codes.Internal status returned when one
// or more invoked MMFs fail. Every match the MMFs returned has already been streamed
// to the caller by the time this status is returned, so the caller receives all matches
// followed by this error. One errdetails.ErrorInfo per failed MMF carries the MMF name,
// gRPC code, and message for programmatic inspection.
func buildMmfFailureStatus(totalMmfs int, failures []mmfFailureRecord) error {
	summaryParts := make([]string, 0, len(failures))
	details := make([]protoadapt.MessageV1, 0, len(failures))
	for _, failure := range failures {
		st := unwrapGRPCStatus(failure.err)
		summaryParts = append(summaryParts, fmt.Sprintf("%s: %s", failure.mmfName, st.Message()))
		details = append(details, &errdetails.ErrorInfo{
			Reason: mmfFailureErrorInfoReason,
			Domain: mmfFailureErrorInfoDomain,
			Metadata: map[string]string{
				"mmf_name":  failure.mmfName,
				"grpc_code": st.Code().String(),
				"message":   st.Message(),
			},
		})
	}

	baseStatus := status.New(codes.Internal, fmt.Sprintf(
		"%d of %d matchmaking functions failed: %s",
		len(failures), totalMmfs, strings.Join(summaryParts, "; "),
	))
	detailedStatus, err := baseStatus.WithDetails(details...)
	if err != nil {
		logger.Errorf("Failed to attach MMF failure details to status: %v", err)
		return baseStatus.Err()
	}
	return detailedStatus.Err()
}

func waitForMatchTicketDeactivation(
	ctx context.Context,
	logger *logrus.Entry,
	tc *cache.ReplicatedTicketCache,
	match *pb.Match,
	mmfName string,
	profileName string,
	ticketId string,
	targetReplId string,
	annotateOnTimeout bool,
) bool {
	if ticketId == "" {
		logger.WithFields(logrus.Fields{
			"match_id": match.GetId(),
		}).Warn("No valid deactivated ticket ID to wait on; all deactivation writes failed")
		if annotateOnTimeout {
			annotateMatchDeactivationTimeout(match)
		}
		return false
	}

	timeoutMs := 60000
	if cfg != nil && cfg.GetInt("OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS") > 0 {
		timeoutMs = cfg.GetInt("OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS")
	} else if tc != nil && tc.Cfg != nil && tc.Cfg.GetInt("OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS") > 0 {
		timeoutMs = tc.Cfg.GetInt("OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS")
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	attrs := metric.WithAttributes(
		attribute.String("mmf.name", mmfName),
		attribute.String("profile.name", profileName),
	)

	waitStart := time.Now()
	replicated := tc.WaitForDeactivation(ticketId, targetReplId, timeout)
	waitElapsedMs := float64(time.Since(waitStart).Microseconds()) / 1000.0
	if waitElapsedMs < 0 {
		waitElapsedMs = 0
	}
	if otelMmfDeactivationWait != nil {
		otelMmfDeactivationWait.Record(ctx, waitElapsedMs, attrs)
	}

	if !replicated {
		if otelMmfDeactivationTimeouts != nil {
			otelMmfDeactivationTimeouts.Add(ctx, 1, attrs)
		}
		if annotateOnTimeout {
			annotateMatchDeactivationTimeout(match)
		}
		logger.Errorf("Timeout while waiting for ticket %v deactivation to be replicated to local cache", ticketId)
		return false
	}

	logger.Tracef("deactivation of ticket %v replicated to local cache", ticketId)
	return true
}

//----------------------------------------------------------------------------------
// InvokeMatchmakingFunctions()
//----------------------------------------------------------------------------------

// InvokeMatchmakingFunctions loops through each Pool in the provided Profile,
// applying the filters inside and adding participating tickets to those pools.
// It then attempts to connect to every matchmaking function in the provided
// list, and send the Profile with filled Pools to each. It processes resulting
// matches from each matchmaking function asynchronously as they arrive. For
// each match it receives, it deactivates all valid tickets contained in the match
// and streams the match back to the InvokeMatchmakingFunctions caller.
//
// From the caller's point of view, InvokeMatchmakingFunctions behaves like an
// RPC framework: every response an MMF streams back is forwarded to the caller
// (including matches with no valid tickets and responses with no match at all),
// and if any MMF fails, the stream ends with an error status describing every
// failure after all returned matches have been sent.
//
// NOTE: (complexity linters disabled) The complexity here is unavoidable as we're
// using multiple nested goroutines to process lots of MMFs simultaneously
// while minimizing the time to first reponse for clients.
//
//nolint:gocognit,cyclop,gocyclo,maintidx
func (s *grpcServer) InvokeMatchmakingFunctions(req *pb.MmfRequest, stream pb.OpenMatchService_InvokeMatchmakingFunctionsServer) error {
	startTime := time.Now()

	// Set a timeout for this API call, but ignore context cancellations from
	// the background context. It is fully intended that MMFs can run longer
	// than the matchmaker is willing to wait, so we want them not to get
	// cancelled when the calling matchmaker cancels its context. However, best
	// practices dictate that we define /some/ timeout (default: 10 mins)
	mmfTimeout := time.Duration(cfg.GetInt("OM_MMF_TIMEOUT_SECS")) * time.Second
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(context.Background()))
	ctx, timeoutCancel := context.WithTimeoutCause(ctx, mmfTimeout, MMFTimeoutError)
	defer func() {
		logger.Debugf("MMFs complete, sending context cancellation after %04d ms", time.Since(startTime).Milliseconds())
		timeoutCancel()
		cancel(MMFsComplete)
	}()

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

	// Cold-start catch-up barrier: wait for initial ticket cache replication catch-up
	// before snapshotting active tickets.
	if !tc.IsCaughtUp() {
		catchUpCtx, catchUpCancel := context.WithCancel(ctx)
		stopWatch := make(chan struct{})
		if stream != nil && stream.Context() != nil {
			streamCtx := stream.Context()
			go func() {
				select {
				case <-streamCtx.Done():
					catchUpCancel()
				case <-stopWatch:
				}
			}()
		}
		catchUpErr := tc.WaitForCatchUp(catchUpCtx)
		close(stopWatch)
		catchUpCancel()
		if catchUpErr != nil {
			logger.Warnf("Initial ticket cache replication catch-up did not complete in time: %v", catchUpErr)
			return status.Error(codes.Unavailable, "initial ticket cache replication catch-up did not complete in time")
		}
	}

	prepStart := time.Now()

	// Snapshot active tickets from the materialized ActiveTickets index.
	activeTickets := tc.SnapshotActiveTickets()
	if cfg.GetBool("OM_VERBOSE") {
		unassignedTickets := setDifference(&tc.InactiveSet, &tc.Assignments)
		logger.Infof(" %5d tickets active, %5d tickets inactive without assignment",
			len(activeTickets), len(unassignedTickets))
	} else {
		logger.Infof(" %5d tickets active", len(activeTickets))
	}

	// validate pool filters before filling them
	validPools := map[string][]*pb.Ticket{}
	for name, pool := range req.GetProfile().GetPools() {
		if valid, err := filter.ValidatePoolFilters(pool); valid {
			// Initialize a clean roster for this pool
			validPools[name] = make([]*pb.Ticket, 0)
		} else {
			logger.Errorf("Unable to fill pool with tickets, invalid: %v", err)
		}
	}

	// Filter active tickets into the pools and chunk them into pieces that each
	// stay under the 4 MiB gRPC message soft limit.
	chunkedPools := packPoolTicketChunks(req.GetProfile(), validPools, activeTickets)

	logger.Debugf("%v pools packed into %v chunks", len(validPools), len(chunkedPools))
	chunkedRequest := getChunkedRequests(req.GetProfile(), chunkedPools)

	// Packing should always keep every chunk under the limit; if it ever doesn't
	// (for example after a protobuf change), log it loudly since some middleware
	// silently drops larger messages.
	for chunkIndex, chunk := range chunkedRequest {
		if chunkSize := proto.Size(chunk); chunkSize > mmfChunkSoftLimitBytes {
			logger.Errorf("MMF request chunk %d is %d bytes, over the %d byte gRPC message soft limit; some gRPC middleware may drop it",
				chunkIndex, chunkSize, mmfChunkSoftLimitBytes)
		}
	}

	prepElapsedMs := float64(time.Since(prepStart).Microseconds()) / 1000.0
	if prepElapsedMs < 0 {
		prepElapsedMs = 0
	}
	if otelMmfPrepDuration != nil {
		otelMmfPrepDuration.Record(context.Background(), prepElapsedMs,
			metric.WithAttributes(attribute.String("profile.name", profileName)),
		)
	}

	// MMF Result fan-in goroutine
	// Simple fan-in channel pattern implemented as an async inline goroutine.
	// Asynchronously sends matches to InvokeMatchmakingFunctions() caller as
	// they come in from the concurrently-running MMFs. Exits when all
	// MMFs are complete and matchChan is closed.
	matchChan := make(chan *pb.Match)
	var fanwg sync.WaitGroup
	fanwg.Add(1)
	go func() {
		defer fanwg.Done()
		// Local logger with a field to indicate logs are from this goroutine.
		logger := logger.WithFields(logrus.Fields{"stage": "fan-in"})
		logger.Trace("MMF results fan-in goroutine active")
		for match := range matchChan {
			logger.WithFields(logrus.Fields{
				"match_id": match.GetId(),
			}).Trace("streaming back match to matchmaker")

			// Send the match back to the caller.
			err := stream.Send(&pb.StreamedMmfResponse{Match: match})
			// om-core doesn't retry sending MMF results; if your matchmaker cancels the
			// context or stops responding, om-core drops the rest of the match results.
			if err != nil {
				logger.Errorf("Unable to stream match result back to matchmaker, dropping %v matches: %v, %v", len(matchChan)+1, err, context.Cause(ctx))
				logger.Errorf("dropped: %v", match.GetId())
				for droppedMatch := range matchChan {
					logger.Errorf("dropped: %v", droppedMatch.GetId())
				}
				return
			}
		}
		logger.Trace("ALL MMFS COMPLETE: exiting MMF results fan-in goroutine")
	}()

	// Set up base grpc dial options for all MMFs.
	var baseOpts []grpc.DialOption
	baseOpts = append(baseOpts,
		grpc.WithConnectParams(grpc.ConnectParams{MinConnectTimeout: mmfTimeout}),
	)

	// Invoke each requested MMF, and put the matches they stream back into the match channel.
	// Any MMF failures are collected and returned to the caller after all matches have been
	// streamed so that, like an RPC framework, om-core never hides an MMF failure.
	var mmfwg sync.WaitGroup
	var mmfFailuresMu sync.Mutex
	var mmfFailures []mmfFailureRecord
	for _, mmf := range req.GetMmfs() {
		// Add this invocation to the MMF wait group.
		mmfwg.Add(1)

		// MMF fan-out goroutine
		// Call the MMF asynchronously, so all processing happens concurrently
		go func(mmf *pb.MatchmakingFunctionSpec) (retErr error) {
			defer mmfwg.Done()
			// Registered after mmfwg.Done() so by LIFO ordering it runs BEFORE mmfwg.Done(),
			// guaranteeing every MMF failure is recorded before mmfwg.Wait() returns.
			defer func() {
				if retErr != nil {
					mmfFailuresMu.Lock()
					mmfFailures = append(mmfFailures, mmfFailureRecord{
						mmfName: mmf.GetName(),
						err:     retErr,
					})
					mmfFailuresMu.Unlock()
				}
			}()

			if otelMmfCalls != nil {
				otelMmfCalls.Add(ctx, 1, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
				))
			}

			// Parse the connection URI
			mmfUrl, err := parseMmfUrl(mmf)
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
				return status.Error(codes.Internal, fmt.Errorf("REST Mmf invocation NYI %v: %w", mmfUrl.Host, err).Error())
			} // pb.MatchmakingFunctionSpec_gRPC is default.

			// Add mmf details to all logs from this point on.
			logger := logger.WithFields(logrus.Fields{
				"mmf_name": mmf.GetName(),
				"mmf_host": mmfUrl.Host,
			})

			// Goroutine-local context and dial options slice to avoid races across concurrent MMFs.
			mmfCtx := ctx
			mmfOpts := make([]grpc.DialOption, len(baseOpts), len(baseOpts)+2)
			copy(mmfOpts, baseOpts)

			var conn *grpc.ClientConn
			// Default to unauthenticated grpc
			creds := insecure.NewCredentials()

			// If MMF server is running with TLS (https), we must authenticate.
			// Note: TLS is transparently added to the MMF server when running on Cloud Run,
			//       see https://cloud.google.com/run/docs/container-contract#tls
			var currentAccessToken string
			audience := mmf.GetHost()
			if mmfUrl.Scheme == "https" {
				creds = credentials.NewTLS(tlsConfig)
				httpsLogger := logger.WithFields(logrus.Fields{
					"audience": audience,
				})
				httpsLogger.Infof("HTTPS mmf url detected. Attempting to set up secure grpc credentials.")

				token, tokenErr := idTokenCache.Token(audience)
				if tokenErr != nil {
					err = status.Error(codes.Internal, fmt.Errorf(
						"Failed to get ID token to contact gRPC MMF at %v: %w",
						mmfUrl.Host, tokenErr).Error())
					httpsLogger.Error(err)
					otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
						attribute.String("mmf.name", mmf.GetName()),
						attribute.String("profile.name", profileName),
					))
					return fmt.Errorf("%w", err)
				}
				currentAccessToken = token.AccessToken

				if logrus.IsLevelEnabled(logrus.TraceLevel) {
					truncToken := currentAccessToken
					if len(truncToken) >= 8 {
						truncToken = truncToken[0:7]
					}
					httpsLogger.WithFields(logrus.Fields{
						"trunc_access_token": fmt.Sprintf("%v...", truncToken),
					}).Trace("retrieved access token")
				}

				mmfCtx = grpcMetadata.AppendToOutgoingContext(ctx,
					"authorization", "Bearer "+currentAccessToken)
			}

			mmfOpts = append(mmfOpts,
				grpc.WithTransportCredentials(creds),
				grpc.WithAuthority(mmfUrl.Host),
			)

			// Connect to gRPC server for this mmf.
			conn, err = grpc.NewClient(mmfUrl.Host, mmfOpts...)
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

			client := pb.NewMatchMakingFunctionServiceClient(conn)
			logger.Trace("Connected to MMF")

			openAndSendChunks := func(callCtx context.Context) (pb.MatchMakingFunctionService_RunClient, error) {
				streamClient, runErr := client.Run(callCtx)
				if runErr != nil {
					return nil, runErr
				}
				logger.Trace("MMF .Run() invoked, sending profile chunks")
				for index, chunk := range chunkedRequest {
					sendErr := streamClient.Send(chunk)
					if sendErr != nil {
						if errors.Is(sendErr, io.EOF) {
							if _, recvErr := streamClient.Recv(); recvErr != nil && !errors.Is(recvErr, io.EOF) {
								sendErr = recvErr
							}
						}
						logger.Errorf("Failed to send MmfRequest chunk to MMF: %v", sendErr)
						return nil, sendErr
					}
					logger.Tracef("MMF request chunk %02d/%02d: %0.2fmb", index+1, len(chunkedRequest), float64(proto.Size(chunk))/float64(1024*1024))
				}
				if closeErr := streamClient.CloseSend(); closeErr != nil {
					logger.Errorf("Failed to close MMF request send stream: %v", closeErr)
					return nil, closeErr
				}
				return streamClient, nil
			}

			authRetried := false
			mmfStream, err := openAndSendChunks(mmfCtx)
			if err != nil && mmfUrl.Scheme == "https" && mmfauth.IsAuthRejection(err) {
				authRetried = true
				refreshedToken, refreshErr := idTokenCache.Refresh(audience, currentAccessToken)
				if refreshErr != nil {
					err = status.Error(codes.Internal, fmt.Errorf(
						"Failed to refresh ID token to contact gRPC MMF at %v: %w",
						mmfUrl.Host, refreshErr).Error())
					logger.Error(err)
					otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
						attribute.String("mmf.name", mmf.GetName()),
						attribute.String("profile.name", profileName),
					))
					return fmt.Errorf("%w", err)
				}
				currentAccessToken = refreshedToken.AccessToken
				mmfCtx = grpcMetadata.AppendToOutgoingContext(ctx,
					"authorization", "Bearer "+currentAccessToken)
				mmfStream, err = openAndSendChunks(mmfCtx)
			}
			if err != nil {
				logger.Error(fmt.Errorf("Failed to connect to MMF at %v: %w", mmfUrl.Host, err).Error())
				otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
				))
				return status.Error(codes.Internal, fmt.Errorf("Failed to connect to MMF at %v: %w", mmfUrl.Host, err).Error())
			}

			// Make a waitgroup that lets us know when all ticket deactivations
			// are complete. (All tickets in matches returned by the MMF are
			// set to inactive by OM.)
			var tdwg sync.WaitGroup

			// matchesWithTickets and matchesWithoutTickets count the matches received
			// from this MMF for metrics reporting. Every counted match is returned to
			// the caller, split by whether it had any valid ticket IDs.
			// Register defers in LIFO order:
			// 1. First registered (runs second on exit): unconditionally record otelMatches.
			// 2. Second registered (runs first on exit): wait for all ticket deactivation goroutines.
			var matchesWithTickets, matchesWithoutTickets int64
			defer func() {
				otelMatches.Add(ctx, matchesWithTickets, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
					attribute.Bool("match.has_tickets", true),
				))
				otelMatches.Add(ctx, matchesWithoutTickets, metric.WithAttributes(
					attribute.String("mmf.name", mmf.GetName()),
					attribute.String("profile.name", profileName),
					attribute.Bool("match.has_tickets", false),
				))
			}()
			defer tdwg.Wait()

			receivedAnyResponse := false
			var idValidator *regexp.Regexp
			if tc.Replicator != nil {
				idValidator = tc.Replicator.GetReplIdValidator()
			}

			for {
				// Get results from MMF
				var result *pb.StreamedMmfResponse
				result, err = mmfStream.Recv()

				// io.EOF is the error returned by grpc when the server closes a stream.
				if errors.Is(err, io.EOF) {
					logger.Tracef("MMF stream complete")
					break
				}
				if err != nil {
					if !receivedAnyResponse && !authRetried && mmfUrl.Scheme == "https" && mmfauth.IsAuthRejection(err) {
						authRetried = true
						refreshedToken, refreshErr := idTokenCache.Refresh(audience, currentAccessToken)
						if refreshErr != nil {
							err = status.Error(codes.Internal, fmt.Errorf("MMF Failure: %w", refreshErr).Error())
							logger.Error(err)
							otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
								attribute.String("mmf.name", mmf.GetName()),
								attribute.String("profile.name", profileName),
							))
							return fmt.Errorf("%w", err)
						}
						currentAccessToken = refreshedToken.AccessToken
						mmfCtx = grpcMetadata.AppendToOutgoingContext(ctx,
							"authorization", "Bearer "+currentAccessToken)
						mmfStream, err = openAndSendChunks(mmfCtx)
						if err != nil {
							err = status.Error(codes.Internal, fmt.Errorf("MMF Failure: %w", err).Error())
							logger.Error(err)
							otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
								attribute.String("mmf.name", mmf.GetName()),
								attribute.String("profile.name", profileName),
							))
							return fmt.Errorf("%w", err)
						}
						continue
					}

					err = status.Error(codes.Internal, fmt.Errorf("MMF Failure: %w", err).Error())
					logger.Error(err)
					otelMmfFailures.Add(ctx, 1, metric.WithAttributes(
						attribute.String("mmf.name", mmf.GetName()),
						attribute.String("profile.name", profileName),
					))
					return fmt.Errorf("%w", err)
				}

				receivedAnyResponse = true
				resultCopy := result.GetMatch()
				if resultCopy == nil {
					// om-core passes every MMF response through to the caller, like an RPC framework.
					logger.Warn("MMF response contained no match; forwarding it to the caller unchanged")
				}

				// Validate ticket IDs synchronously before spawning the deactivation goroutine
				// so that annotating invalid ticket IDs on the match cannot race with or delay forwarding.
				ticketIdsToDeactivate := []string{}
				invalidIds := []string{}
				for _, roster := range resultCopy.GetRosters() {
					for _, ticket := range roster.GetTickets() {
						tid := ticket.GetId()
						if idValidator != nil && !idValidator.MatchString(tid) {
							logger.WithFields(logrus.Fields{
								"match_id":  resultCopy.GetId(),
								"ticket_id": tid,
							}).Errorf("Invalid ticket id in match roster: %v", InvalidIdErr)
							invalidIds = append(invalidIds, tid)
							continue
						}
						if tid == "" {
							logger.WithFields(logrus.Fields{
								"match_id": resultCopy.GetId(),
							}).Errorf("Invalid empty ticket id in match roster: %v", InvalidIdErr)
							invalidIds = append(invalidIds, tid)
							continue
						}
						ticketIdsToDeactivate = append(ticketIdsToDeactivate, tid)
					}
				}

				annotateMatchInvalidTicketIds(resultCopy, invalidIds)

				// Ticketless matches (for example diagnostic or feedback matches an MMF sends
				// to the Director) are returned to the caller too; they simply have nothing to deactivate.
				if len(ticketIdsToDeactivate) == 0 {
					matchesWithoutTickets++
					logger.WithFields(logrus.Fields{
						"match_id": resultCopy.GetId(),
					}).Debug("Match has no valid ticket IDs; returning it without deactivation")
				} else {
					matchesWithTickets++
				}

				// deactivate tickets & send match back to the matchmaker
				// asynchronously as streamed match results continue to be
				// processed.
				tdwg.Add(1)
				go func(res *pb.Match, ticketIds []string) {
					// Local logger with a flag to indicate logs are from this goroutine.
					logger := logger.WithFields(logrus.Fields{"stage": "deactivation"})
					defer tdwg.Done()

					streamCtx := ctx
					if stream != nil && stream.Context() != nil {
						streamCtx = stream.Context()
					}

					// Ticketless fast path: no valid ticket IDs to deactivate.
					// Note that such a match may reach the caller ahead of earlier matches
					// still waiting on deactivation; om-core makes no ordering guarantee between matches.
					if len(ticketIds) == 0 {
						if streamCtx.Err() != nil || ctx.Err() != nil {
							logger.WithFields(logrus.Fields{
								"error": streamCtx.Err(),
								"stage": "queuing_for_send",
							}).Warn("context cancelled")
							return
						}
						matchChan <- res
						return
					}

					// Option 1 (default): Wait for deactivation to complete
					// BEFORE returning the match.
					if cfg.GetBool("OM_MATCH_TICKET_DEACTIVATION_WAIT") {
						logger.Tracef("deactivating tickets in %v", res.GetId())
						errs, highestReplId := updateTicketsActiveState(ctx, logger, &tc, ticketIds, store.Deactivate)
						if len(errs) > 0 {
							logger.Errorf("Error deactivating match %v tickets: %v", res.GetId(), errs)
						}
						otelMmfTicketDeactivations.Add(ctx, int64(len(ticketIds)-len(errs)),
							metric.WithAttributes(
								attribute.String("mmf.name", mmf.GetName()),
								attribute.String("profile.name", profileName),
							),
						)
						logger.Tracef("Done requesting deactivation of tickets in %v", res.GetId())

						// Tell the caller which tickets om-core failed to deactivate (they may still
						// be active). This is known only when OM_MATCH_TICKET_DEACTIVATION_WAIT is true
						// because deactivation finishes before the match is returned; IDs are listed
						// in roster order (de-duplicated).
						if len(errs) > 0 {
							failedIds := make([]string, 0, len(errs))
							seenFailed := make(map[string]struct{}, len(errs))
							for _, tid := range ticketIds {
								if _, failed := errs[tid]; failed {
									if _, seen := seenFailed[tid]; !seen {
										seenFailed[tid] = struct{}{}
										failedIds = append(failedIds, tid)
									}
								}
							}
							annotateMatchDeactivationFailedTicketIds(res, failedIds)
						}

						var targetTicketId string
						for idx := len(ticketIds) - 1; idx >= 0; idx-- {
							if _, failed := errs[ticketIds[idx]]; !failed {
								targetTicketId = ticketIds[idx]
								break
							}
						}

						waitForMatchTicketDeactivation(
							ctx,
							logger,
							&tc,
							res,
							mmf.GetName(),
							profileName,
							targetTicketId,
							highestReplId,
							true,
						)
						logger.Trace("ticket deactivations complete, returning match")

						logger.WithFields(logrus.Fields{
							"match_id": res.GetId(),
							"stage":    "queuing_for_send",
						}).Trace("sending match to InvokeMMF() output queue")
						if streamCtx.Err() != nil || ctx.Err() != nil {
							logger.WithFields(logrus.Fields{
								"error": streamCtx.Err(),
								"stage": "queuing_for_send",
							}).Warn("context cancelled")
							return
						}
						matchChan <- res
						return
					}

					// Option 2: Return match immediately if caller stream is still active,
					// then deactivate tickets and wait for local replication in the background.
					logger.WithFields(logrus.Fields{
						"match_id": res.GetId(),
						"stage":    "queuing_for_send",
					}).Trace("sending match to InvokeMMF() output queue")
					if streamCtx.Err() != nil || ctx.Err() != nil {
						logger.WithFields(logrus.Fields{
							"error": streamCtx.Err(),
							"stage": "queuing_for_send",
						}).Warn("context cancelled")
						return
					}
					matchChan <- res

					logger.Tracef("deactivating tickets in %v", res.GetId())
					errs, highestReplId := updateTicketsActiveState(ctx, logger, &tc, ticketIds, store.Deactivate)
					if len(errs) > 0 {
						logger.Errorf("Error deactivating match %v tickets: %v", res.GetId(), errs)
					}
					otelMmfTicketDeactivations.Add(ctx, int64(len(ticketIds)-len(errs)),
						metric.WithAttributes(
							attribute.String("mmf.name", mmf.GetName()),
							attribute.String("profile.name", profileName),
						),
					)
					logger.Tracef("Done requesting deactivation of tickets in %v", res.GetId())

					var targetTicketId string
					for idx := len(ticketIds) - 1; idx >= 0; idx-- {
						if _, failed := errs[ticketIds[idx]]; !failed {
							targetTicketId = ticketIds[idx]
							break
						}
					}

					waitForMatchTicketDeactivation(
						ctx,
						logger,
						&tc,
						res,
						mmf.GetName(),
						profileName,
						targetTicketId,
						highestReplId,
						false,
					)
					logger.Trace("ticket deactivations complete for previously returned match")
				}(resultCopy, ticketIdsToDeactivate)
			}

			// The MMF stream ended with io.EOF, which is success.
			logger.Trace("async call to mmf complete")
			return nil
		}(mmf) //nolint:errcheck // End of MMF fan-out goroutine; errors are recorded in mmfFailures.
	} // end of for loop that runs once per MMF in the requested profile.

	// Wait for all mmfs to complete (that is, all copies of the MMF fan-out
	// goroutine have exited).
	mmfwg.Wait()

	// Close matchChan, which will cause the MMF Result fan-in goroutine to exit.
	logger.Trace("MMF waitgroup done, closing matchChan")
	close(matchChan) // tell the fan-in goroutine that we're done sending
	fanwg.Wait()     // wait for the fan-in goroutine to finish anything left in the channel

	// Every match has been streamed; now report any MMF failures.
	if len(mmfFailures) > 0 {
		return buildMmfFailureStatus(len(req.GetMmfs()), mmfFailures)
	}
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
	numReqUpdates := len(req.GetAssignmentRoster().GetTickets())
	if numReqUpdates > cfg.GetInt("OM_MAX_STATE_UPDATES_PER_CALL") {
		return nil, status.Error(codes.InvalidArgument,
			fmt.Errorf("%w (configured maximum %v, requested %v)",
				TooManyUpdatesErr, cfg.GetInt("OM_MAX_STATE_UPDATES_PER_CALL"), numReqUpdates).Error())
	}
	assignmentPb, err := proto.Marshal(req.GetAssignmentRoster().GetAssignment())
	if err != nil {
		err = errors.Wrap(err, "failed to marshal the assignment protobuf")
		logger.Errorf("Error: %v", err)
		return nil, err
	}
	if len(assignmentPb) == 0 {
		return nil, status.Error(codes.InvalidArgument, "roster with non-empty assignment is required")
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
				EnqueuedAt:  time.Now(),
			}
			numUpdates++
		} else {
			// Note the error but continue processing
			err = status.Error(codes.Internal, "Empty ticket ID in assignment request!")
			logger.Error(err)
			if otelTicketAssignmentFailures != nil {
				otelTicketAssignmentFailures.Add(parentCtx, 1)
			}
		}
	}

	for i := 0; i < numUpdates; i++ {
		// The results.result field contains the redis stream id (aka the replication ID)
		// We don't actually need this for anything, so just check for an error
		results := <-rChan

		if results.Err != nil {
			// Wrap redis error and give it a gRPC internal server error status code
			err = status.Error(codes.Internal, fmt.Errorf("Unable to assign ticket: %w", results.Err).Error())
			logger.Error(err)
			if otelTicketAssignmentFailures != nil {
				otelTicketAssignmentFailures.Add(parentCtx, 1)
			}
		}
	}

	logger.Debugf("DEPRECATED CreateAssignments: %v tickets given assignment \"%v\"", numUpdates, req.GetAssignmentRoster().GetAssignment().GetConnection())

	// Record metrics
	if meter != nil && otelAssignmentsPerCall != nil {
		otelAssignmentsPerCall.Add(parentCtx, int64(numUpdates))
	}

	return &pb.CreateAssignmentsResponse{}, err
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

	streamCtx := context.Background()
	if stream != nil && stream.Context() != nil {
		streamCtx = stream.Context()
	}
	watchCtx, watchCancel := context.WithCancel(streamCtx)
	var watchWg sync.WaitGroup

	// var init
	newestTicketCtime := time.Now()
	updateChan := make(chan *pb.StreamedWatchAssignmentsResponse)
	defer func() {
		watchCancel()
		watchWg.Wait()
		close(updateChan)
	}()

	for _, id := range req.GetTicketIds() {
		// Launch an async goroutine for each ticket ID in the request that polls
		// the sync.Map for an assignment until found or watchCtx is cancelled.
		watchWg.Add(1)
		go func(ticketId string) {
			defer watchWg.Done()
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				if value, ok := tc.Assignments.Load(ticketId); ok {
					assignment := value.(*pb.Assignment)
					logger.Debugf(" watchassignments loop got assignment %v for ticketid: %v",
						assignment.GetConnection(), ticketId)
					select {
					case updateChan <- &pb.StreamedWatchAssignmentsResponse{
						Assignment: assignment,
						Id:         ticketId,
					}:
					case <-watchCtx.Done():
					}
					return
				}
				select {
				case <-watchCtx.Done():
					return
				case <-ticker.C:
				}
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
	// Exit when we've gotten all results or timeout/cancellation is reached.
	// From newest ticket id, get the ticket creation time and deduce the max
	// time to wait from its creation time + configured TTL.
	timeout := time.After(time.Until(newestTicketCtime.Add(time.Millisecond * time.Duration(cfg.GetInt("OM_CACHE_TICKET_TTL_MS")))))
	var err error
	for i := range req.GetTicketIds() {
		select {
		case thisAssignment := <-updateChan:
			otelAssignmentWatches.Add(context.Background(), -1)
			err = stream.Send(thisAssignment)
			if err != nil {
				logger.Error(err)
			}
		case <-timeout:
			otelAssignmentWatches.Add(context.Background(), -int64(len(req.GetTicketIds())-i))
			return nil
		case <-streamCtx.Done():
			otelAssignmentWatches.Add(context.Background(), -int64(len(req.GetTicketIds())-i))
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
