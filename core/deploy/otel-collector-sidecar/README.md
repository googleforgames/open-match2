Users: The Open Match artifact registry already has a pre-built version of this image. You shouldn't need to modify it unless you're doing something advanced, like replatforming Open Match.

OM Maintainers: To build the OpenTelemetry Collector sidecar image, run the following command, substituting the Open Match artifact registry URL:
`gcloud builds submit . --tag $(gcloud config get artifacts/location)-docker.pkg.dev/$(gcloud config get project)/open-match/otel-collector-sidecar`
If you want to make sure you're using the latest configuration, go get a new permanent URL for the config file from the official Google golang-samples and update the second line of the Dockerfile.
