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

// Package config contains convenience functions for reading and managing viper
// configs used by Open Match.
package config

import (
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// readConfig reads Open Match configuration from the environment.
//
// readConfig sets default config values, and configures Viper to read from the environment.
// The AutomaticEnv function of Viper reads values from env vars, but critically,
// ONLY FOR KEYS IT ALREADY HAS A DEFAULT VALUE FOR. If you've defined a new env var and you're
// trying to access it as a config value in OM, you MUST specify a default for it here, or
// Viper will NOT read the value you set in the env var!
//
// By convention:
// - all Open Match configuration keys should be ALL CAPS and start with "OM_"
// - all keys with time duration values should end in units, for example "_SECS" or "_MS"
// If both a default and env var value exist for the same variable, the env var value wins.
func Read() *viper.Viper {
	cfg := viper.New()

	// Logging config. See the logging module in core/github.com/googleforgames/open-match2/v2/internal/logging for valid values.
	cfg.SetDefault("OM_LOGGING_FORMAT", "json")
	cfg.SetDefault("OM_LOGGING_LEVEL", "info")

	// OTel metrics config.
	cfg.SetDefault("OM_OTEL_SIDECAR", true)

	// False: wait until all tickets in a match have had their deactivation
	// saved to state storage before returning the match to the matchmaker.
	// Slower results but (theoretically) fewer ticket collisions among
	// matches.
	// True (default): return the match to the matchmaker as soon as the mmf
	// streams it to om-core. Deactivate tickets in the match after it has been
	// successfully returned. Fastest results but possibly more ticket
	// collisions if you have lots of mmfs running concurrently.
	// TODO: validate this has the expected effect when using redis
	cfg.SetDefault("OM_MATCH_TICKET_DEACTIVATION_WAIT", false)

	// Maximum number of updates allowed in activate/deactivate/assignment gRPC requests.
	// Must be a positive value that fits in a signed int32.
	cfg.SetDefault("OM_MAX_STATE_UPDATES_PER_CALL", 500)

	// How long OM-Core will wait for your MMF to complete before cancelling its context.
	cfg.SetDefault("OM_MMF_TIMEOUT_SECS", 500)

	// Where the OM state is stored.
	// 'redis' (default): write to host in OM_REDIS_WRITE* config vars, and
	// read from the host in the OM_REDIS_READ* config vars.
	// 'memory': should not be used in production! It makes this
	// instance into an island that does not receive/send to/from other om-core
	// instances. Useful for local development.
	cfg.SetDefault("OM_STATE_STORAGE_TYPE", "redis")

	// By default, OM is configured to read and write to the same redis
	// instance. When moving to production, you may want to direct reads to a
	// replica and writes to the Redis master, or even load-balance read
	// requests across several read replicas. See documentation for more
	// details.
	//
	// Redis connection configuration, applies to both read and write
	// connections.
	// When overriding a time.Duration value like this one with an
	// environment variable, the string needs to match the format expected by
	// time.ParseDuration() - see https://pkg.go.dev/time#ParseDuration
	cfg.SetDefault("OM_REDIS_DIAL_MAX_BACKOFF_TIMEOUT", 2*time.Minute)
	// These map directly to the redigo library pool configuration
	// https://pkg.go.dev/github.com/gomodule/redigo/redis#Pool
	cfg.SetDefault("OM_REDIS_POOL_MAX_IDLE", 500)
	cfg.SetDefault("OM_REDIS_POOL_MAX_ACTIVE", 500)
	cfg.SetDefault("OM_REDIS_POOL_IDLE_TIMEOUT", time.Minute)

	// these env vars are populated with the memorystore instance details at
	// runtime if running on Cloud Run with the Memorystore for Redis
	// integration
	// https://cloud.google.com/run/docs/integrate/redis-memorystore#accessing_redis_cache_in_your_code
	cfg.SetDefault("REDISHOST", "127.0.0.1")
	cfg.SetDefault("REDISPORT", 6379)

	// Redis write configuration
	// use host value REDISHOST and port REDISPORT to read from
	// those environment variables
	cfg.SetDefault("OM_REDIS_WRITE_HOST", "REDISHOST")
	cfg.SetDefault("OM_REDIS_WRITE_PORT", "REDISPORT")
	cfg.SetDefault("OM_REDIS_WRITE_USER", "default")
	cfg.SetDefault("OM_REDIS_WRITE_PASSWORD", "om-redis")

	// Redis read configuration
	// use host value REDISHOST and port REDISPORT to read from
	// those environment variables
	cfg.SetDefault("OM_REDIS_READ_HOST", "REDISHOST")
	cfg.SetDefault("OM_REDIS_READ_PORT", "REDISPORT")
	cfg.SetDefault("OM_REDIS_READ_USER", "default")
	cfg.SetDefault("OM_REDIS_READ_PASSWORD", "om-redis")

	// Replicated ticket cache configuration.
	// IN vars refer to the cache updates from the incoming replication
	// queue (i.e. reading from state storage)
	// OUT vars refer to the cache sending its local updates out to be
	// replicated to all other instances (i.e. writing to state storage)
	cfg.SetDefault("OM_CACHE_IN_MAX_UPDATES_PER_POLL", 10000)            // In number of update operations
	cfg.SetDefault("OM_CACHE_IN_WAIT_TIMEOUT_MS", 1500)                  // In milliseconds
	cfg.SetDefault("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS", 500) // In milliseconds
	cfg.SetDefault("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD", 50)               // In number of update operations
	cfg.SetDefault("OM_CACHE_OUT_WAIT_TIMEOUT_MS", 500)                  // In milliseconds
	cfg.SetDefault("OM_CACHE_TICKET_TTL_MS", 600000)                     // In milliseconds
	// How long assignments will be retained AFTER ticket expiration
	cfg.SetDefault("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS", 600000) // DEPRECATED In milliseconds

	// knative env vars https://cloud.google.com/run/docs/container-contract#env-vars
	cfg.SetDefault("PORT", 8080)
	cfg.SetDefault("K_SERVICE", "open_match_core")
	cfg.SetDefault("K_REVISION", "open_match_core_rev.1")
	cfg.SetDefault("K_CONFIGURATION", "open_match_core_cfg")

	// developer options
	cfg.SetDefault("OM_VERBOSE", false) // Add extra log fields when log level set to debug
	// unexposed gRPC port that grpc-gateway proxies to http on PORT defined above
	cfg.SetDefault("OM_GRPC_PORT", 50581)

	// Override default values with those from the environment variables of the same name.
	cfg.AutomaticEnv()

	return cfg
}

// LogConfig is a convenience function to output all config settings to the log.
func LogConfig(cfg *viper.Viper) {
	for key, value := range cfg.AllSettings() {
		logrus.WithFields(logrus.Fields{
			"app":       "open_match",
			"component": "internal.config",
		}).Debugf(" CONFIG %v: %v", key, value)
	}
}
