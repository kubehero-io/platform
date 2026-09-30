// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseInnermostTgid(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   uint32
		ok     bool
	}{
		{"Name:\tapi\nTgid:\t4242\nNStgid:\t4242\t17\t1\nNSpid:\t4242\t17\t1\n", 1, true}, // three levels deep
		{"NStgid:\t812\n", 812, true},             // same namespace as the reader
		{"Name:\tkthreadd\nTgid:\t2\n", 0, false}, // no NStgid line (pre-4.1 kernel)
		{"NStgid:\n", 0, false},                   // malformed
		{"NStgid:\t12\tzero\n", 0, false},         // not a number
	} {
		got, ok := parseInnermostTgid(strings.NewReader(tc.status))
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseInnermostTgid(%q) = %d, %v; want %d, %v", tc.status, got, ok, tc.want, tc.ok)
		}
	}
}

// A fake node: /proc with a few processes and three container cgroups.
func TestScanPidns(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	cg := filepath.Join(root, "cgroup")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// api: two processes in one pod namespace (tgids 1 and 7 inside it),
	// plus one that unshared a namespace of its own.
	write(filepath.Join(cg, "api", "cgroup.procs"), "100\n101\n102\n")
	write(filepath.Join(proc, "100", "status"), "NStgid:\t100\t1\n")
	write(filepath.Join(proc, "101", "status"), "NStgid:\t101\t7\n")
	write(filepath.Join(proc, "102", "status"), "NStgid:\t102\t9\t1\n")
	// collector: holds the scanning process, so it must stay out.
	write(filepath.Join(cg, "collector", "cgroup.procs"), "200\n")
	write(filepath.Join(proc, "200", "status"), "NStgid:\t200\n")
	// idle: no processes. gone: a member that exited before the read.
	write(filepath.Join(cg, "idle", "cgroup.procs"), "")
	write(filepath.Join(cg, "gone", "cgroup.procs"), "300\n")

	podNS := pidnsID{dev: 4, ino: 4026532701}
	stat := func(path string) (pidnsID, error) {
		switch filepath.Base(filepath.Dir(filepath.Dir(path))) {
		case "100", "101":
			return podNS, nil
		case "102":
			return pidnsID{dev: 4, ino: 4026532999}, nil
		case "200":
			return pidnsID{dev: 4, ino: 4026531836}, nil
		}
		return pidnsID{}, errors.New("no such process")
	}
	dirs := map[uint64]string{
		11: filepath.Join(cg, "api"),
		12: filepath.Join(cg, "collector"),
		13: filepath.Join(cg, "idle"),
		14: filepath.Join(cg, "gone"),
	}
	s := scanPidns(proc, dirs, 200, stat)

	if len(s.ns) != 1 || s.ns[11] != podNS {
		t.Fatalf("namespaces = %v, want only cgroup 11 -> %v", s.ns, podNS)
	}
	want := map[nsProc]uint32{{cgroup: 11, tgid: 1}: 100, {cgroup: 11, tgid: 7}: 101}
	if len(s.local) != len(want) {
		t.Fatalf("local = %v, want %v", s.local, want)
	}
	for k, v := range want {
		if s.local[k] != v {
			t.Errorf("local[%+v] = %d, want %d", k, s.local[k], v)
		}
	}
}
