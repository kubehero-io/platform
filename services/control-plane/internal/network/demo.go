// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package network

// demoFlows is one hour of a believable shop: internet → frontend →
// checkout → payments → postgres, checkout ↔ cart ↔ redis with
// cross-zone chatter, and egress to S3 and a card processor. Pod→pod
// flows are recorded at both ends (like real eBPF data) so the demo
// exercises the same de-duplication as live data. Prices: $0.09/GB
// internet egress, $0.01/GB cross-zone.
func demoFlows(namespace string) []FlowRow {
	const gb = 1e9
	type f struct {
		srcKind, srcNS, srcName, srcZone string
		dstKind, dstNS, dstName, dstZone string
		port                             int32
		gb                               float64
		crossZone, egress                bool
		retrans                          float64
	}
	flows := []f{
		{"external", "", "", "", "pod", "edge", "frontend-gateway", "us-east-1a", 443, 18, false, false, 120},
		{"pod", "edge", "frontend-gateway", "us-east-1a", "pod", "checkout", "checkout", "us-east-1a", 8080, 6.5, false, false, 3},
		{"pod", "edge", "frontend-gateway", "us-east-1a", "pod", "checkout", "cart", "us-east-1b", 8080, 3.2, true, false, 1},
		{"pod", "checkout", "checkout", "us-east-1a", "pod", "payments", "payments-api", "us-east-1a", 8443, 1.4, false, false, 0},
		{"pod", "checkout", "checkout", "us-east-1a", "pod", "checkout", "redis", "us-east-1c", 6379, 9.8, true, false, 40},
		{"pod", "checkout", "cart", "us-east-1b", "pod", "checkout", "redis", "us-east-1c", 6379, 4.1, true, false, 12},
		{"pod", "payments", "payments-api", "us-east-1a", "pod", "payments", "postgres", "us-east-1a", 5432, 2.7, false, false, 0},
		{"pod", "payments", "payments-api", "us-east-1a", "external", "", "api.stripe.com", "", 443, 0.35, false, true, 2},
		{"pod", "checkout", "checkout", "us-east-1a", "external", "", "s3.us-east-1.amazonaws.com", "", 443, 7.5, false, true, 0},
		{"pod", "checkout", "checkout", "us-east-1a", "service", "kube-system", "kube-system/kube-dns", "", 53, 0.02, false, false, 0},
		{"pod", "ml-inference", "embedder", "us-east-1b", "external", "", "huggingface.co", "", 443, 12, false, true, 0},
	}
	var out []FlowRow
	for _, x := range flows {
		if namespace != "" && x.srcNS != namespace && x.dstNS != namespace {
			continue
		}
		bytes := x.gb * gb
		cost := 0.0
		side := Side{Seen: true, Bytes: bytes, Retransmits: x.retrans}
		if x.crossZone {
			cost = x.gb * 0.01
			side.CrossZoneBytes, side.CrossZoneCost = bytes, cost
		}
		if x.egress {
			cost = x.gb * 0.09
			side.NetBytes, side.NetCost = bytes, cost
		}
		side.Cost = cost
		r := FlowRow{Cluster: "eks-use1-prod", SrcKind: x.srcKind, SrcNamespace: x.srcNS, SrcName: x.srcName, SrcZone: x.srcZone,
			DstKind: x.dstKind, DstNamespace: x.dstNS, DstName: x.dstName, DstZone: x.dstZone, Port: x.port, Protocol: "tcp"}
		switch {
		case x.dstKind == "pod" && x.srcKind == "pod":
			r.In, r.Eg = side, side // seen at both ends
		case x.dstKind == "pod":
			r.In = side // inbound from outside: only the receiver sees it
		default:
			r.Eg = side
		}
		out = append(out, r)
	}
	return out
}
