// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package telemetry

import (
	"context"
	"math"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
)

func TestTelemetryIngestIntegration(t *testing.T) {
	db := chtest.Open(t, "kh_ingest_tel")
	ctx := context.Background()
	now := time.Now().UTC()
	s := New(Options{
		Writer: &clickhouse.SignalWriter{Conn: db.Native},
		Clouds: &CloudResolver{Lookup: ClickHouseCloudLookup(db.SQL)},
	})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.Run(runCtx)

	// The cluster reports gcp through its node cost rows; flows must be
	// priced with gcp's egress rate even though the zone looks like AWS.
	pw := &clickhouse.PodCostWriter{DB: db.SQL, Linger: 10 * time.Millisecond}
	if _, err := pw.Write(ctx, nil, []clickhouse.NodeSample{{OrgID: "default", ClusterID: "c1", Node: "n1", Cloud: "gcp", PricePerHour: 1, CostUSDSec: 1.0 / 3600}}); err != nil {
		t.Fatal(err)
	}

	mctx := memberCtx()
	src := &kuberov1.PodRef{Namespace: "shop", Pod: "api-1", Container: "app", Workload: "api", WorkloadKind: "Deployment", Node: "n1", Team: "payments"}
	lr, err := s.IngestLogs(mctx, connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "c1", Entries: []*kuberov1.LogEntry{
		{TsUnixNano: now.UnixNano(), Source: src, Stream: "stdout", Level: "error", Body: `{"msg":"boom"}`, Labels: map[string]string{"app": "api"}, TraceId: "4bf92f3577b34da6a3ce929d0e0e4736"},
		{TsUnixNano: now.UnixNano(), Source: src, Level: "info", Body: "fine"},
	}}))
	if err != nil || lr.Msg.GetAccepted() != 2 {
		t.Fatalf("logs: %v %v", lr, err)
	}

	frames := []string{"main.main", "net/http.(*conn).serve", "encoding/json.Marshal"}
	pr, err := s.IngestProfiles(mctx, connect.NewRequest(&kuberov1.IngestProfilesRequest{ClusterId: "c1", Profiles: []*kuberov1.Profile{{
		TsUnixNano: now.UnixNano(), DurationNano: 15e9, Type: "cpu", Origin: "ebpf", Source: src,
		Samples: []*kuberov1.StackSample{{Frames: frames, Value: 3e7}, {Frames: frames[:2], Value: 1e7}},
	}}}))
	if err != nil || pr.Msg.GetSamples() != 2 {
		t.Fatalf("profiles: %v %v", pr, err)
	}

	pod := func(zone string) *kuberov1.FlowEndpoint {
		return &kuberov1.FlowEndpoint{Kind: "pod", Zone: zone, Pod: &kuberov1.PodRef{Namespace: "shop", Workload: "api", Pod: "api-1"}}
	}
	fr, err := s.IngestFlows(mctx, connect.NewRequest(&kuberov1.IngestFlowsRequest{ClusterId: "c1", Flows: []*kuberov1.Flow{
		{TsUnixMs: now.UnixMilli(), WindowSec: 15, Src: pod("us-east-1a"), Dst: &kuberov1.FlowEndpoint{Kind: "external", Ip: "52.1.2.3"}, Port: 443, Protocol: "tcp", Bytes: 1e9, Direction: "egress"},
		{TsUnixMs: now.UnixMilli(), WindowSec: 15, Src: pod("us-east-1a"), Dst: pod("us-east-1b"), Port: 5432, Protocol: "tcp", Bytes: 2e9, Direction: "ingress"},
	}}))
	if err != nil || fr.Msg.GetAccepted() != 2 {
		t.Fatalf("flows: %v %v", fr, err)
	}
	ur, err := s.IngestUsage(mctx, connect.NewRequest(&kuberov1.IngestUsageRequest{ClusterId: "c1", Usage: []*kuberov1.ContainerUsage{
		{TsUnixMs: now.UnixMilli(), Source: src, CpuUsageCores: 0.3, MemWorkingSetBytes: 1 << 28, CpuRequestCores: 1, MemRequestBytes: 1 << 30},
	}}))
	if err != nil || ur.Msg.GetAccepted() != 1 {
		t.Fatalf("usage: %v %v", ur, err)
	}
	er, err := s.IngestEvents(mctx, connect.NewRequest(&kuberov1.IngestEventsRequest{ClusterId: "c1", Events: []*kuberov1.ClusterEvent{
		{TsUnixMs: now.UnixMilli(), Kind: "unschedulable", Source: src, Attributes: map[string]string{"cpu_millicores": "4000", "workload": "api"}},
	}}))
	if err != nil || er.Msg.GetAccepted() != 1 {
		t.Fatalf("events: %v %v", er, err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if n := db.Count(t, `SELECT count() FROM logs WHERE cluster_id = 'c1' AND labels['app'] = 'api' AND trace_id != ''`); n != 1 {
		t.Fatalf("logs with label+trace = %d", n)
	}
	if n := db.Count(t, `SELECT sum(lines) FROM log_volume_1m WHERE cluster_id = 'c1' AND team = 'payments'`); n != 2 {
		t.Fatalf("log_volume_1m lines = %d", n)
	}
	// Go's stack hash is ClickHouse's cityHash64 over NUL-joined frames.
	if n := db.Count(t, `SELECT count() FROM profile_stacks FINAL WHERE stack_hash = cityHash64(arrayStringConcat(frames, char(0)))`); n != 2 {
		t.Fatalf("stacks whose hash matches cityHash64 in SQL = %d, want 2", n)
	}
	if n := db.Count(t, `SELECT count() FROM profile_samples s INNER JOIN profile_stacks st ON s.stack_hash = st.stack_hash WHERE s.service = 'api'`); n != 2 {
		t.Fatalf("samples joined to stacks = %d", n)
	}
	var egressCost, crossCost float64
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT sumIf(cost_usd, egress = 1), sumIf(cost_usd, cross_zone = 1) FROM net_flows_1h WHERE cluster_id = 'c1'`).Scan(&egressCost, &crossCost); err != nil {
		t.Fatal(err)
	}
	if math.Abs(egressCost-0.12) > 1e-9 || math.Abs(crossCost-0.02) > 1e-9 {
		t.Fatalf("flow cost egress=%v cross=%v, want gcp 0.12 and 2GB×0.01", egressCost, crossCost)
	}
	if n := db.Count(t, `SELECT sum(samples) FROM container_usage_5m WHERE cluster_id = 'c1' AND workload = 'api'`); n != 1 {
		t.Fatalf("container_usage_5m = %d", n)
	}
	if n := db.Count(t, `SELECT count() FROM cluster_events WHERE kind = 'unschedulable' AND attributes['cpu_millicores'] = '4000' AND severity = 'warn'`); n != 1 {
		t.Fatalf("cluster_events = %d", n)
	}
}
