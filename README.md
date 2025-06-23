![Open Match](https://github.com/googleforgames/open-match-docs/blob/master/site/static/images/logo-with-name.png)

Open Match is an open source game matchmaking framework that simplifies building
a scalable and extensible Matchmaker. It is designed to give the game developer
full control over how to make matches while removing the burden of dealing with
the challenges of running a production service at scale.

This repository contains the core components for the public preview of Open Match 2. It includes the main Go application, Protocol Buffer (protobuf) definitions, and generated Go client libraries. Here's a high-level overview of the key directories:

* `main.go` (also called `om-core` in places where it may not be obvious that it is part of Open Match) is the core Open Match 2 application. It serves all the Open Match 2 API endpoints from a single horizontally-scalable container image.

* `proto` contains [Protocol Buffer](https://protobuf.dev/) definitions used to produce libraries in your language of choice. Open Match 2 is written in golang but you can write your matchmaker in any language supported by gRPC.

* `pkg` contains two external golang modules:

  * `pkg/pb` contains the compiled golang modules produced by `protoc`.

  * `pkg/api` contains the [`grpc-gateway`](https://github.com/grpc-ecosystem/grpc-gateway) reverse-proxy server which translates RESTful HTTP API calls into gRPC.

## What is a Matchmaker?

In the context of Open Match, the term 'matchmaker' refers to the collection of services and logic responsible for the entire matchmaking process. While Open Match provides the core framework, you are responsible for building the matchmaker itself. Minimally, a matchmaker using Open Match is expected to:

1. Queue player matchmaking requests.
1. Request matches from Open Match and distribute them to game servers.
1. Notify players of their match assignments.
1. Provide the custom matchmaking logic (as a Matchmaking Function, or MMF).

These components can be a single, monolithic service, or have their duties federated across a number of platform services.  For an example of one way to build a matchmaker using Open Match 2, look at the [open-match-ecosystem repository v2/ directory](https://github.com/googleforgames/open-match-ecosystem/tree/main/v2).

## Documentation

We have started to build out documentation for Open Match 2. Here are some of the resources we have created so far:

* [**MMF Sample README**](https://github.com/googleforgames/open-match-ecosystem/tree/main/v2/examples/mmf/README.md): This document provides a detailed guide to getting started with the Matchmaking Function (MMF) sample. It covers best practices, common misconceptions, and explains how to get started with creating your own custom matching logic in Go. It also explains that Open Match 2 is language-agnostic, and that you can write your MMFs in any language that supports gRPC.

## Contributing to Open Match

* We aim to keep this application scope as small as possible.
* Before creating a PR, we request that you open an issue or discussion to talk about your proposed changes.
* If a proposed change does not have a clear benefit to the majority of users, it will probably not be accepted.

[The Development guide](docs/DEVELOPMENT.md) contains instructions on getting the source code, making changes, testing and submitting a pull request to Open Match.

## Support

* [Slack Channel](https://open-match.slack.com/) ([Signup](https://join.slack.com/t/open-match/shared_invite/zt-5k57lph3-Oe0WdatzL32xv6tPG3PfzQ))

## Code of Conduct

Participation in this project comes under the [Contributor Covenant Code of Conduct](code-of-conduct.md)

## License

Apache 2.0
