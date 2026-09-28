module github.com/kubehero-io/platform/services/advisor

go 1.26.2

toolchain go1.26.8

require (
	connectrpc.com/connect v1.20.0
	github.com/anthropics/anthropic-sdk-go v1.75.0
	github.com/kubehero-io/platform/packages/proto v0.0.0-20260526182454-514b669b0f0f
	github.com/spf13/cobra v1.10.2
	golang.org/x/net v0.50.0
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/text v0.34.0 // indirect
)

replace github.com/kubehero-io/platform/packages/proto => ../../packages/proto
