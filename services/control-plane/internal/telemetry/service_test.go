// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package telemetry

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
)

// fakeWriter records rows instead of writing them.
type fakeWriter struct {
	mu      sync.Mutex
	logs    []clickhouse.LogRow
	samples []clickhouse.ProfileSampleRow
	stacks  []clickhouse.ProfileStackRow
	flows   []clickhouse.FlowRow
	usage   []clickhouse.UsageRow
	events  []clickhouse.EventRow
	failStk bool
}

func (f *fakeWriter) WriteLogs(_ context.Context, r []clickhouse.LogRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, r...)
	return nil
}
func (f *fakeWriter) WriteProfileSamples(_ context.Context, r []clickhouse.ProfileSampleRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, r...)
	return nil
}
func (f *fakeWriter) WriteProfileStacks(_ context.Context, r []clickhouse.ProfileStackRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStk {
		return errors.New("stacks down")
	}
	f.stacks = append(f.stacks, r...)
	return nil
}
func (f *fakeWriter) WriteFlows(_ context.Context, r []clickhouse.FlowRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flows = append(f.flows, r...)
	return nil
}
func (f *fakeWriter) WriteUsage(_ context.Context, r []clickhouse.UsageRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, r...)
	return nil
}
func (f *fakeWriter) WriteEvents(_ context.Context, r []clickhouse.EventRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, r...)
	return nil
}

var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func newTestService(t *testing.T, w Writer) *Service {
	t.Helper()
	s := New(Options{Writer: w, Now: func() time.Time { return testNow }, FlushInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.Run(ctx)
	return s
}

func memberCtx() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Sub: "collector", Role: auth.RoleMember})
}

func clusterCtx(id string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Sub: "cluster:" + id, Role: auth.RoleMember, ClusterID: id})
}

func code(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func TestIngestLogsValidation(t *testing.T) {
	w := &fakeWriter{}
	s := newTestService(t, w)
	long := strings.Repeat("x", logschema.MaxLineBytes+10)
	req := &kuberov1.IngestLogsRequest{ClusterId: "c1", Entries: []*kuberov1.LogEntry{
		{TsUnixNano: testNow.Add(-time.Minute).UnixNano(), Level: "WARNING", Body: "ok",
			Source: &kuberov1.PodRef{Namespace: "shop", Pod: "api-1", Container: "app", Workload: "api"},
			Labels: map[string]string{"app.kubernetes.io/name": "api", "namespace": "evil", "__error__": "x", "empty": ""}},
		{TsUnixNano: 0, Body: long},                                     // stamped now, truncated
		{TsUnixNano: testNow.Add(time.Hour).UnixNano(), Body: "future"}, // > 10 min ahead
		{TsUnixNano: testNow.Add(-15 * 24 * time.Hour).UnixNano(), Body: "expired"},
		{TsUnixNano: testNow.UnixNano(), Body: "bad\xffutf8\x00", TraceId: "not a trace id!"},
	}}
	res, err := s.IngestLogs(memberCtx(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetAccepted() != 3 || res.Msg.GetDropped() != 2 {
		t.Fatalf("accepted/dropped = %d/%d, want 3/2", res.Msg.GetAccepted(), res.Msg.GetDropped())
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.logs) != 3 {
		t.Fatalf("stored %d rows", len(w.logs))
	}
	first := w.logs[0]
	if first.Level != "warn" || first.Namespace != "shop" || first.ClusterID != "c1" || first.OrgID != "default" {
		t.Fatalf("row = %+v", first)
	}
	if first.Labels["app_kubernetes_io_name"] != "api" || len(first.Labels) != 1 {
		t.Fatalf("labels = %v (want only the sanitised app label; column names, __ names and empties dropped)", first.Labels)
	}
	second := w.logs[1]
	if !second.TS.Equal(testNow) || len(second.Body) != logschema.MaxLineBytes || second.Labels[logschema.TruncatedLabel] != "true" {
		t.Fatalf("truncated row: ts=%v len=%d labels=%v", second.TS, len(second.Body), second.Labels)
	}
	third := w.logs[2]
	if third.Body != "bad�utf8" || third.TraceID != "" {
		t.Fatalf("cleaned row: body=%q trace=%q", third.Body, third.TraceID)
	}
}

func TestIngestLogsAuthLimitsAndClusters(t *testing.T) {
	s := newTestService(t, &fakeWriter{})
	one := []*kuberov1.LogEntry{{Body: "x"}}

	if _, err := s.IngestLogs(context.Background(), connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "c1", Entries: one})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("anonymous: %v", err)
	}
	many := make([]*kuberov1.LogEntry, DefaultLimits.Logs+1)
	if _, err := s.IngestLogs(memberCtx(), connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "c1", Entries: many})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("over limit: %v", err)
	}
	// Enrollment-token callers default to their own cluster …
	res, err := s.IngestLogs(clusterCtx("uuid-1"), connect.NewRequest(&kuberov1.IngestLogsRequest{Entries: one}))
	if err != nil || res.Msg.GetAccepted() != 1 {
		t.Fatalf("scoped default: %v %v", res, err)
	}
	// … and cannot write for another one.
	if _, err := s.IngestLogs(clusterCtx("uuid-1"), connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "uuid-2", Entries: one})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-cluster write: %v", err)
	}
	// No cluster anywhere: rows are dropped, not attributed to "".
	res, err = s.IngestLogs(memberCtx(), connect.NewRequest(&kuberov1.IngestLogsRequest{Entries: one}))
	if err != nil || res.Msg.GetAccepted() != 0 || res.Msg.GetDropped() != 1 {
		t.Fatalf("no cluster: %v %v", res, err)
	}
	if _, err := s.IngestLogs(memberCtx(), connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "bad cluster!", Entries: one})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid cluster id: %v", err)
	}
}

func TestNoClickHouseAcceptsAndDrops(t *testing.T) {
	s := New(Options{}) // no writer
	ctx := memberCtx()
	lr, err := s.IngestLogs(ctx, connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "c1", Entries: []*kuberov1.LogEntry{{Body: "a"}, {Body: "b"}}}))
	if err != nil || lr.Msg.GetDropped() != 2 || lr.Msg.GetAccepted() != 0 {
		t.Fatalf("logs: %v %v", lr, err)
	}
	fr, err := s.IngestFlows(ctx, connect.NewRequest(&kuberov1.IngestFlowsRequest{ClusterId: "c1", Flows: []*kuberov1.Flow{{Bytes: 1}}}))
	if err != nil || fr.Msg.GetDropped() != 1 {
		t.Fatalf("flows: %v %v", fr, err)
	}
	pr, err := s.IngestProfiles(ctx, connect.NewRequest(&kuberov1.IngestProfilesRequest{ClusterId: "c1", Profiles: []*kuberov1.Profile{
		{Samples: []*kuberov1.StackSample{{Frames: []string{"a"}, Value: 1}, {Frames: []string{"b"}, Value: 1}}},
	}}))
	if err != nil || pr.Msg.GetDropped() != 2 || pr.Msg.GetAccepted() != 0 {
		t.Fatalf("profiles: %v %v", pr, err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestQueueFullIsResourceExhausted(t *testing.T) {
	s := New(Options{Writer: &fakeWriter{}, LogQueueBytes: 1, Now: func() time.Time { return testNow }})
	// Not running: nothing drains, and one row exceeds a 1-byte budget.
	_, err := s.IngestLogs(memberCtx(), connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "c1", Entries: []*kuberov1.LogEntry{{Body: "x"}}}))
	if code(err) != connect.CodeResourceExhausted {
		t.Fatalf("err = %v, want ResourceExhausted", err)
	}
}

func TestStackHashMatchesClickHouseConvention(t *testing.T) {
	// Values checked against ClickHouse 26.8:
	//   SELECT cityHash64('a' || char(0) || 'b'), cityHash64('main')
	if got := StackHash([]string{"a", "b"}); got != 174674430866144721 {
		t.Fatalf("StackHash(a,b) = %d", got)
	}
	if got := StackHash([]string{"main"}); got != 15854150226879163722 {
		t.Fatalf("StackHash(main) = %d", got)
	}
	// NUL separation keeps frame boundaries significant.
	if StackHash([]string{"a;b"}) == StackHash([]string{"a", "b"}) || StackHash([]string{"ab"}) == StackHash([]string{"a", "b"}) {
		t.Fatal("frame boundaries must change the hash")
	}
}

func TestIngestProfiles(t *testing.T) {
	w := &fakeWriter{}
	s := newTestService(t, w)
	deep := make([]string, maxStackDepth+50)
	for i := range deep {
		deep[i] = "f"
	}
	deep[len(deep)-1] = "leaf"
	prof := &kuberov1.Profile{
		TsUnixNano: testNow.Add(-time.Minute).UnixNano(), DurationNano: 15e9, Type: "CPU", Origin: "ebpf",
		Source: &kuberov1.PodRef{Namespace: "shop", Workload: "api", Pod: "api-1"},
		Samples: []*kuberov1.StackSample{
			{Frames: []string{"main", "handler"}, Value: 10},
			{Frames: []string{"main", "handler"}, Value: 5}, // merged
			{Frames: []string{"main", "gc"}, Value: 0},      // dropped
			{Frames: nil, Value: 3},                         // dropped
			{Frames: deep, Value: 1},
		},
	}
	res, err := s.IngestProfiles(memberCtx(), connect.NewRequest(&kuberov1.IngestProfilesRequest{ClusterId: "c1", Profiles: []*kuberov1.Profile{prof,
		{Type: "not a type!", Samples: []*kuberov1.StackSample{{Frames: []string{"x"}, Value: 1}}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetAccepted() != 1 || res.Msg.GetSamples() != 2 || res.Msg.GetDropped() != 3 {
		t.Fatalf("accepted/samples/dropped = %d/%d/%d, want 1/2/3", res.Msg.GetAccepted(), res.Msg.GetSamples(), res.Msg.GetDropped())
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.samples) != 2 || w.samples[0].Value != 15 || w.samples[0].Service != "api" || w.samples[0].Type != "cpu" || w.samples[0].Unit != "nanoseconds" {
		t.Fatalf("samples = %+v", w.samples)
	}
	if !w.samples[0].TS.Equal(testNow.Add(-time.Minute)) {
		t.Fatalf("sample ts = %v", w.samples[0].TS)
	}
	var deepRow clickhouse.ProfileStackRow
	for _, st := range w.stacks {
		if len(st.Frames) > 2 {
			deepRow = st
		}
	}
	if len(deepRow.Frames) != maxStackDepth || deepRow.Frames[0] != truncatedRootName || deepRow.Frames[len(deepRow.Frames)-1] != "leaf" {
		t.Fatalf("deep stack: len=%d root=%q leaf=%q", len(deepRow.Frames), deepRow.Frames[0], deepRow.Frames[len(deepRow.Frames)-1])
	}
	if deepRow.StackHash != StackHash(deepRow.Frames) {
		t.Fatal("stack hash must be computed over the stored frames")
	}

	// Same stacks again: samples are written, stacks are not.
	before := len(w.stacks)
	if _, err := s.IngestProfiles(memberCtx(), connect.NewRequest(&kuberov1.IngestProfilesRequest{ClusterId: "c1", Profiles: []*kuberov1.Profile{prof}})); err != nil {
		t.Fatal(err)
	}
	_ = s.Flush(context.Background())
	if len(w.stacks) != before || len(w.samples) != 4 {
		t.Fatalf("second ingest: stacks %d→%d samples %d", before, len(w.stacks), len(w.samples))
	}
}

func TestFailedStacksAreRewritten(t *testing.T) {
	w := &fakeWriter{failStk: true}
	s := New(Options{Writer: w, Now: func() time.Time { return testNow }, FlushInterval: time.Hour})
	prof := &kuberov1.Profile{Samples: []*kuberov1.StackSample{{Frames: []string{"main"}, Value: 1}}}
	req := connect.NewRequest(&kuberov1.IngestProfilesRequest{ClusterId: "c1", Profiles: []*kuberov1.Profile{prof}})
	if _, err := s.IngestProfiles(memberCtx(), req); err != nil {
		t.Fatal(err)
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Flush(flushCtx) // stack write fails (after retries)
	w.mu.Lock()
	w.failStk = false
	w.mu.Unlock()
	if _, err := s.IngestProfiles(memberCtx(), req); err != nil {
		t.Fatal(err)
	}
	_ = s.Flush(context.Background())
	if len(w.stacks) != 1 {
		t.Fatalf("stack rewritten %d times after a failed write, want 1", len(w.stacks))
	}
}

func TestStackCache(t *testing.T) {
	c := NewStackCache(4, time.Hour)
	if !c.ShouldWrite(1, testNow) || c.ShouldWrite(1, testNow.Add(time.Minute)) {
		t.Fatal("second sighting within refresh must be skipped")
	}
	if !c.ShouldWrite(1, testNow.Add(2*time.Hour)) {
		t.Fatal("stale entry must be rewritten (bumps last_seen)")
	}
	for h := uint64(2); h < 100; h++ {
		c.ShouldWrite(h, testNow)
	}
	if c.Len() > 4 {
		t.Fatalf("cache holds %d entries, bound is 4", c.Len())
	}
	c.Forget([]uint64{99})
	if !c.ShouldWrite(99, testNow) {
		t.Fatal("forgotten hash must be written again")
	}
}

func flow(src, dst *kuberov1.FlowEndpoint, dir string, bytes uint64) *kuberov1.Flow {
	return &kuberov1.Flow{TsUnixMs: testNow.UnixMilli(), WindowSec: 15, Src: src, Dst: dst, Port: 443, Protocol: "TCP", Bytes: bytes, Packets: 1, Direction: dir}
}

func TestFlowRowPricing(t *testing.T) {
	c := clock{now: testNow}
	pod := func(zone string) *kuberov1.FlowEndpoint {
		return &kuberov1.FlowEndpoint{Kind: "pod", Zone: zone, Ip: "10.0.0.1", Pod: &kuberov1.PodRef{Namespace: "shop", Workload: "api", Pod: "api-1"}}
	}
	ext := &kuberov1.FlowEndpoint{Kind: "external", Ip: "52.1.2.3"}
	tests := []struct {
		name      string
		f         *kuberov1.Flow
		cloud     string
		pricing   NetPricing
		ok        bool
		egress    bool
		crossZone bool
		cost      float64
		dir       string
	}{
		{"internet egress aws", flow(pod("a"), ext, "egress", 2e9), "aws", DefaultNetPricing(), true, true, false, 0.18, "egress"},
		{"internet egress gcp", flow(pod("a"), ext, "egress", 1e9), "gcp", DefaultNetPricing(), true, true, false, 0.12, "egress"},
		{"internet egress azure", flow(pod("a"), ext, "egress", 1e9), "azure", DefaultNetPricing(), true, true, false, 0.087, "egress"},
		{"egress direction inferred", flow(pod("a"), ext, "", 1e9), "", DefaultNetPricing(), true, true, false, 0.09, "egress"},
		{"inbound from internet is free", flow(ext, pod("a"), "", 1e9), "aws", DefaultNetPricing(), true, false, false, 0, "ingress"},
		{"cross zone aws", flow(pod("a"), pod("b"), "ingress", 5e9), "aws", DefaultNetPricing(), true, false, true, 0.05, "ingress"},
		{"cross zone azure free", flow(pod("a"), pod("b"), "ingress", 5e9), "azure", DefaultNetPricing(), true, false, true, 0, "ingress"},
		{"same zone", flow(pod("a"), pod("a"), "egress", 5e9), "aws", DefaultNetPricing(), true, false, false, 0, "egress"},
		{"unknown zone is not cross-zone", flow(pod(""), pod("b"), "egress", 5e9), "aws", DefaultNetPricing(), true, false, false, 0, "egress"},
		{"override", flow(pod("a"), ext, "egress", 1e9), "gcp", NetPricing{EgressOverride: 0.05, CrossZoneOverride: -1}, true, true, false, 0.05, "egress"},
		{"bad kind", flow(&kuberov1.FlowEndpoint{Kind: "satellite"}, ext, "egress", 1), "aws", DefaultNetPricing(), false, false, false, 0, ""},
		{"bad port", &kuberov1.Flow{Src: pod("a"), Dst: ext, Port: 70000, Bytes: 1}, "aws", DefaultNetPricing(), false, false, false, 0, ""},
		{"empty flow", flow(pod("a"), ext, "egress", 0), "aws", DefaultNetPricing(), false, false, false, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "empty flow" {
				tc.f.Packets = 0
			}
			row, ok := c.FlowRow("c1", tc.cloud, tc.pricing, tc.f)
			if ok != tc.ok {
				t.Fatalf("ok = %v", ok)
			}
			if !ok {
				return
			}
			if row.Egress != tc.egress || row.CrossZone != tc.crossZone || math.Abs(row.CostUSD-tc.cost) > 1e-12 || row.Direction != tc.dir {
				t.Fatalf("row egress=%v cross=%v cost=%v dir=%q", row.Egress, row.CrossZone, row.CostUSD, row.Direction)
			}
			if row.Protocol != "tcp" || row.SrcName == "" {
				t.Fatalf("protocol=%q src_name=%q", row.Protocol, row.SrcName)
			}
		})
	}
}

func TestCloudFromZone(t *testing.T) {
	tests := map[string]string{
		"us-east-1a": "aws", "us-gov-west-1b": "aws", "eu-central-1c": "aws", "us-east-1-bos-1a": "aws", "use1-az1": "aws",
		"us-central1-a": "gcp", "europe-west4-b": "gcp", "northamerica-northeast1-c": "gcp",
		"eastus-1": "azure", "westeurope-3": "azure", "eastus2-2": "azure",
		"": "", "rack-7": "", "zone1": "", "zone-1": "",
	}
	for zone, want := range tests {
		if got := CloudFromZone(zone); got != want {
			t.Errorf("CloudFromZone(%q) = %q, want %q", zone, got, want)
		}
	}
}

func TestCloudResolverPrefersReportedCloud(t *testing.T) {
	calls := 0
	r := &CloudResolver{Lookup: func(context.Context, string) (string, error) { calls++; return "GCP", nil }}
	if got := r.Cloud(context.Background(), "c1", "us-east-1a"); got != "gcp" {
		t.Fatalf("cloud = %q, want the reported gcp over the zone guess", got)
	}
	_ = r.Cloud(context.Background(), "c1")
	if calls != 1 {
		t.Fatalf("lookups = %d, want 1 (cached)", calls)
	}
	none := &CloudResolver{Lookup: func(context.Context, string) (string, error) { return "", nil }}
	if got := none.Cloud(context.Background(), "c2", "us-central1-a"); got != "gcp" {
		t.Fatalf("zone fallback = %q", got)
	}
	var nilResolver *CloudResolver
	if got := nilResolver.Cloud(context.Background(), "c3", "eastus-1"); got != "azure" {
		t.Fatalf("nil resolver zone fallback = %q", got)
	}
}

func TestPricingFromEnv(t *testing.T) {
	env := map[string]string{"KUBEHERO_NET_EGRESS_USD_PER_GB": "0.05", "KUBEHERO_NET_CROSS_ZONE_USD_PER_GB": "nope"}
	p := PricingFromEnv(func(k string) string { return env[k] }, nil)
	e, x := p.Rates("azure")
	if e != 0.05 || x != 0 {
		t.Fatalf("rates = %v, %v (override egress, keep azure's free cross-zone)", e, x)
	}
}

func TestUsageAndEventRows(t *testing.T) {
	c := clock{now: testNow}
	ref := &kuberov1.PodRef{Namespace: "shop", Pod: "api-1", Container: "app", Workload: "api"}
	if _, ok := c.UsageRow("c1", &kuberov1.ContainerUsage{Source: &kuberov1.PodRef{Namespace: "shop", Pod: "p"}, CpuUsageCores: 1}); ok {
		t.Fatal("usage without container must be dropped")
	}
	if _, ok := c.UsageRow("c1", &kuberov1.ContainerUsage{Source: ref, CpuUsageCores: math.NaN()}); ok {
		t.Fatal("NaN usage must be dropped")
	}
	u, ok := c.UsageRow("c1", &kuberov1.ContainerUsage{Source: ref, CpuUsageCores: 0.5, CpuRequestCores: -1, MemWorkingSetBytes: 42, LastTerminationReason: "OOMKilled"})
	if !ok || u.CPUUsageCores != 0.5 || u.CPURequestCores != 0 || u.MemWorkingSetBytes != 42 || u.LastTerminationReason != "OOMKilled" {
		t.Fatalf("usage row = %+v %v", u, ok)
	}

	e, ok := c.EventRow("c1", &kuberov1.ClusterEvent{Kind: "OOM_KILLED", Source: ref, Count: 0, Attributes: map[string]string{"cpu_millicores": "500"}})
	if !ok || e.Kind != "oom_killed" || e.Severity != "warn" || e.Count != 1 || e.Attributes["cpu_millicores"] != "500" {
		t.Fatalf("event row = %+v", e)
	}
	e, _ = c.EventRow("c1", &kuberov1.ClusterEvent{Kind: "meteor_strike", Severity: "error"})
	if e.Kind != "warning" || e.Severity != "critical" || e.Attributes["original_kind"] != "meteor_strike" {
		t.Fatalf("unknown kind row = %+v", e)
	}
	e, _ = c.EventRow("c1", &kuberov1.ClusterEvent{Kind: "node_not_ready"})
	if e.Severity != "critical" {
		t.Fatalf("default severity = %q", e.Severity)
	}
}

func TestIngestUsageFlowsEventsRoundTrip(t *testing.T) {
	w := &fakeWriter{}
	s := newTestService(t, w)
	ctx := memberCtx()
	ref := &kuberov1.PodRef{Namespace: "shop", Pod: "api-1", Container: "app"}
	ur, err := s.IngestUsage(ctx, connect.NewRequest(&kuberov1.IngestUsageRequest{ClusterId: "c1", Usage: []*kuberov1.ContainerUsage{{Source: ref, CpuUsageCores: 1}, {}}}))
	if err != nil || ur.Msg.GetAccepted() != 1 || ur.Msg.GetDropped() != 1 {
		t.Fatalf("usage: %v %v", ur, err)
	}
	er, err := s.IngestEvents(ctx, connect.NewRequest(&kuberov1.IngestEventsRequest{ClusterId: "c1", Events: []*kuberov1.ClusterEvent{{Kind: "evicted"}, {TsUnixMs: testNow.Add(time.Hour).UnixMilli()}}}))
	if err != nil || er.Msg.GetAccepted() != 1 || er.Msg.GetDropped() != 1 {
		t.Fatalf("events: %v %v", er, err)
	}
	fr, err := s.IngestFlows(ctx, connect.NewRequest(&kuberov1.IngestFlowsRequest{ClusterId: "c1", Flows: []*kuberov1.Flow{
		flow(&kuberov1.FlowEndpoint{Kind: "pod", Zone: "us-central1-a"}, &kuberov1.FlowEndpoint{Kind: "external"}, "egress", 1e9),
	}}))
	if err != nil || fr.Msg.GetAccepted() != 1 {
		t.Fatalf("flows: %v %v", fr, err)
	}
	_ = s.Flush(context.Background())
	if len(w.usage) != 1 || len(w.events) != 1 || len(w.flows) != 1 {
		t.Fatalf("stored usage=%d events=%d flows=%d", len(w.usage), len(w.events), len(w.flows))
	}
	// No resolver configured: the gcp zone name prices the egress.
	if math.Abs(w.flows[0].CostUSD-0.12) > 1e-12 {
		t.Fatalf("flow cost = %v, want gcp's 0.12", w.flows[0].CostUSD)
	}
}
