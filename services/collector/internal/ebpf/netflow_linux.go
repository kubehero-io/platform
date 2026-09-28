// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build linux

package ebpf

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// Slots of the per-CPU error arrays (bpf/netflow.bpf.c, tcpretrans.bpf.c).
const (
	nfErrFlowUpdate = 0
	rtErrUpdate     = 0
	rtErrRead       = 1
)

// netflow owns the flow accounting programs and their drain loop.
type netflow struct {
	cfg        Config
	log        *slog.Logger
	maxEntries uint32

	objs  netflowObjects
	links []link.Link

	rt     *tcpretransObjects // nil when retransmit counting is unavailable
	rtLink link.Link

	lastDrain                    time.Time
	flowUpdateErrs, rtUpdateErrs deltaCounter
	rtReadErrs                   deltaCounter
}

func startNetflow(ctx context.Context, cfg Config, entries uint32, log *slog.Logger) (err error) {
	spec, err := loadNetflow()
	if err != nil {
		return err
	}
	if err := checkLayout(spec.Maps[netflowMapKhFlows], flowKey{}, flowValue{}); err != nil {
		return err
	}
	spec.Maps[netflowMapKhFlows].MaxEntries = entries
	if err := setVariables(spec, map[string]any{
		netflowVarIncludeLoopback: boolByte(cfg.IncludeLoopback),
	}); err != nil {
		return err
	}

	nf := &netflow{cfg: cfg, log: log, maxEntries: entries}
	if err := spec.LoadAndAssign(&nf.objs, nil); err != nil {
		return unsupported(fmt.Errorf("loading flow programs: %w", err))
	}
	defer func() {
		if err != nil {
			nf.detach()
			nf.closeObjects()
		}
	}()

	// link.AttachCgroup uses bpf_link (5.7+) or falls back to
	// BPF_F_ALLOW_MULTI, so programs other agents (Cilium, Calico,
	// systemd) keep on the root cgroup are left alone.
	for _, a := range []struct {
		prog *cebpf.Program
		typ  cebpf.AttachType
	}{
		{nf.objs.KhFlowEgress, cebpf.AttachCGroupInetEgress},
		{nf.objs.KhFlowIngress, cebpf.AttachCGroupInetIngress},
	} {
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cfg.CgroupRoot, Attach: a.typ, Program: a.prog})
		if err != nil {
			return unsupported(fmt.Errorf("attaching %s to %s: %w", a.typ, cfg.CgroupRoot, err))
		}
		nf.links = append(nf.links, l)
	}

	// Retransmits are a refinement: without tracefs (or on a kernel whose
	// tracepoint lacks the fields we read) flows still flow.
	if err := nf.startRetransmits(); err != nil {
		log.Warn("ebpf: TCP retransmit counting unavailable; flows report 0 retransmits", "err", err)
	}

	nf.lastDrain = time.Now()
	counters.netflowAttached.Store(true)
	log.Info("ebpf netflow attached",
		"cgroup_root", cfg.CgroupRoot, "flow_map_entries", entries,
		"retransmits", nf.rt != nil, "flush_interval", cfg.FlushInterval)
	loops.Add(1)
	go nf.run(ctx)
	return nil
}

func (nf *netflow) startRetransmits() error {
	offs, err := readRetransmitOffsets()
	if err != nil {
		return err
	}
	spec, err := loadTcpretrans()
	if err != nil {
		return err
	}
	if err := checkLayout(spec.Maps[tcpretransMapKhRetrans], flowKey{}, uint64(0)); err != nil {
		return err
	}
	if err := setVariables(spec, map[string]any{
		tcpretransVarIncludeLoopback: boolByte(nf.cfg.IncludeLoopback),
		tcpretransVarOffSport:        offs.Sport,
		tcpretransVarOffDport:        offs.Dport,
		tcpretransVarOffSaddrV6:      offs.SaddrV6,
		tcpretransVarOffDaddrV6:      offs.DaddrV6,
	}); err != nil {
		return err
	}
	var objs tcpretransObjects
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		return fmt.Errorf("loading retransmit program: %w", err)
	}
	l, err := link.Tracepoint("tcp", "tcp_retransmit_skb", objs.KhTcpRetransmit, nil)
	if err != nil {
		_ = objs.Close()
		return fmt.Errorf("attaching tcp:tcp_retransmit_skb: %w", err)
	}
	nf.rt, nf.rtLink = &objs, l
	counters.retransmitsAttached.Store(true)
	return nil
}

func (nf *netflow) run(ctx context.Context) {
	defer loops.Done()
	t := time.NewTicker(nf.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			nf.flush(ctx)
		case <-ctx.Done():
			// Stop counting, then ship the final partial window with a
			// deadline of its own (ctx is already cancelled).
			nf.detach()
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
			nf.flush(fctx)
			cancel()
			nf.closeObjects()
			nf.log.Info("ebpf netflow detached")
			return
		}
	}
}

// flush drains both maps, translates them into Flow rows and emits them.
func (nf *netflow) flush(ctx context.Context) {
	now := time.Now()
	window := now.Sub(nf.lastDrain)
	nf.lastDrain = now

	agg := newFlowAggregator(nf.cfg.Resolver, nf.cfg.IncludeLoopback)
	n, err := drainMap(nf.objs.KhFlows, func(ks []flowKey, vs []flowValue) {
		for i := range ks {
			agg.addFlow(&ks[i], &vs[i])
		}
	})
	counters.flowEntriesDrained.Add(uint64(n))
	if err != nil {
		counters.drainErrors.Add(1)
		nf.log.Warn("ebpf: flow map drain failed", "err", err, "drained", n)
	}
	if uint64(n)*10 >= uint64(nf.maxEntries)*9 {
		counters.mapFullEvents.Add(1)
		nf.log.Warn("ebpf: flow table reached 90% of capacity; the least recently updated flows may have been evicted",
			"entries", n, "capacity", nf.maxEntries,
			"hint", "shorten the flush interval or raise "+FlowMapEntriesEnv)
	}
	if nf.rt != nil {
		if _, err := drainMap(nf.rt.KhRetrans, func(ks []flowKey, vs []uint64) {
			for i := range ks {
				agg.addRetransmits(&ks[i], vs[i])
			}
		}); err != nil {
			counters.drainErrors.Add(1)
			nf.log.Warn("ebpf: retransmit map drain failed", "err", err)
		}
	}
	nf.readKernelErrors()

	flows := agg.flows(now, window)
	emitted := 0
	for _, batch := range batchFlows(flows, flowEmitBatch) {
		if err := nf.cfg.EmitFlows(ctx, batch); err != nil {
			counters.drainErrors.Add(1)
			nf.log.Warn("ebpf: EmitFlows failed", "err", err, "flows", len(batch))
			continue
		}
		emitted += len(batch)
		counters.flowsEmitted.Add(uint64(len(batch)))
	}
	nf.log.Debug("ebpf netflow drain", "entries", n, "rows", len(flows), "emitted", emitted,
		"skipped", agg.skipped, "window", window)
}

// readKernelErrors folds the programs' cumulative failure counters into
// the package counters.
func (nf *netflow) readKernelErrors() {
	if total, err := sumPerCPU(nf.objs.KhNfErrors, nfErrFlowUpdate); err == nil {
		if d := nf.flowUpdateErrs.delta(total); d > 0 {
			counters.mapFullEvents.Add(d)
			nf.log.Warn("ebpf: flow table updates failed in the kernel", "count", d)
		}
	}
	if nf.rt == nil {
		return
	}
	if total, err := sumPerCPU(nf.rt.KhRtErrors, rtErrUpdate); err == nil {
		if d := nf.rtUpdateErrs.delta(total); d > 0 {
			counters.mapFullEvents.Add(d)
			nf.log.Warn("ebpf: retransmit table updates failed in the kernel", "count", d)
		}
	}
	if total, err := sumPerCPU(nf.rt.KhRtErrors, rtErrRead); err == nil {
		if d := nf.rtReadErrs.delta(total); d > 0 {
			nf.log.Warn("ebpf: tcp_retransmit_skb records could not be read", "count", d)
		}
	}
}

func (nf *netflow) detach() {
	for _, l := range nf.links {
		_ = l.Close()
	}
	nf.links = nil
	if nf.rtLink != nil {
		_ = nf.rtLink.Close()
		nf.rtLink = nil
	}
	counters.netflowAttached.Store(false)
	counters.retransmitsAttached.Store(false)
}

func (nf *netflow) closeObjects() {
	_ = nf.objs.Close()
	if nf.rt != nil {
		_ = nf.rt.Close()
	}
}

// setVariables assigns load-time constants, failing loudly if the object
// no longer declares one (a rename in the C source).
func setVariables(spec *cebpf.CollectionSpec, vals map[string]any) error {
	for name, v := range vals {
		vs, ok := spec.Variables[name]
		if !ok {
			return fmt.Errorf("BPF object has no variable %q", name)
		}
		if err := vs.Set(v); err != nil {
			return fmt.Errorf("setting %s: %w", name, err)
		}
	}
	return nil
}

func boolByte(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
