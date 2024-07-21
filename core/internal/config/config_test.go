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
package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRead(t *testing.T) {

	// Test Case 1: Default Values
	t.Run("default values", func(t *testing.T) {
		cfg := Read()

		assert.NotNil(t, cfg)
	})

	// Test Case 2: Environment Variables Override Defaults
	t.Run("environment variables override", func(t *testing.T) {
		os.Setenv("OM_LOGGING_FORMAT", "text")
		os.Setenv("OM_REDIS_POOL_MAX_IDLE", "100")
		defer os.Unsetenv("OM_LOGGING_FORMAT")
		defer os.Unsetenv("OM_REDIS_POOL_MAX_IDLE")

		cfg := Read()

		assert.Equal(t, "text", cfg.GetString("OM_LOGGING_FORMAT"))
		assert.Equal(t, 100, cfg.GetInt("OM_REDIS_POOL_MAX_IDLE"))
	})

	// Test Case 3: Specific Type Assertions
	t.Run("specific types", func(t *testing.T) {
		cfg := Read()

		// String type assertions
		assert.IsType(t, "", cfg.GetString("OM_LOGGING_FORMAT"))
		assert.IsType(t, "", cfg.GetString("OM_LOGGING_LEVEL"))
		assert.IsType(t, "", cfg.GetString("OM_STATE_STORAGE_TYPE"))
		assert.IsType(t, "", cfg.GetString("REDISHOST"))
		assert.IsType(t, "", cfg.GetString("OM_REDIS_WRITE_HOST"))
		assert.IsType(t, "", cfg.GetString("OM_REDIS_WRITE_PORT"))
		assert.IsType(t, "", cfg.GetString("OM_REDIS_WRITE_USER"))
		assert.IsType(t, "", cfg.GetString("OM_REDIS_WRITE_PASSWORD"))
		assert.IsType(t, "", cfg.GetString("OM_REDIS_READ_HOST"))
		assert.IsType(t, "", cfg.GetString("OM_REDIS_READ_PORT"))
		assert.IsType(t, "", cfg.GetString("OM_REDIS_READ_USER"))
		assert.IsType(t, "", cfg.GetString("OM_REDIS_READ_PASSWORD"))
		assert.IsType(t, "", cfg.GetString("K_SERVICE"))
		assert.IsType(t, "", cfg.GetString("K_REVISION"))
		assert.IsType(t, "", cfg.GetString("K_CONFIGURATION"))

		// Integer type assertions
		assert.IsType(t, 0, cfg.GetInt("OM_MAX_STATE_UPDATES_PER_CALL"))
		assert.IsType(t, 0, cfg.GetInt("OM_CACHE_IN_MAX_UPDATES_PER_POLL"))
		assert.IsType(t, 0, cfg.GetInt("OM_CACHE_IN_WAIT_TIMEOUT_MS"))
		assert.IsType(t, 0, cfg.GetInt("OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS"))
		assert.IsType(t, 0, cfg.GetInt("OM_CACHE_OUT_MAX_QUEUE_THRESHOLD"))
		assert.IsType(t, 0, cfg.GetInt("OM_CACHE_OUT_WAIT_TIMEOUT_MS"))
		assert.IsType(t, 0, cfg.GetInt("OM_CACHE_TICKET_TTL_MS"))
		assert.IsType(t, 0, cfg.GetInt("OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS"))
		assert.IsType(t, 0, cfg.GetInt("OM_MMF_TIMEOUT_SECS"))
		assert.IsType(t, 0, cfg.GetInt("OM_REDIS_POOL_MAX_IDLE"))
		assert.IsType(t, 0, cfg.GetInt("OM_REDIS_POOL_MAX_ACTIVE"))
		assert.IsType(t, 0, cfg.GetInt("REDISPORT"))
		assert.IsType(t, 0, cfg.GetInt("PORT"))
		assert.IsType(t, 0, cfg.GetInt("OM_GRPC_PORT"))

		// Boolean type assertion
		assert.IsType(t, false, cfg.GetBool("OM_MATCH_TICKET_DEACTIVATION_WAIT"))
		assert.IsType(t, false, cfg.GetBool("OM_VERBOSE"))

		// Duration type assertion
		assert.IsType(t, time.Minute, cfg.GetDuration("OM_REDIS_POOL_IDLE_TIMEOUT"))
		assert.IsType(t, time.Minute, cfg.GetDuration("OM_REDIS_DIAL_MAX_BACKOFF_TIMEOUT"))
	})
}
