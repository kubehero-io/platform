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
// Endpoints are interned to small integers so the key stays 12 bytes:
// the index sees up to a full table (131072 entries) every interval.
type flowRowKey struct {
	src, dst  uint32 // endpoint ids, see flowAggregator.intern
	port      uint16
	protocol  uint8
	direction uint8
}

// flowAggregator translates drained kernel entries into Flow rows. One
// aggregator serves one drain; the Resolver is consulted once per
// distinct address. Rows live in one slice (indexed by key) rather than
// one allocation each.
type flowAggregator struct {
	resolver        Resolver
	includeLoopback bool

	byAddr map[netip.Addr]addrEndpoint
	byID   map[string]uint32 // endpointID -> interned id

	index map[flowRowKey]int
	rows  []flowRow

	skipped uint64 // entries dropped: loopback, malformed
}

// addrEndpoint is one address's completed endpoint and the interned id
// of its identity.
type addrEndpoint struct {
	ep *kuberov1.FlowEndpoint
	id uint32
}

type flowRow struct {
	key                         flowRowKey
	srcAddr, dstAddr            netip.Addr // representative (lowest) addresses
	bytes, packets, retransmits uint64
}

// newFlowAggregator sizes its tables for sizeHint rows (the previous
// drain's count) so a steady node does not regrow them every interval.
func newFlowAggregator(r Resolver, includeLoopback bool, sizeHint int) *flowAggregator {
	sizeHint = max(0, min(sizeHint, maxFlowMapEntries))
	return &flowAggregator{
		resolver:        r,
		includeLoopback: includeLoopback,
		byAddr:          make(map[netip.Addr]addrEndpoint),
		byID:            make(map[string]uint32),
		index:           make(map[flowRowKey]int, sizeHint),
		rows:            make([]flowRow, 0, sizeHint),
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

// row returns the row for k, creating it. The pointer is only valid
// until the next call (the slice may grow).
func (a *flowAggregator) row(k *flowKey) *flowRow {
	src, dst, ok := k.addrs()
	if !ok || k.Direction > dirIngress {
		a.skipped++
		return nil
	}
	// The kernel side filters loopback already (unless configured to
	// keep it); this also catches v4-mapped loopback.
	if !a.includeLoopback && (src.IsLoopback() || dst.IsLoopback()) {
		a.skipped++
		return nil
	}
	rk := flowRowKey{
		src:       a.intern(src),
		dst:       a.intern(dst),
		port:      k.Port,
		protocol:  k.Protocol,
		direction: k.Direction,
	}
	i, ok := a.index[rk]
	if !ok {
		a.index[rk] = len(a.rows)
		a.rows = append(a.rows, flowRow{key: rk, srcAddr: src, dstAddr: dst})
		return &a.rows[len(a.rows)-1]
	}
	// Several addresses behind one endpoint: report the lowest so output
	// does not depend on map iteration order.
	row := &a.rows[i]
	if src.Less(row.srcAddr) {
		row.srcAddr = src
	}
	if dst.Less(row.dstAddr) {
		row.dstAddr = dst
	}
	return row
}

// intern resolves addr once per drain and returns the id of its
// endpoint identity, so addresses of one pod / service / node share an
// id. The Resolver's result is cloned (resolvers typically hand out
// cached objects) and completed so ip, kind and name are always set.
func (a *flowAggregator) intern(addr netip.Addr) uint32 {
	if e, ok := a.byAddr[addr]; ok {
		return e.id
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
	eid := endpointID(ep)
	id, ok := a.byID[eid]
	if !ok {
		id = uint32(len(a.byID))
		a.byID[eid] = id
	}
	a.byAddr[addr] = addrEndpoint{ep: ep, id: id}
	return id
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

// flows returns all aggregated rows as one batch (see batches).
func (a *flowAggregator) flows(ts time.Time, window time.Duration) []*kuberov1.Flow {
	var out []*kuberov1.Flow
	a.batches(ts, window, max(1, len(a.rows)), func(b []*kuberov1.Flow) { out = b })
	return out
}

// batches emits the aggregated rows heaviest first, in batches of at
// most size. ts is the end of the window (drain time), window its
// measured length. Messages are built per batch, so only one batch is
// alive at a time even when the table was full. Endpoint messages are
// shared between rows: consumers must treat batches as read-only. The
// aggregator is spent afterwards.
func (a *flowAggregator) batches(ts time.Time, window time.Duration, size int, emit func([]*kuberov1.Flow)) {
	windowSec := int32(min(math.Round(window.Seconds()), math.MaxInt32))
	if windowSec < 1 {
		windowSec = 1
	}
	rows := a.rows
	a.index = nil // sorting invalidates it
	slices.SortFunc(rows, func(x, y flowRow) int {
		if c := cmp.Compare(y.bytes, x.bytes); c != 0 {
			return c
		}
		if c := cmp.Compare(x.key.direction, y.key.direction); c != 0 {
			return c
		}
		if c := x.srcAddr.Compare(y.srcAddr); c != 0 {
			return c
		}
		if c := x.dstAddr.Compare(y.dstAddr); c != 0 {
			return c
		}
		if c := cmp.Compare(x.key.port, y.key.port); c != 0 {
			return c
		}
		return cmp.Compare(x.key.protocol, y.key.protocol)
	})
	ms := ts.UnixMilli()
	for len(rows) > 0 {
		n := min(size, len(rows))
		// One backing array per batch; elements are filled in place,
		// never copied.
		msgs := make([]kuberov1.Flow, n)
		out := make([]*kuberov1.Flow, n)
		for i := range n {
			r, m := &rows[i], &msgs[i]
			dir, _ := directionName(r.key.direction)
			m.TsUnixMs = ms
			m.WindowSec = windowSec
			// The endpoint as resolved from the row's representative address.
			m.Src, m.Dst = a.byAddr[r.srcAddr].ep, a.byAddr[r.dstAddr].ep
			m.Port = int32(r.key.port)
			m.Protocol = protocolName(r.key.protocol)
			m.Bytes, m.Packets = r.bytes, r.packets
			m.Direction = dir
			m.Retransmits = uint32(min(r.retransmits, math.MaxUint32))
			out[i] = m
		}
		emit(out)
		rows = rows[n:]
	}
}
