module github.com/kubehero-io/platform/services/control-plane

go 1.26.2

toolchain go1.26.8

require (
	connectrpc.com/connect v1.20.0
	github.com/ClickHouse/clickhouse-go/v2 v2.45.0
	github.com/go-faster/city v1.0.1
	github.com/golang-migrate/migrate/v4 v4.19.1
	github.com/google/pprof v0.0.0-20250403155104-27863c87afa6
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/klauspost/compress v1.20.1
	github.com/kubehero-io/platform/packages/proto v0.0.0-20260526182454-514b669b0f0f
	github.com/spf13/cobra v1.10.2
	go.opentelemetry.io/proto/otlp v1.10.0
	golang.org/x/net v0.59.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/ClickHouse/ch-go v0.71.0 // indirect
	github.com/andybalholm/brotli v1.2.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/lib/pq v1.10.9 // indirect
	github.com/paulmach/orb v0.12.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.25 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/kubehero-io/platform/packages/proto => ../../packages/proto
