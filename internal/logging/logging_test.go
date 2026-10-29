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

package logging

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFormatter(t *testing.T) {
	expectedJSON := &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	testCases := []struct {
		in       string
		expected interface{}
	}{
		{"", &logrus.TextFormatter{}},
		{"json", expectedJSON},
		{"JSON", expectedJSON},
		{"text", &logrus.TextFormatter{}},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(fmt.Sprintf("newFormatter(%s) => %s", tc.in, reflect.TypeOf(tc.expected)), func(t *testing.T) {
			require := require.New(t)
			actual := newFormatter(tc.in)
			require.Equal(reflect.TypeOf(tc.expected), reflect.TypeOf(actual))
			assert.Equal(t, tc.expected, actual)
		})
	}
}

func TestConfigureLogging(t *testing.T) {
	origFormatter := logrus.StandardLogger().Formatter
	origLevel := logrus.GetLevel()
	origOut := logrus.StandardLogger().Out
	t.Cleanup(func() {
		logrus.SetFormatter(origFormatter)
		logrus.SetLevel(origLevel)
		logrus.SetOutput(origOut)
	})

	testCases := []struct {
		name              string
		format            string
		level             string
		expectedFormatter logrus.Formatter
		expectedLevel     logrus.Level
	}{
		{
			name:              "json info",
			format:            "json",
			level:             "info",
			expectedFormatter: &logrus.JSONFormatter{},
			expectedLevel:     logrus.InfoLevel,
		},
		{
			name:              "text debug dumps config",
			format:            "text",
			level:             "debug",
			expectedFormatter: &logrus.TextFormatter{},
			expectedLevel:     logrus.DebugLevel,
		},
		{
			name:              "trace level",
			format:            "text",
			level:             "trace",
			expectedFormatter: &logrus.TextFormatter{},
			expectedLevel:     logrus.TraceLevel,
		},
		{
			name:              "invalid level defaults to info",
			format:            "json",
			level:             "invalid_level",
			expectedFormatter: &logrus.JSONFormatter{},
			expectedLevel:     logrus.InfoLevel,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("OM_LOGGING_FORMAT", tc.format)
			cfg.Set("OM_LOGGING_LEVEL", tc.level)

			ConfigureLogging(cfg)

			assert.IsType(t, tc.expectedFormatter, logrus.StandardLogger().Formatter)
			assert.Equal(t, tc.expectedLevel, logrus.GetLevel())
		})
	}
}
