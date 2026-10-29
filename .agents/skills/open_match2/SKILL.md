---
name: open-match2
description: Architecture, API contracts, replication cache internals, configuration tuning, local testing, and Cloud Run / GKE deployment workflows for Open Match 2 (om-core). Use when modifying, debugging, testing, tuning, or deploying open-match2.
---

# Open Match 2 (`om-core`) Agent Skill

Open Match 2 (`github.com/googleforgames/open-match2/v2`) consolidates the multi-service architecture of Open Match 1 into a single stateless-interface binary (`om-core`) backed by an in-memory eventually consistent `ReplicatedTicketCache` synchronized over Redis Streams (`om-replication`).

---

## 1. Repository Map

| Path | Package / Role |
| :--- | :--- |
| `main.go` | `package main` — Application entrypoint, signal handling, OTel and state replicator lifecycle, `CreateTicket`, `ActivateTickets`, `DeactivateTickets`, `InvokeMatchmakingFunctions` (cold-start catch-up barrier, 4 MB pool chunking, URL parsing, HTTPS OIDC auth, automatic match ticket deactivation), and deprecated `CreateAssignments` / `WatchAssignments` handlers. |
| `server.go` | `package main` — Dual gRPC server (`OM_GRPC_PORT`, default `50581`) and HTTP `grpc-gateway` reverse proxy (`PORT`, default `8080`, `ReadHeaderTimeout: 10s`), unary/stream panic recovery interceptors, `stats.Handler` RPC latency/error instrumentation, and bounded 5s graceful shutdown. |
| `metrics.go` | `package main` — OpenTelemetry SDK initialization (`initializeOtel` for OTLP gRPC sidecar and `initializeOtelWithLocalProm` on `OM_PROM_PORT`), resource attribute precedence, and registration of the 20 Core `om_core.*` metric instruments. |
| `main_test.go` | `package main` — Unit and integration test suite covering ticket lifecycle, state updates, pool chunking, 23-case MMF URL parsing, HTTPS OIDC auth refresh, catch-up barrier, panic recovery, graceful shutdown, OTel metrics, and deployment manifests. |
| `internal/config/config.go` | `package config` — Centralized Viper configuration (`Read()`, `LogConfig()`), registering all 51 configuration defaults and enforcing strictly positive (`> 0`) integer bounds on 9 critical channel/batch/interval keys. |
| `internal/filter/filter.go` | `package filter` — Pool filter validation (`ValidatePoolFilters`) and evaluation (`In`) across `CreationTimeRangeFilter` (supporting bounded, half-open, and open-ended ranges), `DoubleRangeFilters`, `StringEqualsFilters`, and `TagPresentFilters`. |
| `internal/logging/logging.go` | `package logging` — Logrus setup (`ConfigureLogging`) supporting `json` (Cloud Logging compatible `timestamp`, `severity`, `message` fields) and `text` formatters. |
| `internal/mmfauth/idtokencache.go` | `package mmfauth` — Concurrency-safe per-audience Google OIDC ID token cache (`IDTokenCache`) with single-flight `Refresh` on `codes.Unauthenticated` / `codes.PermissionDenied` (`IsAuthRejection`). |
| `internal/statestore/datatypes/store.go` | `package store` — `StateReplicator` interface, `StateUpdate` / `StateResponse` types, sentinel errors, `PrimaryKey()`, `ParseReplIdTimestampMs()`, and numeric `CompareReplIds()`. |
| `internal/statestore/redis/redis.go` | `package redisReplicator` — Production Redis Streams replicator (`XADD om-replication *` pipelining, multi-key packed `activate`/`deactivate`, approximate `XTRIM om-replication MINID ~`, blocking `XREAD COUNT ... BLOCK ...`, exponential backoff dialing, and idempotent `Close()`). |
| `internal/statestore/memory/memory.go` | `package memoryReplicator` — Single-process in-memory replicator for local development and unit testing (`OM_STATE_STORAGE_TYPE=memory`). |
| `internal/statestore/cache/replicatedticketcache.go` | `package cache` — `ReplicatedTicketCache` with lock-free `ActiveTickets` index (`SnapshotActiveTickets`), cross-caller outgoing request coalescing, adaptive incoming polling, cold-start catch-up barrier (`WaitForCatchUp`), event-driven deactivation watermark (`WaitForDeactivation`), and asynchronous chunked min-heap TTL expiration. |
| `internal/statestore/cache/metrics.go` | `package cache` — Registration of the 24 Cache `om_core.cache.*` OpenTelemetry metrics, 17-bucket sub-millisecond duration histogram boundaries, and atomic gauge callbacks. |
| `proto/` & `pkg/` | Protobuf definitions (`proto/api.proto`, `proto/mmf.proto`, `proto/messages.proto`, `proto/grpc_gateway_http_mapping.yaml`) and generated Go gRPC (`pkg/pb`) and `grpc-gateway` (`pkg/api`) code. |
| `deploy/cloud_run/` | Cloud Run service template (`deploy/cloud_run/service.yaml`), Cloud Build pipeline (`deploy/cloud_run/cloudbuild.yaml`), IAM role list (`deploy/cloud_run/cloudrun-sa.iam`), and OpenTelemetry Collector sidecar (`deploy/cloud_run/otel-collector-sidecar/`). |
| `deploy/gke/om.yaml` | GKE Deployment, ServiceAccount, Service, and OTel Collector ConfigMap template. |
| `ci.yaml` | Repository CI Cloud Build definition (unit tests, Cloud Buildpacks image build, Cloud Run deployment, and ticket creation smoke test). |
| `docs/` | English and Japanese (`docs/jp/`) architecture, development, metrics, FAQ/tuning, and OM1-to-OM2 migration documentation. |

---

## 2. Eight Core Architectural Rules and Gotchas

1. **Tickets Start `inactive` and Cannot Be Manually Deleted**:
   - `CreateTicket` writes a new ticket to state storage in the **`inactive`** state (`InactiveSet`). A newly created ticket will **never** appear in matchmaking pools until `ActivateTickets` is called for its ticket ID.
   - There is no `DeleteTicket` RPC. Tickets are immutable once created and are automatically evicted when their TTL (`OM_CACHE_TICKET_TTL_MS`, applied relative to `Ticket.attributes.creation_time` when `Ticket.expiration_time` is omitted) expires via asynchronous min-heap sweeps and Redis Stream `XTRIM`.

2. **Viper `SetDefault` Registration & Positive Integer Enforcement**:
   - Viper's `AutomaticEnv()` only reads environment variables for keys that already have a default registered via `cfg.SetDefault(...)` in `internal/config/config.go` (`Read()`). Never read an unregistered env var via Viper without registering its default in `Read()`.
   - Nine integer configuration keys control channel capacities, ticker intervals, and batch sizes (`OM_CACHE_EXPIRATION_INTERVAL_MS`, `OM_CACHE_IN_MAX_APPLY_DURATION_MS`, `OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE`, `OM_CACHE_IN_QUEUE_BUFFER_SIZE`, `OM_CACHE_OUT_QUEUE_BUFFER_SIZE`, `OM_CACHE_OUT_MAX_PACKED_KEYS`, `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`, `OM_CACHE_IN_MAX_UPDATES_PER_POLL`, and `OM_MAX_STATE_UPDATES_PER_CALL`). Setting any of them to `<= 0` causes `config.Read()` to log a warning and restore the positive default.

3. **Event-Sourced Replication & Cold-Start Catch-Up Barrier**:
   - `om-core` instances never mutate their local `ReplicatedTicketCache` directly during an RPC write. All state mutations (`Ticket`, `Activate`, `Deactivate`, `Assign`) flow through `OutgoingReplicationQueue` to the central stream (`om-replication`) and are applied locally only when read back by `IncomingReplicationQueue`.
   - On cold start, `InvokeMatchmakingFunctions` waits on `tc.WaitForCatchUp` until the initial stream backlog is polled and applied (returning `codes.Unavailable` with `"initial ticket cache replication catch-up did not complete in time"` if the context expires first).

4. **Automatic Match Ticket Deactivation & `"deactivation_timeout"` Extension**:
   - For every `Match` streamed back by an MMF, `om-core` validates all roster ticket IDs against `^\d{13}-\d+$`, skips invalid IDs for deactivation while listing them in `Match.Extensions["invalid_ticket_ids"]` (`MatchInvalidTicketIdsExtensionKey`, `structpb.ListValue` of strings), and automatically deactivates all valid tickets.
   - Every MMF response is returned to the caller (RPC-framework semantics): ticketless matches and match-less responses are forwarded immediately without deactivation and counted in `om_core.match.received` with `match.has_tickets` set to `false` (`true` when at least one valid ticket ID is present). If any MMF fails, all returned matches are still streamed, and then the stream ends with `codes.Internal` summarizing every failure plus one `errdetails.ErrorInfo` (reason `MMF_FAILURE`, metadata `mmf_name`, `grpc_code`, `message`) per failed MMF.
   - When `OM_MATCH_TICKET_DEACTIVATION_WAIT=true` (default), `om-core` blocks streaming the match to the caller until `tc.WaitForDeactivation` confirms local cache replication (up to `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`, default `60000` ms). In this mode only, failed-write tickets are also listed in `Match.Extensions["deactivation_failed_ticket_ids"]` (`MatchDeactivationFailedTicketIdsExtensionKey`, `structpb.ListValue` of strings); when `OM_MATCH_TICKET_DEACTIVATION_WAIT=false`, that extension is never set. If all deactivation writes fail or the wait times out in `WAIT=true` mode, `om-core` increments `om_core.mmf.deactivation.timeouts`, annotates `Match.Extensions["deactivation_timeout"]` (`MatchDeactivationTimeoutExtensionKey`) with `wrapperspb.Bool(true)`, and streams the match to the Director.

5. **Decoupled Match Evaluation & Director Ticket Collision Resolution**:
   - Open Match 2 removes OM1's Synchronizer and Evaluator. Concurrent MMFs can propose matches containing overlapping tickets.
   - The caller (Director) is the final arbiter: it must detect ticket collisions across streamed matches, allocate game servers for winning matches, and call `ActivateTickets` to re-activate tickets belonging to discarded matches.

6. **Cross-Repository Per-Ticket Error Contract (`codes.Internal` vs. `codes.InvalidArgument`)**:
   - `ActivateTickets` and `DeactivateTickets` validate all ticket IDs up front and process valid IDs even when some IDs in the batch are malformed.
   - Invalid ticket IDs produce `codes.InvalidArgument`, whereas state-storage replication failures produce `codes.Internal` (`"unable to update ticket state"`) with per-ticket `errdetails.BadRequest_FieldViolation` entries (`Field: "ticket_id/<id>"`) whose `Description` starts with `"Unable to update ticket <id> state to <active|inactive> : <err>"`. `omclient.ActivateTicketsWithResults` in `open-match-ecosystem` depends on this exact prefix and status code to retry only transient storage failures.

7. **MMF URL Parsing & Strict `https://` OIDC Authentication**:
   - `parseMmfUrl` enables TLS transport credentials and Google Cloud Run OIDC ID token injection (`internal/mmfauth.IDTokenCache`) **exclusively** when the parsed MMF URL scheme is `https://`.
   - A scheme-less host (even when `port` is `443`, such as `host: "mmf.example.com", port: 443`) defaults to `http://` (plaintext gRPC without OIDC token injection). Always include the `https://` prefix in `MatchmakingFunctionSpec.host` for Cloud Run MMFs.
   - `om-core` always calls `CloseSend()` after sending all `ChunkedMmfRunRequest` chunks (4 MB max per chunk) so MMFs reading until `io.EOF` never hang.

8. **gRPC-Gateway JSON Streaming Envelope & Bounded Metric Cardinality**:
   - Over the HTTP `grpc-gateway` (`POST /matches:fetch` and `POST /assignments:watch`), streamed responses are newline-delimited JSON objects wrapped inside a `"result"` envelope: `{"result":{"match":{...}}}`. HTTP clients must unwrap `"result"` before unmarshaling the protobuf message.
   - Name profiles using reverse-DNS notation with a dynamic trailing segment (e.g., `com.example.mode.region.1716339182`): `om-core` strips the substring after the last `.` when recording the `profile.name` OpenTelemetry attribute, and `om_core.rpc.errors` records only bounded `rpc` and `grpc.status` attributes.

---

## 3. Local Development and Test Workflows

### Run `om-core` Locally (No Redis or OTel Sidecar Required)
```bash
OM_STATE_STORAGE_TYPE=memory \
OM_OTEL_SIDECAR=false \
OM_LOGGING_FORMAT=text \
OM_LOGGING_LEVEL=debug \
go run .
```
- HTTP REST (`grpc-gateway`) listens on `localhost:8080` (`PORT`).
- Internal gRPC listens on `localhost:50581` (`OM_GRPC_PORT`).
- Prometheus metrics listen on `http://localhost:2223/metrics` (`OM_PROM_PORT`) when `OM_OTEL_SIDECAR=false`.

### Build, Vet, and Run Unit Tests
```bash
# Compile and run static analysis
go build ./...
go vet ./...

# Run full race-enabled unit test suite without Redis or Docker
OM_SKIP_REDIS_TESTS=true go test -race -count=1 ./...

# Run full test suite including Testcontainers Redis integration tests
go test -race -count=1 ./...
```

### Run Synthetic Burst Replication Stress Tests and Cache Benchmarks
```bash
# Run high-concurrency match deactivation + TTL expiration stress test
go test -race -v -run TestSyntheticBurstReplicationStress ./internal/statestore/cache

# Run ReplicatedTicketCache microbenchmarks (expiration, snapshot, packed vs unpacked throughput)
go test -bench=. -benchmem ./internal/statestore/cache
```

### Upgrade Dependencies (Maintainer Workflow)
Upgrade **direct dependencies only** (never run a blanket `go get -u ./...` across indirect dependencies):
```bash
go get $(go list -m -f '{{if not (or .Main .Indirect)}}{{.Path}}@latest{{end}}' all) && \
go mod tidy && go vet ./... && OM_SKIP_REDIS_TESTS=true go test -race ./...
```

---

## 4. Cloud Run and GKE Deployment Workflows

### Critical Cloud Run Requirements for `om-core`
Because `om-core` relies on continuous background goroutines (`IncomingReplicationQueue`, `OutgoingReplicationQueue`, and asynchronous min-heap TTL expiration) to replicate ticket state from Redis Streams and evict expired tickets, Cloud Run's default request-scoped CPU allocation freezes replication between active HTTP requests. Every Cloud Run deployment of `om-core` **must** configure:

| Requirement | `gcloud run deploy` Flag (`deploy/cloud_run/cloudbuild.yaml` / `ci.yaml`) | Knative Service Annotation (`deploy/cloud_run/service.yaml`) |
| :--- | :--- | :--- |
| **Always-on CPU (CPU throttling disabled)** | `--no-cpu-throttling` | `run.googleapis.com/cpu-throttling: 'false'` |
| **Gen2 Execution Environment** | `--execution-environment` `gen2` (`--execution-environment=gen2`) | `run.googleapis.com/execution-environment: gen2` |
| **Warm Replicas (Minimum 2 instances)** | `--min-instances` `2` (`--min-instances=2`) | `autoscaling.knative.dev/minScale: '2'` |
| **Production Ingress Control** | `--ingress` `internal` (`--ingress=internal` in `deploy/cloud_run/cloudbuild.yaml`; `--ingress` `all` with `--no-allow-unauthenticated` in `ci.yaml` for default-pool smoke tests) | `run.googleapis.com/ingress: internal` and `run.googleapis.com/ingress-status: internal` |

### OpenTelemetry Collector Sidecar Configuration
The sidecar image in `deploy/cloud_run/otel-collector-sidecar/Dockerfile` is built on `us-docker.pkg.dev/cloud-ops-agents-artifacts/google-cloud-opentelemetry-collector/otelcol-google:0.121.0` and exports metrics via `googlemanagedprometheus`.
- `otelcol-google` reads `/etc/otelcol-google/config.yaml` by default, while `otel/opentelemetry-collector-contrib` (used in `deploy/gke/om.yaml`) reads `/etc/otelcol-contrib/config.yaml`.
- Therefore, `deploy/cloud_run/otel-collector-sidecar/Dockerfile` **must** copy `config.yaml` to **both** `/etc/otelcol-google/config.yaml` and `/etc/otelcol-contrib/config.yaml` and must **not** specify a bare `ENTRYPOINT` override that drops default `--config` arguments.

### Deploying and Validating Manifests
Before deploying with `gcloud run services replace deploy/cloud_run/service.yaml`, `gcloud builds submit --config deploy/cloud_run/cloudbuild.yaml`, or `kubectl apply -f deploy/gke/om.yaml`, run the deployment configuration validator:
```bash
# Audit default repository deployment templates (warns on unreplaced $PLACEHOLDERS, exits 0 if valid)
python3 .agents/skills/open_match2/scripts/validate_deploy_config.py

# Strict mode for rendered production manifests (fails with exit code 1 if any $PLACEHOLDER or sample ID remains)
python3 .agents/skills/open_match2/scripts/validate_deploy_config.py --strict-templates deploy/cloud_run/service.yaml deploy/gke/om.yaml
```

---

## 5. Reference Documents and Validation Scripts

- **[Architecture and API Reference](references/architecture_and_api.md)**: Dual gRPC / `grpc-gateway` architecture, RPC/REST endpoint table, protobuf schema and filter semantics, MMF URL parsing (`https://` requirement) and Cloud Run OIDC token caching (`internal/mmfauth`), replication/watermark/expiration internals, and OM1 vs. OM2 comparison table.
- **[Configuration and Tuning Reference](references/configuration_and_tuning.md)**: Exhaustive catalog of all 51 configuration variables and defaults across 4 type tables, symptom-based replication lag tuning matrix, and all 20 Core + 24 Cache OpenTelemetry metrics.
- **[Deployment Manifest Validator Script](scripts/validate_deploy_config.py)**: Executable Python 3 CLI auditor for Cloud Run Knative YAML, Cloud Build YAML, OTel Collector sidecar Dockerfiles, and GKE manifests.
