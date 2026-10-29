この文書を日本語で[Open Match 2 よくある質問とチューニングガイド](jp/FAQ.md)

# Open Match \- Frequently Asked Questions & Tuning Guide

Here are answers to common questions regarding the tuning, configuration, and best practices for Open Match.

#### **Tuning & Performance**

**Q: How do I know if my Open Match core instances are keeping up with state replication, and how do I tune replication performance?**

A: Start by monitoring `om_core.cache.replication.lag`, which measures the end-to-end time (in milliseconds) between when a state update is written to the Redis Stream and when it is applied to an instance's local `ReplicatedTicketCache`, alongside the queue depth gauges `om_core.cache.incoming.backlog` and `om_core.cache.outgoing.backlog`.

For high-throughput or bursty matchmaking workloads, the most impactful first step is enabling packed ticket state updates by setting `OM_CACHE_PACK_TICKET_STATE_UPDATES=true` (and optionally tuning `OM_CACHE_OUT_MAX_PACKED_KEYS`, default `500`). This coalesces contiguous ticket activation and deactivation operations—including concurrent requests across callers—into multi-key Redis Stream entries, substantially reducing both outgoing write overhead and incoming stream entry volume.

If `om_core.cache.replication.lag` remains elevated, you can isolate and tune the specific stage of the pipeline using the following four bottleneck scenarios:

1. **Slow Incoming Polling**:
   * **Symptoms**: `om_core.cache.incoming.polls.full` increments frequently and `om_core.cache.incoming.updates` is consistently at `OM_CACHE_IN_MAX_UPDATES_PER_POLL`, or `om_core.cache.incoming.poll.duration` / `om_core.cache.incoming.poll.wait` are high while `om_core.cache.incoming.backlog` stays low.
   * **Tuning**: Increase `OM_CACHE_IN_MAX_UPDATES_PER_POLL` (default `10000`) to fetch larger batches per `XREAD` call, and lower `OM_CACHE_IN_FULL_POLL_WAIT_MS` (default `100` ms) or `OM_CACHE_IN_POLL_WAIT_MS` (default `250` ms) to poll more frequently. If `om_core.cache.incoming.poll.duration` is high during active polls, scale out Redis read replicas (`OM_REDIS_READ_HOST`).
2. **Slow Local Cache Apply Cycles**:
   * **Symptoms**: `om_core.cache.incoming.backlog` and `om_core.cache.incoming.queue.wait` grow, `om_core.cache.incoming.timeouts.full` increments (indicating the applier hit `OM_CACHE_IN_MAX_APPLY_DURATION_MS` before draining pending updates), or `om_core.cache.incoming.apply.sleep` dominates the cycle time.
   * **Tuning**: Lower `OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS` (default `500` ms) and `OM_CACHE_IN_FULL_APPLY_SLEEP_MS` (default `100` ms), increase `OM_CACHE_IN_MAX_APPLY_DURATION_MS` (default `500` ms) or `OM_CACHE_IN_QUEUE_BUFFER_SIZE` (default `20000`), and ensure the container has always-on CPU allocated (e.g., `--no-cpu-throttling` on Cloud Run). Be aware that reducing applier sleep leaves slightly less CPU time for handling API requests.
3. **Outgoing Queue Contention**:
   * **Symptoms**: `om_core.cache.outgoing.backlog` and `om_core.cache.outgoing.queue.wait` are elevated, `om_core.cache.outgoing.maxqueuethresholdreached` climbs rapidly, or `om_core.cache.outgoing.send.duration` is high.
   * **Tuning**: Enable `OM_CACHE_PACK_TICKET_STATE_UPDATES=true` (and tune `OM_CACHE_OUT_MAX_PACKED_KEYS`), increase `OM_CACHE_OUT_MAX_QUEUE_THRESHOLD` (default `50`) and `OM_CACHE_OUT_QUEUE_BUFFER_SIZE` (default `500`), or lower `OM_CACHE_OUT_WAIT_TIMEOUT_MS` (default `500` ms) so smaller batches flush sooner.
4. **High MMF Prep or Deactivation Wait**:
   * **Symptoms**: `om_core.mmf.prep.duration` is high before MMFs are invoked, or `om_core.mmf.deactivation.wait` is high and `om_core.mmf.deactivation.timeouts` increments after MMFs return matches.
   * **Tuning**: High `om_core.mmf.prep.duration` means `InvokeMatchmakingFunctions` is spending significant time filtering active tickets and chunking pools—favor more profiles with narrower pool filters and keep `OM_CACHE_TICKET_TTL_MS` tight. High `om_core.mmf.deactivation.wait` or `om_core.mmf.deactivation.timeouts` reflects round-trip outgoing write plus incoming apply latency; apply the outgoing and incoming tuning steps above and adjust `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` (default `60000` ms) to match your latency budget.

**Q: Should I wait for ticket deactivation before returning a match (`OM_MATCH_TICKET_DEACTIVATION_WAIT`)?**

A: This setting controls a trade-off between speed and consistency.

* **`true` (Default & Recommended)**: Open Match will wait up to `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` (default `60000` ms) to confirm that all valid tickets in a match have been marked as `inactive` in its local cache before returning the match to your Director (recording the wait time on `om_core.mmf.deactivation.wait`). This is safer and significantly reduces the chance that the same ticket will be proposed in another match by a different MMF. If the deactivation write fails or the wait reaches `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`, Open Match increments `om_core.mmf.deactivation.timeouts`, annotates the match's `extensions` field with a boolean `true` value (`google.protobuf.BoolValue`) under the `"deactivation_timeout"` key, and returns the match so your Director can detect the timeout and handle potential ticket collisions accordingly.
* **`false`**: Open Match returns the match immediately and *then* deactivates the tickets asynchronously in the background. This is faster but less safe. It's possible that before the deactivation is replicated, another matchmaking cycle could begin and propose the same tickets in a new match.

**Q: What happens if my matchmaker cancels the context during `InvokeMatchmakingFunctions`?**

A: Cancelling the context puts the system into an unknown state and should be avoided. The `InvokeMatchmakingFunctions` call may have already sent matches back to your Director, and the deactivation of tickets for those matches might be in progress. If you do cancel and need to recover, you are responsible for manually activating or deactivating tickets as needed to restore the system to a state your matchmaker expects.

---

#### **State Storage (Redis)**

**Q: Do I need Redis persistence and clustering? What happens if my Redis instance goes down?**

A: While Redis persistence (like AOF or RDB) is an option, you should carefully consider if it aligns with your game's needs. If your primary Redis instance goes down, a typical failover can take up to 30 seconds. In that time, most of the matchmaking tickets would have become stale anyway. For many games, it's a better player experience to have clients simply retry their matchmaking request after a short delay rather than waiting for a Redis failover and resuming with potentially old data.

**Q: Can I configure Open Match to use multiple Redis instances for scaling?**

A: Yes and no.

* **Read Replicas (Yes)**: Open Match has built-in support for using Redis read replicas. You can configure a separate host for read operations (`OM_REDIS_READ_HOST`) and another for write operations (`OM_REDIS_WRITE_HOST`). This is a common and effective way to scale read-heavy workloads.  
* **Sharding (No)**: Open Match does not support Redis sharding. It assumes that the single write instance can accept writes for any key, and that all read instances can see all the data that was written.

---

#### **Configuration & Best Practices**

**Q: Why doesn't the OpenTelemetry connection use TLS by default?**

A: The default configuration assumes Open Match core and the OpenTelemetry collector are running as sidecars within the same secure execution context (like a Kubernetes Pod or a Cloud Run service). In this environment, traffic does not leave the secure boundary, and TLS is not typically necessary. If your deployment requires it, you are responsible for securing this connection.

**Q: How should I handle "backfill" profiles versus regular profiles?**

A: If you have profiles designed to fill partially-empty, running game servers, you should generally invoke the MMFs for these profiles *before* you invoke the MMFs for creating brand-new matches. This is because regular profiles can drain the pool of available players very quickly. It is your Director's responsibility to decide the order of MMF invocations to best serve your game's logic.

**Q: My ticket expired immediately after I created it. What happened?**

A: Open Match always calculates a ticket's expiration time based on its `creation_time`. If you manually provide a `creation_time` that is very old (e.g., the Unix epoch of January 1st, 1970\) and the default TTL is 10 minutes, the ticket will be considered expired the moment it is created.

**Q: Can I just set a really long TTL to prevent tickets from expiring?  How should I calculate the correct value for `OM_CACHE_TICKET_TTL_MS?`**

A: Do not use this setting to handle edge cases or "forever" queues. A TTL that is too long will cause successfully matched (inactive) tickets to accumulate in the system memory until their TTL expires, which increases load and memory pressure. You should set this value to be slightly longer than the time it takes to find a match for 95% of your player base (assuming a normal distribution), but no longer.

A practical example of how to derive this number:

1. Analyze your matchmaking data: Determine how long it takes to find a match for the vast majority of your players. For example, if your player skill distribution is normal, you might find that 95% of players are matched within 5 minutes.
1. Define your "Hard Deadline": Decide on the maximum time a player should wait before your game client takes drastic action (like restarting the search with wider parameters or telling the player to try again later). Let's say this is 7 minutes.
1. Set the TTL: Set `OM_CACHE_TICKET_TTL_MS` to slightly more than that hard deadline (e.g., 7 minutes and 30 seconds).

Any player waiting longer than 7 minutes is an edge case. You should handle them using the [Long-Lived Tickets pattern from the Advanced topics documentation](ADVANCED.md#handling-long-lived-tickets): detect the approaching expiration on the client/frontend, and transparently re-create a new ticket with high-priority flags. This keeps the Open Match core lean and performant while still supporting long wait times for specific players.

Under the hood, `OM_CACHE_TICKET_TTL_MS` governs two cleanup mechanisms:
* **Local Min-Heap Expiration**: Each `om-core` instance runs an asynchronous expiration sweep every `OM_CACHE_EXPIRATION_INTERVAL_MS` (default `1000` ms) over deadline-ordered min-heaps, evicting up to `OM_CACHE_EXPIRATION_MAX_DELETES_PER_CYCLE` (default `5000`) expired entries per cycle in small lock-bounded chunks.
* **Redis Stream Trimming**: Each outgoing replication write batch trims expired entries from the `om-replication` Redis Stream using `XTRIM om-replication MINID ~ <threshold>` (computed from `OM_CACHE_TICKET_TTL_MS` plus `OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS`). Keeping `OM_CACHE_TICKET_TTL_MS` tight bounds both in-memory cache size and Redis Stream memory usage.

**Q: Why doesn't `InvokeMatchmakingFunctions` tell me how many tickets were in each pool?**

A: Open Match assumes that if this information is important to you, your MMF will be responsible for tracking it. Your MMF can count the tickets in the pools it receives and include this data in the `extensions` field of the `Match` object it returns.

**Q: Why should I use reverse-DNS notation for profile names?**

A: This is a best practice for managing metric cardinality. Telemetry systems can be overwhelmed if they receive too many unique labels. By using a format like `com.mygame.gamemode.ranked-4v4.us-west1.2025-09-2718:00:00.000`, monitoring tools can easily aggregate metrics by stripping the most specific parts of the name (e.g., aggregating all metrics under `com.mygame.gamemode.ranked-4v4.us-west1`). For a deeper dive on this topic, [the Grafana blog has an excellent article on cardinality spikes](https://grafana.com/blog/2022/02/15/what-are-cardinality-spikes-and-why-do-they-matter/).

**Can I prevent Open Match from deactivating tickets in a match returned by my MMF? I want to evaluate the match quality in my Director first.**

A: No, you cannot disable this behavior directly. Open Match will always deactivate the tickets found in the `roster` of any matches your MMF returns.

However, you can work around this by having your MMF leave the `roster` field empty and instead place the proposed players into the `extensions` field of the match. Your Director can then be coded to inspect the `extensions`. If it approves the match, it is then responsible for manually calling `DeactivateTickets` for the players in that match.

Be aware of the trade-off: until your Director deactivates those tickets, they remain active in Open Match and will be included in subsequent matchmaking cycles!

---

#### **Troubleshooting**

**Q: I'm seeing `Redis Failure; exiting: dial tcp <host:port>: i/o timeout` in my logs.**

A: This is almost always a networking issue. Check to ensure that the service where Open Match is running is deployed on the correct network and that it has the necessary network egress rules and VPC peering configurations to reach your Redis instance.

**Q: I'm using the gRPC-gateway and my client is failing to unmarshal the response from `InvokeMMFs`.**

A: This is an easy-to-miss detail of how the gRPC-gateway works. It wraps the actual response payload inside a parent JSON object under the key `"result"`. When you are unmarshalling the response, you need to extract the content of the `"result"` key first before passing it to your protobuf JSON unmarshaller.

---

#### **Division of responsibility**

**Q: There's an edge case that sometimes happens when matching. Can Open Match handle it for me?**

A: The design philosophy of Open Match is to remain unopinionated about how to handle specific edge cases. If a remediation for an edge case _could_ be handled by Open Match but would also likely need to be handled by most matchmakers, the responsibility is left to the matchmaker. This approach keeps the behavior of Open Match consistent and avoids being prescriptive. For example, one game's discarded match might be another game's backfill opportunity.

For more examples of failure states your matchmaker is expected to handle, see the **Failure Handling** section of this FAQ.

---

#### **Failure Handling**

**Q: What failure states does my matchmaker need to handle?**

A: Your matchmaker must be resilient to the realities of a distributed system. The eventual consistency model of Open Match means your code already needs to be robust against a number of scenarios. Key failure states you must handle include:

* **The Disappearing Player**: A player may disconnect or cancel matchmaking at any time. Your system needs a way to detect this and call `DeactivateTickets` to remove them from the pool.  
* **The Disappearing Server**: A game server that was available may become unhealthy or have a networking failure. What is the solution when a server allocation fails, or the allocation succeeds but players are never able to connect?  
* **Competing Match Proposals**: As discussed in the advanced MMF patterns, your Director will receive competing match proposals for the same tickets and must have logic to de-collide them and re-activate the tickets from discarded matches.

