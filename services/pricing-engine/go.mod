module github.com/kubehero-io/platform/services/pricing-engine

go 1.26.2

toolchain go1.26.8

require (
	connectrpc.com/connect v1.20.0
	github.com/kubehero-io/platform/packages/proto v0.0.0-20260526182454-514b669b0f0f
	github.com/spf13/cobra v1.10.2
	golang.org/x/net v0.59.0
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/kubehero-io/platform/packages/proto => ../../packages/proto
