![Open Match](https://github.com/googleforgames/open-match-docs/blob/master/site/static/images/logo-with-name.png)

Open Match is an open source game matchmaking framework that simplifies building
a scalable and extensible Matchmaker. It is designed to give the game developer
full control over how to make matches while removing the burden of dealing with
the challenges of running a production service at scale.

This repository is the public preview of the proposed second version of Open Match and contains protobuffer definition files (`proto`), golang application (`core`), and external-facing golang modules (`pkg`). The subdirectories themselves contain further documentation, but at a high level:
* `proto` contains [Protocol Buffer](https://protobuf.dev/) definitions used to produce libraries in your language of choice. Open Match 2 is written in golang but you can write your matchmaker in any language supported by gRPC.
* `core` (also called `om-core` in places where it may not be obvious that it is part of Open Match) is the core Open Match 2 application. It serves all the Open Match 2 API endpoints from a single horizontally-scalable container image.
* `pkg` contains two external golang modules:
  * `open-match.dev/open-match2/pkg/pb/v2` contains the compiled golang modules produced by `protoc`.
  * `open-match.dev/open-match2/pkg/api/v2` contains the [`grpc-gateway`](https://github.com/grpc-ecosystem/grpc-gateway) reverse-proxy server which translates RESTful HTTP API calls into gRPC.
