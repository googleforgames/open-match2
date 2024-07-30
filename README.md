![Open Match](https://github.com/googleforgames/open-match-docs/blob/master/site/static/images/logo-with-name.png)

Open Match is an open source game matchmaking framework that simplifies building
a scalable and extensible Matchmaker. It is designed to give the game developer
full control over how to make matches while removing the burden of dealing with
the challenges of running a production service at scale.

This repository is the public preview of the proposed second version of the "Core" Open Match golang application. The repository also contains protobuffer definition files (in `/proto`), and external-facing golang modules generated from those files (in `/pkg`). The subdirectories themselves contain further documentation, but at a high level:
* `main.go` (also called `om-core` in places where it may not be obvious that it is part of Open Match) is the core Open Match 2 application. It serves all the Open Match 2 API endpoints from a single horizontally-scalable container image.
* `proto` contains [Protocol Buffer](https://protobuf.dev/) definitions used to produce libraries in your language of choice. Open Match 2 is written in golang but you can write your matchmaker in any language supported by gRPC.
* `pkg` contains two external golang modules:
  * `pkg/pb` contains the compiled golang modules produced by `protoc`.
  * `pkg/api` contains the [`grpc-gateway`](https://github.com/grpc-ecosystem/grpc-gateway) reverse-proxy server which translates RESTful HTTP API calls into gRPC.

Documentation will be added to the project over time.

## Contributing to Open Match

* We aim to keep this application scope as small as possible.
* Please don't send a PR without creating a discussion/issue about it first.
* If proposed change does not have a clear benefit to the majority of users, it will probably not be accepted.

The Development guide contains instructions on getting the source code, making changes, testing and submitting a pull request to Open Match.

## Support

* [Slack Channel](https://open-match.slack.com/) ([Signup](https://join.slack.com/t/open-match/shared_invite/zt-5k57lph3-Oe0WdatzL32xv6tPG3PfzQ))

## Code of Conduct

Participation in this project comes under the [Contributor Covenant Code of Conduct](code-of-conduct.md)

## License

Apache 2.0
