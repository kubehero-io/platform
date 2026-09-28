// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"slices"
	"testing"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

func TestAssembleFrames(t *testing.T) {
	tests := []struct {
		name         string
		user, kernel []string // leaf first, as recorded
		want         []string // root first
	}{
		{"user only",
			[]string{"main.hot", "main.work", "main.main", "runtime.main", "runtime.goexit.abi0"}, nil,
			[]string{"runtime.goexit.abi0", "runtime.main", "main.main", "main.work", "main.hot"}},
		{"syscall: user outside, kernel inside",
			[]string{"syscall.Syscall", "os.(*File).Write", "main.main"},
			[]string{"copy_page_to_iter [k]", "vfs_write [k]", "__arm64_sys_write [k]"},
			[]string{"main.main", "os.(*File).Write", "syscall.Syscall", "__arm64_sys_write [k]", "vfs_write [k]", "copy_page_to_iter [k]"}},
		{"kernel thread",
			nil, []string{"schedule [k]", "kthread [k]", "ret_from_fork [k]"},
			[]string{"ret_from_fork [k]", "kthread [k]", "schedule [k]"}},
		{"unknown runs collapse, across the user/kernel boundary too",
			[]string{unknownFrame, unknownFrame, "main.f", unknownFrame},
			[]string{unknownFrame, unknownFrame},
			[]string{unknownFrame, "main.f", unknownFrame}},
		{"no stacks at all", nil, nil, []string{unknownFrame}},
	}
	for _, tt := range tests {
		if got := assembleFrames(tt.user, tt.kernel); !slices.Equal(got, tt.want) {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, got, tt.want)
		}
	}
}

func TestStackIPs(t *testing.T) {
	var raw [stackDepth]uint64
	if got := stackIPs(&raw); len(got) != 0 {
		t.Errorf("empty stack: %v", got)
	}
	raw[0], raw[1], raw[2] = 0x10, 0x20, 0x30
	if got := stackIPs(&raw); !slices.Equal(got, []uint64{0x10, 0x20, 0x30}) {
		t.Errorf("got %v", got)
	}
	for i := range raw {
		raw[i] = uint64(i + 1)
	}
	if got := stackIPs(&raw); len(got) != stackDepth {
		t.Errorf("full stack: %d frames", len(got))
	}
}

func TestProfileBuilder(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	b := newProfileBuilder(start, 15*time.Second, 49)
	web := &kuberov1.PodRef{Namespace: "shop", Pod: "web-1", Container: "app", Workload: "web"}
	sidecar := &kuberov1.PodRef{Namespace: "shop", Pod: "web-1", Container: "proxy", Workload: "web"}
	db := &kuberov1.PodRef{Namespace: "shop", Pod: "db-0", Container: "pg", Workload: "db"}

	hot := []string{"main.main", "main.hot"}
	cold := []string{"main.main", "main.cold"}
	b.add(web, "web", hot, 3)
	b.add(web, "web", cold, 1)
	b.add(web, "web", slices.Clone(hot), 4) // same frames, separate slice: merged
	b.add(sidecar, "web", hot, 2)
	b.add(db, "db", []string{"postgres", "ExecQuery"}, 5)

	ps := b.profiles()
	if len(ps) != 3 {
		t.Fatalf("got %d profiles, want one per container", len(ps))
	}
	period := int64(time.Second) / 49
	// Sorted by namespace/pod/container: db-0/pg, web-1/app, web-1/proxy.
	wantOrder := []string{"pg", "app", "proxy"}
	for i, p := range ps {
		if p.Source.Container != wantOrder[i] {
			t.Errorf("profile %d is %s, want %s", i, p.Source.Container, wantOrder[i])
		}
		if p.Type != "cpu" || p.Unit != "nanoseconds" || p.Origin != "ebpf" || p.Period != period ||
			p.TsUnixNano != start.UnixNano() || p.DurationNano != int64(15*time.Second) {
			t.Errorf("profile header: %+v", p)
		}
	}
	app := ps[1]
	if app.Service != "web" || len(app.Samples) != 2 {
		t.Fatalf("app profile: service %q, %d samples", app.Service, len(app.Samples))
	}
	if !slices.Equal(app.Samples[0].Frames, hot) || app.Samples[0].Value != 7*period {
		t.Errorf("heaviest sample = %v %d, want hot x7", app.Samples[0].Frames, app.Samples[0].Value)
	}
	if !slices.Equal(app.Samples[1].Frames, cold) || app.Samples[1].Value != period {
		t.Errorf("second sample = %v %d", app.Samples[1].Frames, app.Samples[1].Value)
	}
	if ps[0].Samples[0].Value != 5*period {
		t.Errorf("db value = %d", ps[0].Samples[0].Value)
	}
}

func TestBatching(t *testing.T) {
	mk := func(samples int) *kuberov1.Profile {
		return &kuberov1.Profile{Samples: make([]*kuberov1.StackSample, samples)}
	}
	sizes := func(bs [][]*kuberov1.Profile) (out []int) {
		for _, b := range bs {
			n := 0
			for _, p := range b {
				n += len(p.Samples)
			}
			out = append(out, n)
		}
		return out
	}
	got := sizes(batchProfiles([]*kuberov1.Profile{mk(4), mk(4), mk(3), mk(12), mk(1)}, 10))
	if !slices.Equal(got, []int{8, 3, 12, 1}) {
		t.Errorf("profile batches = %v", got)
	}
	if batchProfiles(nil, 10) != nil {
		t.Error("no profiles, no batches")
	}

}

func TestTopSamples(t *testing.T) {
	keys := []stackKey{{Tgid: 1}, {Tgid: 2}, {Tgid: 3}, {Tgid: 4}}
	counts := []uint64{5, 50, 5, 20}
	k, c, dropped := topSamples(keys, counts, 3)
	if dropped != 1 || len(k) != 3 {
		t.Fatalf("dropped %d, kept %d", dropped, len(k))
	}
	// Heaviest first; the tie between tgid 1 and 3 resolves by key.
	if k[0].Tgid != 2 || k[1].Tgid != 4 || k[2].Tgid != 1 || c[0] != 50 || c[2] != 5 {
		t.Errorf("kept %+v %v", k, c)
	}
	if k2, _, d := topSamples(keys, counts, 10); d != 0 || len(k2) != 4 {
		t.Error("under the limit nothing is dropped")
	}
}
