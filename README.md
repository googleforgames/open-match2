![Open Match](https://github.com/googleforgames/open-match-docs/blob/master/site/static/images/logo-with-name.png)

Open Match is an open source game matchmaking framework that simplifies building
a scalable and extensible Matchmaker. It is designed to give the game developer
full control over how to make matches while removing the burden of dealing with
the challenges of running a production service at scale.

This is the public preview of the proposed second version of Open Match. Documentation will be added to the project over time.

This repository contains two external golang modules, one golang application, and the protobuffer definition files for Open Match. The directories themselves contain further documentation.

* `core` (also called `om-core` in places where it may not be obvious that it is part of Open Match) is the core Open Match 2 application. It serves all the Open Match 2 API endpoints from a single horizontally-scalable container image.
* `pkg` contains two external golang modules:
** `open-match.dev/open-match2/pkg/pb/v2` contains the compiled golang modules produced by the `protoc` compiler from the [Protocol Buffer](https://protobuf.dev/) definitions in the `proto` directory. You'll import these if you're writing code that interfaces with the `core` gRPC services using golang. 
** `open-match.dev/open-match2/pkg/api/v2` contains the [`grpc-gateway`](https://github.com/grpc-ecosystem/grpc-gateway) reverse-proxy server which translates RESTful HTTP API calls into gRPC. It is generated from the `proto/v2/api.yaml` and `proto/v2/api.swagger.yaml` files.
* `proto` contains the protobuffer message and service definition files, as well as the [gRPC service configuration file](https://cloud.google.com/endpoints/docs/grpc/grpc-service-config) and [OpenAPIv2 definitions configuration file](https://grpc-ecosystem.github.io/grpc-gateway/docs/mapping/customizing_openapi_output/) used by [`grpc-gateway`](https://github.com/grpc-ecosystem/grpc-gateway) to generate a reverse-proxy server which translates RESTful HTTP API calls into gRPC. If you're compiling these yourself to produce libraries in your language of choice, you may find the `Dockerfile` useful as a reference for the `protoc` command line arguments used by `core`.
