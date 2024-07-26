module open-match.dev/open-match2/pkg/api/v2

go 1.21.5

require (
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.20.0
	google.golang.org/grpc v1.65.0
	google.golang.org/protobuf v1.34.2
	open-match.dev/open-match2/pkg/pb/v2 v2.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.27.0 // indirect
	golang.org/x/sys v0.22.0 // indirect
	golang.org/x/text v0.16.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240711142825-46eb208f015d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240711142825-46eb208f015d // indirect
)

replace open-match.dev/open-match2/pkg/pb/v2 => ../../pb/v2
