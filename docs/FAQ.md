# Open Match \- Frequently Asked Questions & Tuning Guide

Here are answers to common questions regarding the tuning, configuration, and best practices for Open Match.

#### **Tuning & Performance**

**Q: How do I know if my Open Match core instances are keeping up with state replication?**

A: You should monitor the `om_core.cache.incoming.updates` and `om_core.cache.incoming.timeouts.full` metrics.

* `om_core.cache.incoming.updates`: This metric shows how many updates an instance processes in each polling cycle. It's normal for this number to occasionally spike to the maximum configured value (`OM_CACHE_IN_MAX_UPDATES_PER_POLL`). However, if this metric is *consistently* at the maximum, it's a sign that the instance may not be processing updates from Redis as fast as they are arriving.  
* `om_core.cache.incoming.timeouts.full`: This counter increments when an instance cannot process all pending updates within its internal time limit. Occasional increments are fine, but if this counter is increasing rapidly, it's a clear indicator that the instance is falling behind on replication.

If you see these signs, you can try lowering the `OM_CACHE_IN_SLEEP_BETWEEN_APPLYING_UPDATES_MS` value. Be aware that every millisecond you remove from this sleep time is a millisecond that the core process cannot spend on handling API calls, so there is a trade-off.

**Q: Should I wait for ticket deactivation before returning a match (`OM_MATCH_TICKET_DEACTIVATION_WAIT`)?**

A: This setting controls a trade-off between speed and consistency.

* **`true` (Default & Recommended)**: Open Match will wait to confirm that all tickets in a match have been marked as `inactive` in its local cache before returning the match to your Director. This is safer and significantly reduces the chance that the same ticket will be proposed in another match by a different MMF.  
* **`false`**: Open Match returns the match immediately and *then* attempts to deactivate the tickets. This is faster but less safe. It's possible that before the deactivation is replicated, another matchmaking cycle could begin and propose the same tickets in a new match.

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

**Q: Can I just set a really long TTL to prevent tickets from expiring?**

A: This is strongly discouraged. Open Match relies on a reasonably-configured TTL to manage the size of its in-memory cache and to keep the replication process snappy. The best practice is to set a TTL that covers the vast majority of your matchmaking times. For exceptional cases where a ticket needs to live longer, you should implement a "re-up" mechanism in your own matchmaker logic, as described in the [advanced topic](ADVANCED.md) on handling long-lived tickets.

**Q: Why doesn't `InvokeMatchmakingFunctions` tell me how many tickets were in each pool?**

A: Open Match assumes that if this information is important to you, your MMF will be responsible for tracking it. Your MMF can count the tickets in the pools it receives and include this data in the `extensions` field of the `Match` object it returns.

**Q: Why should I use reverse-DNS notation for profile names?**

A: This is a best practice for managing metric cardinality. Telemetry systems can be overwhelmed if they receive too many unique labels. By using a format like `com.mygame.gamemode.ranked-4v4.us-west1.2025-09-2718:00:00.000`, monitoring tools can easily aggregate metrics by stripping the most specific parts of the name (e.g., aggregating all metrics under `com.mygame.gamemode.ranked-4v4.us-west1`). For a deeper dive on this topic, [the Grafana blog has an excellent article on cardinality spikes](https://grafana.com/blog/2022/02/15/what-are-cardinality-spikes-and-why-do-they-matter/).

---

#### **Troubleshooting**

**Q: I'm seeing `Redis Failure; exiting: dial tcp <host:port>: i/o timeout` in my logs.**

A: This is almost always a networking issue. Check to ensure that the service where Open Match is running is deployed on the correct network and that it has the necessary network egress rules and VPC peering configurations to reach your Redis instance.

**Q: I'm using the gRPC-gateway and my client is failing to unmarshal the response from `InvokeMMFs`.**

A: This is an easy-to-miss detail of how the gRPC-gateway works. It wraps the actual response payload inside a parent JSON object under the key `"result"`. When you are unmarshalling the response, you need to extract the content of the `"result"` key first before passing it to your protobuf JSON unmarshaller.

---

#### **Failure Handling**

**Q: What failure states does my matchmaker need to handle?**

A: Your matchmaker must be resilient to the realities of a distributed system. The eventual consistency model of Open Match means your code already needs to be robust against a number of scenarios. Key failure states you must handle include:

* **The Disappearing Player**: A player may disconnect or cancel matchmaking at any time. Your system needs a way to detect this and call `DeactivateTickets` to remove them from the pool.  
* **The Disappearing Server**: A game server that was available may become unhealthy or have a networking failure. What is the solution when a server allocation fails, or the allocation succeeds but players are never able to connect?  
* **Competing Match Proposals**: As discussed in the advanced MMF patterns, your Director will receive competing match proposals for the same tickets and must have logic to de-collide them and re-activate the tickets from discarded matches.

