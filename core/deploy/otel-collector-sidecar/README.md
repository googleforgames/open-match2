# Open Telemetry collector sidecar

Open-Match 2 supports the stock 'contrib' Open Telemetry collector Docker image(`otel/opentelemetry-collector-contrib`), run as a sidecar to collect metrics.  The example collector configuration provided in the [Google Cloud golang samples repository for Cloud Run](https://github.com/GoogleCloudPlatform/golang-samples/tree/main/run/custom-metrics/collector) works fine with Open Match 2.  You can find more details in the [Write OTLP metrics by using an OpenTelemetry sidecar](https://cloud.google.com/run/docs/tutorials/custom-metrics-opentelemetry-sidecar) documentation.

## Open Match Users
The Open Match artifact registry maintains a pre-built otel collector image with the necessary config file included, and it's already referenced directly in the core/deploy/service.yaml example. You shouldn't need to modify or rebuild the image unless you're doing something advanced, like replatforming Open Match or replacing the Open Telemetry backend with a different metrics/tracing/logging provider.

## Open Match Maintainers 
To build the OpenTelemetry Collector sidecar image:
1.  Make sure your `gcloud` installation is configured to use the Open Match development project
1.  Make sure your `glcoud` installation has the `artifacts/location` config set to the location for the Open Match artifact registry
1.  (optional) If you want to update the configuration file, update the permanent URL on the second line of the Dockerfile with the latest from the [official Google golang-samples repo](https://github.com/GoogleCloudPlatform/golang-samples/blob/main/run/custom-metrics/collector/collector-config.yaml). Probably unnecessary unless something is broken with metrics/tracing.
1. run the following command from this directory:

 `gcloud builds submit . --tag $(gcloud config get artifacts/location)-docker.pkg.dev/$(gcloud config get project)/open-match/otel-collector-sidecar`

