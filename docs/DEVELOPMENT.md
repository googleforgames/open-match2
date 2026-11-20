# Development

Open Match 2 `core` (aka `om-core`) is a golang application designed to be built using [CNCF Buildpacks](https://www.cncf.io/projects/buildpacks/), and our primary build process specifically uses [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview). 

Our target development environment is the default Cloud Shell you get by clicking 'Activate Cloud Shell' button (terminal icon) in the upper-right of the [console.cloud.google.com](https://console.cloud.google.com/) web UI.

## Build

> [!NOTE]
> Users developing against Open Match probably don't need to re-compile `core`, and should in almost all cases just use one of the pre-built images provided in the public Artifact Registry.

The Open Match maintainers build `core` [remotely using Cloud Build on Google Cloud](https://cloud.google.com/docs/buildpacks/build-application#remote_builds).  A typical build goes something like this, with the repo cloned,  `gcloud` initialized, and an existing [Docker Artifact Registry](https://cloud.google.com/artifact-registry/docs/docker/store-docker-container-images) called `open-match`:
```
# Upgrade direct dependencies only, tidy modules, run static analysis and race-enabled tests, and kick off a Cloud Build
go get $(go list -m -f '{{if not (or .Main .Indirect)}}{{.Path}}@latest{{end}}' all) && \
go mod tidy && go vet ./... && OM_SKIP_REDIS_TESTS=true go test -race ./... && \
gcloud builds submit --async --pack \
image=$(gcloud config get artifacts/location)-docker.pkg.dev/$(gcloud config get project)/open-match/om-core
```
There is no `cloudbuild.yaml` or `Dockerfile` required for a pack build. [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview) takes care of everything.

The `go 1.27.4` directive in `go.mod` specifies the **minimum Go toolchain version, not a pin**: Google Cloud Buildpacks build with the newest available Go release by default, and no buildpack Go-version pin (`project.toml` or `GOOGLE_GO_VERSION`) is checked into the repository. If a newly released Go version breaks your build, you can pin locally at build time using the buildpack Go-version environment variable (`GOOGLE_GO_VERSION`, e.g., `--build-env-vars GOOGLE_GO_VERSION=1.27.4`).

### For Maintainers

The following represent best practices when doing a new release:

* Go to [cloud.google.com](https://cloud.google.com/) and open a clean Cloud Shell terminal. Run `go version` to find the version offered by default in Cloud Shell. Then kick off a Cloud Build using pack (as in the command above) and find the lines that specify the default version of Golang being used there.  You should see lines similar to this in the build logs:
```
Step #1: ===> BUILDING
Step #1: [builder] target distro name/version labels not found, reading /etc/os-release file
Step #1: [builder] === Go - Runtime (google.go.runtime@0.9.1) ===
Step #1: [builder] Go version not specified, using latest available Go runtime for the stack "ubuntu2204"
Step #1: [builder] 2025/08/27 06:00:39 [DEBUG] GET https://go.dev/dl/?mode=json
Step #1: [builder] ***** CACHE MISS: "go"
Step #1: [builder] Installing Go v1.24.6.
``` 
set the minimum Go version in `go.mod` (`go mod edit -go=<VERSION>`, currently `1.27.4`) without adding a repository-level buildpack Go pin. This ensures that users following the instructions to build in their own projects can build using Cloud Shell and the latest Google Cloud Buildpacks while preserving the ability to pin locally via `GOOGLE_GO_VERSION` if needed.
* When upgrading dependencies, upgrade **direct dependencies only** (avoiding a blanket `go get -u ./...` across indirect dependencies), and then run `go mod tidy`, `go vet ./...`, and `go test -race ./...` to verify that everything builds and passes cleanly before committing changes to `go.mod` and `go.sum`.

## Deploy
The `deploy/cloud_run` directory contains sample `deploy/cloud_run/service.yaml` and `deploy/cloud_run/cloudbuild.yaml` files, either of which you can edit to deploy an `om-core` service to Cloud Run in Google Cloud. Typically `deploy/cloud_run/service.yaml` is easier to adapt to manually deploying via command line, and `deploy/cloud_run/cloudbuild.yaml` is easier to adapt to a continuous build system, although feel free to use them as you see fit.

You should populate the following:
* Your [VPC network](https://cloud.google.com/vpc/docs/overview).  Unless your company has turned on the `constraints/compute.skipDefaultNetworkCreation` org policy, your Google Cloud project will have a VPC created already, named `default`.
* Your Service Account created for `om-core`. The `deploy/cloud_run/cloudrun-sa.iam` file lists all the roles the service account will need. 
* Your Redis instance IP address(es, OM supports configuring reads and writes to go to a master/replica respectively if you wish) that can be reached from Cloud Run. We test against Cloud Memorystore for Redis using the configuration detailed in [this guide](https://cloud.google.com/memorystore/docs/redis/connect-redis-instance-cloud-run).

With the `deploy/cloud_run/service.yaml` file populated with everything you've configured, you can deploy the service with a command like this:
```
gcloud run services replace deploy/cloud_run/service.yaml
```
If you instead intend to use the `deploy/cloud_run/cloudbuild.yaml` file populated with everything you've configured, you can deploy the service with a command like the one listed in the comment at the top of `deploy/cloud_run/cloudbuild.yaml` (requires you to have enabled Cloud Build and have appropriate service account permissions).

The Cloud Run manifests default to `OM_LOGGING_LEVEL=info`, `2` CPU and `1Gi` (`1` GiB) memory limits on the `core` container, and a `900` s request timeout (`timeoutSeconds: 900` / `--timeout 900`). The Cloud Run request timeout must be at least `OM_MMF_TIMEOUT_SECS` + `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` ÷ 1000. Both are defined in `internal/config/config.go` and overridable by environment variables. With the default timeouts set in `internal/config/config.go`, that is 660 s. The Cloud Run maximum is 3600 s. You may need to adjust the scaling configuration and amount of CPU/memory `om-core` is allowed to use depending on your matchmaker.

For GKE (`deploy/gke/om.yaml`), the `om-core` container configures a native `lifecycle.preStop.sleep.seconds: 10` hook (requiring Kubernetes 1.30 or later, letting the pod leave Service endpoints before `SIGTERM`) and Pod `terminationGracePeriodSeconds: 30`. The termination grace period must exceed the `preStop` sleep plus the graceful shutdown defaults in `start` (`server.go`, `5` s) and `main.go` (`3` s). Validate edited manifests before deploying with `python3 .agents/skills/open_match2/scripts/validate_deploy_config.py`.

`core` is just a gRPC/HTTP golang application in a container image and can be deployed in most environments (local Docker/Minikube/Kubernetes/kNative) with a little effort. 

## Protocol Buffers
The `proto` directory contains the protocol buffer definition files and documentation about using and building them. If you're developing against Open Match, you probably want to write your Matchmaking Functions ('MMFs') in a language you're comfortable in, so you'll want to use `protoc` to generate libraries in your preferred language.

## Testing and Benchmarks
You can run the full Go unit test suite (with the race detector enabled) from the repository root:
```
go test -race ./...
```

The test suite supports two Redis-backed execution modes (plus a Redis-free fast loop):
* **Local mode**: When `REDISHOST` is unset, `TestMain` (and `internal/statestore/redis`) tries Redis on `localhost:6379`; if unreachable, it automatically falls back to Testcontainers using `redis:7.0.10`. End users may change that Testcontainers image tag locally to try later Redis versions, which are untested and unsupported.
* **Cloud mode**: In CI / Cloud Build, the build runner provides a Redis container or instance and sets `REDISHOST` and `REDISPORT` in the test environment, which `TestMain` uses directly for all read and write Redis connection settings.

The supported Redis release line is **`7.2`** (the last 3-clause-BSD Redis release); Redis `7.4` and later may work, but are untested and unsupported.

### Running Unit Tests Without Redis or a Container Runtime
If you are running in an environment without a local Redis server or container runtime (such as Cloud Shell or a restricted CI container), set `OM_SKIP_REDIS_TESTS=true` to run all unit and in-memory integration tests without Redis:
```
OM_SKIP_REDIS_TESTS=true go test -race -count=1 ./...
```

### Running the Synthetic Burst Replication Stress Test and Cache Benchmarks
The `internal/statestore/cache` package includes a synthetic burst replication stress test (`TestSyntheticBurstReplicationStress`) that simulates high-concurrency match ticket deactivations and concurrent min-heap TTL expirations, as well as microbenchmarks for cache expiration (`BenchmarkExpireCacheEntries`), active ticket snapshotting (`BenchmarkSnapshotActiveTickets`), and unpacked vs. packed replication throughput (`BenchmarkReplicationThroughput`):
```
# Run the synthetic burst replication stress test with the race detector
go test -race -v -run TestSyntheticBurstReplicationStress ./internal/statestore/cache

# Run the internal/statestore/cache benchmarks
go test -bench=. -benchmem ./internal/statestore/cache
```

### Smoke-Testing a Running Instance
If you want to quickly test a running copy of `om-core`, the file `docs/example_ticket.json` has an example of a JSON-formatted ticket that will pass validation against the `v2/tickets` RESTful HTTP API endpoint with a command like this:
```
curl -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
-d "$(jq -c . example_ticket.json)" ${URL}/tickets
```
If the creation is successful you should get a response like this:
```
{"ticketId":"1716339182-0"}
```

## Running locally
If you want to just run the `om-core` application locally, you can `go run .` in the root directory, and it will start up. However, by default, most of the configuration variables are set to work out-of-the-box in Cloud Run, so when running locally you'll probably want to set (at least) the following environment variables:

* `OM_OTEL_SIDECAR=false` By default, the application tries to export metrics to a local OpenTelemetry sidecar, and running without one will cause an error. Use `false` to instead run a local metrics exporter that doesn't rely on an OpenTelemetry Collector.
* `OM_STATE_STORAGE_TYPE=memory` By default, the application tries to store state in Redis (and you should always use that setting in production).  `memory` instead uses only a volitile local copy of state in RAM, and ignores the values set in all the `*REDIS*` config variables. Only use this for testing.
* `OM_LOGGING_FORMAT=text` By default log lines are written in JSON, setting this to `text` is a bit more human-readable.

Additionally you may also want to adjust these:

* `PORT` defaults to 8080, but if you are running another process on that port locally, you'll want to pick a new port.
* `OM_LOGGING_LEVEL` defaults to `info`, but you may want to see more details by using `debug` or even `trace` when testing locally.

