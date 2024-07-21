** API definition files.

Open Match uses gRPC and Protocol Buffers as its API definition language, for legacy reasons.
These files are compiled using the protocol buffers compiler with the following plugins:
 - google.golang.org/protobuf/cmd/protoc-gen-go@latest
 - google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
 - github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
 - github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@latest
to generate an OpenAPI v2 (aka Swagger) reverse-proxy which translates RESTful HTTP into gRPC. The resulting output files are committed to the github repository.

files, and what they are:
 - `Dockerfile`: container image definition file.
 - `cloudbuild.yaml`: Google Cloud Build job definition file.
 - `api.proto`: Definition of the Open Match core API calls, and their input/output formats.
 - `api.yaml`: gRPC-gateway external configuration file [https://grpc-ecosystem.github.io/grpc-gateway/docs/mapping/grpc_api_configuration/]
 - `api.swagger.yaml`: gRPC-gateway OpenAPIv2 external configuration file (For an example of an OpenAPI configuration file, see [`unannotated_echo_service.swagger.yaml`](https://github.com/grpc-ecosystem/grpc-gateway/tree/main/examples/internal/proto/examplepb/unannotated_echo_service.swagger.yaml), which adds OpenAPI options to [`unannotated_echo_service.proto`](https://github.com/grpc-ecosystem/grpc-gateway/tree/main/examples/internal/proto/examplepb/unannotated_echo_service.proto).)
 - `messages.proto`: Definition of data types used by the apis. Required by `protoc` when compiling either the `api.proto` or the `mmf.proto` file to your language of choice, as both of those files `include messages.proto`.
 - `mmf.proto`: Definition of the matchmaking function service you need to implement to build a matchmaking function for Open Match. If you're planning to use `protoc` to make mmf bindings for your language of choice, you'll need this file, and the `messages.proto` file. 

** Development 

For nearly all cases, you'll just be using [`protoc`] to compile `mmf.proto` (you'll also need to make sure `messages.proto` is in the same directory, as `mmf.proto` imports it) in whatever language you wish to write matchmaking functions in. This is a fairly simple process but we recommend you read the official gRPC documentation if you're not already familiar with the project.

The `api.proto` file can be helpful when developing against Open Match, as it provides a reference for the API call request/response arguments, but should be considered **only** that - reference. You should not find yourself modifying these files; doing so requires completely re-compiling Open Match core and maintaining your own fork of the repository! If you think you need to make changes here to accomplish some matchmaking functionality required for your game, you should verify your idea with the Open Match community first; chances are very good that unmodified Open Match can do what you want without you having to maintain a fork.

** Compiling

Generally speaking, don't recompile `api.proto`. If you're writing an MMF, read the Development section.

If you are a core developer and need to make a change to the API, `Dockerfile` and `cloudbuild.yaml` are provided for your use. The `Dockerfile` has an `ARG` line near the top where you can specify the version of `protoc` the build will use. Near the bottom of the `cloudbuild.yaml` file you'll find the `artifacts` section, with a list of output files from `protoc` and Google Cloud Storage URIs where you want them to go [https://cloud.google.com/build/docs/building/store-artifacts-in-cloud-storage]. Make a GCS bucket, update the `cloudbuild.yaml` file to point to your bucket, and run a Cloud Build (with GCS write permissions). The output files will be uploaded when the build finishes. Download the files, move them to their appropriate directories in the Open Match repo, and commit your latest changes.
