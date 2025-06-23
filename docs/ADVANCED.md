# Advanced Topics

### **The Role of a Queue in Open Match**

In a production environment with a large number of players, it's a best practice to introduce a queue between your game clients and the Open Match core. While it's technically possible for clients to directly communicate with Open Match, a queue provides several critical benefits for scalability, reliability, and managing the flow of matchmaking requests.

Here’s a breakdown of why a queue is a vital component of a robust matchmaking architecture with Open Match:

* **Load Management and Throttling**: A primary function of the queue is to act as a buffer, absorbing the potentially massive and spiky influx of matchmaking requests from game clients. The queue can then be configured to **batch ticket creation and rate-limit the calls** it makes to Open Match per second (see the `Run` function in [example `v2/internal/mmqueue/mmqueue.go`](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/internal/mmqueue/mmqueue.go) for how ticket creation is managed). This prevents the Open Match core from being overwhelmed by a sudden surge of requests, which could lead to performance degradation or even service outages. The document on common pitfalls explicitly advises against rejecting requests when a ticket limit is reached, and instead recommends that the user "monitor speeds and implement queueing for entry into matchmaking in their frontend".

* **Decoupling and Resilience**: The queue decouples the game clients from the matchmaking service. This means that if Open Match core is temporarily unavailable or experiencing high load, the queue can hold the matchmaking requests and retry them later	 (the `proxyCreateTicket` function in [`v2/internal/mmqueue/mmqueue.go`](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/internal/mmqueue/mmqueue.go) shows an implementation with retries.). This makes the overall system more resilient to transient failures and ensures that no player requests are lost.

* **Improved Player Experience**: By managing the flow of requests, the queue helps to provide a more consistent and predictable experience for players. Instead of potentially facing errors or long delays when the system is under heavy load, players can be placed in a queue and given an estimated wait time, which is a much better user experience.

In essence, while a direct client-to-Open-Match connection might seem simpler for a small-scale test, a queue is an indispensable component for any production-level game that needs to handle a significant number of concurrent players. It ensures the stability and scalability of your matchmaking system, leading to a better experience for your players.

### **Profiles: The Arguments for Your Matchmaking "Function Call"**

In Open Match, it's best to think of the interaction between your **matchmaker** and your **matchmaking functions (MMFs)** as a remote procedure call (RPC). Open Match Core acts as the powerful, scalable framework that facilitates this call, but the logic—the "call" and the "function" itself—is all yours.

Here's how this RPC analogy breaks down:

#### **Your Matchmaker (The Caller)**

Your matchmaker, specifically the Director component, is the **caller**. Its primary responsibility is to decide *when* to make a match and *what kind* of match it wants.

* **Constructing the Arguments (`Profile`)**: The `Profile` you create in your matchmaker is the set of **arguments** for your remote function call (see the `Run` function in [the sample `v2/internal/gsdirector/gsdirector.go`](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/internal/gsdirector/gsdirector.go) for one way to construct and use profiles). It precisely defines the parameters for the matchmaking cycle: "I need a 5v5 match for players with an MMR between 1000 and 1500 who have the 'premium' tag."  
* **Initiating the Call**: When your matchmaker calls `InvokeMatchmakingFunctions` on Open Match core, it's like executing the RPC. You pass in the `Profile` (your arguments) and the address of the function you want to call (the MMF).

#### **Open Match Core (The RPC Framework)**

Open Match core is the **framework or intermediary layer** that makes this RPC possible at scale. It's not the final destination; it's the sophisticated plumbing that connects your caller to your function.

* **Argument Pre-processing**: Before calling your MMF, Open Match core performs a crucial pre-processing step. It takes your `Profile` arguments, efficiently queries its massive in-memory cache of all available tickets, and **populates the Profile's pools with every single matching ticket**.  
* **Invoking the Remote Function**: Open Match core then calls the `Run` method on your MMF, passing the now-populated Profile as the argument. It handles the network communication, authentication, and data serialization (chunking the profile and tickets to stay within gRPC message size limits if necessary). This allows your MMF to focus purely on its logic without worrying about the underlying infrastructure. This aligns with the core design principle: "Matchmakers do matchmaking, and only matchmaking."

#### **Your Matchmaking Function (The Callee)**

Your MMF is the **remote function (the "callee")** that gets executed. It receives the fully prepared arguments from Open Match core and performs the final, specific logic (the `Run` function in [`v2/examples/mmf/functions/fifo/fifo.go`](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/examples/mmf/functions/fifo/fifo.go) shows one way for an MMF to process a request).

* **Receiving the Arguments**: The MMF receives a stream of `ChunkedMmfRunRequest` messages, which it reassembles into a complete `Profile` with the pools already filled with potential players. It doesn't need to know *how* the tickets were found, only that they match the criteria defined by the matchmaker.  
* **Executing the Core Logic**: The MMF's job is to take the provided pools of tickets and create `Matches`. This is where your most nuanced logic lives—forming teams, balancing skills, or any other custom rules that define a "good" match for your game.

In summary, as a developer, you should focus on the direct logical relationship between your **matchmaker** and your **MMF**. Think of it as your code calling your other code. Open Match core is the essential, but transparent, framework that makes this interaction robust, scalable, and reliable in a live production environment. It handles the state, the searching, and the networking, so you can focus on what matters most: the matchmaking logic itself.

### **The Ticket: The Single Source of Truth in Open Match Core**

To build powerful and predictable matchmaking, it's vital to understand what Open Match core actually *manages*. The answer is simple and deliberate: the **`Ticket`** is the only data primitive that "lives" inside Open Match. Everything else is a transient instruction.

#### **What does this mean for you?**

When you interact with Open Match, you only need to reason about the lifecycle of one persistent object: the `Ticket`. This is the fundamental unit of matchmaking—representing a player, a group, or any entity waiting to be matched.

* **Tickets are the State**: The entire stateful component of Open Match core is designed around managing a massive, in-memory cache of `Tickets`. This includes their filterable attributes and their active/inactive status.  
* **Replication is All About Tickets**: The underlying persistence and replication layer (whether using Redis in production or the in-memory store for testing) is exclusively built to handle `Ticket` state changes. When you create a ticket, activate it, or when a match is made, the replication event is a simple, ticket-centric operation: `CreateTicket`, `ActivateTicket`, `DeactivateTicket`. This is explicitly shown in the system's internal data types.  
* **Everything Else is an Action**: Data structures like `Profiles` are not stored in Open Match core. A `Profile` is a set of instructions you send in an RPC call (`InvokeMatchmakingFunctions`). Open Match uses this profile for a single cycle to filter the central pool of tickets and then discards it. It does not save the profile itself.

#### **Why this Design? Focus and Scalability.**

This design choice is a direct consequence of Open Match's core design principles and provides two major benefits:

1. **Clarity and Simplicity**: By having only one persistent data type, the system's behavior becomes much easier to predict and debug. You, the developer, don't need to worry about complex, nested state or how different stored objects might interact. You create tickets, and you send instructions (profiles) to operate on them.  
2. **Massive Scalability**: This minimalist approach to state is the key to Open Match's performance. The system is hyper-optimized for a few simple operations on a single data type at an enormous scale. 

Think of Open Match core as a high-performance, specialized database where the only "table" is for `Tickets`. Your matchmaker acts as the "application layer," sending queries and instructions (`Profiles`) to operate on the data in that table. This strict separation of concerns is what makes Open Match a robust and scalable foundation for your game's matchmaking.

### **The Ticket Lifecycle: Fire-and-Forget, Don't Delete**

In Open Match, managing `Tickets` follows a simple, robust "fire-and-forget" model. Once a ticket is created, you don't need to worry about deleting it. Instead, you control its participation in matchmaking by toggling its state between `active` and `inactive`.

#### **1\. Creation and Automatic Expiration**

When you create a `Ticket`, it is assigned a **Time-to-Live (TTL)**. This TTL is configured in Open Match core (via the `OM_CACHE_TICKET_TTL_MS` setting).

* **No Manual Deletion**: There is no `DeleteTicket` function in the Open Match API. You cannot manually delete a ticket.  
* **Automatic Cleanup**: Once a ticket's TTL is reached, Open Match's internal cache management automatically expires and removes it from the system. This design prevents orphaned tickets and simplifies your logic, as you don't need to build a separate cleanup process.

#### **2\. The Two States: Active and Inactive**

After a `Ticket` is created, its lifecycle is managed through two simple states:

* **Inactive**: When you first create a ticket using `CreateTicket`, it starts in the `inactive` state (*example:* See the `proxyCreateTicket` function in [`v2/internal/mmqueue/`](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/internal/mmqueue/mmqueue.go)`mmqueue.go` and its associated client call in [`v2/internal/omclient/omclient.go`](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/internal/omclient/omclient.go)). An inactive ticket will **not** be included in any pools when your matchmaking functions are invoked. This ensures that a player isn't accidentally matched before you are ready (e.g., before their game client has fully loaded).  
* **Active**: You use the `ActivateTickets` RPC to move a ticket into the `active` state. Only active tickets are considered for matchmaking and will be populated into the pools sent to your MMFs.

#### **3\. Automatic Deactivation by Open Match**

This is the most important part of the "fire-and-forget" model: **You are not responsible for deactivating tickets that have been successfully matched.**

When your Matchmaking Function (MMF) returns a `Match` to Open Match core, the core service automatically performs the following actions on your behalf:

* It inspects all the `Tickets` within the `Match`.  
* It marks every one of those tickets as **inactive**.

This automatic deactivation, as seen in the `InvokeMatchmakingFunctions` logic, ensures that a player who has just been assigned to a game isn't immediately put back into the matchmaking pool for another one. It's a core feature that simplifies your matchmaker logic significantly.

#### **4\. When to Use Manual Deactivation**

So, why does the `DeactivateTickets` RPC exist? It is provided specifically for handling **exceptional scenarios**. The most common use case is when a player leaves the matchmaking queue unexpectedly.

For example, if a player:

* Closes the game client.  
* Disconnects from your service.  
* Explicitly cancels their search for a match.

In these cases, their `Ticket` is still `active` in Open Match. You should then call `DeactivateTickets` with that player's `TicketId` to immediately remove them from matchmaking consideration. This prevents them from being placed in a match they can no longer join.

**Handling Long-Lived Tickets**

What if your game design requires players to wait in a queue for longer than the configured Ticket TTL? Since you cannot override the maximum TTL, you must handle this logic in your own application code, outside of Open Match core. This is an advanced pattern for an exceptional use case.

The recommended approach is to have your frontend service (e.g., your `mmqueue` application) monitor the age of the tickets it creates.

1. **Detect Expiration**: When your service detects that a player's ticket is nearing its expiration time, your service should take action.  
2. **Create a New Ticket**: It should programmatically call `CreateTicket` to generate a **new** ticket for that player.  
3. **Preserve Data in Extensions**: When creating the new ticket, copy all the necessary attributes from the old ticket. Crucially, use the `extensions` field of the `Ticket` protobuf message to carry over any important metadata. This is where you would store custom data like:  
   * `originalCreationTime`: The timestamp of when the very first ticket was created for this player's matchmaking session.  
   * `requeueCount`: A counter that you increment each time you re-create the ticket.  
4. **Implement Custom MMF Logic**: Your Matchmaking Functions (MMFs) must then be written to understand and act on this custom data in the `extensions` field. For example, your MMF could be designed to:  
   * Prioritize players who have been waiting longer by reading the `originalCreationTime`.  
   * Widen the skill bracket for players who have been re-queued multiple times.

From Open Match's perspective, it simply sees a brand-new ticket being created. It has no awareness that this is a "re-queued" player. This pattern perfectly adheres to the Open Match design philosophy: the core service handles the simple, scalable lifecycle of a ticket, while your game-specific, complex queuing logic remains entirely within your control.

### **Pool Size: Favor Concurrency with Many, Smaller Profiles**

A common question is whether Open Match limits the number of tickets a pool can contain. The answer is **no, there is no artificial limit.** However, this flexibility comes with a crucial responsibility. For optimal performance, you should architect your matchmaker to **favor creating more `Profiles` with finely-sliced pools, rather than one large `Profile` with very broad filters.**

#### **The "More Profiles, Smaller Pools" Strategy**

Instead of creating a single, monolithic `Profile` that might return a million tickets, the more efficient and scalable pattern is to break your matchmaking logic into many smaller, concurrent requests.

For example, instead of one `Profile` for "All North American Players", you should create multiple, more specific `Profiles` that run simultaneously:

* `Profile`: `us-east.4v4.skill-tier-1`  
* `Profile`: `us-east.4v4.skill-tier-2`  
* `Profile`: `us-west.4v4.skill-tier-1`  
* ...and so on.

#### **Why This Is More Efficient**

This strategy is central to how Open Match achieves its speed and scalability. It's a direct application of the "concurrent processing" design principle mentioned in the system's foundational documents.

1. **Concurrency is Key**: Open Match is designed to process many `InvokeMatchmakingFunctions` calls concurrently. The `main.go` implementation shows that each MMF invocation runs in its own goroutine, allowing for massive parallelism. By sending many small requests, you leverage this concurrent power to find matches across your entire player base simultaneously.  
2. **Reduces MMF Overhead**: Sending a smaller, more relevant set of tickets to each MMF is significantly faster. Your MMF doesn't waste memory or CPU cycles processing tickets that have no chance of being in the same match. This avoids the high memory, network, and deserialization costs associated with enormous pools.  
3. **Faster Time-to-Match**: Because each MMF has a smaller, more targeted problem to solve, it can return a result much more quickly. This means players get into games faster, which is the ultimate goal of any matchmaking system.  
4. **Avoids Chunking**: Open Match automatically chunks requests larger than the 4MB gRPC message limit. While this prevents failures, the process of chunking and reassembling the data adds latency. Keeping your pools small and targeted helps you stay under this limit and avoid the overhead altogether.

### **The Decoupled Evaluation Strategy: Your Director as the Final Arbiter**

This concurrent approach leads to an important question: What if multiple MMFs, running at the same time, want to use the same player ticket?

The answer is that this is not only allowed, but is an intentional part of the design. It's perfectly fine—and often desirable—to have multiple, concurrently running MMFs evaluating overlapping pools. This will inevitably result in the same ticket appearing in multiple proposed matches.

This isn't chaos; it's a **decoupled evaluation strategy**. It frees your MMFs to be simple and fast. They can independently propose the best possible matches based on the pools they receive, without needing complex locking or synchronization logic. The responsibility for resolving these competing proposals is cleanly delegated to your matchmaker's Director.

Your Director must be written to handle this scenario:

1. **Receive Competing Proposals**: As matches stream back from the `InvokeMatchmakingFunctions` call, your Director will see these independent proposals from different MMFs.  
2. **De-collision and Selection**: The Director is responsible for de-colliding tickets (*example:* the `Run` function in [`v2/internal/gsdirector/gsdirector.go`](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/internal/gsdirector/gsdirector.go) shows one approach to handle ticket collisions). If `Ticket A` appears in both `Match 1` and `Match 2`, your code must choose which match to accept. You could have a simple "first-come, first-served" rule, or you could evaluate the quality of both matches and pick the better one.  
3. **Allocate and Finalize**: Once the Director has chosen the winning matches, it proceeds with server allocation for those matches.  
4. **Re-activate Unused Tickets**: This is a critical final step. For any matches that the Director discards, it is **your matchmaker's responsibility to re-activate all the tickets** in those discarded matches by calling `ActivateTickets` (*example:* the `rejectMatch` local function in [`v2/internal/gsdirector/gsdirector.go`](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/internal/gsdirector/gsdirector.go) shows this). This immediately returns them to the active pool so they can be considered for the very next matchmaking cycle, ensuring no players are lost.

By embracing this pattern of decoupled evaluation, you empower your MMFs to be simple, focused, and fast. Your Director then acts as the final, intelligent arbiter, making the ultimate decision on which matches are best for your players and ensuring the system remains efficient and robust.

### **Scaling Your Profiles: A Fleet-Wide Strategy**

A core design principle in Open Match is that **matchmaking flows from the servers, not the players**. This means your available game servers are the ultimate source of truth for what kind of matches they are ready to host. While you could conceptually create a unique `Profile` for every single server, this approach doesn't scale to a fleet of thousands.

Instead, a robust matchmaker, specifically its **Director** component, should implement an intelligent aggregation and slicing strategy.

#### **The Recommended Pattern: From Servers to Aggregated Profiles**

While it's helpful to think of each server having its own ideal profile, in a large-scale environment, it's more practical to combine the needs of many servers into a single request.

The recommended pattern is as follows:

1. **Read Server Needs**: Your Director should continuously scan your entire fleet of available game servers to determine what kinds of matches they are configured to host. This "source of truth" could be metadata attached to each server instance or a central database that maps server configurations to their desired match types.  
2. **Aggregate and Count**: Your Director's logic should then aggregate these requests, counting how many servers are ready for the exact same type of match (e.g., "150 servers are ready to host a 4v4 Capture the Flag match in `us-east`").  
3. **Send a Combined Profile**: For each unique match request, the Director sends a single, combined `Profile` to Open Match core.  
4. **Pass the Count in Extensions**: Crucially, your Director should add the available server count (e.g., `150`) to the `extensions` field of the `Profile`. This provides your Matchmaking Function (MMF) with the vital context that it can create up to 150 matches of this type.

#### **The "Uber-Profile" Problem and the Solution: Intelligent Slicing**

Simply aggregating all identical requests can lead to its own inefficiency: an "uber-profile" representing thousands of servers. Sending one massive request to an MMF is less efficient than sending many small ones.

The solution is for your Director to intelligently slice these large, aggregated requests into smaller, more logical `Profiles` before sending them to Open Match. This is where your game's specific logic is most important. Common and effective ways to slice profiles include:

* **Physical Region**: Grouping servers by regions like `us-east`, `eu-west`, or `sea`.  
* **Game Mode**: Separating `deathmatch` from `capture-the-flag`.  
* **Platform**: If your game doesn't support cross-play, you would slice by `pc`, `console`, etc.

By splitting a request for 10,000 "4v4" servers into more granular profiles like `us-east.4v4`, `eu-west.4v4`, and so on, you leverage Open Match's highly concurrent design. Each smaller `Profile` is processed in parallel, leading to faster match creation across your entire player base.

#### **Your Director is in Control**

It is vital to understand that this entire process of reading server state, aggregating requests, slicing profiles, and adding extensions happens **entirely within the Director component that you write**.

Open Match core remains focused on its single, specialized task: receiving profiles, finding matching tickets at scale, and invoking your MMFs. It has no awareness of your server fleet. This clean separation of concerns gives you the power to implement sophisticated, game-specific fleet management logic while relying on Open Match to handle the high-performance, concurrent task of finding players.

### **Advanced MMF Patterns: Beyond Simple Matchmaking**

While the primary role of a Matchmaking Function (MMF) is to create matches, it's more powerful to think of it as **a general-purpose engine for executing your custom code against a filtered set of tickets.** The `InvokeMatchmakingFunctions` API in Open Match core is the mechanism to securely and scalably run this code. This opens the door to several advanced patterns that go beyond basic matchmaking.

#### **1\. MMFs for Analytics and Data Aggregation**

Sometimes, you don't want to make a match; you just want to ask a question about the current player population.

* **The Goal**: Find out how many players in the `us-east` region are currently queuing for the `capture-the-flag` game mode.  
* **The Pattern**:  
  1. Your Director creates a `Profile` with a pool whose filters select for that specific region and game mode.  
  2. It calls `InvokeMatchmakingFunctions`, but points it to a special MMF you've written called `count-players-mmf`.  
  3. This MMF doesn't try to form teams. Its only job is to count the number of tickets it received in the pool.  
  4. It then returns a "dummy" `Match` object. Instead of containing rosters of players, it uses the `extensions` field to hold the result, for example: `{"player_count": 127}`.  
  5. Your Director receives this dummy match, recognizes it's for analytics, and uses the player count to inform its strategy, perhaps by spinning up more servers for that mode.

#### **2\. Decoupling Logic for Performance (e.g., Logging)**

Imagine your primary MMF has complex logic, and you also need to log every potential match for later analysis. Adding logging directly to your main MMF could slow it down, increasing player wait times. The solution is to decouple these tasks.

* **The Goal**: Generate matches on a critical "hot path" as fast as possible, while still logging all match data for analysis on a non-critical "cold path".  
* **The Pattern**:  
  1. Create two MMFs: `find-matches-mmf` (your fast, production logic) and `log-matches-mmf` (an identical copy, but it only logs results and doesn't return anything).  
  2. When your Director calls `InvokeMatchmakingFunctions`, it populates the `mmfs` field of the request with *both* of these MMFs.  
  3. Open Match core will then send the exact same populated pools to both MMFs concurrently.  
  4. `find-matches-mmf` returns results to your Director with minimal latency, while `log-matches-mmf` independently handles the slower task of writing detailed logs.

#### **3\. Safely Testing New Strategies (A/B Testing)**

This is one of the most powerful advanced patterns. You have a trusted, live matchmaking strategy but want to test a new one without risking a poor player experience.

* **The Goal**: Test an experimental `New_MMF` against your current `Production_MMF` using live player traffic.  
* **The Pattern**:  
  1. This follows the same concurrent pattern as above. Your Director calls `InvokeMatchmakingFunctions` and requests that *both* `Production_MMF` and `New_MMF` be invoked with the same profile.  
  2. Both MMFs run in parallel, each creating their own set of proposed matches from the same ticket pools. This approach is a core part of the Open Match design, which favors **evaluating the same tickets with multiple strategies and making the final decision later**.  
  3. Your Director now receives two streams of matches. It can act as the final arbiter and decide what to do with the experimental results:  
     * **Shadow Mode**: Log the results from `New_MMF` to compare its match quality against `Production_MMF` offline, without ever using its matches for live players.  
     * **Quality-Based Rollout**: Your MMFs can include a `match_quality_score` in the `Match` extensions. The Director can then compare the scores and perhaps choose the match from `New_MMF` if it's demonstrably better than the one from `Production_MMF`.  
     * **Percentage-Based Rollout**: The Director can choose to send, for example, 1% of the matches proposed by `New_MMF` to live servers, allowing you to gather real-world data on a small scale before a full launch.

These patterns demonstrate that an MMF is not just a function, but a flexible tool. By leveraging Open Match's concurrent and stateless design, you can implement sophisticated operational, analytical, and testing strategies that are cleanly separated from your core matchmaking logic.

### **Understanding the Open Match Cache: Eventual Consistency in Action**

In a production environment, you will run multiple instances of Open Match core for scalability and high availability. These instances share state through a powerful, distributed caching model. It is essential to understand that this model is **eventually consistent**.

#### **What is Eventual Consistency?**

Eventual consistency means that while all Open Match core instances will converge on an identical state, there is no guarantee that they are all perfectly in sync at the exact same millisecond. One instance might process an update a few milliseconds before another.

This is not a bug; it is a deliberate and standard design pattern for high-performance distributed systems. It prioritizes availability and throughput over the extreme overhead required for guaranteed, instantaneous consistency.

#### **How Replication Works: The Central Event Log**

Open Match achieves this state synchronization using Redis Streams as a central, ordered log for all ticket-related events.

1. **Writing to the Log**: When an Open Match instance receives an API call (e.g., `CreateTicket`), it writes a new event to the central Redis Stream.  
2. **Reading from the Log**: Every Open Match instance, including the one that wrote the event, is constantly listening to this stream for new events.  
3. **Applying Changes**: As events arrive from the stream, each instance applies the changes to its own local, in-memory ticket cache.

This event-sourcing pattern provides two critical guarantees:

* **Total Order**: Every instance receives and processes all events in the exact same order.  
* **Durability**: The Redis stream acts as the durable source of truth for the system's state.

To prevent an instance from being "ahead" of the global state, an instance that creates a ticket **will not add that ticket to its own local cache until it receives that same creation event back from the Redis stream**. This ensures all instances update their state based on the same public information.

#### **What This Means for Your Matchmaker**

Your Director's `InvokeMatchmakingFunctions` calls will be load-balanced across all running Open Match core instances. Because of eventual consistency, each instance might have a slightly different view of the ticket pool at any given moment.

This is where you must consider a critical trade-off. The Open Match API allows you to invoke **multiple MMFs from a single API call**.

* **The Benefit**: When you invoke multiple MMFs in one call, you get a powerful guarantee: **all MMFs in that single operation see an identical, atomic snapshot of the ticket pool** from the one instance that handled your request. This is perfect for tasks like A/B testing or analytics where you need to compare results on the exact same data set.

* **The Trade-Off**: This guarantee comes at a cost. Forcing multiple MMFs into a single call pins that entire workload to a single Open Match core instance. For that operation, you are intentionally **bypassing the benefits of load balancing and horizontal scalability**.

Therefore, you should only use the multi-MMF invocation pattern when it is **absolutely critical** that the functions see an identical pool. For general-purpose, high-throughput matchmaking, it is often better to make many separate, single-MMF API calls. This allows your requests to be distributed across all available Open Match core instances, maximizing the performance and resilience of your entire system.

### **The Dimension of Time in Open Match: A Strategic Ally**

In matchmaking, time is often seen as an enemy—a countdown clock to player frustration. However, in Open Match's asynchronous, continuous-flow model, time becomes a powerful strategic ally. The key is to embrace the flow of players rather than trying to force it into a deterministic, stop-the-world process.

#### **The Misconception: The "Game Loop" Approach to Matching**

A common misconception among developers new to distributed matchmaking is to apply a "game loop" mindset: stop everything, process every available player, create matches, then repeat. This approach, while perfect for rendering a single frame in a game engine, is inefficient for a high-throughput service like a matchmaker. It ignores a fundamental truth of online games: **a new batch of players is always arriving.**

Open Match is designed not as a discrete loop, but as a continuous stream of operations. Attempting to halt this flow to achieve perfect determinism in a single cycle is counterproductive and fights against the system's core strengths.

#### **The Core Pattern: Expanding the Search Over Time**

The correct, more powerful pattern is to use time to your advantage by creating a matchmaking process that evolves with player wait times.

1. **Your Director Defines Time Brackets**: Your Director's main job is to create `Profiles` that segment the player population by how long they have been waiting. It does this by using the `CreationTimeRangeFilter` in its pool definitions.  
2. **It Invokes Multiple, Concurrent MMFs**: For each time bracket, the Director can invoke a corresponding MMF with different rules. For example:  
   * **For players waiting 0-10 seconds**: An MMF (`strict-mmf`) that enforces a very tight skill rating (MMR) range, aiming for the highest possible match quality.  
   * **For players waiting 10-30 seconds**: A different MMF (`relaxed-mmf`) that uses a wider MMR range.  
   * **For players waiting \> 30 seconds**: A third MMF (`wide-search-mmf`) that prioritizes getting players into *any* reasonable game, even if the ideal skill balance isn't met.  
3. **Time Creates Better Opportunities**: This strategy embraces the continuous flow of players. You don't have to make a poor-quality match for a player who has only been waiting for 5 seconds. During peak hours, you may find that by waiting just a few more seconds, a much better-suited group of players will have joined the queue, allowing you to form a higher-quality match for everyone involved.

This dovetails perfectly with the idea of a **match quality score**. Your MMFs can be designed to calculate a score for each match they propose. The `strict-mmf` might only return matches with a score of 90 or higher. The `wide-search-mmf`, on the other hand, might be configured to accept any match with a score above 50, ensuring that long-waiting players find a game. Your Director can then use these scores to choose between competing match proposals.

#### **Time as a Safeguard: The MMF Timeout (`OM_MMF_TIMEOUT_SECS`)**

Finally, the MMF timeout should be viewed not as a matchmaking tool, but as a critical **safety net**. Its sole purpose is to prevent a malfunctioning MMF from running indefinitely and harming your matchmaker's stability.

As discussed in the advanced patterns, an MMF might be designed to have a long, valid runtime for complex analytics or simulations. Your `OM_MMF_TIMEOUT_SECS` value must be set intentionally. You should carefully evaluate the longest possible *valid* runtime for any of your MMFs and set the timeout to a value comfortably longer than that. This ensures that only a truly broken MMF—one stuck in an infinite loop, for example—will ever be terminated, protecting your system's stability without interfering with legitimate, long-running tasks.