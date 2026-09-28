// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

func emitFlows(context.Context, []*kuberov1.Flow) error       { return nil }
func emitProfiles(context.Context, []*kuberov1.Profile) error { return nil }

func TestConfigNormalize(t *testing.T) {
	full := Config{Netflow: true, Profiler: true, Resolver: newFakeResolver(), EmitFlows: emitFlows, EmitProfiles: emitProfiles}

	got, err := full.normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.CgroupRoot != "/sys/fs/cgroup" || got.FlushInterval != 15*time.Second || got.ProfileHz != 49 || got.Logger == nil {
		t.Errorf("defaults not applied: %+v", got)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string // substring of the error; "" = valid
	}{
		{"explicit values kept", func(c *Config) { c.CgroupRoot = "/host/cgroup/"; c.FlushInterval = time.Second; c.ProfileHz = 1000 }, ""},
		{"relative cgroup root", func(c *Config) { c.CgroupRoot = "sys/fs/cgroup" }, "absolute"},
		{"flush too short", func(c *Config) { c.FlushInterval = 500 * time.Millisecond }, "FlushInterval"},
		{"flush too long", func(c *Config) { c.FlushInterval = time.Hour }, "FlushInterval"},
		{"negative hz", func(c *Config) { c.ProfileHz = -1 }, "ProfileHz"},
		{"hz too high", func(c *Config) { c.ProfileHz = 5000 }, "ProfileHz"},
		{"no resolver", func(c *Config) { c.Resolver = nil }, "Resolver"},
		{"netflow without emitter", func(c *Config) { c.EmitFlows = nil }, "EmitFlows"},
		{"profiler without emitter", func(c *Config) { c.EmitProfiles = nil }, "EmitProfiles"},
		{"disabled needs nothing", func(c *Config) { *c = Config{} }, ""},
		{"profiler only needs no flow emitter", func(c *Config) { c.Netflow = false; c.EmitFlows = nil }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full
			tt.mutate(&cfg)
			got, err := cfg.normalize()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tt.name == "explicit values kept" && (got.CgroupRoot != "/host/cgroup" || got.ProfileHz != 1000) {
					t.Errorf("values changed: %+v", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || !errors.Is(err, errInvalidConfig) {
				t.Fatalf("err = %v, want errInvalidConfig mentioning %q", err, tt.want)
			}
			if errors.Is(err, ErrUnsupported) {
				t.Error("config errors must not look like ErrUnsupported")
			}
		})
	}
}

func TestFlowMapEntries(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == FlowMapEntriesEnv {
				return v
			}
			return ""
		}
	}
	for v, want := range map[string]uint32{"": defaultFlowMapEntries, "1024": 1024, "262144": 262144, "1048576": 1 << 20} {
		if got, err := flowMapEntries(env(v)); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"1023", "1048577", "-5", "lots", "1e6"} {
		if _, err := flowMapEntries(env(v)); !errors.Is(err, errInvalidConfig) {
			t.Errorf("%q: err = %v", v, err)
		}
	}
}

func TestStartUnsupportedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("covered by the ebpfintegration tests on Linux")
	}
	err := Start(context.Background(), Config{Netflow: true, Resolver: newFakeResolver(), EmitFlows: emitFlows})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Start = %v, want ErrUnsupported", err)
	}
}

func TestStatsAndDeltas(t *testing.T) {
	before := Stats()
	counters.flowsEmitted.Add(3)
	counters.mapFullEvents.Add(1)
	after := Stats()
	if after.FlowsEmitted-before.FlowsEmitted != 3 || after.MapFullEvents-before.MapFullEvents != 1 {
		t.Errorf("Stats() did not reflect counters: %+v -> %+v", before, after)
	}

	var d deltaCounter
	for _, step := range []struct{ total, want uint64 }{{5, 5}, {5, 0}, {12, 7}, {3, 0} /* reset */, {4, 1}} {
		if got := d.delta(step.total); got != step.want {
			t.Errorf("delta(%d) = %d, want %d", step.total, got, step.want)
		}
	}
}
