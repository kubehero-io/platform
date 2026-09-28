// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"cmp"
	"math"
	"net/netip"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// Kernel constants mirrored from bpf/kh_uapi.h and bpf/kh_flow.h.
const (
	afInet  = 2
	afInet6 = 10

	ipprotoICMP   = 1
	ipprotoTCP    = 6
	ipprotoUDP    = 17
	ipprotoICMPv6 = 58

	dirEgress  = 0
	dirIngress = 1
)

// flowKey mirrors struct flow_key (bpf/kh_flow.h) byte for byte: the
// drain unmarshals kernel keys straight into it. Addresses are network
// byte order, IPv4 in the first four bytes; Port is host byte order.
type flowKey struct {
	Saddr     [16]byte
	Daddr     [16]byte
	Port      uint16
	Family    uint8
	Protocol  uint8
	Direction uint8
	Pad       [3]uint8
}

// flowValue mirrors struct flow_val.
type flowValue struct {
	Bytes   uint64
	Packets uint64
}

// addrs decodes the key's endpoints. IPv4-mapped IPv6 addresses are
// unmapped so both spellings of an IPv4 peer aggregate together.
func (k *flowKey) addrs() (src, dst netip.Addr, ok bool) {
	switch k.Family {
	case afInet:
		return netip.AddrFrom4([4]byte(k.Saddr[:4])), netip.AddrFrom4([4]byte(k.Daddr[:4])), true
	case afInet6:
		return netip.AddrFrom16(k.Saddr).Unmap(), netip.AddrFrom16(k.Daddr).Unmap(), true
	}
	return netip.Addr{}, netip.Addr{}, false
}

func protocolName(p uint8) string {
	switch p {
	case ipprotoTCP:
		return "tcp"
	case ipprotoUDP:
		return "udp"
	case ipprotoICMP, ipprotoICMPv6:
		return "icmp"
	}
	return "other"
}

func directionName(d uint8) (string, bool) {
	switch d {
	case dirEgress:
		return "egress", true
	case dirIngress:
		return "ingress", true
	}
	return "", false
}

// flowRowKey is the aggregation key of emitted rows: kernel keys that
// differ only in addresses resolving to the same endpoint (a pod's IPv4
// and IPv6 address, a node's several IPs) collapse into one row.
type flowRowKey struct {
	src, dst  string // endpointID
	port      uint16
	protocol  string
	direction string
}

// flowAggregator translates drained kernel entries into Flow rows. One
// aggregator serves one drain; the Resolver is consulted once per
// distinct address.
type flowAggregator struct {
	resolver        Resolver
	includeLoopback bool

	endpoints map[netip.Addr]*kuberov1.FlowEndpoint
	rows      map[flowRowKey]*flowRow

	skipped uint64 // entries dropped: loopback, malformed
}

type flowRow struct {
	src, dst            *kuberov1.FlowEndpoint
	srcAddr, dstAddr    netip.Addr
	bytes, packets      uint64
	retransmits         uint64
	port                uint16
	protocol, direction string
}

func newFlowAggregator(r Resolver, includeLoopback bool) *flowAggregator {
	return &flowAggregator{
		resolver:        r,
		includeLoopback: includeLoopback,
		endpoints:       make(map[netip.Addr]*kuberov1.FlowEndpoint),
		rows:            make(map[flowRowKey]*flowRow),
	}
}

// addFlow accounts one kernel flow entry.
func (a *flowAggregator) addFlow(k *flowKey, v *flowValue) {
	if row := a.row(k); row != nil {
		row.bytes += v.Bytes
		row.packets += v.Packets
	}
}

// addRetransmits accounts one retransmit-map entry. Retransmit keys are
// egress TCP keys of the sender's flow, so they land on the row the
// matching egress bytes went to. A retransmit without such bytes in this
// window (flow entry evicted, or the counters straddled a drain) still
// becomes a zero-byte row: loss is signal, it must not vanish.
func (a *flowAggregator) addRetransmits(k *flowKey, count uint64) {
	if row := a.row(k); row != nil {
		row.retransmits += count
	}
}

func (a *flowAggregator) row(k *flowKey) *flowRow {
	src, dst, ok := k.addrs()
	dir, dirOK := directionName(k.Direction)
	if !ok || !dirOK {
		a.skipped++
		return nil
	}
	// The kernel side filters loopback already (unless configured to
	// keep it); this also catches v4-mapped loopback.
	if !a.includeLoopback && (src.IsLoopback() || dst.IsLoopback()) {
		a.skipped++
		return nil
	}
	srcEP, dstEP := a.endpoint(src), a.endpoint(dst)
	rk := flowRowKey{
		src:       endpointID(srcEP),
		dst:       endpointID(dstEP),
		port:      k.Port,
		protocol:  protocolName(k.Protocol),
		direction: dir,
	}
	row := a.rows[rk]
	if row == nil {
		row = &flowRow{src: srcEP, dst: dstEP, srcAddr: src, dstAddr: dst, port: k.Port, protocol: rk.protocol, direction: dir}
		a.rows[rk] = row
		return row
	}
	// Several addresses behind one endpoint: report the lowest so output
	// does not depend on map iteration order.
	if src.Less(row.srcAddr) {
		row.src, row.srcAddr = srcEP, src
	}
	if dst.Less(row.dstAddr) {
		row.dst, row.dstAddr = dstEP, dst
	}
	return row
}

// endpoint resolves addr once per drain. The Resolver's result is cloned
// (resolvers typically hand out cached objects) and completed so every
// emitted endpoint has ip, kind and name set.
func (a *flowAggregator) endpoint(addr netip.Addr) *kuberov1.FlowEndpoint {
	if ep, ok := a.endpoints[addr]; ok {
		return ep
	}
	var ep *kuberov1.FlowEndpoint
	if got := a.resolver.LookupIP(addr); got != nil {
		ep = proto.Clone(got).(*kuberov1.FlowEndpoint)
	} else {
		ep = &kuberov1.FlowEndpoint{}
	}
	if ep.Ip == "" {
		ep.Ip = addr.String()
	}
	if ep.Kind == "" {
		ep.Kind = "external"
	}
	if ep.Name == "" {
		ep.Name = ep.Ip
	}
	a.endpoints[addr] = ep
	return ep
}

// endpointID is the identity rows aggregate on: the Kubernetes object
// for pods, services and nodes; the address for everything else (two
// external IPs are two peers even if reverse DNS names them alike).
func endpointID(ep *kuberov1.FlowEndpoint) string {
	switch ep.GetKind() {
	case "pod":
		if p := ep.GetPod(); p.GetPod() != "" {
			return "pod/" + p.GetNamespace() + "/" + p.GetPod()
		}
	case "service":
		if ep.GetService() != "" {
			return "service/" + ep.GetService()
		}
	case "node":
		if ep.GetName() != "" {
			return "node/" + ep.GetName()
		}
	}
	return "ip/" + ep.GetIp()
}

// flows returns the aggregated rows, heaviest first. ts is the end of
// the window (drain time), window its measured length. Endpoint messages
// are shared between rows of one batch: consumers must treat the batch
// as read-only.
func (a *flowAggregator) flows(ts time.Time, window time.Duration) []*kuberov1.Flow {
	windowSec := int32(min(math.Round(window.Seconds()), math.MaxInt32))
	if windowSec < 1 {
		windowSec = 1
	}
	rows := make([]*flowRow, 0, len(a.rows))
	for _, r := range a.rows {
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(x, y *flowRow) int {
		if c := cmp.Compare(y.bytes, x.bytes); c != 0 {
			return c
		}
		if c := cmp.Compare(x.direction, y.direction); c != 0 {
			return c
		}
		if c := x.srcAddr.Compare(y.srcAddr); c != 0 {
			return c
		}
		if c := x.dstAddr.Compare(y.dstAddr); c != 0 {
			return c
		}
		if c := cmp.Compare(x.port, y.port); c != 0 {
			return c
		}
		return cmp.Compare(x.protocol, y.protocol)
	})
	out := make([]*kuberov1.Flow, len(rows))
	ms := ts.UnixMilli()
	for i, r := range rows {
		out[i] = &kuberov1.Flow{
			TsUnixMs:    ms,
			WindowSec:   windowSec,
			Src:         r.src,
			Dst:         r.dst,
			Port:        int32(r.port),
			Protocol:    r.protocol,
			Bytes:       r.bytes,
			Packets:     r.packets,
			Direction:   r.direction,
			Retransmits: uint32(min(r.retransmits, math.MaxUint32)),
		}
	}
	return out
}
