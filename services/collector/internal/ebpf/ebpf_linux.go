// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build linux

package ebpf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

const (
	// procRoot is where the host's /proc is visible. The profiler needs
	// the host PID namespace (hostPID: true), so this is plain /proc.
	procRoot = "/proc"
	// finalFlushTimeout bounds the last drain after ctx is cancelled.
	finalFlushTimeout = 5 * time.Second
)

// loops tracks the drain goroutines so tests can wait for detach and
// the final flush after cancelling ctx.
var loops sync.WaitGroup

func start(ctx context.Context, cfg Config) error {
	cfg, err := cfg.normalize()
	if err != nil {
		return err
	}
	log := cfg.Logger.With("component", "ebpf")
	if !cfg.Netflow && !cfg.Profiler {
		log.Info("ebpf: no kernel programs enabled")
		return nil
	}
	flowEntries, err := flowMapEntries(os.Getenv)
	if err != nil {
		return err
	}
	if err := requireCgroup2(cfg.CgroupRoot); err != nil {
		return unsupported(err)
	}
	// Kernels before 5.11 charge BPF memory to RLIMIT_MEMLOCK; later ones
	// use memcg and this is a no-op. Failure only matters on old kernels,
	// where the load below will then report EPERM itself.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Debug("ebpf: raising RLIMIT_MEMLOCK failed", "err", err)
	}

	var errs []error
	started := 0
	if cfg.Netflow {
		if err := startNetflow(ctx, cfg, flowEntries, log); err != nil {
			errs = append(errs, fmt.Errorf("netflow: %w", err))
			log.Warn("ebpf netflow unavailable", "err", err)
		} else {
			started++
		}
	}
	if cfg.Profiler {
		if err := startProfiler(ctx, cfg, log); err != nil {
			errs = append(errs, fmt.Errorf("profiler: %w", err))
			log.Warn("ebpf profiler unavailable", "err", err)
		} else {
			started++
		}
	}
	if started == 0 {
		return errors.Join(errs...)
	}
	return nil
}

// unsupported marks err as an environment limitation (kernel too old,
// missing privileges, no cgroup v2 / tracefs) as opposed to a bug.
func unsupported(err error) error {
	if errors.Is(err, ErrUnsupported) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnsupported, err)
}

// requireCgroup2 checks that root is a cgroup v2 mount: cgroup_skb
// programs attach to v2 cgroups and bpf_get_current_cgroup_id returns
// v2 ids, so hybrid/v1-only hosts cannot be supported.
func requireCgroup2(root string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", root, err)
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("%s is not a cgroup v2 mount (fs magic %#x)", root, st.Type)
	}
	return nil
}

// inHostPIDNamespace reports whether /proc shows the host's PID
// namespace. PID 2 is always kthreadd there, and kernel threads are
// never visible inside a container's PID namespace.
func inHostPIDNamespace() bool {
	b, err := os.ReadFile(procRoot + "/2/comm")
	return err == nil && strings.TrimSpace(string(b)) == "kthreadd"
}
