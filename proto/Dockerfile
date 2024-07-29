# TODO: clean this up a bit
FROM debian as downloader

ARG PROTOC_VER=27.2

RUN apt-get update && apt-get install -y curl unzip

WORKDIR /workspace
RUN curl -LO https://github.com/protocolbuffers/protobuf/releases/download/v$PROTOC_VER/protoc-$PROTOC_VER-linux-x86_64.zip
RUN unzip protoc-$PROTOC_VER-linux-x86_64.zip -d protoc 
#RUN curl -LO https://raw.githubusercontent.com/googleapis/googleapis/master/google/rpc/status.proto

# grpc-gateway dependencies 
#RUN curl -LO https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/annotations.proto
#RUN curl -LO https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/http.proto
#RUN curl -LO https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/httpbody.proto
#RUN curl -LO https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/field_behavior.proto
RUN ls -lR

FROM golang

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
RUN go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
RUN go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
RUN go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@latest

WORKDIR /workspace

COPY --from=downloader /workspace/protoc/           /workspace/protoc/
#COPY --from=downloader /workspace/status.proto      /workspace/proto/google/rpc/
#COPY --from=downloader /workspace/annotations.proto    /workspace/proto/google/api/
#COPY --from=downloader /workspace/http.proto           /workspace/proto/google/api/
#COPY --from=downloader /workspace/httpbody.proto       /workspace/proto/google/api/
#COPY --from=downloader /workspace/field_behavior.proto /workspace/proto/google/api/
COPY . .

RUN protoc/bin/protoc \
        --go_out .              --go_opt            paths=source_relative \
        --go-grpc_out .         --go-grpc_opt       paths=source_relative \
        --grpc-gateway_out .    --grpc-gateway_opt  paths=source_relative \ 
                                --grpc-gateway_opt  standalone=true \ 
                                --grpc-gateway_opt  allow_delete_body=true \ 
                                --grpc-gateway_opt  grpc_api_configuration=api.yaml \ 
        --openapiv2_out .       --openapiv2_opt     allow_merge=true \
                                --openapiv2_opt     merge_file_name=api \
                                --openapiv2_opt     grpc_api_configuration=api.yaml \
                                --openapiv2_opt     allow_delete_body=true \ 
                                --openapiv2_opt     openapi_configuration=api.swagger.yaml \ 
        ./*.proto 
RUN rm -rf protoc 
RUN ls -l
ENTRYPOINT go version 
