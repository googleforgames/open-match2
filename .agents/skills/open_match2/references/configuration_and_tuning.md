# Open Match 2 Configuration, Tuning, and Telemetry Reference

This reference catalogs all **51 configuration variables** registered in `internal/config/config.go` (`Read()`), provides a **symptom-based replication lag tuning matrix**, and lists all **20 Core** (`metrics.go`) and **24 Cache** (`internal/statestore/cache/metrics.go`) OpenTelemetry metrics.

---

## 1. Complete 51-Key Configuration Catalog (`internal/config/config.go`)

Viper's `AutomaticEnv()` only resolves environment variables that have a default value registered via `cfg.SetDefault(...)` in `config.Read()`. The 51 registered keys are organized below across four Go type tables (`15 string + 25 int + 6 bool + 5 time.Duration = 51 keys`).

### Table 1.A: String Configuration Variables (15 Keys)

| Configuration Key | Default Value | Go Type | Purpose |
| :--- | :--- | :--- | :--- |
| `OM_LOGGING_FORMAT` | `"json"` | `string` | Log output format (`"json"` for structured Cloud Logging with `timestamp`/`severity`/`message` keys, or `"text"` for local console output). |
| `OM_LOGGING_LEVEL` | `"info"` | `string` | Minimum Logrus severity level (`"trace"`, `"debug"`, `"info"`, `"warn"`, `"error"`, `"fatal"`, `"panic"`). Setting `"debug"` or `"trace"` dumps all active config keys on startup. |
| `OM_STATE_STORAGE_TYPE` | `"redis"` | `string` | State replication backend (`"redis"` for production Redis Streams or `"memory"` for single-instance local development/testing). Unknown values terminate startup via `logger.Fatalf`. |
| `REDISHOST` | `"127.0.0.1"` | `string` | Fallback Redis host populated automatically by Google Cloud Run's Memorystore integration when `OM_REDIS_WRITE_HOST` or `OM_REDIS_READ_HOST` equals `"REDISHOST"`. |
| `OM_REDIS_WRITE_HOST` | `"REDISHOST"` | `string` | Hostname or IP address of the primary Redis instance used for `XADD`/`XTRIM` write pipelines (resolves indirect `"REDISHOST"` env var when set to `"REDISHOST"`). |
| `OM_REDIS_WRITE_PORT` | `"REDISPORT"` | `string` | TCP port of the primary Redis write instance (resolves indirect `"REDISPORT"` env var when set to `"REDISPORT"`). |
| `OM_REDIS_WRITE_USER` | `"default"` | `string` | Redis ACL username for write connections (`redis.DialUsername`). |
| `OM_REDIS_WRITE_PASSWORD` | `"om-redis"` | `string` | Redis AUTH password for write connections (`redis.DialPassword`). |
| `OM_REDIS_READ_HOST` | `"REDISHOST"` | `string` | Hostname or IP address of the Redis read replica (or primary) used for blocking `XREAD` polling (resolves indirect `"REDISHOST"` env var when set to `"REDISHOST"`). |
| `OM_REDIS_READ_PORT` | `"REDISPORT"` | `string` | TCP port of the Redis read instance (resolves indirect `"REDISPORT"` env var when set to `"REDISPORT"`). |
| `OM_REDIS_READ_USER` | `"default"` | `string` | Redis ACL username for read connections (`redis.DialUsername`). |
| `OM_REDIS_READ_PASSWORD` | `"om-redis"` | `string` | Redis AUTH password for read connections (`redis.DialPassword`). |
| `K_SERVICE` | `"open_match_core"` | `string` | Knative / Cloud Run service name metadata environment variable. |
| `K_REVISION` | `"open_match_core_rev.1"` | `string` | Knative / Cloud Run revision name metadata environment variable. |
| `K_CONFIGURATION` | `"open_match_core_cfg"` | `string` | Knative / Cloud Run configuration name metadata environment variable. |

### Table 1.B: Integer Configuration Variables (25 Keys)

> **Positive-Bound Enforcement**: The 9 integer keys marked with **Yes (`> 0`)** in the **Guarded** column are validated immediately after `cfg.AutomaticEnv()` in `config.Read()`. If any of those 9 keys is configured as `<= 0`, `config.Read()` logs a warning (`"<KEY> must be > 0 (got <val>); resetting to default <default>"`) and restores the positive default value so `om-core` cannot panic on negative channel capacities or non-positive ticker durations.

| Configuration Key | Default Value | Go Type | Guarded (`> 0`) | Purpose |
| :--- | :--- | :--- | :--- | :--- |
| `PORT` | `8080` | `int` | No | External HTTP port on which the `grpc-gateway` REST reverse proxy listens. |
| `OM_GRPC_PORT` | `50581` | `int` | No | Local internal TCP port on which the unexposed gRPC server listens. |
| `REDISPORT` | `6379` | `int` | No | Fallback Redis TCP port used when `OM_REDIS_WRITE_PORT` or `OM_REDIS_READ_PORT` equals `"REDISPORT"`. |
| `OM_PROM_PORT` | `2223` | `int` | No | Local HTTP port serving `/metrics` via an isolated `http.ServeMux` when `OM_OTEL_SIDECAR=false`. |
| `OM_REDIS_POOL_MAX_IDLE` | `500` | `int` | No | Maximum number of idle connections retained in the Redigo read and write pools. |
| `OM_REDIS_POOL_MAX_ACTIVE` | `500` | `int` | No | Maximum number of active connections allocated by the Redigo read and write pools (`Wait: true`). |
| `OM_CACHE_IN_MAX_UPDATES_PER_POLL` | `10000` | `int` | **Yes (`> 0`)** | Maximum number of stream entries fetched per `XREAD COUNT` poll from state storage. |
| `OM_CACHE_IN_WAIT_TIMEOUT_MS` | `1500` | `int` | No | Maximum blocking wait time in milliseconds (`XREAD BLOCK`) when polling state storage for incoming replication updates. |
| `OM_CACHE_IN_POLL_WAIT_MS` | `250` | `int` | No | Post-poll pause in milliseconds after a non-empty partial poll (`< OM_CACHE_IN_MAX_UPDATES_PER_POLL`) so trickling updates coalesce into batches. |
| `OM_CACHE_IN_FULL_POLL_WAIT_MS` | `100` | `int` | No | Post-poll pause in milliseconds after a full poll (`>= OM_CACHE_IN_MAX_UPDATES_PER_POLL`) to yield CPU briefly while rapidly draining a backlog. |
| `OM_CACHE_IN_QUEUE_BUFFER_SIZE` | `20000` | `int` | **Yes (`> 0`)** | Buffer capacity of the local incoming replication update channel between the storage poller and cache applier. |
| `OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS` | `500` | `int` | No | Sleep duration in milliseconds between incoming cache apply cycles when the local incoming channel was fully drained. |
| `OM_CACHE_IN_FULL_APPLY_SLEEP_MS` | `100` | `int` | No | Guaranteed CPU-yield sleep in milliseconds after an apply cycle is interrupted by `OM_CACHE_IN_MAX_APPLY_DURATION_MS`. |
| `OM_CACHE_IN_MAX_APPLY_DURATION_MS` | `500` | `int` | **Yes (`> 0`)** | Maximum duration in milliseconds spent applying incoming updates to the local cache in a single cycle before yielding. |
| `OM_CACHE_OUT_MAX_QUEUE_THRESHOLD` | `50` | `int` | No | Maximum number of outgoing `UpdateRequest` items batched together before flushing to state storage. |
| `OM_CACHE_OUT_WAIT_TIMEOUT_MS` | `500` | `int` | No | Maximum time in milliseconds `OutgoingReplicationQueue` waits to fill a batch up to `OM_CACHE_OUT_MAX_QUEUE_THRESHOLD` before flushing. |
| `OM_CACHE_OUT_QUEUE_BUFFER_SIZE` | `500` | `int` | **Yes (`> 0`)** | Buffer capacity of the outgoing replication request channel (`ReplicatedTicketCache.UpRequests`). |
| `OM_CACHE_OUT_MAX_PACKED_KEYS` | `500` | `int` | **Yes (`> 0`)** | Maximum number of ticket IDs packed into a single multi-key `activate` or `deactivate` update when `OM_CACHE_PACK_TICKET_STATE_UPDATES=true`. |
| `OM_CACHE_TICKET_TTL_MS` | `600000` | `int` | No | Ticket time-to-live in milliseconds (default 10 minutes) added to `creation_time` for default `expiration_time`, local min-heap eviction, and Redis `XTRIM`. |
| `OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS` | `600000` | `int` | No | *(Deprecated)* Additional retention window in milliseconds after `OM_CACHE_TICKET_TTL_MS` before assignments and Redis stream entries are evicted. |
| `OM_CACHE_EXPIRATION_INTERVAL_MS` | `1000` | `int` | **Yes (`> 0`)** | Interval in milliseconds between asynchronous min-heap TTL expiration sweeps on the local cache. |
| `OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE` | `5000` | `int` | **Yes (`> 0`)** | Maximum number of expired tickets/assignments evicted from the local cache in a single expiration cycle (in 256-pop lock chunks). |
| `OM_MAX_STATE_UPDATES_PER_CALL` | `500` | `int` | **Yes (`> 0`)** | Maximum number of ticket IDs permitted in a single `ActivateTickets`, `DeactivateTickets`, or `CreateAssignments` RPC call. |
| `OM_MMF_TIMEOUT_SECS` | `600` | `int` | No | Maximum deadline in seconds for an `InvokeMatchmakingFunctions` invocation before `om-core` cancels MMF contexts with `MMFTimeoutError`. |
| `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` | `60000` | `int` | **Yes (`> 0`)** | Maximum time in milliseconds to wait for match ticket deactivations to replicate to the local cache before returning the match (with `deactivation_timeout=true` on timeout). |

### Table 1.C: Boolean Configuration Variables (6 Keys)

| Configuration Key | Default Value | Go Type | Purpose |
| :--- | :--- | :--- | :--- |
| `OM_OTEL_SIDECAR` | `true` | `bool` | When `true`, exports OpenTelemetry metrics over OTLP gRPC to a local Collector sidecar; when `false`, serves Prometheus metrics locally on `OM_PROM_PORT`. |
| `OM_MATCH_TICKET_DEACTIVATION_WAIT` | `true` | `bool` | When `true`, waits up to `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` for match ticket deactivations to replicate to the local cache before streaming the match to the caller (annotating `Match.Extensions["deactivation_timeout"] = true` on timeout or write failure); when `false`, streams the match immediately and deactivates tickets afterward. |
| `OM_CACHE_PACK_TICKET_STATE_UPDATES` | `false` | `bool` | When `true`, packs multi-ticket `ActivateTickets` / `DeactivateTickets` calls and coalesces contiguous cross-caller outgoing requests into multi-key stream updates (up to `OM_CACHE_OUT_MAX_PACKED_KEYS` keys). |
| `OM_REDIS_USE_TLS` | `false` | `bool` | Enables TLS (`redis.DialUseTLS(true)`) on both Redis read and write connections. |
| `OM_REDIS_TLS_SKIP_VERIFY` | `false` | `bool` | Skips certificate verification (`redis.DialTLSSkipVerify(true)`) when `OM_REDIS_USE_TLS=true` (for self-signed certificates in non-production environments). |
| `OM_VERBOSE` | `false` | `bool` | Enables verbose diagnostic logging (such as unassigned inactive ticket counts during `InvokeMatchmakingFunctions` and cache dumps on shutdown). |

### Table 1.D: Duration Configuration Variables (5 Keys)

Override strings in environment variables must follow Go's `time.ParseDuration` syntax (for example, `"2m"`, `"60s"`, `"5s"`, `"10s"`).

| Configuration Key | Default Value | Go Type | Purpose |
| :--- | :--- | :--- | :--- |
| `OM_REDIS_DIAL_MAX_BACKOFF_TIMEOUT` | `2m0s` (`2 * time.Minute`) | `time.Duration` | Maximum elapsed retry duration for exponential backoff when dialing Redis read or write instances. |
| `OM_REDIS_POOL_IDLE_TIMEOUT` | `1m0s` (`1 * time.Minute`) | `time.Duration` | Time after which idle connections in the Redigo read and write pools are closed (also used as the fallback when a dial timeout is configured as `<= 0`). |
| `OM_REDIS_DIAL_CONNECT_TIMEOUT` | `5s` (`5 * time.Second`) | `time.Duration` | TCP connection establishment timeout (`redis.DialConnectTimeout`) when dialing Redis read or write instances. |
| `OM_REDIS_DIAL_READ_TIMEOUT` | `10s` (`10 * time.Second`) | `time.Duration` | Socket read timeout (`redis.DialReadTimeout`) on Redis connections; automatically clamped to at least `OM_CACHE_IN_WAIT_TIMEOUT_MS + 1s` so blocking `XREAD` polls do not time out prematurely. |
| `OM_REDIS_DIAL_WRITE_TIMEOUT` | `5s` (`5 * time.Second`) | `time.Duration` | Socket write timeout (`redis.DialWriteTimeout`) on Redis connections. |

---

## 2. Symptom-Based Replication Lag Tuning Matrix

Always begin tuning by observing `om_core.cache.replication.lag` alongside `om_core.cache.incoming.backlog` and `om_core.cache.outgoing.backlog`, and enable `OM_CACHE_PACK_TICKET_STATE_UPDATES=true` for any high-throughput or bursty matchmaking workload.

| Bottleneck Stage | Primary Metric Symptoms | Root Cause | Recommended Configuration Adjustments |
| :--- | :--- | :--- | :--- |
| **1. Match Burst Write Amplification** | High `om_core.cache.replication.lag` during match waves; high `om_core.cache.outgoing.updates` and `om_core.cache.incoming.updates` counts per match. | Unpacked mode (`OM_CACHE_PACK_TICKET_STATE_UPDATES=false`) writes one Redis Stream entry per deactivated ticket ID (e.g., 40 `XADD` commands for a 40-player match). | Set `OM_CACHE_PACK_TICKET_STATE_UPDATES=true` and tune `OM_CACHE_OUT_MAX_PACKED_KEYS` (default `500`) to coalesce multi-ticket and cross-caller `Activate`/`Deactivate` updates into single stream entries. |
| **2. Slow Incoming Polling** | `om_core.cache.incoming.polls.full` increments frequently; `om_core.cache.incoming.updates` is pinned at `OM_CACHE_IN_MAX_UPDATES_PER_POLL`; or `om_core.cache.incoming.poll.duration` / `om_core.cache.incoming.poll.wait` are high while `om_core.cache.incoming.backlog` remains low. | The storage poller is either hitting the batch size cap on every `XREAD`, sleeping too long between polls, or saturating a single Redis instance on reads. | Increase `OM_CACHE_IN_MAX_UPDATES_PER_POLL` (default `10000`), decrease `OM_CACHE_IN_FULL_POLL_WAIT_MS` (default `100` ms) and `OM_CACHE_IN_POLL_WAIT_MS` (default `250` ms), and point `OM_REDIS_READ_HOST` at dedicated Redis read replicas. |
| **3. Slow Local Cache Apply Cycles** | `om_core.cache.incoming.backlog` and `om_core.cache.incoming.queue.wait` climb; `om_core.cache.incoming.timeouts.full` increments; or `om_core.cache.incoming.apply.sleep` dominates total cycle time. | The cache applier is hitting `OM_CACHE_IN_MAX_APPLY_DURATION_MS` before draining `incomingQueue`, sleeping too long between cycles, or suffering from Cloud Run CPU throttling. | Verify `--no-cpu-throttling` (`run.googleapis.com/cpu-throttling: 'false'`) and `gen2` are enabled; lower `OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS` (default `500` ms) and `OM_CACHE_IN_FULL_APPLY_SLEEP_MS` (default `100` ms); increase `OM_CACHE_IN_MAX_APPLY_DURATION_MS` (default `500` ms) and `OM_CACHE_IN_QUEUE_BUFFER_SIZE` (default `20000`). |
| **4. Outgoing Queue Contention** | `om_core.cache.outgoing.backlog` and `om_core.cache.outgoing.queue.wait` are elevated; `om_core.cache.outgoing.maxqueuethresholdreached` spikes; or `om_core.cache.outgoing.send.duration` is high. | Callers are queuing updates faster than `OutgoingReplicationQueue` flushes pipelines, or batches wait too long on `OM_CACHE_OUT_WAIT_TIMEOUT_MS` under moderate load. | Enable `OM_CACHE_PACK_TICKET_STATE_UPDATES=true`, increase `OM_CACHE_OUT_MAX_QUEUE_THRESHOLD` (default `50`) and `OM_CACHE_OUT_QUEUE_BUFFER_SIZE` (default `500`), or reduce `OM_CACHE_OUT_WAIT_TIMEOUT_MS` (default `500` ms) for lower tail latency. |
| **5. High MMF Preparation Latency** | `om_core.mmf.prep.duration` is elevated prior to MMF invocation while `om_core.cache.tickets.active` is large. | Broad profiles with many pools are evaluating complex filters across a large active ticket population and generating multi-chunk `ChunkedMmfRunRequest` payloads. | Partition monolithic profiles into smaller, concurrent regional/mode profiles; keep `OM_CACHE_TICKET_TTL_MS` tight (slightly above p95 time-to-match); and move non-filterable player metadata from `Ticket.attributes` to `Ticket.extensions`. |
| **6. Match Deactivation Wait Timeouts** | `om_core.mmf.deactivation.wait` is high; `om_core.mmf.deactivation.timeouts` increments; Directors observe `Match.Extensions["deactivation_timeout"] == true`. | End-to-end round-trip time (outgoing deactivation write + Redis stream read + local cache apply) exceeds `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`, or Cloud Run instances are CPU-throttled between requests. | Apply the outgoing and incoming tuning steps above, verify `--no-cpu-throttling` and `--min-instances=2`, and adjust `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` (default `60000` ms) to align with your Director's retry budget. |
| **7. High Inactive Ticket Memory / Expiration Backlog** | `om_core.cache.tickets.inactive` stays high relative to `om_core.cache.tickets.active`, or `om_core.cache.ticket.expirations` is pinned at `OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE`. | Ticket creation rate exceeds `OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE` per `OM_CACHE_EXPIRATION_INTERVAL_MS`, or `OM_CACHE_TICKET_TTL_MS` is configured much longer than actual player queue times. | Lower `OM_CACHE_TICKET_TTL_MS` (default `600000` ms), decrease `OM_CACHE_EXPIRATION_INTERVAL_MS` (default `1000` ms), or increase `OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE` (default `5000`). |

---

## 3. OpenTelemetry Metrics Reference (44 Total Instruments)

### Resource Attribute Precedence (`metrics.go`)
Both `initializeOtel()` (OTLP gRPC exporter for sidecar mode) and `initializeOtelWithLocalProm()` (local Prometheus endpoint on `OM_PROM_PORT`) construct the OpenTelemetry `resource.Resource` in ascending precedence order (later options override earlier options on colliding keys):
1. `resource.WithDetectors(gcp.NewDetector())` (lowest precedence: populates underlying GCP/GKE/Cloud Run infrastructure metadata)
2. `resource.WithTelemetrySDK()`
3. `resource.WithAttributes(semconv.ServiceNameKey.String("open_match"), semconv.ServiceVersionKey.String("2.1.0"))`
4. `resource.WithFromEnv()` (highest precedence: allows operator `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` environment variables to override compiled defaults)

### Sub-Millisecond Histogram Bucket Boundaries (`internal/statestore/cache/metrics.go`)
All 9 millisecond duration histograms in `internal/statestore/cache` use explicit sub-millisecond to multi-second bucket boundaries (17 boundaries in milliseconds) and microsecond-precision duration conversion (`float64(d.Microseconds()) / 1000.0`):
```text
[0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000]
```

### Table 3.A: Core Metrics — 20 Instruments (`metrics.go`)

| # | Metric Name | OTel Instrument Type | Unit | Bucket Boundaries | Exact Description String |
| :--- | :--- | :--- | :--- | :--- | :--- |
| 1 | `om_core.rpc.duration` | `Float64Histogram` | `ms` | SDK default | `RPC duration` |
| 2 | `om_core.rpc.errors` | `Int64Counter` | *(none)* | N/A | `Number of RPC errors` |
| 3 | `om_core.ticket.creations` | `Int64Counter` | *(none)* | N/A | `Number of tickets created` |
| 4 | `om_core.ticket.size` | `Float64Histogram` | `KB` | SDK default | `Size of tickets in kilobytes` |
| 5 | `om_core.ticket.deactivations.per_call` | `Int64Histogram` | *(none)* | SDK default | `Number of tickets deactivated per DeactivateTickets call` |
| 6 | `om_core.ticket.deactivation.failures.invalid_id` | `Int64Histogram` | *(none)* | SDK default | `Number of invalid ticket IDs per DeactivateTickets call` |
| 7 | `om_core.ticket.deactivation.failures.unspecified` | `Int64Histogram` | *(none)* | SDK default | `Number of tickets that failed to deactivate for unspecified reasons per DeactivateTickets call` |
| 8 | `om_core.ticket.activations.per_call` | `Int64Histogram` | *(none)* | SDK default | `Number of tickets activated per ActivateTickets call` |
| 9 | `om_core.ticket.activation.failures.invalid_id` | `Int64Histogram` | *(none)* | SDK default | `Number of invalid ticket IDs per ActivateTickets call` |
| 10 | `om_core.ticket.activation.failures.unspecified` | `Int64Histogram` | *(none)* | SDK default | `Number of tickets that failed to activate for unspecified reasons per ActivateTickets call` |
| 11 | `om_core.mmf.calls` | `Int64Counter` | *(none)* | N/A | `Number of Matchmaking Function (MMF) calls made` |
| 12 | `om_core.mmf.failures` | `Int64Counter` | *(none)* | N/A | `Number of Matchmaking Function (MMF) calls that returned an error` |
| 13 | `om_core.mmf.ticket.deactivations` | `Int64Counter` | *(none)* | N/A | `Number of ticket deactivations initiated on behalf of Matchmaking Functions (MMFs)` |
| 14 | `om_core.match.received` | `Int64Counter` | *(none)* | N/A | `Number of matches received from Matchmaking Functions (MMFs) and returned to the caller; attribute match.has_tickets indicates whether the match had any valid ticket IDs` |
| 15 | `om_core.mmf.prep.duration` | `Float64Histogram` | `ms` | SDK default | `Time spent snapshotting active tickets, filtering pools, and chunking requests in InvokeMatchmakingFunctions before invoking MMFs` |
| 16 | `om_core.mmf.deactivation.wait` | `Float64Histogram` | `ms` | SDK default | `Time spent waiting for match ticket deactivations to replicate to the local cache` |
| 17 | `om_core.mmf.deactivation.timeouts` | `Int64Counter` | *(none)* | N/A | `Number of times waiting for match ticket deactivations to replicate to the local cache timed out` |
| 18 | `om_core.ticket.assignment` | `Int64Counter` | *(none)* | N/A | `Number of tickets assigned (DEPRECATED)` |
| 19 | `om_core.ticket.assignment.failures` | `Int64Counter` | *(none)* | N/A | `Number of ticket assignment failures (DEPRECATED)` |
| 20 | `om_core.ticket.assignment.watch` | `Int64UpDownCounter` | *(none)* | N/A | `Number of tickets currently being watched for assignment` |

### Table 3.B: Cache Metrics — 24 Instruments (`internal/statestore/cache/metrics.go`)

| # | Metric Name | OTel Instrument Type | Unit | Bucket Boundaries | Exact Description String |
| :--- | :--- | :--- | :--- | :--- | :--- |
| 1 | `om_core.cache.expiration.duration` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Duration of cache expiration logic` |
| 2 | `om_core.cache.assignment.expirations` | `Int64Histogram` | *(none)* | SDK default | `Number of assignments expired per cache expiration cycle` |
| 3 | `om_core.cache.ticket.inactive.expirations` | `Int64Histogram` | *(none)* | SDK default | `Number of inactive tickets expired per cache expiration cycle` |
| 4 | `om_core.cache.ticket.expirations` | `Int64Histogram` | *(none)* | SDK default | `Number of tickets expired per cache expiration cycle` |
| 5 | `om_core.cache.outgoing.updates` | `Int64Histogram` | *(none)* | SDK default | `Number of outgoing replication updates per cycle` |
| 6 | `om_core.cache.outgoing.timeouts` | `Int64Counter` | *(none)* | N/A | `Number of times the outgoing replication queue waited OM_CACHE_OUT_WAIT_TIMEOUT_MS and did not reach OM_CACHE_OUT_MAX_QUEUE_THRESHOLD updates to send as a batch to the replicator` |
| 7 | `om_core.cache.outgoing.maxqueuethresholdreached` | `Int64Counter` | *(none)* | N/A | `Number of times the outgoing replication queue saw OM_CACHE_OUT_MAX_QUEUE_THRESHOLD updates in less than OM_CACHE_OUT_WAIT_TIMEOUT_MS milliseconds` |
| 8 | `om_core.cache.outgoing.queue.wait` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Time an outgoing replication update spends queued and being sent before returning a response` |
| 9 | `om_core.cache.outgoing.send.duration` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Duration of sending a batch of outgoing replication updates to state storage` |
| 10 | `om_core.cache.incoming.updates` | `Int64Histogram` | *(none)* | SDK default | `Number of incoming replication updates per poll` |
| 11 | `om_core.cache.incoming.timeouts.empty` | `Int64Counter` | *(none)* | N/A | `Number of times the incoming replication queue saw no updates after waiting for OM_CACHE_IN_WAIT_TIMEOUT_MS` |
| 12 | `om_core.cache.incoming.timeouts.full` | `Int64Counter` | *(none)* | N/A | `Number of times the incoming replication queue couldn't process all pending updates in OM_CACHE_IN_MAX_APPLY_DURATION_MS` |
| 13 | `om_core.cache.incoming.polls.full` | `Int64Counter` | *(none)* | N/A | `Number of times an incoming replication poll returned the maximum OM_CACHE_IN_MAX_UPDATES_PER_POLL updates` |
| 14 | `om_core.cache.incoming.poll.duration` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Duration of polling state storage for incoming replication updates` |
| 15 | `om_core.cache.incoming.poll.wait` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Post-poll pause before the next incoming replication poll (OM_CACHE_IN_POLL_WAIT_MS after a partial poll, OM_CACHE_IN_FULL_POLL_WAIT_MS after a full poll, or any remaining OM_CACHE_IN_WAIT_TIMEOUT_MS if an empty poll returned early on error)` |
| 16 | `om_core.cache.incoming.queue.wait` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Time the incoming poller spent pushing polled updates into the local replication channel` |
| 17 | `om_core.cache.incoming.apply.duration` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Time spent applying incoming replication updates to the local cache per cycle` |
| 18 | `om_core.cache.incoming.apply.sleep` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Time spent sleeping between incoming cache apply cycles (OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS after draining the queue, or OM_CACHE_IN_FULL_APPLY_SLEEP_MS after hitting OM_CACHE_IN_MAX_APPLY_DURATION_MS)` |
| 19 | `om_core.cache.replication.lag` | `Float64Histogram` | `ms` | 17-bucket sub-ms (`0.05..10000`) | `Time elapsed between when a state update is written to state storage and when it is applied to the local cache` |
| 20 | `om_core.cache.tickets.active` | `Int64ObservableUpDownCounter` | *(none)* | N/A | `Active tickets that can appear in pools` |
| 21 | `om_core.cache.tickets.inactive` | `Int64ObservableUpDownCounter` | *(none)* | N/A | `Inactive tickets that won't appear in pools` |
| 22 | `om_core.cache.assignments` | `Int64ObservableUpDownCounter` | *(none)* | N/A | `Assignments in Open Match cache` |
| 23 | `om_core.cache.incoming.backlog` | `Int64ObservableUpDownCounter` | *(none)* | N/A | `Number of incoming replication updates buffered and waiting to be applied to the local cache` |
| 24 | `om_core.cache.outgoing.backlog` | `Int64ObservableUpDownCounter` | *(none)* | N/A | `Number of outgoing replication updates queued and waiting to be sent to state storage` |
