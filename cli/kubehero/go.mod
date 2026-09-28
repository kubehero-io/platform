module github.com/kubehero-io/platform/cli/kubehero

go 1.26.2

toolchain go1.26.5

require (
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

require (
	connectrpc.com/connect v1.20.0
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kubehero-io/platform/packages/proto v0.0.0-20260526182454-514b669b0f0f
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/spf13/pflag v1.0.9 // indirect
	google.golang.org/protobuf v1.36.11
)

replace github.com/kubehero-io/platform/packages/proto => ../../packages/proto
