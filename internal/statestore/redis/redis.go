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
// redisReplicator is an implementation of open match state storage replication
// using Redis Streams as the mechanism.
// https://redis.io/docs/latest/develop/data-types/streams/
//
// If you're thinking about updating how data is stored in Redis by editing this file:
//   - All updates should be modelled as a single redis command that adds one item to the stream.
//     Each addition to the stream gets its own entry ID from redis, meaning
//     every update has a unique entry ID, which simplifies replication
//     significantly.  Although putting multiple key/value pairs modelling
//     multiple  updates into one stream addition is possible, it isn't
//     accounted for in this design and shouldn't be used.
//   - There is a memoryReplicator as well, that aims to reproduce this
//     redisReplicator's behavior in local memory. If you change anything here,
//     you'll likely need to update that file too.
package redisReplicator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gomodule/redigo/redis"
	store "github.com/googleforgames/open-match2/v2/internal/statestore/datatypes"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	logger = logrus.WithFields(logrus.Fields{
		"app":        "open_match",
		"component":  "statestore",
		"replicator": "redis",
	})
	NoTicketKeyErr  = errors.New("Missing ticket key")
	NoTicketDataErr = errors.New("No ticket data")
	NoAssignmentErr = errors.New("Missing assignment")
	InvalidInputErr = errors.New("Invalid input")
)

type redisReplicator struct {
	rConnPool       *redis.Pool
	wConnPool       *redis.Pool
	cfg             *viper.Viper
	replId          string
	replIdValidator *regexp.Regexp
}

// errors returned by inline functions aren't seen by our code and we can't
// print them or use them to make decisions. They are just used by the imported
// module internals to decide if connections need to be retried/closed, so
// disable this linting check.
//
//nolint:wrapcheck,cyclop
func New(cfg *viper.Viper) (*redisReplicator, error) {
	// Exit on signal
	ctx, cancel := context.WithCancel(context.Background())
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)

	var err error

	// Request all updates that are newer than the configured
	// expiration time.
	initialReplId := strconv.FormatInt(time.Now().UnixMilli()-
		cfg.GetInt64("OM_CACHE_TICKET_TTL_MS")-
		cfg.GetInt64("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS"), 10)
	logger.WithFields(logrus.Fields{
		"repl_id": initialReplId,
	}).Debugf("Initializing cache")

	// Construct read host URL and logger
	readRedisHost := cfg.GetString("OM_REDIS_READ_HOST")
	if readRedisHost == "REDISHOST" {
		// Google Cloud Run + Memorystore Redis integration
		readRedisHost = cfg.GetString("REDISHOST")
		logger.Debugf("Read Redis host set to REDISHOST: %v", readRedisHost)
	}
	readRedisPort := cfg.GetString("OM_REDIS_READ_PORT")
	if readRedisPort == "REDISPORT" {
		// Google Cloud Run + Memorystore Redis integration
		readRedisPort = cfg.GetString("REDISPORT")
		logger.Debugf("Read Redis port set to REDISPORT: %v", readRedisPort)
	}
	readRedisUrl := fmt.Sprintf("%s:%s", readRedisHost, readRedisPort)
	rConnLogger := logger.WithFields(logrus.Fields{
		"read_redis_url": readRedisUrl,
	})

	// Construct write host URL and logger
	writeRedisHost := cfg.GetString("OM_REDIS_WRITE_HOST")
	if writeRedisHost == "REDISHOST" {
		// Google Cloud Run + Memorystore Redis integration
		writeRedisHost = cfg.GetString("REDISHOST")
		logger.Debugf("Write Redis host set to REDISHOST: %v", writeRedisHost)
	}
	writeRedisPort := cfg.GetString("OM_REDIS_WRITE_PORT")
	if writeRedisPort == "REDISPORT" {
		// Google Cloud Run + Memorystore Redis integration
		writeRedisPort = cfg.GetString("REDISPORT")
		logger.Debugf("Write Redis port set to REDISPORT: %v", writeRedisPort)
	}
	writeRedisUrl := fmt.Sprintf("%s:%s", writeRedisHost, writeRedisPort)
	wConnLogger := logger.WithFields(logrus.Fields{
		"write_redis_url": writeRedisUrl,
	})

	rr := &redisReplicator{
		// replIdValidator is a regular expression that can be used to match
		// replication id strings, which are redis stream entry ids.
		// https://redis.io/docs/data-types/streams/#entry-ids
		replIdValidator: regexp.MustCompile(`^\d{13}-\d+$`),
		replId:          initialReplId,
		cfg:             cfg,
		rConnPool: &redis.Pool{ // Redis read pool
			MaxIdle:     cfg.GetInt("OM_REDIS_POOL_MAX_IDLE"),
			MaxActive:   cfg.GetInt("OM_REDIS_POOL_MAX_ACTIVE"),
			IdleTimeout: cfg.GetDuration("OM_REDIS_POOL_IDLE_TIMEOUT"),
			Wait:        true,
			TestOnBorrow: func(c redis.Conn, lastUsed time.Time) error {
				// Assume the connection is valid if it was used in 15 sec.
				if time.Since(lastUsed) < 15*time.Second {
					return nil
				}

				_, err := c.Do("PING")
				return err
			},
			Dial: func() (redis.Conn, error) {
				// This bit of code is particularly hard to parse, containing
				// nested closures.  Basically: this function gets called every
				// time the redis connection pool needs to dial out to the redis
				// instance to establish a new connection to add to the pool. It
				// needs to be robust and handle transient Redis contact failures
				// as per
				// https://cloud.google.com/memorystore/docs/redis/general-best-practices#operations_and_scenarios_that_require_a_connection_retry
				// Therefore, the Dial function uses the
				// https://pkg.go.dev/github.com/cenkalti/backoff/v4 package to
				// implement exponential backoff with jitter for retries, until an
				// upper timeout limit is reached. Only when a retry succeeds or
				// the final retry fails does the Dial function return.
				var conn redis.Conn
				err := backoff.RetryNotify(
					func() error { // Operation that the backoff module will retry

						// Local closure var
						var err error

						select {
						// Check to see if an SIGTERM or SIGINT were received
						// before executing the dial - if they were, cancel the
						// context to end the backoff retries.
						case <-signalChan:
							cancel()
						default:
							// 'conn' here refers to the variable in the enclosing
							// redis.Pool{Dial: func() (redis.Conn, error)}
							// scope, which is returned when the dial was successful.
							// 'err' is the local variable, which
							// backoff.RetryNotify() evaluates to determine if it
							// needs to retry this operation.
							rConnLogger.Debug("dialing Redis read replica")

							// Dial options
							dialOptions := []redis.DialOption{
								redis.DialUsername(cfg.GetString("OM_REDIS_READ_USER")),
								redis.DialPassword(cfg.GetString("OM_REDIS_READ_PASSWORD")),
								redis.DialConnectTimeout(cfg.GetDuration("OM_REDIS_POOL_IDLE_TIMEOUT")),
								redis.DialReadTimeout(cfg.GetDuration("OM_REDIS_POOL_IDLE_TIMEOUT")),
							}

							// Add option to use TLS if config flag is set
							if cfg.GetBool("OM_REDIS_USE_TLS") {
								rConnLogger.Info("OM_REDIS_USE_TLS is set to true, will attempt to connect to Redis read replica(s) using TLS.")
								dialOptions = append(dialOptions, redis.DialUseTLS(true))
							}

							// Skip TLS cert verification if config flag is set
							// (e.g., for self-signed certs)
							if cfg.GetBool("OM_REDIS_TLS_SKIP_VERIFY") {
								rConnLogger.Info("OM_REDIS_TLS_SKIP_VERIFY is set to true, will attempt to connect to Redis read replica(s) using TLS without verifying the TLS certificate.")
								dialOptions = append(dialOptions, redis.DialTLSSkipVerify(true))
							}

							conn, err = redis.Dial("tcp",
								readRedisUrl,
								dialOptions...,
							)
							if err != nil { // Check for error on dial
								rConnLogger.Error("failure dialing Redis read replica")
							}

						}
						return err
					},
					backoff.WithContext(backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(
						cfg.GetDuration("OM_REDIS_DIAL_MAX_BACKOFF_TIMEOUT"))), ctx),
					func(err error, bo time.Duration) { // Function that fires when the operation returns an error
						rConnLogger.WithFields(logrus.Fields{"error": err}).Debugf(
							"Error attempting to connect to Redis read replica. Retrying in %s", bo)
					},
				)
				return conn, err
			},
		},
		wConnPool: &redis.Pool{ // Redis write pool
			MaxIdle:     cfg.GetInt("OM_REDIS_POOL_MAX_IDLE"),
			MaxActive:   cfg.GetInt("OM_REDIS_POOL_MAX_ACTIVE"),
			IdleTimeout: cfg.GetDuration("OM_REDIS_POOL_IDLE_TIMEOUT"),
			Wait:        true,
			TestOnBorrow: func(c redis.Conn, lastUsed time.Time) error {
				// Assume the connection is valid if it was used in 15 sec.
				if time.Since(lastUsed) < 15*time.Second {
					return nil
				}

				_, err := c.Do("PING")
				return err
			},
			Dial: func() (redis.Conn, error) {
				// See the comment on the read redis host Dial: function for
				// details on how this works (it is unfortunately complex due
				// to nested closures required by the retry module).
				var conn redis.Conn
				err := backoff.RetryNotify(
					func() error { // Operation that the backoff module will retry

						select {
						// Check to see if an SIGTERM or SIGINT were received
						// before executing the dial - if they were, cancel the
						// context to end the backoff retries.
						case <-signalChan:
							cancel()
						default:
							// Local closure var
							var err error

							// 'conn' here refers to the variable in the enclosing
							// redis.Pool{Dial: func() (redis.Conn, error)}
							// scope, which is returned when the dial was successful.
							// 'err' is the local variable, which
							// backoff.RetryNotify() evaluates to determine if it
							// needs to retry this operation.
							wConnLogger.Debug("dialing Redis write instance")

							// Dial options
							dialOptions := []redis.DialOption{
								redis.DialUsername(cfg.GetString("OM_REDIS_WRITE_USER")),
								redis.DialPassword(cfg.GetString("OM_REDIS_WRITE_PASSWORD")),
								redis.DialConnectTimeout(cfg.GetDuration("OM_REDIS_POOL_IDLE_TIMEOUT")),
								redis.DialReadTimeout(cfg.GetDuration("OM_REDIS_POOL_IDLE_TIMEOUT")),
							}

							// Add option to use TLS if config flag is set
							if cfg.GetBool("OM_REDIS_USE_TLS") {
								wConnLogger.Info("OM_REDIS_USE_TLS is set to true, will attempt to connect to Redis write instance using TLS.")
								dialOptions = append(dialOptions, redis.DialUseTLS(true))
							}

							// Skip TLS cert verification if config flag is set
							// (e.g., for self-signed certs)
							if cfg.GetBool("OM_REDIS_TLS_SKIP_VERIFY") {
								wConnLogger.Info("OM_REDIS_TLS_SKIP_VERIFY is set to true, will attempt to connect to Redis write instance using TLS without verifying the TLS certificate.")
								dialOptions = append(dialOptions, redis.DialTLSSkipVerify(true))
							}

							conn, err = redis.Dial("tcp",
								writeRedisUrl,
								dialOptions...,
							)

							if err != nil { // Check for error on dial
								wConnLogger.Error("failure dialing Redis write instance")
							}

						}
						return err
					},
					backoff.WithContext(backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(
						cfg.GetDuration("OM_REDIS_DIAL_MAX_BACKOFF_TIMEOUT"))), ctx),
					func(err error, bo time.Duration) { // Function that fires when the operation returns an error
						wConnLogger.WithFields(logrus.Fields{"error": err}).Debugf(
							"Error attempting to connect to Redis write instance. Retrying in %s", bo)
					},
				)
				return conn, err
			},
		},
	}

	// attempt to access Redis hosts during initialization in an effort to fail quickly
	rConnLogger.Debug("testing read redis connection")
	rConn, err := rr.rConnPool.GetContext(context.Background())
	if err == nil {
		// https://github.com/gomodule/redigo/blob/247f6c0e0a0ea200f727a5280d0d55f6bce6d2e7/redis/pool.go#L204
		// "If the function completes without error, then the application must
		// close the returned connection."
		defer rConn.Close()
	} else {
		rConnLogger.WithFields(logrus.Fields{
			"error": err,
		}).Debug("read redis connection error")
		return nil, err
	}
	wConnLogger.Debug("testing write redis connection")
	wConn, err := rr.wConnPool.GetContext(context.Background())
	if err == nil {
		// https://github.com/gomodule/redigo/blob/247f6c0e0a0ea200f727a5280d0d55f6bce6d2e7/redis/pool.go#L204
		// "If the function completes without error, then the application must
		// close the returned connection."
		defer wConn.Close()
	} else {
		wConnLogger.WithFields(logrus.Fields{
			"error": err,
		}).Debug("write redis connection error")
		return nil, err
	}

	// Check if the context has been cancelled by SIGINT or SIGTERM.
	if ctx.Err() != nil {
		rConnLogger.Fatal("cancellation requested")
		return nil, ctx.Err()
	}

	return rr, err
}

// sendUpdates accepts an array of state update structs and writes them to data storage, to be
// replicated to all clients (e.g. other instances of om-core).
// When using redis, these will be pipelined together as a batch update to improve performance.
// In addition to the updates sent to the function, it always ends every batch with an XTRIM
// command to delete all updates older than the ttl configured in the environment variable
// OM_CACHE_TICKET_TTL_MS.
//
//nolint:gocognit,cyclop
func (rr *redisReplicator) SendUpdates(updates []*store.StateUpdate) []*store.StateResponse {
	logger := logrus.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "redisReplicator.sendUpdates",
	})

	// Var init
	var err error
	const redisCmd = "XADD"
	out := make([]*store.StateResponse, len(updates))

	// Get connection from the write pool
	rConn := rr.wConnPool.Get()
	defer rConn.Close()

	// Process all the requested redis commands
	// If a command cannot be constructed because the update is invalid, add an
	// error to the output for that update.
	for i, update := range updates {
		out[i] = &store.StateResponse{Result: "", Err: nil}
		redisArgs := make([]interface{}, 0)
		redisArgs = append(redisArgs, "om-replication", "*")
		switch update.Cmd {
		case store.Ticket:
			// Validate input
			if update.Value == "" {
				out[i].Err = NoTicketDataErr
				continue
			}
			redisArgs = append(redisArgs, "ticket")
			redisArgs = append(redisArgs, update.Value)
		case store.Activate:
			// Validate input
			if update.Key == "" {
				out[i].Err = NoTicketKeyErr
				continue
			}
			redisArgs = append(redisArgs, "activate")
			redisArgs = append(redisArgs, update.Key)
		case store.Deactivate:
			// Validate input
			if update.Key == "" {
				out[i].Err = NoTicketKeyErr
				continue
			}
			redisArgs = append(redisArgs, "deactivate")
			redisArgs = append(redisArgs, update.Key)
		case store.Assign:
			// TODO: decide if we want multiple assignments in one redis record
			// Right now, each entry in the Redis stream only holds one
			// assignment. It is possible to put multiple assignments in a
			// single entry (it is an arbitrary number of key/value pairs), but
			// since assignments are deprecated we're punting this optimization
			// in order to focus on other things.
			//
			// Validate input
			if update.Key == "" {
				out[i].Err = NoTicketKeyErr
				continue
			}
			if update.Value == "" {
				out[i].Err = NoAssignmentErr
				continue
			}
			redisArgs = append(redisArgs, "assign")
			redisArgs = append(redisArgs, update.Key)
			redisArgs = append(redisArgs, "connection")
			redisArgs = append(redisArgs, update.Value)
		default:
			// Something has gone seriously wrong
			out[i].Err = InvalidInputErr
			continue
		}

		// The update was validated and the redis command successfully constructed.
		// Send command to redis.
		redisCmdWithArgs := fmt.Sprintf("%v %v", redisCmd, strings.Trim(fmt.Sprint(redisArgs), "[]"))
		logger.Debug(redisCmdWithArgs)
		err = rConn.Send(redisCmd, redisArgs...)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"redis_command": redisCmdWithArgs,
			}).Errorf("Redis error: %v", err)
		}
	}

	// Append one additional command to the pipeline to remove expired entries.
	expirationThresh := strconv.FormatInt(time.Now().UnixMilli()-rr.cfg.GetInt64("OM_CACHE_TICKET_TTL_MS"), 10)
	redisCmdWithArgs := fmt.Sprintf("XTRIM om-replication MINID %v", expirationThresh)
	logger.Debug(redisCmdWithArgs)
	err = rConn.Send("XTRIM", "om-replication", "MINID", expirationThresh)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"redis_command": redisCmdWithArgs,
		}).Errorf("Redis error: %v", err)
	}

	// Send pipelined commands, get results.
	r, err := rConn.Do("")
	if err != nil {
		logger.Errorf("Redis error when executing batch: %v", err)
	}

	// Make sure we have results before we parse them.
	if r == nil {
		logger.Error("Redis returned empty results from update!")
		return out
	}

	// The last result from Redis is a count of number of entries we removed with the XTRIM command.
	// Can be useful when debugging, so log it.
	expiredCount, err := redis.Int64(r.([]interface{})[len(r.([]interface{}))-1], err)
	if err != nil {
		logger.Errorf("Redis output int64 conversion error: %v", err)
	}
	if expiredCount > 0 {
		logger.WithFields(logrus.Fields{
			"redis_expiration_count":  expiredCount,
			"expiration_threshold_ts": expirationThresh,
		}).Debug("Expired Redis entries older than expiration threshold timestamp")
	}

	// Process all other Redis results into the return output array.
	for index := 0; index < len(r.([]interface{}))-1; index++ {

		// First, make sure the update parsing didn't find any issues.  If
		// parsing the update generated an error, then the update didn't
		// have all the required fields so it was never sent to Redis.
		if out[index].Err != nil {
			logger.WithFields(logrus.Fields{
				"update": updates[index],
			}).Error("an update could not be parsed and was skipped")

			// There is no Redis result to return, so return the update parsing error
			// and the key that generated the error.
			out[index].Result = updates[index].Key

		} else {
			// The update was valid and sent to Redis. Try to convert the
			// Redis response to a string.
			t, err := redis.String(r.([]interface{})[index], err)
			if err != nil {
				// Error, the redis result isn't a string (meaning it caused some issue).
				// Return the error code, and the result is the key that generated the error.
				t = updates[index].Key
				out[index].Err = fmt.Errorf("Redis output string conversion error: %w", err)
				logger.WithFields(logrus.Fields{
					"err":    err,
					"update": updates[index],
				}).Error("Redis returned an error while trying to update")
			}

			// Successful result.
			logger.WithFields(logrus.Fields{
				"update": updates[index],
				"result": t,
			}).Tracef("Redis successfully processed update")
			out[index].Result = t
		}
	}

	return out
}

// getUpdates performs a blocking read on state (with a configurable timeout read from
// the environment variable OM_CACHE_IN_WAIT_TIMEOUT_MS) to receive
// all update structs that have been sent to the replicator since the last getUpdates request.
// They are returned in an array, and om-core is applies them
// as events in the order they were originally received.
//
//nolint:cyclop
func (rr *redisReplicator) GetUpdates() []*store.StateUpdate {
	logger := logrus.WithFields(logrus.Fields{
		"app":       "open_match",
		"component": "redisReplicator.getUpdates",
	})

	// Output array of stateUpdates.
	out := make([]*store.StateUpdate, 0)

	// Generate redis command to retrieve updates
	redisCmd := "XREAD"
	redisArgs := make([]interface{}, 0)
	// Max num of updates to get in one go
	redisArgs = append(redisArgs, "COUNT", rr.cfg.GetString("OM_CACHE_IN_MAX_UPDATES_PER_POLL"))
	// Timeout when waiting for stream updates (GetUpdates is run
	// asynchronously, so this doesn't block execution)
	redisArgs = append(redisArgs, "BLOCK", rr.cfg.GetString("OM_CACHE_IN_WAIT_TIMEOUT_MS"))
	// Replication stream name
	redisArgs = append(redisArgs, "STREAMS", "om-replication")
	// Last ID read from this stream.
	redisArgs = append(redisArgs, rr.replId)

	// Get redis connection from the read pool
	rConn := rr.rConnPool.Get()
	defer rConn.Close()

	// Execute XREAD
	logger.WithFields(logrus.Fields{
		"redisCmd": fmt.Sprint(redisCmd, strings.Trim(fmt.Sprint(redisArgs), "[]")),
	}).Debugf("Executing redis command")
	data, err := rConn.Do(redisCmd, redisArgs...)
	if err != nil {
		logger.Errorf("Redis error: %v", err)
	}

	// Redigo module returns nil for the data if it reaches the timeout (BLOCK
	// Xms) without seeing any updates. In that case, just return gracefully.
	if data != nil {
		switch data.(type) {
		case redis.Error:
			logger.Errorf("Redis error: %v", data.(redis.Error))
		case []interface{}:
			// the data is down a couple of nested array levels in the response:
			// https://redis.io/docs/latest/develop/data-types/streams/#listening-for-new-items-with-xread
			replStream := data.([]interface{})[0].([]interface{})[1].([]interface{})
			for _, v := range replStream {
				// Element 0 is the redis stream entry ID, which we use as the replication ID.
				replId, err := redis.String(v.([]interface{})[0], nil)
				if err != nil {
					logger.Error(err)
				}
				thisUpdate := &store.StateUpdate{}

				// Element 1 is the actual data in the stream entry.
				// Our implementation assumes only one update is stored at each
				// stream entry (Redis allows multiple, but our implementation
				// does not)
				y, err := redis.Strings(v.([]interface{})[1], nil)
				if err != nil {
					logger.Error(err)
				}

				// Update type/key/value data
				switch y[0] {
				case "ticket":
					thisUpdate.Cmd = store.Ticket
					thisUpdate.Key = replId
					thisUpdate.Value = y[1] // Only argument for a ticket is the ticket PB
				case "activate":
					thisUpdate.Cmd = store.Activate
					thisUpdate.Key = y[1] // Only argument for a ticket activation is the ticket's ID
				case "deactivate":
					thisUpdate.Cmd = store.Deactivate
					thisUpdate.Key = y[1] // Only argument for a ticket deactivation is the ticket's ID
				case "assign":
					thisUpdate.Cmd = store.Assign
					thisUpdate.Key = y[1]   // ticket's ID
					thisUpdate.Value = y[3] // assignment
				}

				// Populated all the fields without an error
				out = append(out, thisUpdate)

				// update the current replId to indicate this update was processed
				rr.replId = replId
			}
		}
	}
	return out
}

// GetReplIdValidator returns a compiled regular expression that
// can be used to verify that a string has the format of a valid
// replication id (redis stream entry ID).
func (rr *redisReplicator) GetReplIdValidator() *regexp.Regexp {
	return rr.replIdValidator
}
