// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// containerRef is what a pod's cgroup path encodes.
type containerRef struct {
	podUID      string // canonical form, dashes
	containerID string // bare 64-hex runtime ID; "" for pod-level cgroups
}

// containerPrefixes are the runtime-specific names systemd-style scopes
// carry, e.g. "cri-containerd-<id>.scope". cgroupfs layouts use the bare
// ID as the directory name.
var containerPrefixes = []string{"cri-containerd-", "containerd-", "crio-", "docker-", "cri-dockerd-", "libpod-"}

// parseCgroupPath extracts the pod UID and container ID from a cgroup v2
// path (relative to the cgroup root). Supported layouts:
//
//	systemd:  /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid_with_underscores>.slice/cri-containerd-<id>.scope
//	          (also crio-<id>.scope, docker-<id>.scope, kubepods-pod<uid>.slice for Guaranteed pods,
//	          and kind's /kubelet.slice/kubelet-kubepods.slice/... nesting)
//	cgroupfs: /kubepods/burstable/pod<uid>/<id>, /kubepods/pod<uid>/<id>
//
// Deeper components (a container that creates its own sub-cgroups) are
// attributed to the enclosing container. ok is false for anything that
// is not inside a pod cgroup.
func parseCgroupPath(path string) (ref containerRef, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	podIdx := -1
	for i, p := range parts {
		if uid, isPod := podUIDFromComponent(p); isPod {
			ref.podUID, podIdx = uid, i
			// Keep scanning: a pod cgroup nested under another pod-like
			// name (kind-in-kind) must resolve to the innermost pod.
		}
	}
	if podIdx < 0 || !strings.Contains(path, "kubepods") {
		return containerRef{}, false
	}
	if podIdx+1 < len(parts) {
		ref.containerID = containerIDFromComponent(parts[podIdx+1])
	}
	return ref, true
}

// podUIDFromComponent recognises "pod<uid>" (cgroupfs) and
// "<qos prefix>-pod<uid_with_underscores>.slice" (systemd).
func podUIDFromComponent(p string) (string, bool) {
	var raw string
	switch {
	case strings.HasSuffix(p, ".slice"):
		i := strings.LastIndex(p, "-pod")
		if i < 0 {
			return "", false
		}
		raw = strings.ReplaceAll(strings.TrimSuffix(p[i+len("-pod"):], ".slice"), "_", "-")
	case strings.HasPrefix(p, "pod"):
		raw = p[len("pod"):]
	default:
		return "", false
	}
	if !validPodUID(raw) {
		return "", false
	}
	return strings.ToLower(raw), true
}

// validPodUID accepts API-server UIDs (RFC 4122, 36 chars) and the 32-hex
// config hashes the kubelet uses as static-pod UIDs.
func validPodUID(s string) bool {
	switch len(s) {
	case 36:
		for i, c := range s {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if c != '-' {
					return false
				}
			} else if !isHex(c) {
				return false
			}
		}
		return true
	case 32:
		return isHexString(s)
	}
	return false
}

// containerIDFromComponent returns the bare container ID in a cgroup
// directory name, or "" if the directory is not a container (the crio
// conmon scope, a pod-level sub-cgroup, ...).
func containerIDFromComponent(p string) string {
	id := strings.TrimSuffix(p, ".scope")
	for _, prefix := range containerPrefixes {
		if strings.HasPrefix(id, prefix) {
			id = id[len(prefix):]
			break
		}
	}
	if len(id) == 64 && isHexString(id) {
		return strings.ToLower(id)
	}
	return ""
}

func isHex(c rune) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func isHexString(s string) bool {
	for _, c := range s {
		if !isHex(c) {
			return false
		}
	}
	return s != ""
}

const (
	// cgroupRescanMin rate-limits rescans triggered by unknown cgroup ids
	// (a container started since the last scan, or a non-Kubernetes
	// cgroup, which no scan will ever find). Callers memoize per drain,
	// so this is at most one rescan per drain.
	cgroupRescanMin = 10 * time.Second
	// cgroupRescanMax forces a periodic rescan so entries for deleted
	// cgroups do not accumulate.
	cgroupRescanMax = 5 * time.Minute
	// kubepodsSearchDepth is how deep under the root kubepods* trees are
	// looked for (kind and some distros nest them under kubelet.slice).
	kubepodsSearchDepth = 3
	// cgroupWalkDepth bounds the walk inside a kubepods tree: qos/pod/
	// container is 3 levels; containers may nest a few more.
	cgroupWalkDepth = 8
	// maxIndexedCgroups bounds the index (hundreds of pods x a few
	// containers on the busiest nodes).
	maxIndexedCgroups = 1 << 16
)

// cgroupIndex maps cgroup v2 ids (the directory inode number, which is
// what bpf_get_current_cgroup_id returns on 64-bit kernels) to the
// Kubernetes container they belong to. Safe for concurrent use.
type cgroupIndex struct {
	root string
	now  func() time.Time

	mu       sync.Mutex
	ids      map[uint64]cgroupEntry
	lastScan time.Time
}

// cgroupEntry is one indexed pod or container cgroup.
type cgroupEntry struct {
	ref containerRef
	dir string
}

func newCgroupIndex(root string) *cgroupIndex {
	return &cgroupIndex{root: root, now: time.Now}
}

// lookup resolves a cgroup id, rescanning the hierarchy for unknown ids
// at most every cgroupRescanMin.
func (ix *cgroupIndex) lookup(id uint64) (containerRef, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	now := ix.now()
	if ix.ids == nil || now.Sub(ix.lastScan) >= cgroupRescanMax {
		ix.rescanLocked(now)
	}
	if e, ok := ix.ids[id]; ok {
		return e.ref, true
	}
	if now.Sub(ix.lastScan) >= cgroupRescanMin {
		ix.rescanLocked(now)
		e, ok := ix.ids[id]
		return e.ref, ok
	}
	return containerRef{}, false
}

// containerDirs returns the directory of every indexed container cgroup
// (pod-level cgroups hold no processes), rescanning at most every
// cgroupRescanMin so containers started since show up quickly.
func (ix *cgroupIndex) containerDirs() map[uint64]string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	now := ix.now()
	if ix.ids == nil || now.Sub(ix.lastScan) >= cgroupRescanMin {
		ix.rescanLocked(now)
	}
	out := make(map[uint64]string, len(ix.ids))
	for id, e := range ix.ids {
		if e.ref.containerID != "" {
			out[id] = e.dir
		}
	}
	return out
}

func (ix *cgroupIndex) rescanLocked(now time.Time) {
	ix.ids = scanCgroups(ix.root)
	ix.lastScan = now
}

// scanCgroups finds kubepods trees near the root and indexes every pod /
// container cgroup inside them. Errors (a cgroup removed mid-walk) only
// skip the affected directory.
func scanCgroups(root string) map[uint64]cgroupEntry {
	ids := make(map[uint64]cgroupEntry)
	var visit func(dir string, depth int)
	visit = func(dir string, depth int) {
		if depth > cgroupWalkDepth || len(ids) >= maxIndexedCgroups {
			return
		}
		rel := strings.TrimPrefix(dir, root)
		if ref, ok := parseCgroupPath(rel); ok {
			if ino, ok := dirInode(dir); ok {
				ids[ino] = cgroupEntry{ref: ref, dir: dir}
			}
		}
		for _, e := range readDirs(dir) {
			visit(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	var find func(dir string, depth int)
	find = func(dir string, depth int) {
		for _, e := range readDirs(dir) {
			p := filepath.Join(dir, e.Name())
			if strings.Contains(e.Name(), "kubepods") {
				visit(p, 0)
			} else if depth+1 < kubepodsSearchDepth {
				find(p, depth+1)
			}
		}
	}
	find(root, 0)
	return ids
}

func readDirs(dir string) []fs.DirEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	dirs := entries[:0]
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}
	return dirs
}

func dirInode(path string) (uint64, bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Ino), true
}
