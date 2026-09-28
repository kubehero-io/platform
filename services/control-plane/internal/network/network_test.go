// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package network

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

func side(bytes, cost float64) Side { return Side{Seen: true, Bytes: bytes, Cost: cost} }

func TestPreferIngress(t *testing.T) {
	cases := []struct {
		dst          string
		in, eg, want bool
	}{
		{"pod", true, true, true},
		{"pod", false, true, false},
		{"external", true, true, false},
		{"service", false, true, false},
		{"external", true, false, true}, // only the far side observed
	}
	for _, c := range cases {
		if got := PreferIngress(c.dst, c.in, c.eg); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

func TestBuildDedupesPodPairs(t *testing.T) {
	rows := []FlowRow{
		// a → b seen at both ends on two ports: count ingress once.
		{Cluster: "c", SrcKind: "pod", SrcNamespace: "n", SrcName: "a", SrcZone: "z1", DstKind: "pod", DstNamespace: "n", DstName: "b", DstZone: "z2",
			Port: 80, Protocol: "tcp", In: side(100, 1), Eg: side(90, 1)},
		{Cluster: "c", SrcKind: "pod", SrcNamespace: "n", SrcName: "a", SrcZone: "z1", DstKind: "pod", DstNamespace: "n", DstName: "b", DstZone: "z2",
			Port: 81, Protocol: "tcp", Eg: side(10, 0)}, // only egress on this port — pair already chose ingress
		// a → internet: egress only.
		{Cluster: "c", SrcKind: "pod", SrcNamespace: "n", SrcName: "a", SrcZone: "z1", DstKind: "external", DstName: "s3", Port: 443, Protocol: "tcp",
			Eg: Side{Seen: true, Bytes: 50, NetBytes: 50, Cost: 5, NetCost: 5}},
		// a → svc: "ns/name" service ids.
		{Cluster: "c", SrcKind: "pod", SrcNamespace: "n", SrcName: "a", DstKind: "service", DstName: "kube-system/kube-dns", Port: 53, Protocol: "udp",
			Eg: side(1, 0)},
	}
	g := Build(rows)
	if len(g.Edges) != 3 {
		t.Fatalf("edges: %d", len(g.Edges))
	}
	ab := g.Edges[0]
	if ab.Source != "workload:n/a" || ab.Target != "workload:n/b" || ab.Bytes != 100 {
		t.Fatalf("a→b = %+v (must count the ingress side only)", ab)
	}
	if g.Total.Bytes != 151 || g.Total.Cost != 6 {
		t.Fatalf("totals %+v", g.Total)
	}
	if n := g.Nodes["service:kube-system/kube-dns"]; n == nil || n.Kind != "service" {
		t.Fatalf("service node: %+v", n)
	}
	if n := g.Nodes["workload:n/b"]; n.Zone != "z2" || n.BytesIn != 100 {
		t.Fatalf("b = %+v", n)
	}
	if a := g.Nodes["workload:n/a"]; a.BytesOut != 151 || a.Cost != 6 || a.Zone != "z1" {
		t.Fatalf("a = %+v", a)
	}
}

func TestTruncateFoldsIntoOther(t *testing.T) {
	var rows []FlowRow
	for i, name := range []string{"big", "m1", "m2", "s1", "s2"} {
		rows = append(rows, FlowRow{SrcKind: "pod", SrcNamespace: "n", SrcName: "hub", DstKind: "pod", DstNamespace: "n",
			DstName: name, Port: int32(8000 + i), Protocol: "tcp", In: side(float64(100-i*20), 0)})
	}
	g := Build(rows)
	g.Truncate(3) // hub, big + other
	if len(g.Nodes) != 3 || g.Nodes[OtherID] == nil {
		t.Fatalf("nodes: %v", keys(g.Nodes))
	}
	var toOther *Edge
	for _, e := range g.Edges {
		if e.Target == OtherID {
			toOther = e
		}
	}
	if toOther == nil || toOther.Bytes != 80+60+40+20 || toOther.Port != 0 || toOther.Protocol != "tcp" {
		t.Fatalf("merged edge: %+v", toOther)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func viewer() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Role: auth.RoleViewer})
}

func TestDemoServiceMapAndCosts(t *testing.T) {
	s := &Service{Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }}
	r, err := s.GetServiceMap(viewer(), connect.NewRequest(&kuberov1.GetServiceMapRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	m := r.Msg
	if m.GetSource() != "demo" || len(m.GetNodes()) < 8 || m.GetEgressGb() <= 0 || m.GetCrossZoneGb() <= 0 {
		t.Fatalf("demo map: %d nodes egress %v xz %v", len(m.GetNodes()), m.GetEgressGb(), m.GetCrossZoneGb())
	}
	var sawXZ, sawEgress bool
	for _, e := range m.GetEdges() {
		sawXZ = sawXZ || e.GetCrossZone()
		sawEgress = sawEgress || e.GetEgress()
		if e.GetSource() == "workload:checkout/checkout" && e.GetTarget() == "workload:checkout/redis" && e.GetBytes() != 9.8e9 {
			t.Fatalf("pod pair double counted: %v", e.GetBytes())
		}
	}
	if !sawXZ || !sawEgress {
		t.Fatal("demo must show cross-zone and egress edges")
	}
	focus, _ := s.GetServiceMap(viewer(), connect.NewRequest(&kuberov1.GetServiceMapRequest{Namespace: "payments"}))
	if n := len(focus.Msg.GetNodes()); n != 4 { // checkout, payments-api, postgres, stripe
		t.Fatalf("namespace focus: %d nodes", n)
	}
	small, _ := s.GetServiceMap(viewer(), connect.NewRequest(&kuberov1.GetServiceMapRequest{MaxNodes: 4}))
	if len(small.Msg.GetNodes()) != 4 {
		t.Fatalf("max_nodes: %d", len(small.Msg.GetNodes()))
	}

	c, err := s.ListNetworkCosts(viewer(), connect.NewRequest(&kuberov1.ListNetworkCostsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	costs := c.Msg.GetCosts()
	if len(costs) == 0 || costs[0].GetWorkload() != "embedder" || costs[0].GetTopDestination() != "huggingface.co" {
		t.Fatalf("costs: %+v", costs[0])
	}
	// 12 GB × $0.09 per hour → × 720.
	if math.Abs(costs[0].GetEgressUsdMonth()-12*0.09*720) > 0.01 {
		t.Fatalf("egress $ %v", costs[0].GetEgressUsdMonth())
	}
	var sum float64
	for _, x := range costs {
		sum += x.GetTotalUsdMonth()
	}
	if math.Abs(sum-c.Msg.GetTotalUsdMonth()) > 0.05 {
		t.Fatalf("Σ %v != total %v", sum, c.Msg.GetTotalUsdMonth())
	}
}

func TestNetworkValidation(t *testing.T) {
	s := &Service{}
	var ce *connect.Error
	_, err := s.GetServiceMap(viewer(), connect.NewRequest(&kuberov1.GetServiceMapRequest{
		StartUnixMs: time.Now().Add(-100 * 24 * time.Hour).UnixMilli()}))
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("span > 90d: %v", err)
	}
	off := &Service{DemoDisabled: true}
	_, err = off.ListNetworkCosts(viewer(), connect.NewRequest(&kuberov1.ListNetworkCostsRequest{}))
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("demo disabled: %v", err)
	}
}
