# Development

Open Match 2 `core` (aka `om-core`) is a golang application designed to be built using [CNCF Buildpacks](https://www.cncf.io/projects/buildpacks/), 
and our primary build process specifically uses [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview). 

## Build

> [!NOTE]
> Users developing against Open Match probably don't need to re-compile `core`, and should in almost all cases just use one of the pre-built images provided in the public 
Artifact Registry.

The Open Match maintainers build `core` [remotely using Cloud Build on Google Cloud](https://cloud.google.com/docs/buildpacks/build-application#remote_builds). 
A typical build goes something like this, with `gcloud` initialized and an existing
[Docker Artifact Registry](https://cloud.google.com/artifact-registry/docs/docker/store-docker-container-images) called `open-match`:
```
gcloud builds submit --async --pack \
image=$(gcloud config get artifacts/location)-docker.pkg.dev/$(gcloud config get project)/open-match/om-core
```
There is no `cloudbuild.yaml` or `Dockerfile` required for this. [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview) takes care of everything.

## Deploy
The `deploy` directory contains a sample `service.yaml` file you can edit to deploy an `om-core` service to Cloud Run in Google Cloud. 
This file should be populated with the following:
* Your [VPC network](https://cloud.google.com/vpc/docs/overview).
  Unless your company has turned on the `constraints/compute.skipDefaultNetworkCreation` org policy, your Google Cloud project will have a VPC created already, named `default`.
* Your Service Account created for `om-core`. The `deploy/cloudrun-sa.iam` file lists all the roles the service account will need. 
* Your Redis instance IP address(es, OM supports configuring reads and writes to go to a master/replica respectively if you wish)
  that can be reached from Cloud Run. We test against Cloud Memorystore for Redis using the configuration detailed in
  [this guide](https://cloud.google.com/memorystore/docs/redis/connect-redis-instance-cloud-run).

With the `service.yaml` file populated with everything you've configured, you can deploy the service with a command like this:
```
gcloud run services replace service.yaml
```
You may need to adjust the scaling configuration and amount of memory `om-core` is allowed to use depending on your matchmaker.

`core` is just a gRPC/HTTP golang application in a container image and can be deployed in most environments (local Docker/Minikube/Kubernetes/kNative) with a little effort. 

## Protocol Buffers
The `proto` directory contains the protocol buffer definition files and documentation about how Open Match uses them. If you're developing against Open Match, you
probably want to write your Matchmaking Functions ('MMFs') in a language you're comfortable in, so you'll want to use `protoc` to generate libraries in your preferred language.

## Testing
You can run the golang unit tests using `go test ./...` from the `core` directory. 

If you want to quickly test a running copy of `om-core`, the file `docs/example_ticket.json` has an example of a JSON-formatted ticket that will pass validation against the `v2/tickets` RESTful HTTP API endpoint with a command like this:
```
curl -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
-d "$(jq -c . example_ticket.json)" ${URL}/v2/tickets
```
If the creation is successful you should get a response like this:
```
{"ticketId":"1716339182-0"}
```
