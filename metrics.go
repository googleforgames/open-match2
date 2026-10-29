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
package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/detectors/gcp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	otelmetrics "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

const (
	metricsNamePrefix = "om_core."
	serviceVersion    = "2.1.0"
)

var (
	otelLogger = logrus.WithFields(logrus.Fields{
		"app":            "open_match",
		"component":      "core",
		"implementation": "otel",
	})

	// Metric variable declarations are global, so they can be accessed directly in the application code.
	// Metrics populated by the grpc function implementations in main.go.
	otelTicketCreations          otelmetrics.Int64Counter
	otelTicketSize               otelmetrics.Float64Histogram
	otelDeactivationsPerCall     otelmetrics.Int64Histogram
	otelInvalidIdsPerDeactivateCall otelmetrics.Int64Histogram
	otelFailedIdsPerDeactivateCall  otelmetrics.Int64Histogram
	otelActivationsPerCall       otelmetrics.Int64Histogram
	otelInvalidIdsPerActivateCall   otelmetrics.Int64Histogram
	otelFailedIdsPerActivateCall    otelmetrics.Int64Histogram
	otelMmfCalls                 otelmetrics.Int64Counter
	otelMmfFailures              otelmetrics.Int64Counter
	otelMmfTicketDeactivations   otelmetrics.Int64Counter
	otelMatches                  otelmetrics.Int64Counter
	otelMmfPrepDuration          otelmetrics.Float64Histogram
	otelMmfDeactivationWait      otelmetrics.Float64Histogram
	otelMmfDeactivationTimeouts  otelmetrics.Int64Counter
	otelAssignmentsPerCall       otelmetrics.Int64Counter
	otelTicketAssignmentFailures otelmetrics.Int64Counter
	otelAssignmentWatches        otelmetrics.Int64UpDownCounter

	// Metrics populated by the server.go:HandleRPC() grpc StatsHandler function.
	otelRpcDuration otelmetrics.Float64Histogram
	otelRpcErrors   otelmetrics.Int64Counter
)

// initializeOtel sets up the Open Telemetry endpoint to send data to the
// observability backend you've configured (by default, Google Cloud
// Observability Suite). NOTE: the OTEL modules have a set of environment
// variables they read from directly to decide what host/port their endpoint
// runs on. For more details, see
// https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/
func initializeOtel() (*otelmetrics.Meter, func(context.Context) error) {
	ctx := context.Background()

	// Get resource attributes in ascending precedence order (later options override earlier ones).
	res, err := resource.New(
		context.Background(),
		// Use the GCP resource detector to detect information about the GCP platform.
		resource.WithDetectors(gcp.NewDetector()),
		// Discover and provide information about the OpenTelemetry SDK used.
		resource.WithTelemetrySDK(),
		// Compiled default Open Match attributes.
		resource.WithAttributes(
			semconv.ServiceNameKey.String("open_match"),
			semconv.ServiceVersionKey.String(serviceVersion),
		),
		// Discover and provide attributes from OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME environment variables.
		resource.WithFromEnv(),
	)
	if errors.Is(err, resource.ErrPartialResource) || errors.Is(err, resource.ErrSchemaURLConflict) {
		otelLogger.Println(err) // Log non-fatal issues.
	} else if err != nil {
		otelLogger.WithError(err).Errorf("Failed to create open telemetry resource")
	}

	// Init otel exporter for use by the collector sidecar.
	// Gets its configuration directly from OTEL env vars.
	// https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/
	// In the default case (cloud run with collector as a sidecar), the default
	// values work fine.
	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithInsecure())
	if err != nil {
		otelLogger.Fatal(err)
	}

	// Otel meter and meterprovider init
	provider := metric.NewMeterProvider(metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter)))
	meter := provider.Meter("open_match.core")

	return &meter, provider.Shutdown
}

// initializeOtelWithLocalProm runs a default OpenTelemetry Reader and
// implements prometheus.Collector, allowing it to be used as both a Reader and
// Collector. http://localhost:2223/metrics . For use with local development.
func initializeOtelWithLocalProm() (*otelmetrics.Meter, func(context.Context) error) { //nolint:unused
	// Get resource attributes in ascending precedence order (later options override earlier ones).
	res, err := resource.New(
		context.Background(),
		// Use the GCP resource detector to detect information about the GCP platform.
		resource.WithDetectors(gcp.NewDetector()),
		// Discover and provide information about the OpenTelemetry SDK used.
		resource.WithTelemetrySDK(),
		// Compiled default Open Match attributes.
		resource.WithAttributes(
			semconv.ServiceNameKey.String("open_match"),
			semconv.ServiceVersionKey.String(serviceVersion),
		),
		// Discover and provide attributes from OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME environment variables.
		resource.WithFromEnv(),
	)
	if errors.Is(err, resource.ErrPartialResource) || errors.Is(err, resource.ErrSchemaURLConflict) {
		otelLogger.Println(err) // Log non-fatal issues.
	} else if err != nil {
		otelLogger.WithError(err).Errorf("Failed to create open telemetry resource")
	}

	// This exporter embeds a default OpenTelemetry Reader and
	// implements prometheus.Collector
	exporter, err := prometheus.New()
	if err != nil {
		otelLogger.Fatal(err)
	}

	promPort := "2223"
	if cfg != nil {
		if configuredPort := cfg.GetString("OM_PROM_PORT"); configuredPort != "" {
			promPort = configuredPort
		}
	}

	// Start the prometheus HTTP server and pass the exporter Collector to it
	go func() {
		otelLogger.Infof("serving metrics at localhost:%s/metrics", promPort)
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(":"+promPort, mux) //nolint:gosec // Ignoring G114: Use of net/http serve function that has no support for setting timeouts.
		if err != nil {
			fmt.Printf("error serving http: %v", err)
			return
		}
	}()

	// Otel meter and meterprovider init
	provider := metric.NewMeterProvider(metric.WithResource(res), metric.WithReader(exporter))
	meter := provider.Meter("open_match.core")

	return &meter, provider.Shutdown
}

//nolint:cyclop,funlen // Cyclop linter sees each metric initialization as +1 cyclomatic complexity for some reason.
func registerMetrics(meterPointer *otelmetrics.Meter) {
	if meterPointer == nil {
		return
	}
	meter := *meterPointer
	var err error

	// 1. om_core.rpc.duration
	otelRpcDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"rpc.duration",
		otelmetrics.WithDescription("RPC duration"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 2. om_core.rpc.errors
	otelRpcErrors, err = meter.Int64Counter(
		metricsNamePrefix+"rpc.errors",
		otelmetrics.WithDescription("Number of RPC errors"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 3. om_core.ticket.creations
	otelTicketCreations, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.creations",
		otelmetrics.WithDescription("Number of tickets created"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 4. om_core.ticket.size
	otelTicketSize, err = meter.Float64Histogram(
		metricsNamePrefix+"ticket.size",
		otelmetrics.WithDescription("Size of tickets in kilobytes"),
		otelmetrics.WithUnit("KB"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 5. om_core.ticket.deactivations.per_call
	otelDeactivationsPerCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.deactivations.per_call",
		otelmetrics.WithDescription("Number of tickets deactivated per DeactivateTickets call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 6. om_core.ticket.deactivation.failures.invalid_id
	otelInvalidIdsPerDeactivateCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.deactivation.failures.invalid_id",
		otelmetrics.WithDescription("Number of invalid ticket IDs per DeactivateTickets call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 7. om_core.ticket.deactivation.failures.unspecified
	otelFailedIdsPerDeactivateCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.deactivation.failures.unspecified",
		otelmetrics.WithDescription("Number of tickets that failed to deactivate for unspecified reasons per DeactivateTickets call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 8. om_core.ticket.activations.per_call
	otelActivationsPerCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.activations.per_call",
		otelmetrics.WithDescription("Number of tickets activated per ActivateTickets call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 9. om_core.ticket.activation.failures.invalid_id
	otelInvalidIdsPerActivateCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.activation.failures.invalid_id",
		otelmetrics.WithDescription("Number of invalid ticket IDs per ActivateTickets call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 10. om_core.ticket.activation.failures.unspecified
	otelFailedIdsPerActivateCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.activation.failures.unspecified",
		otelmetrics.WithDescription("Number of tickets that failed to activate for unspecified reasons per ActivateTickets call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 11. om_core.mmf.calls
	otelMmfCalls, err = meter.Int64Counter(
		metricsNamePrefix+"mmf.calls",
		otelmetrics.WithDescription("Number of Matchmaking Function (MMF) calls made"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 12. om_core.mmf.failures
	otelMmfFailures, err = meter.Int64Counter(
		metricsNamePrefix+"mmf.failures",
		otelmetrics.WithDescription("Number of Matchmaking Function (MMF) calls that returned an error"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 13. om_core.mmf.ticket.deactivations
	otelMmfTicketDeactivations, err = meter.Int64Counter(
		metricsNamePrefix+"mmf.ticket.deactivations",
		otelmetrics.WithDescription("Number of ticket deactivations initiated on behalf of Matchmaking Functions (MMFs)"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 14. om_core.match.received
	otelMatches, err = meter.Int64Counter(
		metricsNamePrefix+"match.received",
		otelmetrics.WithDescription("Number of matches received from Matchmaking Functions (MMFs) and returned to the caller; attribute match.has_tickets indicates whether the match had any valid ticket IDs"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 15. om_core.mmf.prep.duration
	otelMmfPrepDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"mmf.prep.duration",
		otelmetrics.WithDescription("Time spent snapshotting active tickets, filtering pools, and chunking requests in InvokeMatchmakingFunctions before invoking MMFs"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 16. om_core.mmf.deactivation.wait
	otelMmfDeactivationWait, err = meter.Float64Histogram(
		metricsNamePrefix+"mmf.deactivation.wait",
		otelmetrics.WithDescription("Time spent waiting for match ticket deactivations to replicate to the local cache"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 17. om_core.mmf.deactivation.timeouts
	otelMmfDeactivationTimeouts, err = meter.Int64Counter(
		metricsNamePrefix+"mmf.deactivation.timeouts",
		otelmetrics.WithDescription("Number of times waiting for match ticket deactivations to replicate to the local cache timed out"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 18. om_core.ticket.assignment
	otelAssignmentsPerCall, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.assignment",
		otelmetrics.WithDescription("Number of tickets assigned (DEPRECATED)"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 19. om_core.ticket.assignment.failures
	otelTicketAssignmentFailures, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.assignment.failures",
		otelmetrics.WithDescription("Number of ticket assignment failures (DEPRECATED)"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// 20. om_core.ticket.assignment.watch
	otelAssignmentWatches, err = meter.Int64UpDownCounter(
		metricsNamePrefix+"ticket.assignment.watch",
		otelmetrics.WithDescription("Number of tickets currently being watched for assignment"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}
}
