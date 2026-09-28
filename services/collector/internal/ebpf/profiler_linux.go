// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build linux

package ebpf

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
	"unsafe"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

const (
	// flipGrace is how long a drain waits after switching buffers for
	// samples that read the old selector to finish writing. BPF programs
	// on perf events run for microseconds in interrupt context.
	flipGrace = 10 * time.Millisecond
	// maxStacksPerDrain caps unique (process, stack) entries symbolized
	// per drain; the heaviest are kept.
	maxStacksPerDrain = 50000

	profErrCountsFull = 0
	profErrStackLost  = 1
)

// cpuEvent is one CPU's sampling perf event and its program attachment.
type cpuEvent struct {
	fd   int
	link link.Link // nil when attached with the legacy ioctl
}

func (e *cpuEvent) close() {
	if e.link != nil {
		_ = e.link.Close()
	}
	_ = unix.Close(e.fd)
}

// profiler owns the sampling program, its perf events and drain loop.
type profiler struct {
	cfg Config
	log *slog.Logger

	objs   profilerObjects
	events []*cpuEvent

	active      uint32 // buffer set the kernel currently writes to
	windowStart time.Time

	sym     *symbolizer
	cgroups *cgroupIndex

	countsFull, stacksLost deltaCounter
}

func startProfiler(ctx context.Context, cfg Config, log *slog.Logger) (err error) {
	if !inHostPIDNamespace() {
		return unsupported(errors.New("the profiler needs the host PID namespace (hostPID: true): " +
			"it reads other processes' memory maps and must know its own host PID"))
	}
	spec, err := loadProfiler()
	if err != nil {
		return err
	}
	for _, name := range []string{profilerMapKhCounts0, profilerMapKhCounts1} {
		if err := checkLayout(spec.Maps[name], stackKey{}, uint64(0)); err != nil {
			return err
		}
	}
	// The program skips samples of this process: symbolizing ourselves
	// would only profile the profiler.
	if err := setVariables(spec, map[string]any{profilerVarSelfTgid: uint32(os.Getpid())}); err != nil {
		return err
	}

	p := &profiler{cfg: cfg, log: log}
	if err := spec.LoadAndAssign(&p.objs, nil); err != nil {
		return unsupported(fmt.Errorf("loading profiler program: %w", err))
	}
	defer func() {
		if err != nil {
			p.detach()
			_ = p.objs.Close()
		}
	}()

	cpus, err := onlineCPUs()
	if err != nil {
		return unsupported(err)
	}
	var errs []error
	for _, cpu := range cpus {
		ev, err := openCPUSampler(cpu, cfg.ProfileHz, p.objs.KhProfile)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		p.events = append(p.events, ev)
	}
	if len(p.events) == 0 {
		return unsupported(fmt.Errorf("no CPU could be sampled: %w", errors.Join(errs...)))
	}
	if len(errs) > 0 {
		log.Warn("ebpf profiler: some CPUs are not sampled", "sampled", len(p.events), "failed", len(errs), "err", errs[0])
	}

	p.sym = newSymbolizer(procRoot, log)
	p.cgroups = newCgroupIndex(cfg.CgroupRoot)
	p.windowStart = time.Now()
	counters.profilerAttached.Store(true)
	log.Info("ebpf profiler attached", "cpus", len(p.events), "hz", cfg.ProfileHz, "flush_interval", cfg.FlushInterval)
	loops.Add(1)
	go p.run(ctx)
	return nil
}

func onlineCPUs() ([]int, error) {
	b, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil, err
	}
	return parseCPUList(string(b))
}

// openCPUSampler opens a CPU-clock sampling event on cpu (all tasks) and
// attaches prog. For cpu-clock, frequency mode is turned by the kernel
// into a fixed period of 1e9/hz ns, which is what makes "samples x
// period" a CPU-time estimate.
func openCPUSampler(cpu, hz int, prog *cebpf.Program) (*cpuEvent, error) {
	attr := unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: uint64(hz),
		Bits:   unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	fd, err := unix.PerfEventOpen(&attr, -1, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("perf_event_open(cpu %d): %w", cpu, err)
	}
	ev := &cpuEvent{fd: fd}
	// A bpf_link (5.15+) is the modern attachment; older kernels attach
	// through an ioctl on the event, released when the fd closes.
	if l, err := link.AttachRawLink(link.RawLinkOptions{Target: fd, Program: prog, Attach: cebpf.AttachPerfEvent}); err == nil {
		ev.link = l
	} else if ioErr := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_SET_BPF, prog.FD()); ioErr != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("attaching to cpu %d: %w", cpu, errors.Join(err, ioErr))
	}
	if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
		ev.close()
		return nil, fmt.Errorf("enabling cpu %d sampler: %w", cpu, err)
	}
	return ev, nil
}

func (p *profiler) run(ctx context.Context) {
	defer loops.Done()
	t := time.NewTicker(p.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			guard(p.log, "profiler", func() { p.flush(ctx) })
		case <-ctx.Done():
			p.detach()
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
			guard(p.log, "profiler", func() { p.flush(fctx) })
			cancel()
			_ = p.objs.Close()
			p.log.Info("ebpf profiler detached")
			return
		}
	}
}

func (p *profiler) buffers(set uint32) (counts, stacks *cebpf.Map) {
	if set == 0 {
		return p.objs.KhCounts0, p.objs.KhStacks0
	}
	return p.objs.KhCounts1, p.objs.KhStacks1
}

// flush switches the kernel to the other buffer set, then drains,
// symbolizes and clears the one it just left.
func (p *profiler) flush(ctx context.Context) {
	idle, next := p.active, p.active^1
	if err := p.objs.KhActive.Update(uint32(0), next, cebpf.UpdateAny); err != nil {
		counters.drainErrors.Add(1)
		p.log.Warn("ebpf profiler: switching buffers failed", "err", err)
		return
	}
	p.active = next
	end := time.Now()
	start := p.windowStart
	p.windowStart = end
	time.Sleep(flipGrace)

	counts, stacks := p.buffers(idle)
	var keys []stackKey
	var vals []uint64
	if _, err := drainMap(counts, func(ks []stackKey, vs []uint64) {
		keys = append(keys, ks...)
		vals = append(vals, vs...)
	}); err != nil {
		counters.drainErrors.Add(1)
		p.log.Warn("ebpf profiler: counts drain failed", "err", err)
	}
	p.readKernelErrors()

	var samples uint64
	for _, v := range vals {
		samples += v
	}
	counters.samplesDrained.Add(samples)
	keys, vals, capped := topSamples(keys, vals, maxStacksPerDrain)
	if capped > 0 {
		counters.stacksCapped.Add(uint64(capped))
		p.log.Warn("ebpf profiler: too many distinct stacks in one window; lightest dropped",
			"kept", len(keys), "dropped", capped)
	}

	// Leave the idle set empty whatever happens below: counts a failed
	// drain left behind would reference stack ids that are about to be
	// reused, and stale stack buckets would make next window's stacks
	// collide. Counts go first so nothing references a cleared stack.
	defer func() {
		if n, _ := drainMap(counts, func([]stackKey, []uint64) {}); n > 0 {
			p.log.Warn("ebpf profiler: discarded counts a failed drain left behind", "entries", n)
		}
		if err := clearStackMap(stacks); err != nil {
			counters.drainErrors.Add(1)
			p.log.Warn("ebpf profiler: clearing stack map failed", "err", err)
		}
	}()
	profiles := p.build(keys, vals, stacks, start, end.Sub(start))

	emitted := 0
	for _, batch := range batchProfiles(profiles, profileEmitSamples) {
		if err := p.cfg.EmitProfiles(ctx, batch); err != nil {
			counters.drainErrors.Add(1)
			p.log.Warn("ebpf: EmitProfiles failed", "err", err, "profiles", len(batch))
			continue
		}
		emitted += len(batch)
		counters.profilesEmitted.Add(uint64(len(batch)))
	}
	p.log.Debug("ebpf profiler drain", "samples", samples, "stacks", len(keys),
		"profiles", len(profiles), "emitted", emitted, "window", end.Sub(start))
}

// attribution is a cgroup's resolved Kubernetes identity.
type attribution struct {
	ref     *kuberov1.PodRef
	service string
}

func (p *profiler) attribute(cgroupID uint64) *attribution {
	c, ok := p.cgroups.lookup(cgroupID)
	if !ok {
		return nil
	}
	ref, ok := p.cfg.Resolver.LookupContainer(c.podUID, c.containerID)
	if !ok || ref == nil {
		return nil
	}
	ref = proto.Clone(ref).(*kuberov1.PodRef) // never alias the resolver's cache
	return &attribution{ref: ref, service: p.cfg.Resolver.ServiceName(ref)}
}

// build turns drained samples into profiles. Resolution and
// symbolization are memoized per drain: many samples share a cgroup,
// a process and its stacks.
func (p *profiler) build(keys []stackKey, counts []uint64, stacks *cebpf.Map, start time.Time, window time.Duration) []*kuberov1.Profile {
	p.sym.beginDrain()
	b := newProfileBuilder(start, window, p.cfg.ProfileHz)

	containers := make(map[uint64]*attribution)
	type userStack struct {
		tgid uint32
		id   int32
	}
	userCache := make(map[userStack][]string)
	kernelCache := make(map[int32][]string)
	var raw [stackDepth]uint64
	readStack := func(id int32) []uint64 {
		if id < 0 {
			return nil // no stack recorded (kernel thread, user-mode sample, lost)
		}
		if err := stacks.Lookup(uint32(id), &raw); err != nil {
			return nil
		}
		return stackIPs(&raw)
	}

	for i, k := range keys {
		a, seen := containers[k.CgroupID]
		if !seen {
			a = p.attribute(k.CgroupID)
			containers[k.CgroupID] = a
		}
		if a == nil {
			counters.samplesUnattributed.Add(counts[i])
			continue
		}
		us := userStack{k.Tgid, k.UserStackID}
		user, ok := userCache[us]
		if !ok {
			user = p.sym.userFrames(k.Tgid, readStack(k.UserStackID))
			userCache[us] = user
		}
		kern, ok := kernelCache[k.KernelStackID]
		if !ok {
			kern = p.sym.kernelFrames(readStack(k.KernelStackID))
			kernelCache[k.KernelStackID] = kern
		}
		b.add(a.ref, a.service, assembleFrames(user, kern), counts[i])
	}
	return b.profiles()
}

// clearStackMap empties a STACK_TRACE map. Ids are collected before any
// delete: the kernel's get_next_key restarts from bucket 0 when handed
// an id whose bucket is empty, which would make delete-as-you-walk
// quadratic in the bucket count.
func clearStackMap(m *cebpf.Map) error {
	var ids []uint32
	var id uint32
	err := m.NextKey(nil, &id)
	for err == nil && len(ids) <= int(m.MaxEntries()) {
		ids = append(ids, id)
		err = m.NextKey(id, &id)
	}
	if err != nil && !errors.Is(err, cebpf.ErrKeyNotExist) {
		return err
	}
	for _, id := range ids {
		if err := m.Delete(id); err != nil && !errors.Is(err, cebpf.ErrKeyNotExist) {
			return err
		}
	}
	return nil
}

func (p *profiler) readKernelErrors() {
	if total, err := sumPerCPU(p.objs.KhProfErrors, profErrCountsFull); err == nil {
		if d := p.countsFull.delta(total); d > 0 {
			counters.mapFullEvents.Add(d)
			p.log.Warn("ebpf profiler: samples lost, counts table full", "count", d)
		}
	}
	if total, err := sumPerCPU(p.objs.KhProfErrors, profErrStackLost); err == nil {
		if d := p.stacksLost.delta(total); d > 0 {
			counters.stacksLost.Add(d)
		}
	}
}

func (p *profiler) detach() {
	for _, ev := range p.events {
		ev.close()
	}
	p.events = nil
	counters.profilerAttached.Store(false)
}
