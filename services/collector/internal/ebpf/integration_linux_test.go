// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build linux && ebpfintegration

// Kernel integration tests: they load and attach the real programs, so
// they need root with CAP_BPF/CAP_PERFMON/CAP_SYS_ADMIN, the host PID
// namespace, the host cgroup v2 hierarchy and tracefs. Run them in a
// privileged container (see `make ebpf-test`):
//
//	docker run --rm --privileged --pid=host --cgroupns=host \
//	  -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel:/sys/kernel \
//	  -v $REPO:/src -w /src/services/collector golang:1.26.8 \
//	  go test -tags ebpfintegration -count=1 -v ./internal/ebpf/...

package ebpf

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

const (
	busyEnv     = "KH_EBPF_BUSY_CHILD"
	testUIDBusy = "0f0e0d0c-0b0a-4908-8706-050403020100"
	testCIDBusy = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	testUIDSelf = "11111111-2222-4333-8444-555555555555"
	testCIDSelf = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

func testLogger(t *testing.T) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("kernel integration tests need root in a privileged container")
	}
}

// sink collects emitted batches. keep filters rows so traffic from the
// rest of the (shared) host does not pile up.
type sink struct {
	mu       sync.Mutex
	keep     func(*kuberov1.Flow) bool
	flows    []*kuberov1.Flow
	profiles []*kuberov1.Profile
	calls    int
}

func (s *sink) emitFlows(_ context.Context, fs []*kuberov1.Flow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	for _, f := range fs {
		if s.keep == nil || s.keep(f) {
			s.flows = append(s.flows, f)
		}
	}
	return nil
}

func (s *sink) emitProfiles(_ context.Context, ps []*kuberov1.Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.profiles = append(s.profiles, ps...)
	return nil
}

func (s *sink) snapshotFlows() []*kuberov1.Flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*kuberov1.Flow(nil), s.flows...)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startTelemetry runs Start and returns a stop function that cancels
// and waits until the loops detached and flushed.
func startTelemetry(t *testing.T, cfg Config) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.Logger == nil {
		cfg.Logger = testLogger(t)
	}
	if err := Start(ctx, cfg); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			loops.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}

// localIPv4 is this container's first non-loopback IPv4 address.
func localIPv4(t *testing.T) netip.Addr {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if pfx, err := netip.ParsePrefix(a.String()); err == nil && pfx.Addr().Is4() {
				return pfx.Addr()
			}
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return netip.Addr{}
}

// lowPort picks a free port below the ephemeral range, so the "lower
// port is the server" heuristic is deterministic in tests.
func lowPort(t *testing.T, network string, ip netip.Addr) int {
	t.Helper()
	for p := 20000 + os.Getpid()%5000; p < 32000; p += 7 {
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(p))
		if network == "udp" {
			if c, err := net.ListenPacket("udp", addr); err == nil {
				_ = c.Close()
				return p
			}
		} else if l, err := net.Listen("tcp", addr); err == nil {
			_ = l.Close()
			return p
		}
	}
	t.Fatal("no free low port")
	return 0
}

// tcpExchange sends up bytes to a server on addr, which answers with
// down bytes.
func tcpExchange(t *testing.T, addr string, up, down int) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		if _, err := io.CopyN(io.Discard, c, int64(up)); err != nil {
			done <- err
			return
		}
		_, err = c.Write(make([]byte, down))
		done <- err
	}()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(make([]byte, up)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, c, int64(down)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// sumFlows adds up the rows matching the given 5-tuple-ish filter.
func sumFlows(fs []*kuberov1.Flow, srcIP, dstIP string, port int32, proto, dir string) (bytes, packets uint64, retrans uint32, rows int) {
	for _, f := range fs {
		if f.Src.Ip == srcIP && f.Dst.Ip == dstIP && f.Port == port && f.Protocol == proto && f.Direction == dir {
			bytes += f.Bytes
			packets += f.Packets
			retrans += f.Retransmits
			rows++
		}
	}
	return
}

func podResolver(ip netip.Addr) *fakeResolver {
	r := newFakeResolver()
	r.ips[ip] = &kuberov1.FlowEndpoint{Kind: "pod", Pod: &kuberov1.PodRef{Namespace: "kh-test", Pod: "self"}, Name: "self"}
	return r
}

func TestNetflowTCPOverPodIP(t *testing.T) {
	requireRoot(t)
	ip := localIPv4(t)
	port := lowPort(t, "tcp", ip)
	s := &sink{keep: func(f *kuberov1.Flow) bool {
		return f.Src.Ip == ip.String() || f.Dst.Ip == ip.String() || strings.HasPrefix(f.Src.Ip, "127.")
	}}
	stop := startTelemetry(t, Config{
		Netflow: true, FlushInterval: time.Second,
		Resolver: podResolver(ip), EmitFlows: s.emitFlows,
	})
	if st := Stats(); !st.NetflowAttached {
		t.Fatalf("Stats after Start: %+v", st)
	}

	const up, down = 1 << 20, 64 << 10
	tcpExchange(t, net.JoinHostPort(ip.String(), strconv.Itoa(port)), up, down)
	// Loopback traffic must not be reported without IncludeLoopback.
	tcpExchange(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(lowPort(t, "tcp", netip.MustParseAddr("127.0.0.1")))), 4096, 4096)

	payload := uint64(up + down)
	var egress, ingress uint64
	waitFor(t, 10*time.Second, "egress and ingress rows for the exchange", func() bool {
		fs := s.snapshotFlows()
		egress, _, _, _ = sumFlows(fs, ip.String(), ip.String(), int32(port), "tcp", "egress")
		ingress, _, _, _ = sumFlows(fs, ip.String(), ip.String(), int32(port), "tcp", "ingress")
		return egress >= payload && ingress >= payload
	})
	stop()

	fs := s.snapshotFlows()
	_, ePkts, _, eRows := sumFlows(fs, ip.String(), ip.String(), int32(port), "tcp", "egress")
	_, iPkts, _, _ := sumFlows(fs, ip.String(), ip.String(), int32(port), "tcp", "ingress")
	t.Logf("pod IP %s:%d: egress %d bytes / %d packets in %d rows, ingress %d bytes / %d packets (payload %d)",
		ip, port, egress, ePkts, eRows, ingress, iPkts, payload)
	// Both directions of the connection share (src, dst, port) because
	// client and server are the same pod: every byte is counted once per
	// hook. Headers add a little; double counting would add 100%.
	for name, b := range map[string]uint64{"egress": egress, "ingress": ingress} {
		if b < payload || b > payload+payload/10 {
			t.Errorf("%s bytes = %d, want within [%d, %d]", name, b, payload, payload+payload/10)
		}
	}
	if ePkts == 0 || iPkts == 0 {
		t.Error("packet counts missing")
	}
	for _, f := range fs {
		if f.Src.Ip == ip.String() {
			if f.Src.Kind != "pod" || f.Src.Pod.GetPod() != "self" || f.WindowSec < 1 || f.TsUnixMs == 0 {
				t.Errorf("endpoint attribution / window: %+v", f)
			}
		}
		if strings.HasPrefix(f.Src.Ip, "127.") || strings.HasPrefix(f.Dst.Ip, "127.") {
			t.Errorf("loopback flow reported without IncludeLoopback: %+v", f)
		}
	}
	st := Stats()
	if st.NetflowAttached || st.FlowsEmitted == 0 || st.FlowEntriesDrained == 0 {
		t.Errorf("Stats after stop: %+v", st)
	}
}

func TestNetflowLoopbackUDPAndIPv6(t *testing.T) {
	requireRoot(t)
	lo := netip.MustParseAddr("127.0.0.1")
	s := &sink{keep: func(f *kuberov1.Flow) bool {
		return strings.HasPrefix(f.Src.Ip, "127.") || f.Src.Ip == "::1"
	}}
	startTelemetry(t, Config{
		Netflow: true, IncludeLoopback: true, FlushInterval: time.Second,
		Resolver: newFakeResolver(), EmitFlows: s.emitFlows,
	})

	udpPort := lowPort(t, "udp", lo)
	srv, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(udpPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() { // drain so the socket buffer never drops
		buf := make([]byte, 2048)
		for {
			if _, _, err := srv.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	c, err := net.Dial("udp", srv.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	const datagrams, size = 20, 1000
	for i := 0; i < datagrams; i++ {
		if _, err := c.Write(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close()

	v6 := true
	v6Port := 0
	if l, err := net.Listen("tcp", "[::1]:0"); err != nil {
		v6 = false
		t.Logf("IPv6 loopback unavailable (%v); skipping the IPv6 half", err)
	} else {
		_ = l.Close()
		v6Port = lowPort(t, "tcp", netip.MustParseAddr("::1"))
		tcpExchange(t, net.JoinHostPort("::1", strconv.Itoa(v6Port)), 32<<10, 1024)
	}

	wantUDP := uint64(datagrams * (size + 28)) // + UDP/IPv4 headers
	waitFor(t, 10*time.Second, "loopback UDP (and IPv6 TCP) rows", func() bool {
		fs := s.snapshotFlows()
		ub, up, _, _ := sumFlows(fs, "127.0.0.1", "127.0.0.1", int32(udpPort), "udp", "egress")
		ok := ub >= wantUDP && up >= datagrams
		if v6 {
			vb, _, _, _ := sumFlows(fs, "::1", "::1", int32(v6Port), "tcp", "egress")
			ok = ok && vb >= 32<<10
		}
		return ok
	})
	fs := s.snapshotFlows()
	ub, up, _, _ := sumFlows(fs, "127.0.0.1", "127.0.0.1", int32(udpPort), "udp", "egress")
	ib, _, _, _ := sumFlows(fs, "127.0.0.1", "127.0.0.1", int32(udpPort), "udp", "ingress")
	t.Logf("UDP 127.0.0.1:%d egress %d bytes / %d packets, ingress %d bytes; IPv6 tested: %v", udpPort, ub, up, ib, v6)
	if ub != wantUDP || ib != wantUDP {
		t.Errorf("UDP bytes egress=%d ingress=%d, want exactly %d (20 x (1000 + 8 UDP + 20 IP))", ub, ib, wantUDP)
	}
}

// sendUDPWithOption sends n datagrams of size bytes from a socket that
// carries a sticky IP-level option (level/opt/val) to a local receiver
// on addr, returning the receiver's port.
func sendUDPWithOption(t *testing.T, network, addr string, level, opt int, val []byte, n, size int) int {
	t.Helper()
	srv, err := net.ListenPacket(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, _, err := srv.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	c, err := net.Dial(network, srv.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, err := c.(*net.UDPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptString(int(fd), level, opt, string(val))
	}); err != nil || serr != nil {
		t.Fatalf("setsockopt(%d, %d): %v %v", level, opt, err, serr)
	}
	for i := 0; i < n; i++ {
		if _, err := c.Write(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
	}
	return srv.LocalAddr().(*net.UDPAddr).Port
}

// TestNetflowHeaderParsing drives the BPF parser through headers longer
// than the minimum: IPv4 options (IHL 6) and an IPv6 destination options
// extension header. Ports must still be found, and byte counts must
// include exactly those extra header bytes.
func TestNetflowHeaderParsing(t *testing.T) {
	requireRoot(t)
	s := &sink{keep: func(f *kuberov1.Flow) bool { return f.Protocol == "udp" }}
	stop := startTelemetry(t, Config{
		Netflow: true, IncludeLoopback: true, FlushInterval: time.Second,
		Resolver: newFakeResolver(), EmitFlows: s.emitFlows,
	})
	const n, size = 10, 200

	// NOP, NOP, NOP, EOL: four bytes of options -> 24-byte IPv4 header.
	v4Port := sendUDPWithOption(t, "udp4", "127.0.0.1:0", syscall.IPPROTO_IP, syscall.IP_OPTIONS, []byte{1, 1, 1, 0}, n, size)
	wantV4 := uint64(n * (size + 8 + 24))

	// Destination options header: next header (filled in by the kernel),
	// length 0 (8 bytes in all), one PadN option covering the other 6.
	v6 := true
	v6Port := 0
	if l, err := net.ListenPacket("udp6", "[::1]:0"); err != nil {
		v6 = false
		t.Logf("IPv6 loopback unavailable (%v); skipping the extension-header half", err)
	} else {
		_ = l.Close()
		const ipv6DstOpts = 59 // IPV6_DSTOPTS, include/uapi/linux/in6.h
		v6Port = sendUDPWithOption(t, "udp6", "[::1]:0", syscall.IPPROTO_IPV6, ipv6DstOpts, []byte{0, 0, 1, 4, 0, 0, 0, 0}, n, size)
	}
	wantV6 := uint64(n * (size + 8 + 40 + 8))

	check := func() (v4b, v6b uint64) {
		fs := s.snapshotFlows()
		v4b, _, _, _ = sumFlows(fs, "127.0.0.1", "127.0.0.1", int32(v4Port), "udp", "egress")
		v6b, _, _, _ = sumFlows(fs, "::1", "::1", int32(v6Port), "udp", "egress")
		return v4b, v6b
	}
	waitFor(t, 10*time.Second, "UDP rows with their ports", func() bool {
		v4b, v6b := check()
		return v4b >= wantV4 && (!v6 || v6b >= wantV6)
	})
	stop()
	v4b, v6b := check()
	t.Logf("IPv4 with options: %d bytes on port %d (want %d); IPv6 with dest opts: %d bytes on port %d (want %d, tested %v)",
		v4b, v4Port, wantV4, v6b, v6Port, wantV6, v6)
	if v4b != wantV4 || (v6 && v6b != wantV6) {
		t.Errorf("byte counts off: v4 %d want %d, v6 %d want %d", v4b, wantV4, v6b, wantV6)
	}
	for _, f := range s.snapshotFlows() {
		if (f.Src.Ip == "127.0.0.1" || f.Src.Ip == "::1") && f.Port == 0 {
			t.Errorf("loopback UDP row without a port (header walk failed): %+v", f)
		}
	}
}

// TestNetflowRetransmits provokes SYN retransmits: a listener with a
// full accept queue silently drops new SYNs, and the client's kernel
// retransmits them after the 1s initial RTO.
func TestNetflowRetransmits(t *testing.T) {
	requireRoot(t)
	ip := localIPv4(t)
	port := lowPort(t, "tcp", ip)
	s := &sink{keep: func(f *kuberov1.Flow) bool { return f.Dst.Ip == ip.String() }}
	startTelemetry(t, Config{
		Netflow: true, FlushInterval: time.Second,
		Resolver: podResolver(ip), EmitFlows: s.emitFlows,
	})
	if !Stats().RetransmitsAttached {
		t.Fatal("tcp_retransmit_skb tracepoint not attached (is tracefs mounted?)")
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: ip.As4()}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil { // backlog 0: one queued connection, then drops
		t.Fatal(err)
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		defer c.Close() // fills the accept queue
	}
	start := time.Now()
	if c, err := net.DialTimeout("tcp", addr, 2500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Log("second dial unexpectedly connected")
	}
	t.Logf("stalled dial returned after %s", time.Since(start).Round(time.Millisecond))

	var retrans uint32
	waitFor(t, 10*time.Second, "retransmits on the SYN flow", func() bool {
		_, _, retrans, _ = sumFlows(s.snapshotFlows(), ip.String(), ip.String(), int32(port), "tcp", "egress")
		return retrans >= 1
	})
	t.Logf("retransmits reported for %s:%d: %d", ip, port, retrans)
}

// TestNetflowIterDrainFallback runs the pre-5.6 drain path (per-key
// lookup-and-delete) against the real kernel.
func TestNetflowIterDrainFallback(t *testing.T) {
	requireRoot(t)
	forceIterDrain = true
	t.Cleanup(func() { forceIterDrain = false })
	ip := localIPv4(t)
	port := lowPort(t, "tcp", ip)
	s := &sink{keep: func(f *kuberov1.Flow) bool { return f.Src.Ip == ip.String() }}
	stop := startTelemetry(t, Config{Netflow: true, FlushInterval: time.Second, Resolver: podResolver(ip), EmitFlows: s.emitFlows})
	tcpExchange(t, net.JoinHostPort(ip.String(), strconv.Itoa(port)), 256<<10, 1024)
	waitFor(t, 10*time.Second, "flows via the iterate drain", func() bool {
		b, _, _, _ := sumFlows(s.snapshotFlows(), ip.String(), ip.String(), int32(port), "tcp", "egress")
		return b >= 257<<10
	})
	stop()
}

// ─── profiler ────────────────────────────────────────────────────────────

//go:noinline
func khBusyLoop(until time.Time) uint64 {
	var x uint64 = 1
	for time.Now().Before(until) {
		for i := 0; i < 2_000_000; i++ {
			x = x*6364136223846793005 + 1442695040888963407
		}
	}
	return x
}

//go:noinline
func khSyscallLoop(until time.Time) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 64)
	for time.Now().Before(until) {
		for i := 0; i < 1000; i++ {
			_, _ = f.Write(buf)
		}
	}
}

// TestHelperBusyProcess is the child process of the profiler test: it
// waits for a byte on stdin (sent once it sits in its cgroup), then
// burns one CPU in khBusyLoop and one in khSyscallLoop.
func TestHelperBusyProcess(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv(busyEnv))
	if err != nil {
		t.Skip("helper process only")
	}
	if _, err := os.Stdin.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(d)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); khBusyLoop(until) }()
	go func() { defer wg.Done(); khSyscallLoop(until) }()
	wg.Wait()
}

// fakeContainerCgroup creates a kubepods-style container cgroup under
// dir and returns its path.
func fakeContainerCgroup(t *testing.T, dir, uid, cid string) string {
	t.Helper()
	p := filepath.Join(dir, "kubepods.slice", "kubepods-besteffort.slice",
		"kubepods-besteffort-pod"+strings.ReplaceAll(uid, "-", "_")+".slice",
		"cri-containerd-"+cid+".scope")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("creating cgroup %s: %v", p, err)
	}
	return p
}

func moveToCgroup(t *testing.T, cgroup string, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cgroup, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0); err != nil {
		t.Fatalf("moving %d to %s: %v", pid, cgroup, err)
	}
}

// ownCgroup is this process's cgroup v2 directory (needs --cgroupns=host
// for the path to be meaningful on the mounted hierarchy).
func ownCgroup(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			p := filepath.Join(defaultCgroupRoot, rel)
			if rel == "/" {
				t.Skip("private cgroup namespace; run the container with --cgroupns=host")
			}
			return p
		}
	}
	t.Fatal("no cgroup v2 entry in /proc/self/cgroup")
	return ""
}

func removeCgroupTree(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- { // leaves first
		_ = syscall.Rmdir(dirs[i])
	}
}

func TestProfilerAttributesAndSymbolizes(t *testing.T) {
	requireRoot(t)
	home := ownCgroup(t)
	// Leftovers of a crashed run would confuse nothing, but tidy them.
	if old, _ := filepath.Glob(filepath.Join(defaultCgroupRoot, "kh-ebpf-test-*")); len(old) > 0 {
		for _, d := range old {
			removeCgroupTree(d)
		}
	}
	base := filepath.Join(defaultCgroupRoot, fmt.Sprintf("kh-ebpf-test-%d", os.Getpid()))
	busyCg := fakeContainerCgroup(t, base, testUIDBusy, testCIDBusy)
	selfCg := fakeContainerCgroup(t, base, testUIDSelf, testCIDSelf)
	t.Cleanup(func() { removeCgroupTree(base) })

	r := newFakeResolver()
	r.pods[[2]string{testUIDBusy, testCIDBusy}] = &kuberov1.PodRef{
		Namespace: "kh-test", Pod: "busy-7d9f", Container: "spin", Workload: "busy", WorkloadKind: "Deployment", PodUid: testUIDBusy,
	}
	r.pods[[2]string{testUIDSelf, testCIDSelf}] = &kuberov1.PodRef{Namespace: "kh-test", Pod: "collector", Container: "self", Workload: "collector"}
	s := &sink{}
	const hz = 99
	stop := startTelemetry(t, Config{Profiler: true, ProfileHz: hz, FlushInterval: time.Second, Resolver: r, EmitProfiles: s.emitProfiles})
	if !Stats().ProfilerAttached {
		t.Fatal("profiler not attached")
	}

	const busyFor = 3 * time.Second
	child := exec.Command(os.Args[0], "-test.run=^TestHelperBusyProcess$")
	child.Env = append(os.Environ(), busyEnv+"="+busyFor.String())
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stdout, child.Stderr = os.Stderr, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	moveToCgroup(t, busyCg, child.Process.Pid)

	// This process ("the collector") moves into a container cgroup too
	// (after forking the child, which must not start out in it): its own
	// samples must be skipped in the kernel, never attributed.
	moveToCgroup(t, selfCg, os.Getpid())
	t.Cleanup(func() { moveToCgroup(t, home, os.Getpid()) })
	selfDone := make(chan struct{})
	go func() { defer close(selfDone); khBusyLoop(time.Now().Add(busyFor)) }() // "collector" burns CPU as well
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("busy child: %v", err)
	}
	<-selfDone
	childCPU := child.ProcessState.UserTime() + child.ProcessState.SystemTime()
	time.Sleep(1500 * time.Millisecond) // let the last window close
	stop()

	s.mu.Lock()
	profiles := append([]*kuberov1.Profile(nil), s.profiles...)
	s.mu.Unlock()

	var total int64
	var busyVal, sysVal, kernelVal int64
	nProfiles := 0
	var heaviest *kuberov1.StackSample
	for _, p := range profiles {
		switch p.Source.GetContainer() {
		case "self":
			t.Errorf("the collector's own samples were attributed: %d stacks", len(p.Samples))
			continue
		case "spin":
		default:
			continue
		}
		nProfiles++
		if p.Type != "cpu" || p.Unit != "nanoseconds" || p.Origin != "ebpf" || p.Period != int64(time.Second)/hz ||
			p.Service != "busy" || p.Source.GetPod() != "busy-7d9f" || p.DurationNano <= 0 || p.TsUnixNano <= 0 {
			t.Errorf("profile header: %+v", p)
		}
		for _, smp := range p.Samples {
			total += smp.Value
			joined := strings.Join(smp.Frames, ";")
			if strings.Contains(joined, ".khBusyLoop") {
				busyVal += smp.Value
			}
			if strings.Contains(joined, ".khSyscallLoop") {
				sysVal += smp.Value
				if strings.Contains(joined, " [k]") {
					kernelVal += smp.Value
				}
			}
			if heaviest == nil || smp.Value > heaviest.Value {
				heaviest = smp
			}
		}
	}
	if nProfiles == 0 {
		t.Fatalf("no profile for the busy container among %d profiles", len(profiles))
	}
	t.Logf("busy child: rusage CPU %s; profiled %s in %d profiles (khBusyLoop %s, khSyscallLoop %s, of which kernel-side %s)",
		childCPU, time.Duration(total), nProfiles, time.Duration(busyVal), time.Duration(sysVal), time.Duration(kernelVal))
	t.Logf("heaviest stack (%s): %s", time.Duration(heaviest.Value), strings.Join(heaviest.Frames, " > "))

	// Sampled CPU time must match what the kernel accounted to the child.
	if lo, hi := childCPU*6/10, childCPU*14/10; time.Duration(total) < lo || time.Duration(total) > hi {
		t.Errorf("profiled CPU %s, want within [%s, %s] of rusage %s", time.Duration(total), lo, hi, childCPU)
	}
	if busyVal < total/4 || sysVal < total/4 {
		t.Errorf("symbolized goroutine functions carry too little weight: busy %d, syscall %d of %d", busyVal, sysVal, total)
	}
	if kernelVal == 0 {
		t.Error("no kernel frames under khSyscallLoop: kernel stacks or kallsyms symbolization failed")
	}
	st := Stats()
	if st.ProfilerAttached || st.ProfilesEmitted == 0 || st.SamplesDrained == 0 {
		t.Errorf("Stats after stop: %+v", st)
	}
	t.Logf("stats: %+v", st)
}

// TestSymbolizeSharedLibraryDynsym symbolizes libc (stripped: .dynsym
// only) in a live process, through map_files and PT_LOAD translation.
func TestSymbolizeSharedLibraryDynsym(t *testing.T) {
	requireRoot(t)
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	defer func() { _ = sleep.Process.Kill(); _, _ = sleep.Process.Wait() }()
	pid := uint32(sleep.Process.Pid)
	time.Sleep(200 * time.Millisecond) // let the dynamic loader map libc

	sym := newSymbolizer(procRoot, testLogger(t))
	sym.beginDrain()
	pm := sym.procMaps(pid)
	var libc *mapping
	for i := range pm {
		if strings.Contains(filepath.Base(pm[i].path), "libc.so") {
			libc = &pm[i]
		}
	}
	if libc == nil {
		t.Skip("sleep is not dynamically linked against libc here")
	}
	f, err := elf.Open(fmt.Sprintf("/proc/%d/map_files/%x-%x", pid, libc.start, libc.end))
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := f.DynamicSymbols()
	if err != nil {
		t.Fatal(err)
	}
	names := map[uint64][]string{}
	var target elf.Symbol
	for _, s := range dyn {
		if elf.ST_TYPE(s.Info) == elf.STT_FUNC && s.Value != 0 {
			names[s.Value] = append(names[s.Value], s.Name)
			if s.Name == "clock_nanosleep" || (target.Name == "" && s.Name == "nanosleep") {
				target = s
			}
		}
	}
	if target.Name == "" {
		t.Skip("no clock_nanosleep/nanosleep in libc's dynsym")
	}
	var fileOff uint64
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && target.Value >= p.Vaddr && target.Value < p.Vaddr+p.Filesz {
			fileOff = target.Value - p.Vaddr + p.Off
		}
	}
	_ = f.Close()
	runtimeAddr := libc.start + (fileOff - libc.offset) + 4 // a few bytes into the function
	got := sym.userFrames(pid, []uint64{runtimeAddr})[0]
	ok := false
	for _, n := range names[target.Value] {
		ok = ok || n == got
	}
	if !ok {
		t.Fatalf("libc %s at %#x symbolized as %q, want one of %v", target.Name, runtimeAddr, got, names[target.Value])
	}
	es := sym.binary(pid, libc)
	t.Logf("libc %s (%s, %d symbols from %s) -> %q", libc.path, target.Name, es.table.len(), es.source, got)
}

func TestStartReportsNothingEnabled(t *testing.T) {
	if err := Start(context.Background(), Config{}); err != nil {
		t.Fatalf("Start with nothing enabled: %v", err)
	}
	err := Start(context.Background(), Config{Netflow: true, CgroupRoot: "/proc", Resolver: newFakeResolver(), EmitFlows: emitFlows})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("non-cgroup2 root: err = %v, want ErrUnsupported", err)
	}
}

// TestStartDegradesUnprivileged checks Start in a container without
// privileges (set KH_EBPF_EXPECT=unsupported):
//
//	docker run --rm -e KH_EBPF_EXPECT=unsupported ... go test -tags ebpfintegration -run Degrades ./internal/ebpf/
func TestStartDegradesUnprivileged(t *testing.T) {
	if os.Getenv("KH_EBPF_EXPECT") != "unsupported" {
		t.Skip("run in an unprivileged container with KH_EBPF_EXPECT=unsupported")
	}
	s := &sink{}
	err := Start(context.Background(), Config{
		Netflow: true, Profiler: true, Resolver: newFakeResolver(),
		EmitFlows: s.emitFlows, EmitProfiles: s.emitProfiles, Logger: testLogger(t),
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Start = %v, want an error wrapping ErrUnsupported", err)
	}
	if st := Stats(); st.NetflowAttached || st.ProfilerAttached {
		t.Fatalf("nothing may stay attached: %+v", st)
	}
	t.Logf("Start: %v", err)
}

// TestStartDegradesWithoutHostPID checks subsystem independence in a
// privileged container without --pid=host (KH_EBPF_EXPECT=no-hostpid):
// the profiler must fail alone, netflow must run, Start must succeed.
func TestStartDegradesWithoutHostPID(t *testing.T) {
	if os.Getenv("KH_EBPF_EXPECT") != "no-hostpid" {
		t.Skip("run in a privileged container without --pid=host, KH_EBPF_EXPECT=no-hostpid")
	}
	s := &sink{}
	stop := startTelemetry(t, Config{
		Netflow: true, Profiler: true, FlushInterval: time.Second, Resolver: newFakeResolver(),
		EmitFlows: s.emitFlows, EmitProfiles: s.emitProfiles,
	})
	st := Stats()
	if !st.NetflowAttached || st.ProfilerAttached {
		t.Fatalf("want netflow attached and the profiler not: %+v", st)
	}
	stop()
}
