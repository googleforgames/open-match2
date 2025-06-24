この文書を日本語で[ Open Match 2 テレメトリー](jp/METRICS.md)

# Open Match 2 Telemetry

Open Match 2 uses OpenTelemetry to emit detailed metrics on core operations, RPC calls, and cache performance, providing deep visibility into the matchmaking process.  Some of the provided metrics are provided out-of-the-box by golang modules om-core imports, and aren't covered in this document (i.e. rpc, etc).  We recommend you use OpenTelemetry to emit telemetry from the matchmaker you write against Open Match 2 as well \- for some examples of matchmaker telemetry, have a look at the [example in the open-match-ecosystem repository](https://github.com/googleforgames/open-match-ecosystem/tree/main/v2/METRICS.md).

### **Enabling and Configuring OpenTelemetry for Open Match 2**

To enable OpenTelemetry in Open Match 2, you need to set the `OM_OTEL_SIDECAR` environment variable to `true`. By default, Open Match will try to export metrics to a local OpenTelemetry sidecar. If you are running without a sidecar, you can set `OM_OTEL_SIDECAR` to `false` (for example, in local development).

You can configure the OpenTelemetry exporter by setting the appropriate environment variables. For more information, please refer to the OpenTelemetry documentation.

### **Core Metrics**

These metrics are defined in `metrics.go` and provide insights into the overall health and performance of the Open Match service.

| Metric Name | Description | Unit |
| ----- | ----- | ----- |
| `om_core.ticket.size` | Size of successfully created tickets. | kb |
| `om_core.rpc.duration` | RPC duration. | ms |
| `om_core.rpc.errors` | Total gRPC errors returned. | count |
| `om_core.ticket.activation.requests` | Total tickets set to active, so they appear in pools. | count |
| `om_core.ticket.activation.failures.invalid_id` | Ticket activations per ActivateTickets rpc call that failed due to an invalid id. | count |
| `om_core.ticket.deactivation.failures.unspecified` | Ticket activations per ActivateTickets rpc call that failed due to an unspecified error. | count |
| `om_core.ticket.deactivation.requests` | Tickets set to deactive per DeactivateTickets rpc call. | count |
| `om_core.ticket.deactivation.failures.invalid_id` | Ticket deactivations per DeactivateTickets rpc call that failed due to an invalid id. | count |
| `om_core.ticket.deactivation.failures.unspecified` | Ticket deactivations per DeactivateTickets rpc call that failed due to an unspecified error. | count |
| `om_core.ticket.assignment` | Number of tickets assignments included in each CreateAssignments() call. | count |
| `om_core.ticket.assigment.watch` | Number of tickets assignments included in each CreateAssignments() call. | count |
| `om_core.profile.chunks` | Number of chunks the profile was split into after all tickets were populated in all pools during the InvokeMatchmakingFunctions() call. | count |
| `om_core.cache.tickets.available` | Number of active tickets in the cache at the time the InvokeMatchmakingFunctions() call processed pool filters for the provided profile. | count |
| `om_core.profile.pools` | Number of pools in the profile provided to the InvokeMatchmakingFunctions() call. | count |
| `om_core.mmf.failures` | Number of MMF failures. | count |
| `om_core.mmf.deactivations` | Number of deactivations due to tickets being returned in matches by MMFs. | count |
| `om_core.match.received` | Total matches received from MMFs. | count |

### **Cache Metrics**

These metrics are defined in `internal/statestore/cache/metrics.go` and provide insights into the performance of the replicated ticket cache.

| Metric Name | Description | Unit |
| ----- | ----- | ----- |
| `om_core.cache.tickets.active` | Active tickets that can appear in pools. | count |
| `om_core.cache.tickets.inactive` | Inactive tickets that won't appear in pools. | count |
| `om_core.cache.assignments` | Assignments in Open Match cache. | count |
| `om_core.cache.expiration.duration` | Duration of cache expiration logic. | ms |
| `om_core.cache.assignment.expirations` | Number of assignments expired per cache expiration cycle. | count |
| `om_core.cache.outgoing.updates` | Number of outgoing replication updates per cycle. | count |
| `om_core.cache.ticket.expirations` | Number of tickets expired per cache expiration cycle. | count |
| `om_core.cache.incoming.updates` | Number of incoming replication updates per poll. | count |
| `om_core.cache.ticket.inactive.expirations` | Number of inactive tickets expired per cache expiration cycle. | count |
| `om_core.cache.outgoing.timeouts` | Number of times the outgoing replication queue waited OM\_CACHE\_OUT\_WAIT\_TIMEOUT\_MS and did not reach OM\_CACHE\_OUT\_MAX\_QUEUE\_THRESHOLD updates to send as a batch to the replicator. | count |
| `om_core.cache.outgoing.maxqueuethresholdreached` | Number of times the outgoing replication queue saw OM\_CACHE\_OUT\_MAX\_QUEUE\_THRESHOLD updates in less than OM\_CACHE\_OUT\_WAIT\_TIMEOUT\_MS milliseconds. | count |
| `om_core.cache.incoming.timeouts.empty` | Number of times the incoming replication queue saw no updates after waiting for OM\_CACHE\_IN\_WAIT\_TIMEOUT\_MS. | count |
| `om_core.cache.incoming.timeouts.full` | Number of times the incoming replication queue couldn't process all pending updates in 500ms. | count |

