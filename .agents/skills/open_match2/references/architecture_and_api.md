# Open Match 2 Architecture and API Reference

This document details the dual gRPC / HTTP `grpc-gateway` server architecture, RPC and REST endpoints, protobuf schema and pool filter evaluation semantics, MMF URL parsing and Cloud Run OIDC authentication (`internal/mmfauth`), state replication and cache expiration internals (`internal/statestore`), and the architectural differences between Open Match 1 (OM1) and Open Match 2 (OM2).

---

## 1. Dual gRPC and HTTP `grpc-gateway` Architecture

Each `om-core` process runs two tightly coupled network listeners in `server.go` (`start(parentCtx, cfg)`):

1. **Internal gRPC Server (`OM_GRPC_PORT`, default `50581`)**:
   - Registers `pb.OpenMatchServiceServer` (`&grpcServer{}`).
   - Configured with `grpc.StatsHandler(&Handler{})` to record per-RPC latency (`om_core.rpc.duration` in milliseconds with attribute `rpc`) and bounded RPC error counts (`om_core.rpc.errors` with attributes `rpc` and `grpc.status` only, omitting raw error message strings to prevent cardinality explosion).
   - Configured with `unaryPanicRecoveryInterceptor` and `streamPanicRecoveryInterceptor` to catch unexpected panics inside any RPC handler, log the `FullMethod`, recovered value, and `debug.Stack()` trace, and return a clean gRPC `codes.Internal` error (`"internal server error"`) without crashing the stateful `om-core` instance.
2. **External HTTP/JSON Reverse Proxy (`PORT`, default `8080`)**:
   - Uses `grpc-ecosystem/grpc-gateway/v2` (`gw.RegisterOpenMatchServiceHandlerFromEndpoint`) to transcode incoming HTTP/JSON REST requests to the local gRPC server at `localhost:<OM_GRPC_PORT>`.
   - Enforces `ReadHeaderTimeout: 10 * time.Second` on the `http.Server` to mitigate Slowloris-style header stalls.
   - Fails fast via `logger.Fatalf` if binding either `OM_GRPC_PORT` or `PORT` fails on startup.
3. **Signal-Aware Lifecycle and Bounded Graceful Shutdown**:
   - Listens for `syscall.SIGTERM` and `syscall.SIGINT` (or parent context cancellation) via `signal.NotifyContext`.
   - On termination, derives a 5-second shutdown context, calls `httpServer.Shutdown(shutdownCtx)`, and runs `omServer.GracefulStop()` in a helper goroutine—falling back to `omServer.Stop()` if the 5-second deadline expires before all active streams drain.
   - `main()` also defers a 5-second timeout context for `otelShutdownFunc` and calls `Close()` on the Redis replicator (`rConnPool` and `wConnPool`).

---

## 2. RPC and HTTP REST Endpoint Reference

### `open_match2.OpenMatchService` (Hosted by `om-core`)

| gRPC Method | HTTP Method & Path (`grpc-gateway`) | Request Message | Response Message | Streaming Mode | Notes & Error Contracts |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `CreateTicket` | `POST /tickets` | `CreateTicketRequest` | `CreateTicketResponse` | Unary | Creates an immutable ticket in the **`inactive`** state and returns the server-assigned `ticket_id` (`<13-digit-ms>-<seq>`). Preserves `codes.InvalidArgument` on invalid `expiration_time`. Records `om_core.ticket.creations` and `om_core.ticket.size` only after state storage replication succeeds. |
| `ActivateTickets` | `POST /tickets:activate` | `ActivateTicketsRequest` | `ActivateTicketsResponse` | Unary | Moves up to `OM_MAX_STATE_UPDATES_PER_CALL` (default `500`) ticket IDs from `inactive` to `active` (`ActiveTickets`). Returns `codes.InvalidArgument` if any ID fails regex validation, or `codes.Internal` (`"unable to update ticket state"`) if all IDs are valid but replication fails, attaching per-ticket `errdetails.BadRequest_FieldViolation` (`Field: "ticket_id/<id>"`, `Description: "Unable to update ticket <id> state to active : <err>"`). |
| `DeactivateTickets` | `POST /tickets:deactivate` | `DeactivateTicketsRequest` | `DeactivateTicketsResponse` | Unary | Moves up to `OM_MAX_STATE_UPDATES_PER_CALL` ticket IDs from `active` to `inactive` (`InactiveSet`). Same partial-batch processing and `errdetails.BadRequest_FieldViolation` contract as `ActivateTickets` (`"Unable to update ticket <id> state to inactive : <err>"`). |
| `InvokeMatchmakingFunctions` | `POST /matches:fetch` | `MmfRequest` | `stream StreamedMmfResponse` | Server-streaming | Waits on `tc.WaitForCatchUp` during cold start (returning `codes.Unavailable` on timeout), snapshots `ActiveTickets`, filters pools, chunks populated profiles into `<= 4 MB` `ChunkedMmfRunRequest` messages, fans out concurrently to all requested MMFs, automatically deactivates valid matched tickets (listing invalid roster ticket IDs in `invalid_ticket_ids`), and streams every MMF response back to the caller as a `StreamedMmfResponse` (including ticketless matches and responses with no match). If any MMF fails, ends the stream with `codes.Internal` (one `errdetails.ErrorInfo` per failed MMF) after all matches are sent. Over HTTP `grpc-gateway`, each newline-delimited JSON match chunk is wrapped in `{"result": {"match": ...}}`, and an MMF failure arrives as a final chunk containing an `"error"` object. |
| `CreateAssignments` *(Deprecated)* | `POST /assignments:create` | `CreateAssignmentsRequest` | `CreateAssignmentsResponse` | Unary | **Deprecated (dev/test only)**. Associates a non-empty `Assignment` with up to `OM_MAX_STATE_UPDATES_PER_CALL` tickets in `assignment_roster`. Returns `codes.InvalidArgument` if roster/assignment is nil, marshaled assignment is 0 bytes (`"roster with non-empty assignment is required"`), or ticket count exceeds `OM_MAX_STATE_UPDATES_PER_CALL`. |
| `WatchAssignments` *(Deprecated)* | `POST /assignments:watch` | `WatchAssignmentsRequest` | `stream StreamedWatchAssignmentsResponse` | Server-streaming | **Deprecated (dev/test only)**. Polls `tc.Assignments` every 1s per requested ticket ID until an assignment arrives, the newest ticket's TTL expires, or the caller cancels the stream. Balances the `om_core.ticket.assignment.watch` up-down counter back to 0 on completion, timeout, or cancellation. |

### `open_match2.MatchMakingFunctionService` (Hosted by User MMFs, Invoked by `om-core`)

| gRPC Method | Request Stream | Response Stream | Streaming Mode | Behavioral Contract |
| :--- | :--- | :--- | :--- | :--- |
| `Run` | `stream ChunkedMmfRunRequest` | `stream StreamedMmfResponse` | Bi-directional streaming (client-chunked request stream, server-streamed match responses) | `om-core` streams one or more `ChunkedMmfRunRequest` chunks (each `<= 4 MB`) containing the `Profile` and a slice of matching `Participants` per pool, and then calls `CloseSend()`. The MMF reads chunks until `io.EOF`, concatenates `Pool.Participants.Tickets` by pool name, runs matchmaking logic, and streams back `StreamedMmfResponse` messages (each `< 4 MB`). |

---

## 3. Protobuf Schema and Pool Filter Semantics

### Core Protobuf Messages (`proto/messages.proto`)
- **`Ticket`**:
  - `id` (`string`): Populated by state storage on `CreateTicket` (`^\d{13}-\d+$`). Any user-supplied `id` in `CreateTicketRequest` is logged as a warning and overwritten.
  - `expiration_time` (`google.protobuf.Timestamp`): Optional custom expiration timestamp. Must fall strictly within `(now, now + OM_CACHE_TICKET_TTL_MS]`; otherwise `CreateTicket` rejects the ticket with `codes.InvalidArgument` (`"specified expiration time not between (now, now + <ttl>)"`). When omitted (`nil`), `om-core` sets `expiration_time = attributes.creation_time + OM_CACHE_TICKET_TTL_MS`.
  - `attributes` (`Ticket.FilterableData`): Indexed fields evaluated by pool filters:
    - `tags` (`repeated string`): Matched by `Pool.TagPresentFilter`.
    - `string_args` (`map<string, string>`): Matched by `Pool.StringEqualsFilter`.
    - `double_args` (`map<string, double>`): Matched by `Pool.DoubleRangeFilter`.
    - `creation_time` (`google.protobuf.Timestamp`): Defaults to `timestamppb.Now()` if omitted, invalid, or in the future. Matched by `Pool.CreationTimeRangeFilter`.
  - `extensions` (`map<string, google.protobuf.Any>`): Opaque pass-through metadata ignored by `om-core` filters (recommended for non-filtered player data to minimize CPU/memory overhead).
- **`Profile` & `Pool`**:
  - A `Profile` contains a `name` (use reverse-DNS notation such as `game.mode.region.timestamp`; `om-core` strips the segment after the final `.` for the `profile.name` metric attribute), a map of `pools` (`map<string, Pool>`), and `extensions`.
  - A `Pool` is a logical conjunction (set intersection) of `tag_present_filters`, `string_equals_filters`, `double_range_filters`, and `creation_time_range_filter`.
- **`Match` & `Roster`**:
  - Returned by MMFs inside `StreamedMmfResponse`. `om-core` extracts all tickets from `Match.rosters[*].tickets`, validates each `ticket.id` against `tc.Replicator.GetReplIdValidator()`, and deactivates all valid ticket IDs.
  - Reserved extension key: `MatchDeactivationTimeoutExtensionKey = "deactivation_timeout"`. Set to `anypb.New(wrapperspb.Bool(true))` when `OM_MATCH_TICKET_DEACTIVATION_WAIT=true` and local replication of match ticket deactivations times out or all deactivation writes fail.

### Pool Filter Evaluation Rules (`internal/filter/filter.go`)
1. **`ValidatePoolFilters(pool *pb.Pool)`**:
   - Returns `(true, nil)` if `pool.GetCreationTimeRangeFilter()` is `nil`, or if any non-nil `Start` and `End` timestamps satisfy `IsValid()`.
   - Returns `(false, InvalidCreationTimeFilterStartErrorStatus)` (`codes.InvalidArgument`, `".invalid creation time filter start value"`) if `Start` is non-nil and invalid, or `(false, InvalidCreationTimeFilterEndErrorStatus)` (`".invalid creation time filter end value"`) if `End` is non-nil and invalid.
2. **`In(pool *pb.Pool, ticket *pb.Ticket) bool`**:
   - **Creation-Time Range Filter (`CreationTimeRangeFilter`)**:
     - Evaluated **only** when `pool.GetCreationTimeRangeFilter() != nil`.
     - If the filter is non-nil and `ticket.GetAttributes().GetCreationTime() == nil`, `In` immediately returns `false`.
     - **Half-Open and Open-Ended Bounds**: `Start` and `End` are checked independently for `!= nil` before calling `AsTime()` (avoiding the zero-value Unix epoch `1970-01-01T00:00:00Z` pitfall on `nil` protobuf timestamps):
       - When `Start != nil`: rejects if `crTime < Start`, or if `crTime == Start` and `Bounds` is `EXCLUDE_START` (`EXCLUDE_MIN` / `INCLUDE_MAX_ONLY`) or `EXCLUDE_BOTH` (`INCLUDE_NONE`).
       - When `End != nil`: rejects if `crTime > End`, or if `crTime == End` and `Bounds` is `EXCLUDE_END` (`EXCLUDE_MAX` / `INCLUDE_MIN_ONLY`) or `EXCLUDE_BOTH` (`INCLUDE_NONE`).
       - When both `Start == nil` and `End == nil`: any ticket with a non-nil `creation_time` (including pre-1970 and Unix-epoch timestamps) passes.
   - **Double Range Filters (`DoubleRangeFilters`)**: Every filter requires its `double_arg` key to exist in `ticket.attributes.double_args` and fall within `[minimum, maximum]` according to `Bounds` (`EXCLUDE_NONE`, `EXCLUDE_MIN`, `EXCLUDE_MAX`, `EXCLUDE_BOTH`).
   - **String Equals Filters (`StringEqualsFilters`)**: Every filter requires `ticket.attributes.string_args[string_arg] == value`.
   - **Tag Present Filters (`TagPresentFilters`)**: Every filter requires `tag` to appear in `ticket.attributes.tags`.

### Request Chunking Semantics (`getChunkedRequests` in `main.go`)
- `packPoolTicketChunks` packs populated pools across multiple `ChunkedMmfRunRequest` messages so each chunk stays under the 4 MiB gRPC default soft limit (`mmfChunkSoftLimitBytes = 4,194,304` bytes; some middleware silently drops larger messages, so limits are never raised). Sizes are measured directly from actual protobuf messages: the empty baseline chunk (including every valid pool's empty participant roster built via `getChunkedRequests` with `NumChunks = math.MaxInt32`) is measured via `proto.Size`, and each ticket's per-pool roster contribution (including its repeated-field tag and length prefix) is measured via `encodedRosterTicketSize`. Chunks are filled up to `mmfChunkFillTargetBytes = 4,000,000` bytes, leaving `194,304` bytes of headroom for enclosing varint length-prefix growth, and every built chunk is checked against `mmfChunkSoftLimitBytes` with `proto.Size`.
- **Pool Non-Aliasing**: Each chunk clones its `pb.Pool` descriptors via `proto.Clone` before attaching `Participants` (`<poolName>_roster`), ensuring earlier chunks' rosters are never overwritten by later chunks and the caller's request profile is never mutated.
- **Empty-Pool Preservation**: Valid pools (passing `filter.ValidatePoolFilters`) that match zero active tickets (or have zero tickets in a subsequent chunk) are cloned with an empty non-nil `Participants` roster into every chunk so MMFs always see all valid pool definitions and their `Extensions`.

---

## 4. MMF URL Parsing and Cloud Run OIDC Token Caching (`internal/mmfauth`)

### Important Accuracy Rule: `https://` Scheme Requirement for TLS and OIDC Auth
> **CRITICAL**: In `parseMmfUrl` and `InvokeMatchmakingFunctions` (`main.go`), TLS transport credentials (`credentials.NewTLS`) and Google Cloud Run OIDC ID token injection (`idTokenCache.Token(audience)`) are triggered **exclusively** when the parsed MMF URL scheme is `https://`.
>
> A scheme-less host—even when configured with port `443` (for example, `host: "mmf-service.a.run.app", port: 443` or `host: "mmf-service.a.run.app:443", port: 0`)—automatically receives the default `http://` scheme prefix (`http://mmf-service.a.run.app:443`) and will dial using **unauthenticated plaintext gRPC (`insecure.NewCredentials()`) without an OIDC `Authorization: Bearer` header**. To enable TLS and Cloud Run OIDC authentication, `MatchmakingFunctionSpec.host` **must** explicitly start with `https://` (e.g., `https://mmf-service.a.run.app`).

### Complete `parseMmfUrl` Resolution Rules
Given `mmf *pb.MatchmakingFunctionSpec`:
1. If `mmf == nil`, returns error `"mmf spec is nil"`.
2. Trims whitespace from `mmf.GetHost()`; if empty, returns error `"mmf host is empty"`.
3. Validates `mmf.GetPort()` is in `[0, 65535]`; otherwise returns `"mmf port <port> out of range [0, 65535]"`.
4. If the trimmed host does not contain `"://"`:
   - If `net.ParseIP(target)` parses as an IPv6 address (contains `":"`), wraps it in square brackets (`"[" + target + "]"`).
   - Prepends `"http://"`.
5. Parses via `url.Parse`. Lowercases `Scheme` and validates it is `"http"` or `"https"` (otherwise returns `"unsupported mmf scheme <scheme>"`).
6. Validates `parsedUrl.Hostname()` is non-empty (otherwise returns `"mmf host is missing hostname"`).
7. If `parsedUrl.Port()` is non-empty, validates it parses as an integer in `[1, 65535]` (otherwise returns `"invalid port <port> in mmf host"`).
8. Resolves the target port in precedence order:
   - `mmf.GetPort() > 0` (overrides any port embedded in `host`)
   - `parsedUrl.Port() != ""`
   - `"443"` when `parsedUrl.Scheme == "https"`
   - `"80"` when `parsedUrl.Scheme == "http"`
9. Sets `parsedUrl.Host = net.JoinHostPort(hostname, resolvedPort)` and returns `parsedUrl`.

### Per-Audience OIDC Token Caching and Single-Flight Refresh (`internal/mmfauth`)
- `IDTokenCache` caches both the `oauth2.TokenSource` (created via `idtoken.NewTokenSource(ctx, audience)`) and the most recent `*oauth2.Token` keyed by `audience` (`mmf.GetHost()`).
- **`Token(audience)`**: Returns the cached `*oauth2.Token` if non-nil and `Valid()`; otherwise calls `entry.source.Token()` if the cached source exists; otherwise creates a new token source and token via the factory.
- **`Refresh(audience, rejectedAccessToken)`**: Invoked when an `https://` MMF returns `codes.Unauthenticated` or `codes.PermissionDenied` (`mmfauth.IsAuthRejection(err) == true`) either during `openAndSendChunks` or on the first `mmfStream.Recv()` before any response message has been received:
  - Under `c.mu.Lock()`, if the cache already holds a valid token for `audience` whose `AccessToken != rejectedAccessToken` (meaning another concurrent goroutine already refreshed the rejected token), `Refresh` immediately returns that replacement token without minting a duplicate token.
  - Otherwise, it evicts the stale cache entry, mints a fresh token source and token, and retries `openAndSendChunks` once (`authRetried = true`).

---

## 5. Replication, Watermarks, Catch-Up Barrier, and Expiration Internals

### Replication ID Format (`internal/statestore/datatypes`)
- Every replication event is assigned a Redis Stream entry ID formatted as `<13-digit-unix-ms>-<seq>` (validated by `^\d{13}-\d+$`).
- `store.ParseReplIdTimestampMs(replId)` extracts the Unix millisecond timestamp prefix (rejecting signed `+` prefixes, negative numbers, or malformed strings).
- `store.CompareReplIds(a, b)` compares two replication IDs numerically by `(timestampMs, seq)` using `uint64` parsing so sequence numbers of different digit lengths compare in true causal order.

### Redis Replicator (`internal/statestore/redis`)
- **Write Pipeline (`SendUpdates`)**:
  - Validates all updates in the batch before sending. Invalid updates (`nil`, empty ticket data, empty ticket key, empty assignment) are recorded in `out[i].Err` (`out[i].Result = update.PrimaryKey()`) and skipped without aborting valid updates in the same batch.
  - Packed `Activate` and `Deactivate` updates (`len(update.Keys) > 0`) filter out empty key strings, emit `XADD om-replication * <activate|deactivate> <key1> <key2> ...`, and append a trailing `""` padding token when the total argument count is odd (even number of valid keys) because Redis `XADD` requires field-value pairs.
  - Appends `XTRIM om-replication MINID ~ <nowMs - OM_CACHE_TICKET_TTL_MS - OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS>` to every non-empty pipeline batch.
- **Blocking Read (`GetUpdates`)**:
  - Executes `XREAD COUNT <OM_CACHE_IN_MAX_UPDATES_PER_POLL> BLOCK <OM_CACHE_IN_WAIT_TIMEOUT_MS> STREAMS om-replication <replId>`.
  - Unpacks multi-key `activate` / `deactivate` stream entries (stripping any trailing empty padding string) and advances `rr.replId` past malformed field arrays or unknown commands so a corrupt stream entry cannot wedge replication.
- **Dial Timeouts & Cleanup**:
  - `resolveDialTimeouts` defaults to 5s connect, 10s read, and 5s write timeouts (falling back to `OM_REDIS_POOL_IDLE_TIMEOUT` if non-positive) and clamps `readTimeout` to at least `OM_CACHE_IN_WAIT_TIMEOUT_MS + 1s` so blocking `XREAD` calls never time out prematurely at the socket layer.
  - If `New(cfg)` fails during initial read/write connection testing, `rr.Close()` cancels the signal context and closes both pools to prevent goroutine leaks.

### ReplicatedTicketCache (`internal/statestore/cache`)
1. **Materialized `ActiveTickets` Index**:
   - `ActiveTickets` (`sync.Map`) stores only currently active `*pb.Ticket` pointers:
     - `store.Ticket`: Inserts into `Tickets` and (unless pre-deactivated in `InactiveSet`) into `ActiveTickets`.
     - `store.Activate`: Deletes from `InactiveSet` and, if present in `Tickets`, inserts into `ActiveTickets`.
     - `store.Deactivate`: Deletes from `ActiveTickets` and inserts into `InactiveSet`.
   - `SnapshotActiveTickets()` ranges over `ActiveTickets` lock-free in $O(\text{active tickets})$ time without scanning `InactiveSet`.
2. **Outgoing Batching and Cross-Caller Request Coalescing (`OutgoingReplicationQueue`)**:
   - Collects up to `OM_CACHE_OUT_MAX_QUEUE_THRESHOLD` requests within `OM_CACHE_OUT_WAIT_TIMEOUT_MS`.
   - When `OM_CACHE_PACK_TICKET_STATE_UPDATES=true`, contiguous `Activate` or `Deactivate` requests across concurrent callers are coalesced into packed updates (up to `OM_CACHE_OUT_MAX_PACKED_KEYS` unique non-empty keys in first-seen order). Interleaved `Ticket`/`Assign` commands or switching between `Activate` and `Deactivate` flushes the run to preserve strict causal ordering, and empty-key requests are isolated so they cannot fail adjacent valid callers.
   - Drains all remaining buffered `UpRequests` before exiting when `ctx.Done()` closes.
3. **Cold-Start Catch-Up Barrier (`IsCaughtUp` / `WaitForCatchUp`)**:
   - During startup, the incoming poller tracks `totalPolled` updates. On the first non-full poll (`len(updates) < OM_CACHE_IN_MAX_UPDATES_PER_POLL`), `catchUpTarget` transitions from `-1` to `totalPolled` and wakes the applier immediately via a 1-buffered wake channel. Once `totalApplied >= catchUpTarget`, the readiness channel closes and `IsCaughtUp()` returns `true`.
4. **Event-Driven Deactivation Watermark (`WaitForDeactivation`)**:
   - After each incoming apply cycle, the cache updates `appliedReplId` under `watermarkMu` and closes/replaces `watermarkNotify` (`chan struct{}`) to wake all waiting goroutines without 100ms polling sleeps.
5. **Asynchronous Chunked Min-Heap TTL Expiration (`expireCacheEntries`)**:
   - Runs on an asynchronous ticker every `OM_CACHE_EXPIRATION_INTERVAL_MS` (default `1000` ms) across three min-heaps ordered by millisecond deadline (`inactiveHeap`, `ticketHeap` for custom earlier `ExpirationTime`, and `assignmentHeap`).
   - Acquires `cacheMu` in bounded chunks of at most **256 heap pop operations** up to `OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE` (default `5000`) deletions per cycle.
   - **Re-activation & Resurrection Safety**: When popping from `inactiveHeap`, verifies the ticket ID is still in `InactiveSet` (skipping re-activated tickets) and deletes from `ActiveTickets` and `Tickets` **before** deleting from `InactiveSet`.

---

## 6. Open Match 1 vs. Open Match 2 Comparison

| Dimension | Open Match 1 (OM1) | Open Match 2 (OM2 `v2.1.0`) |
| :--- | :--- | :--- |
| **Service Topology** | Multi-service microservices (`om-frontend`, `om-backend`, `om-query`, `om-synchronizer`, `om-evaluator`). | Single unified binary (`om-core`) handling all ticket and matchmaking RPCs. |
| **Client Protocol & Load Balancing** | Direct gRPC connections requiring client-side gRPC load balancing. | Dual gRPC (`OM_GRPC_PORT`) and HTTP/JSON `grpc-gateway` (`PORT`), enabling standard cloud L7 HTTP load balancers and Cloud Run autoscaling. |
| **Ticket Querying** | `om-query` queried Redis directly on every MMF pool query (`QueryPools`). | Local in-memory `ReplicatedTicketCache` with lock-free `ActiveTickets` index synchronized via Redis Streams (`om-replication`) and Redis read replicas. |
| **Ticket ID Generation** | Optional client-provided or UUID ticket IDs. | Server-assigned Redis Stream entry IDs (`<13-digit-ms>-<seq>`). Custom IDs are stored in `Ticket.attributes.string_args` or `Ticket.extensions`. |
| **Ticket Lifecycle & Deletion** | Tickets started active on creation; required explicit `DeleteTicket` calls to avoid leaks. | Tickets start **`inactive`** on `CreateTicket`, enter pools via `ActivateTickets`, are automatically deactivated on match return, and auto-expire via min-heap TTL and Redis `XTRIM`. |
| **Match Collision Resolution** | Framework-managed synchronization window (`om-synchronizer`) and mandatory `om-evaluator` service. | No synchronization window or evaluator inside `om-core`; MMFs return final matches directly, and the Director de-collides overlapping tickets and re-activates discarded match tickets. |
| **Matchmaking RPC Contract** | Unary/server-streaming `FetchMatches` -> MMF queried `om-query` via `QueryPools`. | Server-streaming `InvokeMatchmakingFunctions(MmfRequest)` -> `om-core` pushes 4 MB `ChunkedMmfRunRequest` stream to `mmf.Run` and fans matches back in real time. |
| **Assignment Delivery** | Client long-polling via `GetAssignments` on `om-frontend` after Director called `AssignTickets`. | Director pushes assignments directly to the game's client notification service (`CreateAssignments` / `WatchAssignments` are deprecated compatibility stubs). |
| **Build & Configuration** | 1000+ line `Makefile` and layered Helm YAML charts. | CNCF / Google Cloud Buildpacks and 12-factor environment variables centralized in `internal/config/config.go` (51 keys). |
