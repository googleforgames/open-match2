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
package memoryReplicator

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	logger = logrus.WithFields(logrus.Fields{
		"app":        "open_match",
		"component":  "statestore",
		"replicator": "memory",
	})
	NoTicketKeyErr          = store.NoTicketKeyErr
	NoTicketDataErr         = store.NoTicketDataErr
	NoAssignmentErr         = store.NoAssignmentErr
	InvalidInputErr         = store.InvalidInputErr
	FullReplicationQueueErr = errors.New("Replication queue full")
)

// Local, in-memory state storage.  This mocks a subset of the
// Redis Streams functionality to provide the same surface area used by om-core.
// Used for tests and local development. NOT RECOMMENDED FOR PRODUCTION.
type memoryReplicator struct {
	cfg             *viper.Viper
	replChan        chan *store.StateUpdate
	replTS          time.Time
	replCount       int
	replIdValidator *regexp.Regexp
	mu              sync.Mutex
}

func New(cfg *viper.Viper) *memoryReplicator {
	logger.WithFields(logrus.Fields{
		"repl_id": "N/A",
	}).Debugf("Initializing cache")
	chanCap := cfg.GetInt("OM_CACHE_IN_MAX_UPDATES_PER_POLL")
	if queueBuf := cfg.GetInt("OM_CACHE_IN_QUEUE_BUFFER_SIZE"); queueBuf > chanCap {
		chanCap = queueBuf
	}
	return &memoryReplicator{
		// replIdValidator is a regular expression that can be used to match
		// replication id strings, which are redis stream entry ids.
		// https://redis.io/docs/data-types/streams/#entry-ids
		replIdValidator: regexp.MustCompile(`^\d{13}-\d+$`),
		replChan:        make(chan *store.StateUpdate, chanCap),
		replTS:          time.Now(),
		replCount:       0,
		cfg:             cfg,
	}
}

// GetUpdates mocks how the statestore/redis module processes a Redis Stream XRANGE command.
// https://redis.io/docs/data-types/streams/#querying-by-range-xrange-and-xrevrange
func (rc *memoryReplicator) GetUpdates() (out []*store.StateUpdate) {
	logger := logger.WithFields(logrus.Fields{
		"direction": "getUpdates",
	})

	// TODO: histogram for number of updates, length of poll

	out = make([]*store.StateUpdate, 0)
	maxUpdates := rc.cfg.GetInt("OM_CACHE_IN_MAX_UPDATES_PER_POLL")
	timer := time.NewTimer(time.Millisecond * time.Duration(rc.cfg.GetInt("OM_CACHE_IN_WAIT_TIMEOUT_MS")))
	defer timer.Stop()

	select {
	case firstUpdate, more := <-rc.replChan:
		if !more {
			return out
		}
		out = append(out, firstUpdate)
	case <-timer.C:
		return out
	}

drainLoop:
	for len(out) < maxUpdates {
		select {
		case nextUpdate, more := <-rc.replChan:
			if !more {
				break drainLoop
			}
			out = append(out, nextUpdate)
		default:
			break drainLoop
		}
	}

	if len(out) > 0 {
		logger.Debugf("read %v updates from state storage", len(out))
	}

	return out
}

// SendUpdates mocks how the statestore/redis module processes a Redis Stream XADD command.
// https://redis.io/docs/data-types/streams/#streams-basics
func (rc *memoryReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	logger := logger.WithFields(logrus.Fields{
		"direction": "sendUpdates",
	})

	out := make([]*store.StateResponse, len(updates))
	for i, up := range updates {
		if up == nil {
			out[i] = &store.StateResponse{Result: "", Err: InvalidInputErr}
			continue
		}

		copied := &store.StateUpdate{Cmd: up.Cmd}
		var validationErr error

		switch up.Cmd {
		case store.Ticket:
			if len(up.Value) == 0 {
				validationErr = NoTicketDataErr
				break
			}
			copied.Value = up.Value
		case store.Activate, store.Deactivate:
			if len(up.Keys) > 0 {
				validKeys := make([]string, 0, len(up.Keys))
				for _, k := range up.Keys {
					if k == "" {
						logger.Warnf("skipping empty ticket key in packed update: %v", up)
						continue
					}
					validKeys = append(validKeys, k)
				}
				if len(validKeys) == 0 {
					validationErr = NoTicketKeyErr
					break
				}
				copied.Key = validKeys[0]
				if len(validKeys) > 1 {
					copied.Keys = validKeys
				}
			} else {
				if len(up.Key) == 0 {
					validationErr = NoTicketKeyErr
					break
				}
				copied.Key = up.Key
			}
		case store.Assign:
			if len(up.Key) == 0 {
				validationErr = NoTicketKeyErr
				break
			}
			if len(up.Value) == 0 {
				validationErr = NoAssignmentErr
				break
			}
			copied.Key = up.Key
			copied.Value = up.Value
		default:
			validationErr = InvalidInputErr
		}

		if validationErr != nil {
			out[i] = &store.StateResponse{
				Result: up.PrimaryKey(),
				Err:    validationErr,
			}
			continue
		}

		rc.mu.Lock()
		prevTS := rc.replTS
		prevCount := rc.replCount
		replId := rc.getReplId()
		// Creating a new ticket generates a new ID in redis
		// so mock that here.
		if copied.Cmd == store.Ticket {
			copied.Key = replId
		}
		copied.ReplId = replId

		select {
		case rc.replChan <- copied:
			out[i] = &store.StateResponse{Result: replId, Err: nil}
		default:
			rc.replTS = prevTS
			rc.replCount = prevCount
			out[i] = &store.StateResponse{
				Result: up.PrimaryKey(),
				Err:    FullReplicationQueueErr,
			}
		}
		rc.mu.Unlock()
	}
	logger.Tracef("%v updates applied to state storage for replication (maximum number set by OM_REDIS_PIPELINE_MAX_QUEUE_THRESHOLD config variable)", len(updates))

	return out
}

// GetReplId mocks how Redis Streams generate entry IDs
// https://redis.io/docs/data-types/streams/#entry-ids
func (rc *memoryReplicator) getReplId() string {
	now := time.Now()
	if now.UnixMilli() <= rc.replTS.UnixMilli() {
		rc.replCount += 1
	} else {
		rc.replTS = now
		rc.replCount = 0
	}
	id := fmt.Sprintf("%013d-%s", rc.replTS.UnixMilli(), strconv.Itoa(rc.replCount))
	return id
}

// GetReplIdValidator returns a compiled regular expression that
// can be used to verify that a string has the format of a valid
// replication id (redis stream entry ID).
func (rc *memoryReplicator) GetReplIdValidator() *regexp.Regexp {
	return rc.replIdValidator
}
