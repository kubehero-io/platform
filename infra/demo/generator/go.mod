module github.com/kubehero-io/platform/infra/demo/generator

go 1.26.2

toolchain go1.26.8

require (
	connectrpc.com/connect v1.19.2
	github.com/kubehero-io/platform/packages/proto v0.0.0-00010101000000-000000000000
)

require google.golang.org/protobuf v1.36.11 // indirect

replace github.com/kubehero-io/platform/packages/proto => ../../../packages/proto
