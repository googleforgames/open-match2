# Deployment files
Deploy using one of these:
* `service.yaml` is an example Cloud Run service yaml file. Modify as necessary for your deployment and apply using:

`gcloud run services replace service.yaml`
* `cloudbuild.yaml` is an example Cloud Build job yaml configuration that creates/updates a Run deployment. More details for using it can be found in the comment at the top of the file.
  
Other included files:
* `cloudrun-sa.iam` is a list of the IAM roles the Open Match 2 om-core container expects it's [Google Cloud Service Account](https://cloud.google.com/iam/docs/service-account-overview) to have, for the documentation/samples to work correctly.
* the `otel-collector-sidecar` contains the files necessary to build the Open Telemetry sidecar container image as detailed in the [Write OTLP metrics by using an OpenTelemetry sidecar](https://cloud.google.com/run/docs/tutorials/custom-metrics-opentelemetry-sidecar) guide.
