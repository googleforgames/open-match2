この文書を日本語で[ Open Match 2: 技術的進化](MIGRATION-JP.md)

# **Open Match 2: A Technical Evolution**

This document provides a detailed technical comparison between Open Match 1 (OM1) and Open Match 2 (OM2), focusing on the key architectural and design changes.

## **A Unified, Flexible Core Architecture & Deployment**

The most significant architectural change in OM2 is the move from a distributed microservices architecture to a single, unified binary called `om-core`. In OM1, components like the `frontend`, `backend`, and `query` services were separate deployments. OM2 consolidates their functionality into one stateless service, which simplifies deployment, management, and reduces the number of network hops for most operations.

Each instance of the `om-core` service is designed to be fully independent. It establishes its own read and write connections to Redis and can handle API requests from both your game client services (for ticket creation via the `CreateTicket` RPC) and your matchmaker (the director, which calls `InvokeMatchmakingFunctions`).

This unified design does not preclude advanced deployment patterns. The architecture explicitly supports the ability to run separate, horizontally-scaled pools of `om-core` instances for different purposes. For example, a team could maintain a large, auto-scaling pool dedicated to the high volume of incoming `CreateTicket` requests, and a separate, smaller pool for running the less frequent but more resource-intensive `InvokeMatchmakingFunction`. We worked hard to ensure no part of the design compromises this flexibility.

## **Build & Local Development**

The developer experience for building and testing Open Match has been fundamentally reworked for simplicity and speed. OM1 relied on a complex, 1000+ line `Makefile` to manage the build and containerization process. This system was powerful but often opaque and difficult to troubleshoot.

OM2 replaces this with **CNCF Buildpacks**. This modern, cloud-native approach automatically transforms source code into an optimized, secure container image without requiring a Dockerfile or complex build scripts. This shift makes the entire build process more transparent and significantly easier to debug.

For local development, OM2 introduces a major quality-of-life improvement: a built-in in-memory state store. Previously, developers needed to run and configure a local Redis instance just to get started. Now, you can run the entire `om-core` service with a single `go run .` command, using the in-memory replicator for a fast, dependency-free development loop.

## **API Streaming, gRPC Gateway, and Simplified Load Balancing**

The OM2 API has been consolidated into a single, unified `OpenMatchService`. This replaces the multiple gRPC endpoints of OM1 and introduces a more efficient, streaming-based approach for receiving match results from your Matchmaking Functions (MMFs).

A key improvement is the native integration of a **gRPC-Gateway**. This gateway automatically exposes a full-featured HTTP/JSON REST interface alongside the primary gRPC endpoint. While gRPC is highly performant, implementing robust, client-side load balancing can be a significant challenge across different languages and environments. By providing a standard HTTP interface, OM2 allows developers to leverage industry-standard, server-side HTTP load balancers provided by cloud platforms. This simplifies client architecture, improves reliability, and makes the API accessible to a wider range of services that may not have mature gRPC tooling.

## **State Management Optimized for Redis Read Replicas**

Open Match 2 introduces an in-memory `ReplicatedTicketCache` within each `om-core` instance to replace direct Redis queries. As a result, all ticket reads for invoking MMFs are now coming from local memory, making them as fast as possible.

For production environments, Redis is used as the ticket replication mechanism between each `om-core` instance. Each `om-core` instance is configured with distinct **read and write Redis endpoints**. In a typical production topology, all instances would point their write configuration to a single Redis primary instance. The read configuration, however, would point to one or more read replicas. This allows you to distribute the high volume of ticket-pooling reads across multiple replicas, dramatically increasing the read throughput of your matchmaking system. This design avoids the need for more operationally complex topologies like Redis Cluster or the use of Redis Sentinel, providing massive scalability with a simpler infrastructure footprint, and one that is simple to stand up and get production ready quickly using Cloud Memorystore for Redis on Google Cloud.

## **Configuration: The 12-Factor Approach**

OM2's configuration system has been entirely reworked to be simpler, more discoverable, and more aligned with modern cloud-native principles. It fully embraces a **12-factor app methodology**, where all configuration is managed through environment variables.

This approach eliminates the need to manage configuration through layered YAML files applied in opaque ways by complicated Helm charts. More importantly, all configuration has been centralized into a single Go code module (`internal/config`). This module serves as the single source of truth where every configuration variable: its default value and purpose are defined and documented directly in the code. When the `om-core` service starts, it logs all the values it is using at the `debug` log level. This makes exploring and understanding the available settings significantly simpler than in OM1, as developers can go to one place in the code to see every possible configuration option without hunting through Helm charts or external documentation.  Updating a value is as simple as making a new `om-core` deployment with the adjusted environment variable values, which is well supported by cloud-native platforms such as GKE and Cloud Run.

## **Match Resolution and Ticket Collision Handling**

One of the most fundamental architectural changes in Open Match 2 is the complete redesign of the match resolution process and the handling of ticket collisions. This change marks a philosophical shift away from a framework-managed resolution process toward a more performant and explicit developer-managed model. The new approach removes significant internal complexity from Open Match, giving developers greater flexibility and clearer ownership over the final state of their matches.

In Open Match 1, the framework was deeply involved in ensuring match uniqueness through a multi-stage process involving a `synchronizer` and an `evaluator`. Matchmaking Functions (MMFs) did not produce final matches; they produced *match proposals*. These proposals were funneled into a "synchronization window," a period during which the `synchronizer` component collected all proposals from concurrently running MMFs. Once this window closed, the collected set was passed to the `evaluator`. The `evaluator` was a mandatory service where developers were expected to inject their own logic to resolve "ticket collisions"—instances where multiple proposals contained the same ticket.

While powerful in theory, this design created a high barrier to entry. Developers had to deeply understand the complex internal loop to write an effective evaluator. In practice, this complexity led most to avoid writing a custom evaluator, instead defaulting to the basic example provided. This anti-pattern meant many developers were entirely unaware that ticket collisions were happening or that their resolution was critical for the performance and correctness of a highly-concurrent matchmaker, invalidating the core assumptions of the OM1 design.

Open Match 2 solves this by removing the `synchronizer` and `evaluator` entirely. Its responsibility now ends with orchestrating MMFs and streaming their results back to the client. OM2 is no longer aware of ticket collisions; if two MMFs return matches with an overlapping ticket, it will not intervene. The responsibility for collision detection and resolution now rests explicitly and entirely with the developer's matchmaker code (the director). The matchmaker receives a stream of finalized matches and must perform its own check to ensure a ticket has not already been used before assigning it to a game server. This trade-off—giving up internal collision management—yields tremendous benefits: significantly lower latency by eliminating the synchronization window, a radically simpler core architecture, and complete flexibility for developers to implement collision resolution in the manner that best suits their needs.

## **A Shift from Polling to Push Assignments**

The process for retrieving match assignments has been fundamentally redesigned in Open Match 2 to vastly improve scalability and simplify client-side logic. The new architecture moves away from the inefficient client-side polling pattern of OM1 to a more robust, centralized model.

In OM1, after creating a ticket, each game client was responsible for calling `GetAssignments` and entering a long-polling loop, repeatedly querying the `om-frontend` service. This pattern created significant scalability challenges, requiring every client to maintain a persistent gRPC stream with Open Match, which consumed substantial resources on the frontend service.

To ease the transition from the previous version, Open Match 2 provides legacy endpoints that replicate OM1's assignment responsibilities. However, these methods are marked as **deprecated** and are included only to lower the initial migration burden. **Developers should consider these a temporary compatibility layer and plan to move away from them as soon as convenient.**

The recommended and forward-looking approach in OM2 is for the **Matchmaker** to handle all assignment logic. The Matchmaker (aka 'director') has always been responsible for determining which game server a match would be assigned to.  With the move to OM2, the path for your matchmaker to send assignments out to game clients should no longer use OM as an intermediate; **instead it is responsible for sending the assignment to your client notification mechanism directly**.  This centralized model is vastly more scalable, moving from thousands of polling clients to a single, authoritative service receiving match data. It simplifies game client logic and fosters a cleaner architecture by decoupling the client from the assignment-retrieval process. It also integrates more cleanly with typical game online service suites, many of which already have a performant client notification mechanism.

## **Migrating Your Open Match Ticket Client from v1 to v2**

This section explains how to update your game client code—the part of your application that creates tickets—from the Open Match 1 (OM1) API to the new, streamlined Open Match 2 (OM2) API.

#### **1\. Updating Your Connection: From gRPC to HTTP**

The most significant infrastructural change is the shift from a direct gRPC connection to an HTTP-based one that leverages the built-in gRPC-Gateway for simplicity and robust load balancing.

* **In OM1 (client.go),** your client connected directly to the om-frontend service using a **pure gRPC client**.
* **In OM2 (omclient.go),** the recommended pattern is to use a **standard HTTP client**. Your protobuf request is marshaled to JSON and sent over HTTP to the om-core service, which handles the transcoding to gRPC on the server side.
* **Why the change?** This was done to simplify client-side infrastructure. While gRPC is highly performant, its client-side load balancing can be complex. By offering an HTTP interface, OM2 allows you to use robust, industry-standard, server-side HTTP load balancers from cloud providers, simplifying your deployment and improving reliability.

#### **2\. The New Assignment Flow**

The method for retrieving match assignments has been completely redesigned to improve scalability. The client-side polling model from OM1 is now an explicit anti-pattern.

* **In OM1 (frontend.txt),** clients would call the GetAssignments RPC and enter a long-polling loop, repeatedly querying the om-frontend service until a match was found.
* In OM2, this model is deprecated. The api.txt definition for the OpenMatchService has comments to make this clear.
* The assignment logic is now handled by your **matchmaker**, which receives the complete Match object. The client should expect a push notification from your game's own backend services, not from Open Match.

#### **3\. Ticket Protobuf Definition Changes**

| Open Match 1 (messages (om1).txt) | Open Match 2 (messages (om2).txt) | Analysis of Change |
| :---- | :---- | :---- |
| Assignment assignment | *Field removed* | The Assignment is no longer part of the Ticket state, reinforcing that it is the client's responsibility to get ticket assignments from whatever part of your Matchmaker is allocating game servers to matches. |
| SearchFields search\_fields | Attributes attributes | Renamed for clarity. This field still holds the searchable tags and key/value data for filtering but is now more logically named. |
| google.protobuf.Timestamp create\_time | *Field removed from proto, expiration\_time added* | The create\_time field is no longer part of the Ticket message definition. This timestamp is now automatically generated by the underlying state replication layer (Redis Streams) when the ticket is persisted. The new expiration\_time field has been added to the message to give developers explicit control over the ticket's lifecycle and time-to-live (TTL) in the cache. |

The Ticket message itself has evolved between versions, reflecting the larger architectural shifts.

#### **4\. Ticket ID Generation**

A critical change in OM2 is that you can no longer set your own custom ticket IDs.

In Open Match 2, ticket IDs are now generated by the core service and are intrinsically linked to the underlying replication system (specifically, the Redis Stream entry ID). The id field of the Ticket returned by a CreateTicket call is the final, authoritative ID for that ticket within Open Match.

If your application needs to track tickets using its own custom identifier, the recommended pattern is to include that identifier as a searchable attribute on the ticket itself. For example, you can add your custom ID as a string attribute when creating the ticket:

```go
ticket.attributes.string_args["custom_id"] = "your-unique-game-id"
```

This allows you to later query Open Match for the ticket associated with your custom ID without interfering with the internal ID generation system.

#### **5\. Ticket Deletion and Lifecycle Management**

The lifecycle of a ticket in Open Match 2 is fundamentally different from OM1. Manual deletion is no longer supported, replaced by an automated, time-based expiration system.

To remove a ticket from matchmaking consideration, you should deactivate it using the `DeactivateTickets` RPC. This moves the ticket into an "inactive" set, where it will be ignored by future matchmaking functions. All tickets are now automatically deleted from the system by the underlying state replication layer when their expiration time is reached. When creating a ticket, you can set the `expiration_time` field. If left unset, a default ticket time-to-live (TTL) is applied from the `om-core` configuration (`OM_CACHE_TICKET_TTL_MS` environment variable). The `expiration_time` is immutable after creation; to extend a ticket's life, you must re-create it. For use cases where you need to preserve priority across these re-creations (e.g., to prevent starvation), you should add a custom timestamp to the ticket's `extensions` field and code your MMFs to use it.

This new lifecycle model provides several key advantages. By offloading deletion to the underlying state store's TTL mechanism, the **`om-core` code is significantly simplified**. This design **eliminates many previous causes of ticket 'leakage'**, where tickets could get stuck in the system due to bugs or race conditions, because every ticket now has a guaranteed maximum lifetime. As a result, **developers no longer need to build or run their own manual cleanup tools** to find and remove old or abandoned tickets; the system is now self-cleaning by design.

## **Migrating Your Open Match Matchmaker from v1 to v2**

This section explains how to update your matchmaking logic to work with the new, more powerful APIs in Open Match 2 (OM2). It covers the evolution from OM1's gRPC-based FetchMatches/AssignTickets pattern to the new InvokeMatchmakingFunctions streaming API in OM2, which is accessible via a gRPC-Gateway.

First, a note on terminology: going forward, we will refer to the developer-written service that orchestrates matchmaking as a **"matchmaker."** The "director" pattern, which periodically fetches matches, is just one of many ways to build a matchmaker.

#### **1\. Shifting from Pure gRPC to a gRPC-Gateway (HTTP) Client**

The most significant infrastructural change for your matchmaker is the move from a direct gRPC connection to an HTTP-based one that leverages gRPC-Gateway for transcoding.

* **In OM1 (director.go),** your matchmaker connected directly to the om-backend service using a pure gRPC client.

```go
// OM1: Direct gRPC connection
conn, err := grpc.Dial("om-backend.open-match.svc.cluster.local:50505", ...)
be := pb.NewBackendClient(conn)
```

* **In OM2 (omclient.go & gsdirector.go),** the recommended pattern is to use a standard HTTP client to communicate with om-core's RESTful endpoint. This endpoint is a **gRPC-Gateway**, which translates the incoming HTTP/JSON requests into gRPC calls on the server side. The omclient.go file provides a reference implementation for this, handling details like authentication and marshaling protobufs to JSON.

* **Why the change?** This was done to simplify client-side infrastructure. While gRPC is highly performant, client-side load balancing can be complex to implement correctly. By offering an HTTP interface, OM2 allows you to use robust, industry-standard, server-side HTTP load balancers from cloud providers, simplifying your deployment and improving reliability.

#### **2\. From FetchMatches to the InvokeMatchmakingFunctions Stream**

The core matchmaking RPC has evolved from a simple request-stream to a more powerful bi-directional stream, enabling more dynamic and continuous matchmaking logic.

* **OM1 Flow:** The FetchMatches RPC involved sending a single request containing all match profiles and then entering a simple loop to drain the response stream of all resulting matches.

* **OM2 Flow:** The new InvokeMatchmakingFunctions RPC is a long-lived, **bi-directional stream**. This allows a matchmaker to send new or updated profiles and receive completed matches concurrently on the same stream without having to initiate new requests.

#### **3\. The New Assignment Flow: AssignTickets is Deprecated**

A critical simplification in OM2 is the removal of the explicit AssignTickets step from the matchmaker's workflow.

* **In OM1,** after receiving matches from FetchMatches, the director had to make a separate AssignTickets RPC call to associate game server connection details with the tickets in each match.

* **In OM2,** this two-step process is eliminated. Your matchmaker receives this match from the InvokeMatchmakingFunctions stream and no longer needs to make a separate call to OM2 to make an assignment; **instead it is responsible for sending the assignment to your online services suite's client notification mechanism directly**. The AssignTickets RPC is now **deprecated**.

While OM2 provides legacy assignment endpoints on the om-core service to ease the initial migration burden, these are intended only as a temporary compatibility layer. You should plan to move away from them as soon as is convenient.

### **Migrating Your Matchmaking Function (MMF) from v1 to v2**

This guide explains how to update your Matchmaking Function (MMF) from the Open Match 1 (OM1) API to the new, more powerful streaming API in Open Match 2 (OM2). The role of the MMF has been elevated in OM2 from a simple "proposal generator" to the core engine that creates final, assignable matches.

#### **1\. Changes to the MMF Service Definition**

The fundamental gRPC contract for the MMF has evolved from a simple request-stream to a more flexible bi-directional stream. This requires changing the signature and core loop of your Run function.

* In OM1 (matchfunction.proto), the Run RPC was a request-response stream:
  rpc Run(RunRequest) returns (stream RunResponse)
  Your MMF received a single RunRequest and then streamed back one or more RunResponse messages containing match proposals.

* In OM2 (mmf.proto), the Run RPC is now a bi-directional stream:
  rpc Run(stream ChunkedMmfRunRequest) returns (stream StreamedMmfResponse)
  This means your MMF's Run function will now concurrently receive requests and send responses on the same stream, allowing for more efficient interaction with om-core.

#### **2\. How You Receive Tickets**

A major simplification is that the MMF is no longer responsible for fetching its own tickets. om-core now handles this and streams the tickets to you.

* **In OM1 (mmf.go),** your MMF had to actively query for tickets. The first step in your Run function was to call a helper like matchfunction.QueryPools, which made a separate RPC call to the Open Match Query Service to get the ticket pool.

* **In OM2 (fifo.go),** your MMF is now a **passive recipient of tickets**. om-core queries for tickets based on the profile sent by the matchmaker and streams them *to* your MMF. Your Run function's logic will now start with a loop that calls stream.Recv() to receive ChunkedMmfRunRequest messages and populate its local ticket pools before starting its matchmaking logic. This removes the need for your MMF to contain any ticket querying code.

#### **3\. Creating Final Matches (Not Proposals)**

The most critical conceptual change is that the MMF's role is no longer to suggest proposals but to create the final, authoritative matches.

* **In OM1,** the RunResponse message contained a Match object that was considered a proposal. This proposal was then sent to a separate Evaluator service to resolve conflicts and make the final decision.

* **In OM2,** with the removal of the Evaluator, the Match your MMF creates is **the final, definitive match**. This means your matchmaking logic must be robust enough to be the source of truth.
