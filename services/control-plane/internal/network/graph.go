// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package network

import (
	"sort"
	"strings"
)

// FlowRow is one aggregated net_flows_1h group, with both observation
// sides kept apart so the de-duplication rule can pick one.
type FlowRow struct {
	Cluster                        string
	SrcKind, SrcNamespace, SrcName string
	SrcZone                        string
	DstKind, DstNamespace, DstName string
	DstZone                        string
	Port                           int32
	Protocol                       string
	In, Eg                         Side // ingress-observed, egress-observed
}

// Side is what one observation point saw.
type Side struct {
	Seen                            bool
	Bytes, CrossZoneBytes, NetBytes float64 // NetBytes: internet egress bytes
	Cost, CrossZoneCost, NetCost    float64
	Retransmits                     float64
}

func (s *Side) add(o Side) {
	s.Seen = s.Seen || o.Seen
	s.Bytes += o.Bytes
	s.CrossZoneBytes += o.CrossZoneBytes
	s.NetBytes += o.NetBytes
	s.Cost += o.Cost
	s.CrossZoneCost += o.CrossZoneCost
	s.NetCost += o.NetCost
	s.Retransmits += o.Retransmits
}

// Node ids are "<kind>:<namespace>/<name>".
func nodeID(kind, namespace, name string) string { return kind + ":" + namespace + "/" + name }

// SrcID / DstID map a flow's endpoints onto service-map nodes: pods
// collapse to their workload; services, nodes and external endpoints
// keep their own identity.
func (r *FlowRow) SrcID() (id, kind, namespace, name string) {
	return endpoint(r.SrcKind, r.SrcNamespace, r.SrcName)
}

// DstID is SrcID for the destination.
func (r *FlowRow) DstID() (id, kind, namespace, name string) {
	return endpoint(r.DstKind, r.DstNamespace, r.DstName)
}

func endpoint(kind, namespace, name string) (string, string, string, string) {
	switch kind {
	case "pod":
		if name == "" {
			name = "unknown"
		}
		return nodeID("workload", namespace, name), "workload", namespace, name
	case "service":
		ns, svc, ok := strings.Cut(name, "/")
		if !ok {
			ns, svc = namespace, name
		}
		return nodeID("service", ns, svc), "service", ns, svc
	case "node":
		if name == "" {
			name = "host-network"
		}
		return nodeID("node", "", name), "node", "", name
	default:
		if name == "" {
			name = "internet"
		}
		return nodeID("external", "", name), "external", "", name
	}
}

// PreferIngress decides which observation side to count for a pair.
// Pod→pod traffic is seen at both ends: prefer the receiver's ingress
// record; for anything leaving to a service, node or the internet
// prefer the sender's egress record; fall back to whichever side
// exists. (cost.PreferredDirectionSQL is the same rule in SQL.)
func PreferIngress(dstKind string, inSeen, egSeen bool) bool {
	if dstKind == "pod" {
		return inSeen
	}
	return inSeen && !egSeen
}

// Edge is one directed service-map edge.
type Edge struct {
	Source, Target string
	Port           int32
	Protocol       string
	Side
}

// Node is one service-map node.
type Node struct {
	ID, Name, Namespace, Kind string
	Zone                      string
	BytesIn, BytesOut, Cost   float64
	zones                     map[string]float64
}

// Graph is the de-duplicated service map.
type Graph struct {
	Nodes map[string]*Node
	Edges []*Edge
	Total Side // Σ of the counted sides
}

// Build de-duplicates rows into a graph. The preference is decided per
// (cluster, source, destination) pair across ports, zones and
// protocols, so one pair is never counted from both ends.
func Build(rows []FlowRow) *Graph {
	type pairKey struct{ cluster, src, dst string }
	type pairAgg struct {
		dstKind string
		in, eg  Side
	}
	pairs := map[pairKey]*pairAgg{}
	for i := range rows {
		r := &rows[i]
		src, _, _, _ := r.SrcID()
		dst, _, _, _ := r.DstID()
		k := pairKey{r.Cluster, src, dst}
		p := pairs[k]
		if p == nil {
			p = &pairAgg{dstKind: r.DstKind}
			pairs[k] = p
		}
		p.in.add(r.In)
		p.eg.add(r.Eg)
	}
	useIngress := map[pairKey]bool{}
	for k, p := range pairs {
		useIngress[k] = PreferIngress(p.dstKind, p.in.Seen, p.eg.Seen)
	}

	g := &Graph{Nodes: map[string]*Node{}}
	type edgeKey struct {
		src, dst string
		port     int32
		proto    string
	}
	edges := map[edgeKey]*Edge{}
	node := func(id, kind, ns, name string) *Node {
		n := g.Nodes[id]
		if n == nil {
			n = &Node{ID: id, Kind: kind, Namespace: ns, Name: name, zones: map[string]float64{}}
			g.Nodes[id] = n
		}
		return n
	}
	for i := range rows {
		r := &rows[i]
		src, sk, sns, sn := r.SrcID()
		dst, dk, dns, dn := r.DstID()
		side := r.Eg
		if useIngress[pairKey{r.Cluster, src, dst}] {
			side = r.In
		}
		if !side.Seen || (side.Bytes == 0 && side.Cost == 0) {
			continue
		}
		ek := edgeKey{src, dst, r.Port, r.Protocol}
		e := edges[ek]
		if e == nil {
			e = &Edge{Source: src, Target: dst, Port: r.Port, Protocol: r.Protocol}
			edges[ek] = e
		}
		e.Side.add(side)
		g.Total.add(side)
		s, d := node(src, sk, sns, sn), node(dst, dk, dns, dn)
		s.BytesOut += side.Bytes
		s.Cost += side.Cost
		d.BytesIn += side.Bytes
		if r.SrcZone != "" {
			s.zones[r.SrcZone] += side.Bytes
		}
		if r.DstZone != "" {
			d.zones[r.DstZone] += side.Bytes
		}
	}
	for _, e := range edges {
		g.Edges = append(g.Edges, e)
	}
	for _, n := range g.Nodes {
		best := 0.0
		for z, b := range n.zones {
			if b > best || (b == best && z < n.Zone) {
				n.Zone, best = z, b
			}
		}
	}
	sortEdges(g.Edges)
	return g
}

func sortEdges(es []*Edge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].Bytes != es[j].Bytes {
			return es[i].Bytes > es[j].Bytes
		}
		if es[i].Source != es[j].Source {
			return es[i].Source < es[j].Source
		}
		return es[i].Target < es[j].Target
	})
}

// OtherID is the node the long tail folds into.
const OtherID = "other:/other"

// Truncate keeps the maxNodes heaviest nodes (bytes in + out) and
// folds the rest into one "other" node, re-pointing and merging their
// edges; traffic between two folded nodes disappears from the edge list
// but stays in the totals.
func (g *Graph) Truncate(maxNodes int) {
	if maxNodes <= 1 || len(g.Nodes) <= maxNodes {
		return
	}
	ranked := make([]*Node, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		ranked = append(ranked, n)
	}
	sort.Slice(ranked, func(i, j int) bool {
		wi, wj := ranked[i].BytesIn+ranked[i].BytesOut, ranked[j].BytesIn+ranked[j].BytesOut
		if wi != wj {
			return wi > wj
		}
		return ranked[i].ID < ranked[j].ID
	})
	keep := map[string]bool{}
	for _, n := range ranked[:maxNodes-1] {
		keep[n.ID] = true
	}
	other := &Node{ID: OtherID, Kind: "other", Name: "other"}
	for _, n := range ranked[maxNodes-1:] {
		other.BytesIn += n.BytesIn
		other.BytesOut += n.BytesOut
		other.Cost += n.Cost
		delete(g.Nodes, n.ID)
	}
	g.Nodes[OtherID] = other
	type key struct{ src, dst string }
	merged := map[key]*Edge{}
	var out []*Edge
	for _, e := range g.Edges {
		src, dst := e.Source, e.Target
		if !keep[src] {
			src = OtherID
		}
		if !keep[dst] {
			dst = OtherID
		}
		if src == e.Source && dst == e.Target {
			out = append(out, e)
			continue
		}
		if src == OtherID && dst == OtherID {
			continue
		}
		k := key{src, dst}
		m := merged[k]
		if m == nil {
			m = &Edge{Source: src, Target: dst, Port: e.Port, Protocol: e.Protocol}
			merged[k] = m
			out = append(out, m)
		} else {
			if m.Port != e.Port {
				m.Port = 0
			}
			if m.Protocol != e.Protocol {
				m.Protocol = "mixed"
			}
		}
		m.Side.add(e.Side)
	}
	sortEdges(out)
	g.Edges = out
}
