// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package network

import (
	"math"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

func TestServiceMapAgainstClickHouse(t *testing.T) {
	db := chtest.Open(t, "network")
	now := time.Now().UTC()
	h0 := timewin.FloorHour(now).Add(-2 * time.Hour) // two full past hours

	cols := []string{"ts", "org_id", "cluster_id", "window_sec", "src_kind", "src_namespace", "src_workload", "src_pod",
		"src_node", "src_zone", "src_ip", "src_name", "dst_kind", "dst_namespace", "dst_workload", "dst_pod", "dst_node",
		"dst_zone", "dst_ip", "dst_service", "dst_name", "port", "protocol", "direction", "bytes", "packets",
		"retransmits", "cross_zone", "egress", "cost_usd"}
	flow := func(ts time.Time, srcWL, srcZone, dstKind, dstNS, dstWL, dstSvc, dstName, dstZone string, port uint16,
		dir string, bytes uint64, xz, eg uint8, usd float64, retrans uint32) []any {
		return []any{ts, "default", "c1", uint16(15), "pod", "shop", srcWL, srcWL + "-7d9f8c6b5-bcdfg", "n1", srcZone,
			"10.0.0.1", srcWL, dstKind, dstNS, dstWL, "", "", dstZone, "10.0.0.2", dstSvc, dstName, port, "tcp", dir,
			bytes, bytes / 1000, retrans, xz, eg, usd}
	}
	var rows [][]any
	for h := 0; h < 2; h++ {
		ts := h0.Add(time.Duration(h)*time.Hour + 5*time.Minute)
		rows = append(rows,
			// checkout → redis cross-zone, observed at both ends.
			flow(ts, "checkout", "z1", "pod", "shop", "redis", "", "redis", "z2", 6379, "egress", 2e9, 1, 0, 0.02, 5),
			flow(ts, "checkout", "z1", "pod", "shop", "redis", "", "redis", "z2", 6379, "ingress", 2e9, 1, 0, 0.02, 7),
			// checkout → S3 (internet egress).
			flow(ts, "checkout", "z1", "external", "", "", "", "s3.amazonaws.com", "", 443, "egress", 5e9, 0, 1, 0.45, 0),
			// checkout → kube-dns service.
			flow(ts, "checkout", "z1", "service", "kube-system", "", "kube-system/kube-dns", "kube-dns", "", 53, "egress", 1e6, 0, 0, 0, 0),
			// cart → redis same zone, only the sender instrumented.
			flow(ts, "cart", "z2", "pod", "shop", "redis", "", "redis", "z2", 6379, "egress", 1e9, 0, 0, 0, 0),
		)
	}
	chtest.Insert(t, db, "net_flows", cols, rows)

	s := &Service{CH: db}
	req := &kuberov1.GetServiceMapRequest{ClusterId: "c1", StartUnixMs: h0.UnixMilli(), EndUnixMs: h0.Add(2 * time.Hour).UnixMilli()}
	r, err := s.GetServiceMap(viewer(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	m := r.Msg
	edges := map[string]*kuberov1.ServiceMapEdge{}
	for _, e := range m.GetEdges() {
		edges[e.GetSource()+" > "+e.GetTarget()] = e
	}
	xz := edges["workload:shop/checkout > workload:shop/redis"]
	if xz == nil || xz.GetBytes() != 4e9 || !xz.GetCrossZone() || xz.GetRetransmits() != 14 {
		t.Fatalf("cross-zone pair (ingress side, counted once): %+v", xz)
	}
	if e := edges["workload:shop/checkout > external:/s3.amazonaws.com"]; e == nil || !e.GetEgress() || e.GetBytes() != 10e9 {
		t.Fatalf("egress edge: %+v", e)
	}
	if e := edges["workload:shop/checkout > service:kube-system/kube-dns"]; e == nil || e.GetPort() != 53 {
		t.Fatalf("service edge: %+v", e)
	}
	if e := edges["workload:shop/cart > workload:shop/redis"]; e == nil || e.GetBytes() != 2e9 {
		t.Fatalf("sender-only pair falls back to egress: %+v", e)
	}
	if math.Abs(m.GetEgressGb()-10) > 1e-9 || math.Abs(m.GetCrossZoneGb()-4) > 1e-9 {
		t.Fatalf("totals: egress %v GB, cross-zone %v GB", m.GetEgressGb(), m.GetCrossZoneGb())
	}
	// 2h of (0.02×2 + 0.45×2) $ → $0.94 over 2h → × 360.
	if math.Abs(m.GetTotalCostUsdMonth()-0.94*360) > 0.01 {
		t.Fatalf("total $/mo %v", m.GetTotalCostUsdMonth())
	}
	if xz.GetBytesPerSec() != 4e9/7200 {
		t.Fatalf("bytes/s %v", xz.GetBytesPerSec())
	}

	c, err := s.ListNetworkCosts(viewer(), connect.NewRequest(&kuberov1.ListNetworkCostsRequest{
		ClusterId: "c1", StartUnixMs: req.StartUnixMs, EndUnixMs: req.EndUnixMs}))
	if err != nil {
		t.Fatal(err)
	}
	top := c.Msg.GetCosts()[0]
	if top.GetWorkload() != "checkout" || top.GetTopDestination() != "s3.amazonaws.com" ||
		math.Abs(top.GetEgressUsdMonth()-0.9*360) > 0.01 || math.Abs(top.GetCrossZoneUsdMonth()-0.04*360) > 0.01 ||
		math.Abs(top.GetEgressGb()-10) > 1e-9 {
		t.Fatalf("checkout costs: %+v", top)
	}
}
