// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build linux

package ebpf

import (
	"reflect"
	"strings"
	"testing"
)

// TestGeneratedLayoutsMatch checks the hand-written Go mirrors the drains
// decode into against the types bpf2go generated from the C structs'
// BTF: same size, same fields (names modulo Go initialisms, e.g.
// CgroupId vs CgroupID), same offsets. A C edit without the
// matching Go edit fails here (and in checkLayout at load time).
func TestGeneratedLayoutsMatch(t *testing.T) {
	for _, tt := range []struct {
		name      string
		gen, mine any
	}{
		{"netflow flow_key", netflowFlowKey{}, flowKey{}},
		{"netflow flow_val", netflowFlowVal{}, flowValue{}},
		{"tcpretrans flow_key", tcpretransFlowKey{}, flowKey{}},
		{"profiler stack_key", profilerStackKey{}, stackKey{}},
	} {
		g, m := reflect.TypeOf(tt.gen), reflect.TypeOf(tt.mine)
		if g.Size() != m.Size() {
			t.Errorf("%s: generated %d bytes, Go mirror %d", tt.name, g.Size(), m.Size())
			continue
		}
		var gf []reflect.StructField
		for i := 0; i < g.NumField(); i++ {
			if f := g.Field(i); f.Name != "_" { // structs.HostLayout marker
				gf = append(gf, f)
			}
		}
		if len(gf) != m.NumField() {
			t.Errorf("%s: %d generated fields, %d in the mirror", tt.name, len(gf), m.NumField())
			continue
		}
		for i, f := range gf {
			mf := m.Field(i)
			if !strings.EqualFold(f.Name, mf.Name) || f.Offset != mf.Offset || f.Type.Size() != mf.Type.Size() {
				t.Errorf("%s: field %d generated %s@%d (%d bytes), mirror %s@%d (%d bytes)",
					tt.name, i, f.Name, f.Offset, f.Type.Size(), mf.Name, mf.Offset, mf.Type.Size())
			}
		}
	}
}
