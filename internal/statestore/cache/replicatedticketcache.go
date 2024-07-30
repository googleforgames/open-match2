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
package cache

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"

	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	logger = logrus.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "cache",
	})
)

// cache.UpdateRequest is basically a wrapper around a StateUpdate (in
// statestorage/datatypes/store.go) that adds context and a channel where
// results should go. The state storage layer shouldn't need to understand the
// underlying context, or where the update request originated (which is where
// it will return). These are necessary for the gRPC server in om-core,
// however!
type UpdateRequest struct {
	Ctx context.Context
	// The update itself.
	Update store.StateUpdate
	// Return channel to confirm the write by sending back the assigned ticket ID
	ResultsChan chan *store.StateResponse
}

// The server instantiates a replicatedTicketCache on startup, and all
// ticket-reading functionality reads from this local cache. The cache also has
// the necessary data structures for replicating ticket cache changes that come
// in to this instance by it handling gRPC calls. This data structure contains
// sync.Map members, so it should not be copied after instantiation (see
// https://pkg.go.dev/sync#Map)
type ReplicatedTicketCache struct {
	// Local copies of all the state data.
	Tickets     sync.Map
	InactiveSet sync.Map
	Assignments sync.Map

	// How this replicatedTicketCache is replicated.
	Replicator store.StateReplicator
	// The queue of cache updates
	UpRequests chan *UpdateRequest

	//
	IdValidator *regexp.Regexp

	// Application config
	Cfg *viper.Viper
}

// outgoingReplicationQueue is an asynchronous goroutine that runs for the
// lifetime of the server.  It processes incoming repliation events that are
// produced by gRPC handlers, and sends those events to the configured state
// storage. The updates aren't applied to the local copy of the ticket cache
// yet at this point; once the event has been successfully replicated and
// received in the incomingReplicationQueue goroutine, the update is applied to
// the local cache.
func (tc *ReplicatedTicketCache) OutgoingReplicationQueue(ctx context.Context) {
	logger := logger.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "replicationQueue",
		"direction": "outgoing",
	})

	logger.Debug("Listening for replication requests")
	exec := false
	pipelineRequests := make([]*UpdateRequest, 0)
	pipeline := make([]*store.StateUpdate, 0)

	for {
		// initialize variables for this loop
		exec = false
		pipelineRequests = pipelineRequests[:0]
		pipeline = pipeline[:0]
		timeout := time.After(time.Millisecond * time.Duration(tc.Cfg.GetInt("OM_CACHE_OUT_WAIT_TIMEOUT_MS")))

		// collect currently pending requests to write to state storage using a single command (e.g. Redis Pipelining)
		for exec != true {
			select {
			case req := <-tc.UpRequests:
				pipelineRequests = append(pipelineRequests, req)
				pipeline = append(pipeline, &req.Update)

				logger.Tracef(" %v requests queued for current batch", len(pipelineRequests))
				if len(pipelineRequests) >= tc.Cfg.GetInt("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD") {
					// Maximum batch size reached
					otelCacheOutgoingThresholdReachedCount.Add(ctx, 1)
					logger.Trace("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD reached")
					exec = true
				}

			case <-timeout:
				// Timeout reached, don't wait for the batch to be full.
				otelCacheOutgoingQueueTimeouts.Add(ctx, 1)
				logger.Trace("OM_CACHE_OUT_WAIT_TIMEOUT_MS reached")
				exec = true
			}
		}

		// If the redis update pipeline batch job has commands to run, execute
		if len(pipelineRequests) > 0 {
			// Record the number of requests in this cycle
			logger.WithFields(logrus.Fields{
				"batch_update_count": len(pipelineRequests),
			}).Trace("sending state update batch to replicator")
			otelCacheOutgoingUpdatesPerPoll.Record(ctx, int64(len(pipelineRequests)))

			results := tc.Replicator.SendUpdates(pipeline)

			// Record the number of results received from the replicator.
			logger.WithFields(logrus.Fields{
				"result_count": len(results),
			}).Trace("state update batch results received from replicator")

			for index, result := range results {
				// send back this result to it's unique return channel
				pipelineRequests[index].ResultsChan <- result
			}
		}
	}
}

// incomingReplicationQueue is an asynchronous goroutine that runs for the
// lifetime of the server. It reads all incoming replication events from the
// configured state storage and applies them to the local ticket cache.  In
// practice, this does almost all the work for every om-core gRPC handler
// /except/ InvokeMatchMakingFunction.
//
//nolint:gocognit,cyclop,maintidx
func (tc *ReplicatedTicketCache) IncomingReplicationQueue(ctx context.Context) {

	logger := logger.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "replicationQueue",
		"direction": "incoming",
	})

	// Listen to the replication streams in Redis asynchronously,
	// and add updates to the channel to be processed as they come in
	replStream := make(chan store.StateUpdate, tc.Cfg.GetInt("OM_CACHE_IN_MAX_UPDATES_PER_POLL"))
	go func() {
		for {
			// The GetUpdates() command returns immediately once it sees an
			// update; we only want to process updates once per
			// OM_CACHE_IN_WAIT_TIMEOUT_MS milliseconds, so set a deadline and
			// only loop after it has expired. Without this, certain cases -
			// like updates that only trickle in one at a time every couple
			// milliseconds - will cause this for to loop rapidly, only getting
			// a tiny amount of work done each time.
			deadline := time.After(time.Millisecond * time.Duration(tc.Cfg.GetInt("OM_CACHE_IN_WAIT_TIMEOUT_MS")))

			// The GetUpdates() function blocks if there are no updates, but
			// its internal implementation respects the timeout defined in the
			// config variable OM_CACHE_IN_WAIT_TIMEOUT_MS, so this is
			// guaranteed to return in a timely fashion. It fetches up to
			// OM_CACHE_IN_MAX_UPDATES_PER_POLL updates if there are that many
			// pending.
			results := tc.Replicator.GetUpdates()

			otelCacheIncomingPerPoll.Record(ctx, int64(len(results)))

			if len(results) == 0 {
				// Record that there were no updates
				otelCacheIncomingEmptyTimeouts.Add(ctx, 1)
			}

			// Put the updates into the replication channel to process.
			for _, curUpdate := range results {
				logger.WithFields(logrus.Fields{
					"update.key":     curUpdate.Key,
					"update.command": curUpdate.Cmd,
				}).Trace("queueing incoming update from state storage")
				replStream <- *curUpdate
			}

			// Make sure OM_CACHE_IN_WAIT_TIMEOUT_MS milliseconds have passed
			// before we attempt to get more updates.
			<-deadline
		}
	}()

	// Check the channel for updates, and apply them
	for {
		// Force sleep time between applying replication updates into the local cache
		// to avoid tight looping and high cpu usage.
		time.Sleep(time.Millisecond * time.Duration(tc.Cfg.GetInt("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS")))
		done := false

		var err error
		for !done {
			// Maximum length of time we can process updates. Access to the
			// ticket cache is locked during updates, so we need a hard limit
			// here to avoid infinite mutex lock / race conditions
			updateTimeout := time.After(time.Millisecond * 500)

			// Process all incoming updates until there are none left or the
			// lock timeout is reached.
			select {
			case curUpdate := <-replStream:
				// Still updates to process.
				switch curUpdate.Cmd {
				case store.Ticket:
					// Convert the update value back to a protobuf message for
					// storage.
					//
					// https://protobuf.dev/programming-guides/api/#use-different-messages
					// states that this is not a preferred pattern, but om-core
					// meets the criteria to be an exception:
					// "If all of the following are true:
					//
					// - your service is the storage system
					// - your system doesn't make decisions based on your
					//   clients' structured data
					// - your system simply stores, loads, and perhaps provides
					//   queries at your client's request"
					ticketPb := &pb.Ticket{}
					err = proto.Unmarshal([]byte(curUpdate.Value), ticketPb)
					if err != nil {
						logger.Error("received ticket replication could not be unmarshalled")
					}

					// Set TicketID. Must do it post-replication since
					// TicketID /is/ the Replication ID
					// (e.g. redis stream entry id)
					// This guarantees that the client can never get an
					// invalid ticketID that was not successfully stored/replicated
					ticketPb.Id = curUpdate.Key

					// All tickets begin inactive
					tc.InactiveSet.Store(curUpdate.Key, true)
					tc.Tickets.Store(curUpdate.Key, ticketPb)
					logger.Tracef("ticket replication received: %v", curUpdate.Key)

				case store.Activate:
					tc.InactiveSet.Delete(curUpdate.Key)
					logger.Tracef("activation replication received: %v", curUpdate.Key)

				case store.Deactivate:
					tc.InactiveSet.Store(curUpdate.Key, true)
					logger.Tracef("deactivate replication received: %v", curUpdate.Key)

				case store.Assign:
					// Convert the assignment back into a protobuf message.
					assignmentPb := &pb.Assignment{}
					err = proto.Unmarshal([]byte(curUpdate.Value), assignmentPb)
					if err != nil {
						logger.Error("received assignment replication could not be unmarshalled")
					}
					tc.Assignments.Store(curUpdate.Key, assignmentPb)
					logger.Tracef("**DEPRECATED** assign replication received %v:%v", curUpdate.Key, assignmentPb.GetConnection())
				}
			case <-updateTimeout:
				// Lock hold timeout exceeded
				otelCacheIncomingProcessingTimeouts.Add(ctx, 1)
				logger.Trace("lock hold timeout")
				done = true
			default:
				// Nothing left to process; exit immediately
				logger.Trace("Incoming update queue empty")
				done = true
			}
		}

		// Expiration closure, contains all code that removes data from the
		// replicated ticket cache.
		//
		// No need for this to be it's own function yet as the performance is
		// satisfactory running it immediately after every cache update.
		//
		// Removal logic is as follows:
		// * ticket ids expired from the inactive list MUST also have their
		//   tickets removed from the ticket cache! Any ticket that exists and
		//   doesn't have it's id on the inactive list is considered active and
		//   will appear in ticket pools for invoked MMFs!
		// * tickets with user-specified expiration times sooner than the
		//   default MUST be removed from the cache at the user-specified time.
		//   Inactive list is not affected by this as inactive list entries for
		//   tickets that don't exist have no effect (except briefly taking up a
		//   few bytes of memory). Dangling inactive list entries will be cleaned
		//   up in expirations cycles after the configured OM ticket TTL anyway.
		// * assignments are expired after the configured OM ticket TTL AND the
		//   configured OM assignment TTL have elapsed. This is to handle cases
		//   where tickets expire after they were passed to invoked MMFs but
		//   before they are in sessions. Such tickets are still allowed to be
		//   assigned and their assignments will be retained until the
		//   expiration time described above. **DEPRECATED**
		//
		// TODO: measure the impact of expiration operations with a timer
		// metric under extreme load, and if it's problematic,
		// revisit / possibly do it asynchronously
		//
		// Ticket IDs are assigned by Redis in the format of
		// <unix_timestamp>-<index> where the index increments every time a
		// ticket is created during the same second. We are only
		// interested in the ticket creation time (the unix timestamp) here.
		//
		// The in-memory replication module just follows the redis entry ID
		// convention so this works fine, but retrieving the creation time
		// would need to be abstracted into a method of the stateReplicator
		// interface if we ever support a different replication layer (for
		// example, pub/sub).
		{
			// Separate logrus instance with its own metadata to aid troubleshooting
			exLogger := logrus.WithFields(logrus.Fields{
				"app":       "open_match",
				"component": "replicatedTicketCache",
				"operation": "expiration",
			})

			// Metrics are tallied in local variables. The values get copied to
			// the module global values that the metrics actually sample after
			// th tally is complete. This way the mid-tally values never get
			// accidentally reported to the metrics sidecar (which could happen
			// if we used the module global values to compute the tally)
			var (
				numInactive            int64
				numInactiveDeletions   int64
				numTickets             int64
				numTicketDeletions     int64
				numAssignments         int64
				numAssignmentDeletions int64
			)
			startTime := time.Now()

			// Cull expired tickets from the local cache inactive ticket set.
			// This expiration is done based on the entry time of the ticket
			// into the system, and the maximum configured ticket TTL rather
			// than when the ticket inactive state was created. This means that
			// it is possible for a ticket that has already expired due to a
			// user-configured expiration time to still persist in the inactive
			// set for a short time. This is fine as an inactive set entry that
			// refers to a non-existent ticket has no effect.
			tc.InactiveSet.Range(func(id, _ any) bool {
				// Get creation timestamp from ID. Timestamp is always assumed
				// to follow the redis stream entry id convention of
				// millisecond precision (13-digit unix timestamp)
				ticketCreationTime, err := strconv.ParseInt(strings.Split(id.(string), "-")[0], 10, 64)
				if err != nil {
					// error; log and go to the next entry
					exLogger.WithFields(logrus.Fields{
						"ticket_id": id,
					}).Error("Unable to parse ticket ID into an unix timestamp when trying to expire old ticket active states")
					return true
				}
				if (time.Now().UnixMilli() - ticketCreationTime) > int64(tc.Cfg.GetInt("OM_CACHE_TICKET_TTL_MS")) {
					// Ensure that when expiring a ticket from the inactive set, the ticket is always deleted as well.
					_, existed := tc.Tickets.LoadAndDelete(id)
					if existed {
						numTickets++
						numTicketDeletions++
					}

					// Remove expired ticket from the inactive set.
					tc.InactiveSet.Delete(id)
					numInactiveDeletions++
				} else {
					numInactive++
				}
				return true
			})

			// cull expired tickets from local cache based on the ticket's expiration time.
			tc.Tickets.Range(func(id, ticket any) bool {
				if time.Now().After(ticket.(*pb.Ticket).GetExpirationTime().AsTime()) {
					tc.Tickets.Delete(id)
					numTicketDeletions++
				} else {
					numTickets++
				}
				return true
			})

			// cull expired assignments from local cache. This uses similar
			// logic to expiring a ticket from the inactive set, but includes
			// an addition configurable TTL so that it is possible for
			// assignments to persist and be retrieved even after the ticket no
			// longer exists in the system.
			//
			// Note that assignment tracking is DEPRECATED functionality
			// intended only for use while doing rapid matchmaker iteration,
			// and we strongly recommend that you use a more robust player
			// status tracking system in production.
			tc.Assignments.Range(func(id, _ any) bool {
				// Get creation timestamp from ID
				ticketCreationTime, err := strconv.ParseInt(strings.Split(id.(string), "-")[0], 10, 64)
				if err != nil {
					exLogger.Error("Unable to parse ticket ID into an unix timestamp when trying to expire old assignments")
				}
				if (time.Now().UnixMilli() - ticketCreationTime) >
					int64(tc.Cfg.GetInt("OM_CACHE_TICKET_TTL_MS")+tc.Cfg.GetInt("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS")) {
					tc.Assignments.Delete(id)
					numAssignmentDeletions++
				} else {
					numAssignments++
				}
				return true
			})

			// Log results and record counter metrics
			AssignmentCount = numAssignments
			TicketCount = numTickets
			InactiveCount = numInactive

			// Record time elapsed for histogram
			elapsed := float64(time.Since(startTime).Microseconds())
			otelCacheExpirationCycleDuration.Record(ctx, elapsed/1000.0) // OTEL measurements are in milliseconds

			// Record expiration count data for histograms
			otelCacheTicketsExpiredPerCycle.Record(ctx, int64(numTicketDeletions))
			otelCacheInactivesExpiredPerCycle.Record(ctx, int64(numInactiveDeletions))
			otelCacheAssignmentsExpiredPerCycle.Record(ctx, int64(numAssignmentDeletions))

			// Trace logging for advanced debugging
			if numAssignmentDeletions > 0 {
				exLogger.Tracef("Removed %v expired assignments from local cache", numAssignmentDeletions)
			}
			if numInactiveDeletions > 0 {
				exLogger.Tracef("%v ticket ids expired from the inactive list in local cache", numInactiveDeletions)
			}
			if numTicketDeletions > 0 {
				exLogger.Tracef("Removed %v expired tickets from local cache", numTicketDeletions)
			}
			if elapsed >= 0.01 {
				exLogger.Tracef("Local cache expiration code took %.2f us", elapsed)
			}
		}

	}
}
