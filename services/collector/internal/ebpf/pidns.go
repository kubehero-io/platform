// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Nested PID namespaces. On kind, k3d or Docker Desktop every Kubernetes
// "node" is a container sharing one kernel, and a collector with hostPID
// shares that node's PID namespace, not the kernel's top-level one. The
// profiler then records each sample's tgid inside the sampled process's
// own namespace (bpf_get_ns_current_pid_tgid, keyed by the namespace
// this file finds for its cgroup) and translates it back to a PID the
// collector's /proc knows.

// pidnsID is a PID namespace as stat(2) reports /proc/<pid>/ns/pid: the
// (device, inode) pair bpf_get_ns_current_pid_tgid expects.
type pidnsID struct{ dev, ino uint64 }

// nsProc identifies a process by its cgroup and its tgid inside its own
// (innermost) PID namespace, which is what the kernel records in
// nested mode.
type nsProc struct {
	cgroup uint64
	tgid   uint32
}

// pidnsScan is one pass over the container cgroups the collector sees.
type pidnsScan struct {
	ns    map[uint64]pidnsID // cgroup id -> the namespace of its processes
	local map[nsProc]uint32  // -> PID in the collector's /proc
}

// maxScannedProcs bounds one scan (thousands of processes on the
// busiest nodes).
const maxScannedProcs = 1 << 16

// scanPidns reads cgroup.procs of every container cgroup in dirs (cgroup
// id -> directory) and resolves each member through procRoot. The
// cgroup holding self (the collector's own PID) is left out, so the
// kernel never samples the profiler. stat resolves a namespace file;
// statPidns in production.
func scanPidns(procRoot string, dirs map[uint64]string, self int, stat func(string) (pidnsID, error)) pidnsScan {
	s := pidnsScan{ns: make(map[uint64]pidnsID), local: make(map[nsProc]uint32)}
	scanned := 0
	for id, dir := range dirs {
		pids := readCgroupProcs(filepath.Join(dir, "cgroup.procs"))
		if len(pids) == 0 {
			continue
		}
		mine := false
		for _, pid := range pids {
			if pid == self {
				mine = true
				break
			}
		}
		if mine {
			continue
		}
		for _, pid := range pids {
			if scanned >= maxScannedProcs {
				return s
			}
			scanned++
			base := filepath.Join(procRoot, strconv.Itoa(pid))
			ns, err := stat(filepath.Join(base, "ns", "pid"))
			if err != nil {
				continue // exited, or not visible from here
			}
			tgid, ok := innermostTgid(filepath.Join(base, "status"))
			if !ok {
				continue
			}
			// A container's processes share one namespace; one that
			// unshared its own fails the kernel's check and is dropped.
			if _, seen := s.ns[id]; !seen {
				s.ns[id] = ns
			}
			if s.ns[id] == ns {
				s.local[nsProc{cgroup: id, tgid: tgid}] = uint32(pid)
			}
		}
	}
	return s
}

// readCgroupProcs lists the PIDs in a cgroup.procs file, as the reading
// process's PID namespace numbers them (members outside it read as 0).
func readCgroupProcs(path string) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 {
			pids = append(pids, n)
		}
	}
	return pids
}

// innermostTgid is the last NStgid field of a /proc/<pid>/status file:
// the process's tgid inside its own PID namespace.
func innermostTgid(statusPath string) (uint32, bool) {
	f, err := os.Open(statusPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return parseInnermostTgid(io.LimitReader(f, 64<<10))
}

func parseInnermostTgid(r io.Reader) (uint32, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "NStgid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		n, err := strconv.ParseUint(fields[len(fields)-1], 10, 32)
		return uint32(n), err == nil && n > 0
	}
	return 0, false
}

// statPidns stats a /proc/<pid>/ns/pid file.
func statPidns(path string) (pidnsID, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return pidnsID{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return pidnsID{}, os.ErrInvalid
	}
	return pidnsID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil //nolint:unconvert // Dev is int32 on darwin
}
