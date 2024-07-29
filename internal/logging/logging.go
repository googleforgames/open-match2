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

// Package logging configures the Logrus logging library.
package logging

import (
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// ConfigureLogging sets up open match logrus instance using the open match config env vars
//   - log line format (text, json[default], or stackdriver)
//   - min log level to include (debug, info [default], warn, error, fatal, panic)
func ConfigureLogging(cfg *viper.Viper) {
	logrus.SetFormatter(newFormatter(cfg.GetString("OM_LOGGING_FORMAT")))
	logrus.SetOutput(os.Stdout)
	level, err := logrus.ParseLevel(cfg.GetString("OM_LOGGING_LEVEL"))
	if err != nil {
		logrus.Warn("Unable to parse OM_LOGGING_LEVEL; defaulting to Info")
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)
	if level >= logrus.TraceLevel {
		logrus.SetReportCaller(true)
		logrus.Warn("Trace logging level configured. Not recommended for production!")
	}
}

func newFormatter(formatter string) logrus.Formatter {
	switch strings.ToLower(formatter) {
	case "json":
		return &logrus.JSONFormatter{
			FieldMap: logrus.FieldMap{
				logrus.FieldKeyTime:  "timestamp",
				logrus.FieldKeyLevel: "severity",
				logrus.FieldKeyMsg:   "message",
			},
			TimestampFormat: time.RFC3339Nano,
		}
	}
	return &logrus.TextFormatter{}
}
