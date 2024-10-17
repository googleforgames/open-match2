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

const metricsNamePrefix = "om_core."

var (
	otelLogger = logrus.WithFields(logrus.Fields{
		"app":            "open_match",
		"component":      "core",
		"implementation": "otel",
	})

	// Metric variable declarations are global, so they can be accessed directly in the application code.
	// Metrics populated by the grpc function implementations in main.go.
	otelActivationsPerCall                                 otelmetrics.Int64Histogram
	otelInvalidIdsPerActivateCall                          otelmetrics.Int64Histogram
	otelFailedIdsPerActivateCall                           otelmetrics.Int64Histogram
	otelDeactivationsPerCall                               otelmetrics.Int64Histogram
	otelInvalidIdsPerDeactivateCall                        otelmetrics.Int64Histogram
	otelFailedIdsPerDeactivateCall                         otelmetrics.Int64Histogram
	otelAssignmentsPerCall                                 otelmetrics.Int64Histogram
	otelProfileChunksPerInvokeMMFCall                      otelmetrics.Int64Histogram
	otelCachedTicketsAvailableForFilteringPerInvokeMMFCall otelmetrics.Int64Histogram
	otelPoolsPerProfile                                    otelmetrics.Int64Histogram
	otelTicketSize                                         otelmetrics.Float64Histogram
	otelMmfTicketDeactivations                             otelmetrics.Int64Counter
	otelMmfFailures                                        otelmetrics.Int64Counter
	otelMatches                                            otelmetrics.Int64Counter
	otelAssignmentWatches                                  otelmetrics.Int64UpDownCounter

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

	// Get resource attributes
	res, err := resource.New(
		context.Background(),
		// Use the GCP resource detector to detect information about the GCP platform
		resource.WithDetectors(gcp.NewDetector()),
		// Discover and provide attributes from OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME environment variables.
		resource.WithFromEnv(),
		// Discover and provide information about the OpenTelemetry SDK used.
		resource.WithTelemetrySDK(),
		// Open Match attributes
		resource.WithAttributes(
			semconv.ServiceNameKey.String("open_match"),
			semconv.ServiceVersionKey.String("2.0.0"),
		),
	)
	if errors.Is(err, resource.ErrPartialResource) || errors.Is(err, resource.ErrSchemaURLConflict) {
		otelLogger.Println(err) // Log non-fatal issues.
	} else if err != nil {
		otelLogger.Errorf(fmt.Errorf("Failed to create open telemetry resource: %w", err).Error())

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

// initializeOtelWithLocalProm runs  a default OpenTelemetry Reader and
// implements prometheus.Collector, allowing it to be used as both a Reader and
// Collector. http://localhost:2223/metrics . For use with local development.
func initializeOtelWithLocalProm() (*otelmetrics.Meter, func(context.Context) error) { //nolint:unused
	// Get resource attributes
	res, err := resource.New(
		context.Background(),
		// Use the GCP resource detector to detect information about the GCP platform
		resource.WithDetectors(gcp.NewDetector()),
		// Discover and provide attributes from OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME environment variables.
		resource.WithFromEnv(),
		// Discover and provide information about the OpenTelemetry SDK used.
		resource.WithTelemetrySDK(),
		// Open Match attributes
		resource.WithAttributes(
			semconv.ServiceNameKey.String("open_match"),
			semconv.ServiceVersionKey.String("2.0.0"),
		),
	)
	if errors.Is(err, resource.ErrPartialResource) || errors.Is(err, resource.ErrSchemaURLConflict) {
		otelLogger.Println(err) // Log non-fatal issues.
	} else if err != nil {
		otelLogger.Errorf(fmt.Errorf("Failed to create open telemetry resource: %w", err).Error())

	}

	// This exporter embeds a default OpenTelemetry Reader and
	// implements prometheus.Collector
	exporter, err := prometheus.New()
	if err != nil {
		otelLogger.Fatal(err)
	}

	// Start the prometheus HTTP server and pass the exporter Collector to it
	go func() {
		otelLogger.Infof("serving metrics at localhost:2223/metrics")
		http.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(":2223", nil) //nolint:gosec // Ignoring G114: Use of net/http serve function that has no support for setting timeouts.
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

//nolint:cyclop // Cyclop linter sees each metric initialization as +1 cyclomatic complexity for some reason.
func registerMetrics(meterPointer *otelmetrics.Meter) {
	meter := *meterPointer
	var err error

	// Initialize all declared Metrics
	otelTicketSize, err = meter.Float64Histogram(
		metricsNamePrefix+"ticket.size",
		otelmetrics.WithDescription("Size of successfully created tickets"),
		otelmetrics.WithUnit("kb"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelRpcDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"rpc.duration",
		otelmetrics.WithDescription("RPC duration"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelRpcErrors, err = meter.Int64Counter(
		metricsNamePrefix+"rpc.errors",
		otelmetrics.WithDescription("Total gRPC errors returned"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelActivationsPerCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.activation.requests",
		otelmetrics.WithDescription("Total tickets set to active, so they appear in pools"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelInvalidIdsPerActivateCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.activation.failures.invalid_id",
		otelmetrics.WithDescription("Ticket activations per ActivateTickets rpc call that failed due to an invalid id"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelFailedIdsPerActivateCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.deactivation.failures.unspecified",
		otelmetrics.WithDescription("Ticket activations per ActivateTickets rpc call that failed due to an unspecified error"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	// TODO: Distinguish between active deactivations (using the API endpoint) and
	// triggered deactivations (by matches returned from mmfs) using metric
	// attributes at the time of recording...?
	otelDeactivationsPerCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.deactivation.requests",
		otelmetrics.WithDescription("Tickets set to deactive per DeactivateTickets rpc call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}
	otelInvalidIdsPerDeactivateCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.deactivation.failures.invalid_id",
		otelmetrics.WithDescription("Ticket deactivations per DeactivateTickets rpc call that failed due to an invalid id"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}
	otelFailedIdsPerDeactivateCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.deactivation.failures.unspecified",
		otelmetrics.WithDescription("Ticket deactivations per DeactivateTickets rpc call that failed due to an unspecified error"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelAssignmentsPerCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.assignment",
		otelmetrics.WithDescription("Number of tickets assignments included in each CreateAssignments() call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelAssignmentWatches, err = meter.Int64UpDownCounter(
		metricsNamePrefix+"ticket.assigment.watch",
		otelmetrics.WithDescription("Number of tickets assignments included in each CreateAssignments() call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelProfileChunksPerInvokeMMFCall, err = meter.Int64Histogram(
		metricsNamePrefix+"profile.chunks",
		otelmetrics.WithDescription("Number of chunks the profile was split into after all tickets were populated in all pools during the InvokeMatchmakingFunctions() call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelCachedTicketsAvailableForFilteringPerInvokeMMFCall, err = meter.Int64Histogram(
		metricsNamePrefix+"cache.tickets.available",
		otelmetrics.WithDescription("Number of active tickets in the cache at the time the InvokeMatchmakingFunctions() call processed pool filters for the provided profile"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelPoolsPerProfile, err = meter.Int64Histogram(
		metricsNamePrefix+"profile.pools",
		otelmetrics.WithDescription("Number of pools in the profile provided to the InvokeMatchmakingFunctions() call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelMmfFailures, err = meter.Int64Counter(
		metricsNamePrefix+"mmf.failures",
		otelmetrics.WithDescription("Number of MMF failures"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelMmfTicketDeactivations, err = meter.Int64Counter(
		metricsNamePrefix+"mmf.deactivations",
		otelmetrics.WithDescription("Number of deactivations due to tickets being returned in matches by MMFs"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelMatches, err = meter.Int64Counter(
		metricsNamePrefix+"match.received",
		otelmetrics.WithDescription("Total matches received from MMFs"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

}
