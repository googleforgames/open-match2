# Open Telemetry collector sidecar

Open Match 2 uses the Google-Built OpenTelemetry Collector Docker image (`us-docker.pkg.dev/cloud-ops-agents-artifacts/google-cloud-opentelemetry-collector/otelcol-google`), and also supports the upstream contrib collector image (`otel/opentelemetry-collector-contrib`), run as a sidecar container on Cloud Run to collect metrics. Because `otelcol-google` reads its default configuration from `/etc/otelcol-google/config.yaml` while `otel/opentelemetry-collector-contrib` reads `/etc/otelcol-contrib/config.yaml`, the `Dockerfile` in this directory copies `config.yaml` to both `/etc/otelcol-google/config.yaml` and `/etc/otelcol-contrib/config.yaml` so the Google Managed Prometheus exporter configuration is loaded automatically under either base image.

The example collector configuration provided in the [Google Cloud golang samples repository for Cloud Run](https://github.com/GoogleCloudPlatform/golang-samples/tree/main/run/custom-metrics/collector) works with Open Match 2. You can find more details in the [Write OTLP metrics by using an OpenTelemetry sidecar](https://cloud.google.com/run/docs/tutorials/custom-metrics-opentelemetry-sidecar) documentation.

## Open Match Users
The Open Match artifact registry maintains a pre-built OpenTelemetry Collector sidecar image with the necessary config file included, and it is already referenced directly in the `deploy/cloud_run/service.yaml` and `deploy/cloud_run/cloudbuild.yaml` examples. You shouldn't need to modify or rebuild the image unless you're doing something advanced, like replatforming Open Match or replacing the OpenTelemetry backend with a different metrics/tracing/logging provider.

## Open Match Maintainers 
To build the OpenTelemetry Collector sidecar image:
1.  Make sure your `gcloud` installation is configured to use the Open Match development project
1.  Make sure your `gcloud` installation has the `artifacts/location` config set to the location for the Open Match artifact registry
1.  Run the following command from this directory:

 `gcloud builds submit . --tag $(gcloud config get artifacts/location)-docker.pkg.dev/$(gcloud config get project)/open-match/otel-collector-sidecar`

