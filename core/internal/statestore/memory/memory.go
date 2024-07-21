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
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	store "open-match.dev/core/internal/statestore/datatypes"
)

var (
	logger = logrus.WithFields(logrus.Fields{
		"app":        "open_match",
		"component":  "statestore",
		"replicator": "memory",
	})
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
}

func New(cfg *viper.Viper) *memoryReplicator {
	logger.WithFields(logrus.Fields{
		"repl_id": "N/A",
	}).Debugf("Initializing cache")
	return &memoryReplicator{
		// replIdValidator is a regular expression that can be used to match
		// replication id strings, which are redis stream entry ids.
		// https://redis.io/docs/data-types/streams/#entry-ids
		replIdValidator: regexp.MustCompile(`^\d{13}-\d+$`),
		replChan:        make(chan *store.StateUpdate),
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

	// Timeout
	timeout := time.After(time.Millisecond * time.Duration(rc.cfg.GetInt("OM_CACHE_IN_WAIT_TIMEOUT_MS")))

	// Local vars
	var thisUpdate *store.StateUpdate
	more := true

	for more {
		select {
		case thisUpdate, more = <-rc.replChan:
			if more {
				// adapted from https://go.dev/ref/spec#Receive_operator
				// The value of more is true if the value received was
				// delivered by a successful send operation to the channel, or
				// false if it is a zero value generated because the channel is
				// closed and empty.
				out = append(out, thisUpdate)
			}
		case <-timeout:
			more = false
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

	out := make([]*store.StateResponse, 0)
	for _, up := range updates {
		replId := rc.getReplId()
		// Creating a new ticket generates a new ID in redis
		// so mock that here.
		if up.Cmd == store.Ticket {
			up.Key = replId
		}
		rc.replChan <- up
		out = append(out, &store.StateResponse{Result: replId, Err: nil})
	}
	logger.Tracef("%v updates applied to state storage for replication (maximum number set by OM_REDIS_PIPELINE_MAX_QUEUE_THRESHOLD config variable)", len(updates))

	return out
}

// GetReplId mocks how Redis Streams generate entry IDs
// https://redis.io/docs/data-types/streams/#entry-ids
func (rc *memoryReplicator) getReplId() string {
	if time.Now().UnixMilli() == rc.replTS.UnixMilli() {
		rc.replCount += 1
	} else {
		rc.replTS = time.Now()
		rc.replCount = 0
	}
	id := fmt.Sprintf("%v-%v", strconv.FormatInt(rc.replTS.UnixMilli(), 10), rc.replCount)
	return id
}

// GetReplIdValidator returns a compiled regular expression that
// can be used to verify that a string has the format of a valid
// replication id (redis stream entry ID).
func (rc *memoryReplicator) GetReplIdValidator() *regexp.Regexp {
	return rc.replIdValidator
}
