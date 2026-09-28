// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"encoding/binary"
	"net/netip"
	"sync"
	"testing"
	"time"
	"unsafe"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// fakeResolver knows a few pods, a service and a node; everything else
// is external. It counts calls to check per-drain memoization.
type fakeResolver struct {
	mu        sync.Mutex
	ips       map[netip.Addr]*kuberov1.FlowEndpoint
	pods      map[[2]string]*kuberov1.PodRef // (podUID, containerID)
	calls     map[netip.Addr]int
	returnNil bool
}

func newFakeResolver() *fakeResolver {
	pod := func(ns, name string) *kuberov1.PodRef {
		return &kuberov1.PodRef{Namespace: ns, Pod: name, Workload: name, WorkloadKind: "Deployment"}
	}
	return &fakeResolver{
		ips: map[netip.Addr]*kuberov1.FlowEndpoint{
			netip.MustParseAddr("10.0.0.1"):    {Kind: "pod", Pod: pod("shop", "web-1"), Name: "web"},
			netip.MustParseAddr("fd00::1"):     {Kind: "pod", Pod: pod("shop", "web-1"), Name: "web"}, // dual stack
			netip.MustParseAddr("10.0.0.2"):    {Kind: "pod", Pod: pod("shop", "db-0"), Name: "db"},
			netip.MustParseAddr("10.96.0.10"):  {Kind: "service", Service: "kube-system/kube-dns", Name: "kube-dns"},
			netip.MustParseAddr("192.168.1.5"): {Kind: "node", Name: "node-a"},
			netip.MustParseAddr("192.168.1.6"): {Kind: "node", Name: "node-a"}, // second NIC
		},
		pods:  map[[2]string]*kuberov1.PodRef{},
		calls: map[netip.Addr]int{},
	}
}

func (r *fakeResolver) LookupIP(ip netip.Addr) *kuberov1.FlowEndpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[ip]++
	if r.returnNil {
		return nil
	}
	if ep, ok := r.ips[ip]; ok {
		return ep
	}
	return &kuberov1.FlowEndpoint{Kind: "external", Name: ip.String()}
}

func (r *fakeResolver) LookupContainer(podUID, containerID string) (*kuberov1.PodRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.pods[[2]string{podUID, containerID}]
	return ref, ok
}

func (r *fakeResolver) ServiceName(ref *kuberov1.PodRef) string { return ref.GetWorkload() }

func key(src, dst string, port uint16, proto, dir uint8) *flowKey {
	k := &flowKey{Port: port, Protocol: proto, Direction: dir}
	s, d := netip.MustParseAddr(src), netip.MustParseAddr(dst)
	if s.Is4() {
		k.Family = afInet
		b4, d4 := s.As4(), d.As4()
		copy(k.Saddr[:], b4[:])
		copy(k.Daddr[:], d4[:])
	} else {
		k.Family = afInet6
		k.Saddr, k.Daddr = s.As16(), d.As16()
	}
	return k
}

func TestFlowKeyLayout(t *testing.T) {
	// Must match struct flow_key in bpf/kh_flow.h (asserted 40 bytes there).
	if s := binary.Size(flowKey{}); s != 40 {
		t.Fatalf("binary.Size(flowKey) = %d, want 40", s)
	}
	if unsafe.Sizeof(flowKey{}) != 40 || unsafe.Offsetof(flowKey{}.Port) != 32 || unsafe.Offsetof(flowKey{}.Family) != 34 ||
		unsafe.Offsetof(flowKey{}.Protocol) != 35 || unsafe.Offsetof(flowKey{}.Direction) != 36 {
		t.Fatal("flowKey field offsets drifted from struct flow_key")
	}
	if binary.Size(flowValue{}) != 16 {
		t.Fatal("flowValue must be 16 bytes")
	}
	if s := binary.Size(stackKey{}); s != 24 || unsafe.Offsetof(stackKey{}.Tgid) != 8 ||
		unsafe.Offsetof(stackKey{}.UserStackID) != 12 || unsafe.Offsetof(stackKey{}.KernelStackID) != 16 {
		t.Fatal("stackKey layout drifted from struct stack_key")
	}
}

func TestFlowKeyAddrs(t *testing.T) {
	tests := []struct {
		name     string
		k        *flowKey
		src, dst string
		ok       bool
	}{
		{"ipv4", key("10.0.0.1", "10.0.0.2", 80, ipprotoTCP, dirEgress), "10.0.0.1", "10.0.0.2", true},
		{"ipv6", key("fd00::1", "fd00::2", 80, ipprotoTCP, dirEgress), "fd00::1", "fd00::2", true},
		{"v4-mapped unmaps", key("::ffff:10.0.0.1", "::ffff:10.0.0.2", 80, ipprotoTCP, dirEgress), "10.0.0.1", "10.0.0.2", true},
		{"unknown family", &flowKey{Family: 99}, "invalid IP", "invalid IP", false},
	}
	for _, tt := range tests {
		src, dst, ok := tt.k.addrs()
		if ok != tt.ok || src.String() != tt.src || dst.String() != tt.dst {
			t.Errorf("%s: addrs() = %s, %s, %v", tt.name, src, dst, ok)
		}
	}
}

func TestProtocolAndDirectionNames(t *testing.T) {
	for p, want := range map[uint8]string{ipprotoTCP: "tcp", ipprotoUDP: "udp", ipprotoICMP: "icmp", ipprotoICMPv6: "icmp", 132: "other", 0: "other"} {
		if got := protocolName(p); got != want {
			t.Errorf("protocolName(%d) = %q, want %q", p, got, want)
		}
	}
	if d, ok := directionName(dirEgress); !ok || d != "egress" {
		t.Error("egress")
	}
	if d, ok := directionName(dirIngress); !ok || d != "ingress" {
		t.Error("ingress")
	}
	if _, ok := directionName(7); ok {
		t.Error("direction 7 must be rejected")
	}
}

func TestFlowAggregation(t *testing.T) {
	r := newFakeResolver()
	agg := newFlowAggregator(r, false)
	v := func(b, p uint64) *flowValue { return &flowValue{Bytes: b, Packets: p} }

	// web -> db:5432, over IPv4 and IPv6 (dual-stack pod): one row.
	agg.addFlow(key("10.0.0.1", "10.0.0.2", 5432, ipprotoTCP, dirEgress), v(1000, 10))
	agg.addFlow(key("fd00::1", "10.0.0.2", 5432, ipprotoTCP, dirEgress), v(500, 5))
	// The same pair seen at the receiver is a separate (ingress) row.
	agg.addFlow(key("10.0.0.1", "10.0.0.2", 5432, ipprotoTCP, dirIngress), v(1500, 15))
	// web -> kube-dns:53/udp
	agg.addFlow(key("10.0.0.1", "10.96.0.10", 53, ipprotoUDP, dirEgress), v(80, 1))
	// node traffic from two node IPs collapses to one node row.
	agg.addFlow(key("192.168.1.5", "8.8.8.8", 443, ipprotoTCP, dirEgress), v(10, 1))
	agg.addFlow(key("192.168.1.6", "8.8.8.8", 443, ipprotoTCP, dirEgress), v(20, 2))
	// Two external peers stay two rows even behind one resolver answer shape.
	agg.addFlow(key("10.0.0.1", "1.1.1.1", 443, ipprotoTCP, dirEgress), v(7, 1))
	agg.addFlow(key("10.0.0.1", "1.0.0.1", 443, ipprotoTCP, dirEgress), v(3, 1))
	// Loopback is dropped (belt and braces: the kernel filters it too).
	agg.addFlow(key("127.0.0.1", "127.0.0.1", 8080, ipprotoTCP, dirEgress), v(99, 9))
	agg.addFlow(key("::1", "::1", 8080, ipprotoTCP, dirEgress), v(99, 9))
	agg.addFlow(key("::ffff:127.0.0.1", "::ffff:10.0.0.2", 8080, ipprotoTCP, dirEgress), v(99, 9))
	// Malformed direction / family are dropped.
	agg.addFlow(key("10.0.0.1", "10.0.0.2", 1, ipprotoTCP, 9), v(1, 1))
	agg.addFlow(&flowKey{Family: 3}, v(1, 1))
	// Retransmits join the egress row; an unmatched one yields a 0-byte row.
	agg.addRetransmits(key("10.0.0.1", "10.0.0.2", 5432, ipprotoTCP, dirEgress), 3)
	agg.addRetransmits(key("10.0.0.2", "10.0.0.1", 8443, ipprotoTCP, dirEgress), 2)

	now := time.UnixMilli(1_700_000_000_000)
	flows := agg.flows(now, 14600*time.Millisecond)

	type row struct {
		src, dst, proto, dir string
		port                 int32
		bytes, pkts          uint64
		retrans              uint32
	}
	var got []row
	for _, f := range flows {
		if f.TsUnixMs != now.UnixMilli() || f.WindowSec != 15 {
			t.Errorf("ts/window = %d/%d", f.TsUnixMs, f.WindowSec)
		}
		got = append(got, row{f.Src.Name, f.Dst.Name, f.Protocol, f.Direction, f.Port, f.Bytes, f.Packets, f.Retransmits})
	}
	want := []row{ // heaviest first, then direction, addresses, port
		{"web", "db", "tcp", "egress", 5432, 1500, 15, 3},
		{"web", "db", "tcp", "ingress", 5432, 1500, 15, 0},
		{"web", "kube-dns", "udp", "egress", 53, 80, 1, 0},
		{"node-a", "8.8.8.8", "tcp", "egress", 443, 30, 3, 0},
		{"web", "1.1.1.1", "tcp", "egress", 443, 7, 1, 0},
		{"web", "1.0.0.1", "tcp", "egress", 443, 3, 1, 0},
		{"db", "web", "tcp", "egress", 8443, 0, 0, 2},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows:\n%+v\nwant %d:\n%+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v\n        want %+v", i, got[i], want[i])
		}
	}
	if agg.skipped != 5 {
		t.Errorf("skipped = %d, want 5", agg.skipped)
	}
	// The row keeps the lowest address of an aggregated endpoint.
	if flows[0].Src.Ip != "10.0.0.1" || flows[3].Src.Ip != "192.168.1.5" {
		t.Errorf("representative IPs: %s, %s", flows[0].Src.Ip, flows[3].Src.Ip)
	}
	// One resolver call per distinct address per drain.
	for ip, n := range r.calls {
		if n != 1 {
			t.Errorf("LookupIP(%s) called %d times", ip, n)
		}
	}
	// Emitted endpoints are clones: mutating them must not touch the
	// resolver's cache.
	flows[0].Src.Name = "mutated"
	if r.ips[netip.MustParseAddr("10.0.0.1")].Name != "web" {
		t.Error("aggregator aliased the resolver's endpoint")
	}
}

func TestFlowEndpointCompletion(t *testing.T) {
	r := newFakeResolver()
	r.returnNil = true // a resolver that breaks its contract
	agg := newFlowAggregator(r, true)
	agg.addFlow(key("127.0.0.1", "10.9.9.9", 80, ipprotoTCP, dirIngress), &flowValue{Bytes: 5, Packets: 1})
	flows := agg.flows(time.Now(), 0)
	if len(flows) != 1 {
		t.Fatalf("got %d flows (IncludeLoopback keeps loopback)", len(flows))
	}
	f := flows[0]
	if f.Src.Ip != "127.0.0.1" || f.Src.Kind != "external" || f.Src.Name != "127.0.0.1" || f.Dst.Name != "10.9.9.9" {
		t.Errorf("endpoints not completed: %+v / %+v", f.Src, f.Dst)
	}
	if f.WindowSec != 1 {
		t.Errorf("window_sec = %d, want the 1s floor", f.WindowSec)
	}
}

func TestEndpointID(t *testing.T) {
	tests := []struct {
		ep   *kuberov1.FlowEndpoint
		want string
	}{
		{&kuberov1.FlowEndpoint{Kind: "pod", Ip: "10.0.0.1", Pod: &kuberov1.PodRef{Namespace: "a", Pod: "b"}}, "pod/a/b"},
		{&kuberov1.FlowEndpoint{Kind: "pod", Ip: "10.0.0.1"}, "ip/10.0.0.1"}, // pod without a ref
		{&kuberov1.FlowEndpoint{Kind: "service", Service: "ns/svc", Ip: "10.96.0.1"}, "service/ns/svc"},
		{&kuberov1.FlowEndpoint{Kind: "node", Name: "n1", Ip: "192.168.0.1"}, "node/n1"},
		{&kuberov1.FlowEndpoint{Kind: "external", Name: "dns.google", Ip: "8.8.8.8"}, "ip/8.8.8.8"},
	}
	for _, tt := range tests {
		if got := endpointID(tt.ep); got != tt.want {
			t.Errorf("endpointID(%v) = %q, want %q", tt.ep, got, tt.want)
		}
	}
}
