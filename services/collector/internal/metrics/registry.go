// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry is a minimal Prometheus-exposition registry: counters and
// gauges with fixed label names, lock-free updates, and deterministic
// output. It exists so the collector's self-telemetry doesn't pull in
// client_golang and its dependency tree for a dozen series; label
// values must come from small, fixed sets (signal names, reasons, gRPC
// codes) — never from pod names or user input.
type Registry struct {
	mu      sync.Mutex
	metrics []*Vec
	byName  map[string]*Vec
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byName: map[string]*Vec{}} }

// Default is the process-wide registry the collector's pipelines report
// into and /metrics serves.
var Default = NewRegistry()

// Vec is a metric family: one name, fixed label names, a value per
// label-value combination.
type Vec struct {
	name, help, typ string
	labels          []string

	mu     sync.RWMutex
	series map[string]*Value
}

// Value is one series. Updates are atomic.
type Value struct {
	labelValues []string
	bits        atomic.Uint64
}

// NewCounterVec registers a counter family. Registering the same name
// twice returns the existing family (label names must match).
func (r *Registry) NewCounterVec(name, help string, labels ...string) *Vec {
	return r.register(name, help, "counter", labels)
}

// NewGaugeVec registers a gauge family.
func (r *Registry) NewGaugeVec(name, help string, labels ...string) *Vec {
	return r.register(name, help, "gauge", labels)
}

func (r *Registry) register(name, help, typ string, labels []string) *Vec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.byName[name]; ok {
		if v.typ != typ || strings.Join(v.labels, ",") != strings.Join(labels, ",") {
			panic(fmt.Sprintf("metrics: %s re-registered with a different shape", name))
		}
		return v
	}
	v := &Vec{name: name, help: help, typ: typ, labels: labels, series: map[string]*Value{}}
	r.metrics = append(r.metrics, v)
	r.byName[name] = v
	return v
}

// With returns the series for the given label values (created on first
// use). The number of values must match the family's label names.
func (v *Vec) With(labelValues ...string) *Value {
	if len(labelValues) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", v.name, len(v.labels), len(labelValues)))
	}
	key := strings.Join(labelValues, "\xff")
	v.mu.RLock()
	s, ok := v.series[key]
	v.mu.RUnlock()
	if ok {
		return s
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.series[key]; ok {
		return s
	}
	s = &Value{labelValues: append([]string(nil), labelValues...)}
	v.series[key] = s
	return s
}

// Add increments the value (counters: n ≥ 0).
func (s *Value) Add(n float64) {
	for {
		old := s.bits.Load()
		nv := math.Float64bits(math.Float64frombits(old) + n)
		if s.bits.CompareAndSwap(old, nv) {
			return
		}
	}
}

// Inc adds 1.
func (s *Value) Inc() { s.Add(1) }

// Set replaces the value (gauges).
func (s *Value) Set(v float64) { s.bits.Store(math.Float64bits(v)) }

// Get reads the value.
func (s *Value) Get() float64 { return math.Float64frombits(s.bits.Load()) }

// Write renders every family in exposition format, families in
// registration order and series sorted by label values.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	families := append([]*Vec(nil), r.metrics...)
	r.mu.Unlock()
	for _, v := range families {
		v.mu.RLock()
		keys := make([]string, 0, len(v.series))
		for k := range v.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		series := make([]*Value, 0, len(keys))
		for _, k := range keys {
			series = append(series, v.series[k])
		}
		v.mu.RUnlock()
		if len(series) == 0 {
			continue
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", v.name, escapeHelp(v.help), v.name, v.typ)
		for _, s := range series {
			writeSample(w, v.name, v.labels, s.labelValues, s.Get())
		}
	}
}

func writeSample(w io.Writer, name string, names, values []string, val float64) {
	var b strings.Builder
	b.WriteString(name)
	if len(names) > 0 {
		b.WriteByte('{')
		for i, n := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(n)
			b.WriteString(`="`)
			b.WriteString(escapeLabel(values[i]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	fmt.Fprintf(w, "%s %s\n", b.String(), formatFloat(val))
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return fmt.Sprintf("%g", v)
}

// escapeLabel applies the exposition-format escapes for label values.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}
