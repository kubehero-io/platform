// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testUID  = "6f1c2a8e-3b1d-4c5e-9f7a-0123456789ab"
	testUIDs = "6f1c2a8e_3b1d_4c5e_9f7a_0123456789ab" // systemd escaping
	testCID  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestParseCgroupPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want containerRef
		ok   bool
	}{
		{"systemd containerd burstable",
			"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + testUIDs + ".slice/cri-containerd-" + testCID + ".scope",
			containerRef{testUID, testCID}, true},
		{"systemd guaranteed",
			"/kubepods.slice/kubepods-pod" + testUIDs + ".slice/cri-containerd-" + testCID + ".scope",
			containerRef{testUID, testCID}, true},
		{"systemd besteffort crio",
			"/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + testUIDs + ".slice/crio-" + testCID + ".scope",
			containerRef{testUID, testCID}, true},
		{"systemd docker",
			"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + testUIDs + ".slice/docker-" + testCID + ".scope",
			containerRef{testUID, testCID}, true},
		{"kind nesting",
			"/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-besteffort.slice/kubelet-kubepods-besteffort-pod" + testUIDs + ".slice/cri-containerd-" + testCID + ".scope",
			containerRef{testUID, testCID}, true},
		{"cgroupfs burstable",
			"/kubepods/burstable/pod" + testUID + "/" + testCID,
			containerRef{testUID, testCID}, true},
		{"cgroupfs guaranteed",
			"/kubepods/pod" + testUID + "/" + testCID,
			containerRef{testUID, testCID}, true},
		{"cgroupfs crio",
			"/kubepods/besteffort/pod" + testUID + "/crio-" + testCID,
			containerRef{testUID, testCID}, true},
		{"sub-cgroup inside a container",
			"/kubepods/burstable/pod" + testUID + "/" + testCID + "/init.scope",
			containerRef{testUID, testCID}, true},
		{"static pod hash uid",
			"/kubepods/burstable/pod0123456789abcdef0123456789abcdef/" + testCID,
			containerRef{"0123456789abcdef0123456789abcdef", testCID}, true},
		{"uppercase ids normalize",
			"/kubepods/pod" + strings.ToUpper(testUID) + "/" + strings.ToUpper(testCID),
			containerRef{testUID, testCID}, true},
		{"pod-level cgroup",
			"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + testUIDs + ".slice",
			containerRef{testUID, ""}, true},
		{"crio conmon is not a container",
			"/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + testUIDs + ".slice/crio-conmon-" + testCID + ".scope",
			containerRef{testUID, ""}, true},
		{"short container id",
			"/kubepods/pod" + testUID + "/abc123",
			containerRef{testUID, ""}, true},
		{"qos level only", "/kubepods.slice/kubepods-burstable.slice", containerRef{}, false},
		{"root", "/", containerRef{}, false},
		{"system service", "/system.slice/containerd.service", containerRef{}, false},
		{"docker container", "/docker/" + testCID, containerRef{}, false},
		{"pod-like name outside kubepods", "/machine.slice/pod" + testUID + "/" + testCID, containerRef{}, false},
		{"LinuxKit podruntime", "/podruntime/runc", containerRef{}, false},
		{"malformed uid", "/kubepods/pod6f1c2a8e-3b1d-4c5e-9f7a-0123456789zz/" + testCID, containerRef{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCgroupPath(tt.path)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("parseCgroupPath(%q) = %+v, %v; want %+v, %v", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCgroupIndex(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) uint64 {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		ino, ok := dirInode(p)
		if !ok {
			t.Fatal("no inode")
		}
		return ino
	}
	podDir := "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + testUIDs + ".slice"
	ctr := mk(podDir + "/cri-containerd-" + testCID + ".scope")
	pod := mk(podDir)
	svc := mk("system.slice/containerd.service")
	// Nested one level deeper (as in kind), still found.
	nested := mk("kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-pod" + testUIDs + ".slice/cri-containerd-" + strings.Repeat("a", 64) + ".scope")

	now := time.Unix(1_000_000, 0)
	ix := newCgroupIndex(root)
	ix.now = func() time.Time { return now }

	if ref, ok := ix.lookup(ctr); !ok || ref != (containerRef{testUID, testCID}) {
		t.Fatalf("container: %+v, %v", ref, ok)
	}
	if ref, ok := ix.lookup(pod); !ok || ref != (containerRef{testUID, ""}) {
		t.Fatalf("pod: %+v, %v", ref, ok)
	}
	if ref, ok := ix.lookup(nested); !ok || ref.containerID != strings.Repeat("a", 64) {
		t.Fatalf("nested: %+v, %v", ref, ok)
	}
	if _, ok := ix.lookup(svc); ok {
		t.Fatal("system service must not resolve")
	}

	// A container started after the last scan is found once the rescan
	// rate limit allows, not before.
	newCID := strings.Repeat("b", 64)
	late := mk(podDir + "/cri-containerd-" + newCID + ".scope")
	now = now.Add(cgroupRescanMin / 2)
	if _, ok := ix.lookup(late); ok {
		t.Fatal("rescan should be rate limited")
	}
	now = now.Add(cgroupRescanMin)
	if ref, ok := ix.lookup(late); !ok || ref.containerID != newCID {
		t.Fatalf("late container: %+v, %v", ref, ok)
	}

	// Deleted cgroups disappear on the periodic rescan.
	if err := os.Remove(filepath.Join(root, podDir, "cri-containerd-"+newCID+".scope")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(cgroupRescanMax)
	if _, ok := ix.lookup(late); ok {
		t.Fatal("deleted cgroup still indexed")
	}
}

// BenchmarkCgroupScan rescans a node-sized hierarchy: 300 pods with 3
// containers each (systemd layout) beside 100 system services.
func BenchmarkCgroupScan(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 100; i++ {
		if err := os.MkdirAll(filepath.Join(root, "system.slice", fmt.Sprintf("svc-%d.service", i)), 0o755); err != nil {
			b.Fatal(err)
		}
	}
	for p := 0; p < 300; p++ {
		uid := fmt.Sprintf("%08x_0000_4000_8000_%012x", p, p)
		pod := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod"+uid+".slice")
		for c := 0; c < 3; c++ {
			if err := os.MkdirAll(filepath.Join(pod, fmt.Sprintf("cri-containerd-%064x.scope", p*3+c)), 0o755); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if ids := scanCgroups(root); len(ids) != 1200 { // 300 pod-level + 900 containers
			b.Fatalf("indexed %d cgroups", len(ids))
		}
	}
}
