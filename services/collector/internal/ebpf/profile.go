// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"cmp"
	"slices"
	"strings"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// stackKey mirrors struct stack_key (bpf/profiler.bpf.c).
type stackKey struct {
	CgroupID      uint64
	Tgid          uint32
	UserStackID   int32
	KernelStackID int32
	Pad           uint32
}

// stackDepth is PERF_MAX_STACK_DEPTH, the STACK_TRACE map value length.
const stackDepth = 127

// stackIPs trims a STACK_TRACE value (zero padded past the recorded
// depth) to its frames, leaf first.
func stackIPs(raw *[stackDepth]uint64) []uint64 {
	n := 0
	for n < len(raw) && raw[n] != 0 {
		n++
	}
	return raw[:n:n]
}

// assembleFrames builds a root-to-leaf frame list from leaf-first user
// and kernel stacks. User code is the outer part of any stack (a sample
// in kernel mode is a syscall or fault taken from user space), so the
// order is: user root ... user leaf, kernel entry ... kernel leaf. Runs
// of unknown frames collapse into one: they carry no information and
// would otherwise split identical stacks by unwinder noise.
func assembleFrames(user, kernel []string) []string {
	frames := make([]string, 0, len(user)+len(kernel))
	push := func(f string) {
		if f == unknownFrame && len(frames) > 0 && frames[len(frames)-1] == unknownFrame {
			return
		}
		frames = append(frames, f)
	}
	for i := len(user) - 1; i >= 0; i-- {
		push(user[i])
	}
	for i := len(kernel) - 1; i >= 0; i-- {
		push(kernel[i])
	}
	if len(frames) == 0 {
		// No stack at all (both unwinds failed): keep the CPU time
		// attributed to the container rather than dropping it.
		frames = append(frames, unknownFrame)
	}
	return frames
}

// profileBuilder aggregates one drain's samples into one CPU Profile per
// (pod, container).
type profileBuilder struct {
	start    time.Time
	duration time.Duration
	period   int64 // nanoseconds of CPU time one sample stands for

	byContainer map[string]*profileAcc
}

type profileAcc struct {
	ref     *kuberov1.PodRef
	service string
	stacks  map[string]*kuberov1.StackSample // key: frames joined by NUL
	order   []*kuberov1.StackSample
}

func newProfileBuilder(start time.Time, duration time.Duration, hz int) *profileBuilder {
	return &profileBuilder{
		start:       start,
		duration:    duration,
		period:      int64(time.Second) / int64(hz),
		byContainer: make(map[string]*profileAcc),
	}
}

// containerKey identifies the profile a sample belongs to.
func containerKey(ref *kuberov1.PodRef) string {
	return ref.GetNamespace() + "/" + ref.GetPod() + "/" + ref.GetContainer()
}

// add records samples for frames (root to leaf) in the container ref.
func (b *profileBuilder) add(ref *kuberov1.PodRef, service string, frames []string, samples uint64) {
	key := containerKey(ref)
	acc := b.byContainer[key]
	if acc == nil {
		acc = &profileAcc{ref: ref, service: service, stacks: make(map[string]*kuberov1.StackSample)}
		b.byContainer[key] = acc
	}
	fk := strings.Join(frames, "\x00")
	s := acc.stacks[fk]
	if s == nil {
		s = &kuberov1.StackSample{Frames: frames}
		acc.stacks[fk] = s
		acc.order = append(acc.order, s)
	}
	s.Value += int64(samples) * b.period
}

// profiles returns one Profile per container, containers ordered by
// name and stacks by descending value, so output is deterministic.
func (b *profileBuilder) profiles() []*kuberov1.Profile {
	keys := make([]string, 0, len(b.byContainer))
	for k := range b.byContainer {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]*kuberov1.Profile, 0, len(keys))
	for _, k := range keys {
		acc := b.byContainer[k]
		slices.SortStableFunc(acc.order, func(x, y *kuberov1.StackSample) int {
			return cmp.Compare(y.Value, x.Value)
		})
		out = append(out, &kuberov1.Profile{
			TsUnixNano:   b.start.UnixNano(),
			DurationNano: int64(b.duration),
			Type:         "cpu",
			Unit:         "nanoseconds",
			Service:      acc.service,
			Source:       acc.ref,
			Samples:      acc.order,
			Origin:       "ebpf",
			Period:       b.period,
		})
	}
	return out
}

// batchProfiles splits profiles into emit batches of at most maxSamples
// stacks each (a single larger profile travels alone) so one RPC stays
// well under common message-size limits.
func batchProfiles(ps []*kuberov1.Profile, maxSamples int) [][]*kuberov1.Profile {
	var batches [][]*kuberov1.Profile
	var cur []*kuberov1.Profile
	n := 0
	for _, p := range ps {
		if len(cur) > 0 && n+len(p.Samples) > maxSamples {
			batches = append(batches, cur)
			cur, n = nil, 0
		}
		cur = append(cur, p)
		n += len(p.Samples)
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

// batchFlows splits flows into batches of at most size rows.
func batchFlows(fs []*kuberov1.Flow, size int) [][]*kuberov1.Flow {
	var batches [][]*kuberov1.Flow
	for len(fs) > 0 {
		n := min(size, len(fs))
		batches = append(batches, fs[:n:n])
		fs = fs[n:]
	}
	return batches
}

// topSamples keeps the limit heaviest entries (ties broken by key so
// the choice is deterministic) and reports how many were dropped.
func topSamples(keys []stackKey, counts []uint64, limit int) ([]stackKey, []uint64, int) {
	if len(keys) <= limit {
		return keys, counts, 0
	}
	idx := make([]int, len(keys))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int {
		if c := cmp.Compare(counts[b], counts[a]); c != 0 {
			return c
		}
		ka, kb := keys[a], keys[b]
		if c := cmp.Compare(ka.CgroupID, kb.CgroupID); c != 0 {
			return c
		}
		if c := cmp.Compare(ka.Tgid, kb.Tgid); c != 0 {
			return c
		}
		if c := cmp.Compare(ka.UserStackID, kb.UserStackID); c != 0 {
			return c
		}
		return cmp.Compare(ka.KernelStackID, kb.KernelStackID)
	})
	outK := make([]stackKey, limit)
	outC := make([]uint64, limit)
	for i := 0; i < limit; i++ {
		outK[i], outC[i] = keys[idx[i]], counts[idx[i]]
	}
	return outK, outC, len(keys) - limit
}
