// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"time"
)

const (
	defaultCgroupRoot    = "/sys/fs/cgroup"
	defaultFlushInterval = 15 * time.Second
	defaultProfileHz     = 49

	minFlushInterval = time.Second
	// maxFlushInterval: the kernel maps are sized for seconds-to-minutes
	// of traffic between drains; longer windows would silently evict.
	maxFlushInterval = 5 * time.Minute
	// maxProfileHz keeps sampling overhead bounded: 1000 Hz on every CPU
	// is already 20x the default.
	maxProfileHz = 1000

	// defaultFlowMapEntries sizes the kernel flow table (kh_flows). Each
	// entry costs ~120 bytes of kernel memory (preallocated).
	defaultFlowMapEntries = 131072
	minFlowMapEntries     = 1024
	maxFlowMapEntries     = 1 << 20

	// FlowMapEntriesEnv overrides the kernel flow table size (entries,
	// 1024..1048576) for nodes with unusually many distinct peers per
	// flush interval. Read once by Start.
	FlowMapEntriesEnv = "KUBEHERO_EBPF_FLOW_MAP_ENTRIES"
)

var errInvalidConfig = errors.New("ebpf: invalid config")

// normalize applies defaults and validates cfg. Its errors are
// programming or configuration mistakes, deliberately not ErrUnsupported:
// callers should surface them rather than log-and-continue.
func (cfg Config) normalize() (Config, error) {
	bad := func(format string, args ...any) (Config, error) {
		return cfg, fmt.Errorf("%w: "+format, append([]any{errInvalidConfig}, args...)...)
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = defaultCgroupRoot
	}
	if !filepath.IsAbs(cfg.CgroupRoot) {
		return bad("CgroupRoot %q must be absolute", cfg.CgroupRoot)
	}
	cfg.CgroupRoot = filepath.Clean(cfg.CgroupRoot)

	switch {
	case cfg.FlushInterval == 0:
		cfg.FlushInterval = defaultFlushInterval
	case cfg.FlushInterval < minFlushInterval || cfg.FlushInterval > maxFlushInterval:
		return bad("FlushInterval %s outside [%s, %s]", cfg.FlushInterval, minFlushInterval, maxFlushInterval)
	}
	switch {
	case cfg.ProfileHz == 0:
		cfg.ProfileHz = defaultProfileHz
	case cfg.ProfileHz < 1 || cfg.ProfileHz > maxProfileHz:
		return bad("ProfileHz %d outside [1, %d]", cfg.ProfileHz, maxProfileHz)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if (cfg.Netflow || cfg.Profiler) && cfg.Resolver == nil {
		return bad("Resolver is required")
	}
	if cfg.Netflow && cfg.EmitFlows == nil {
		return bad("EmitFlows is required when Netflow is enabled")
	}
	if cfg.Profiler && cfg.EmitProfiles == nil {
		return bad("EmitProfiles is required when Profiler is enabled")
	}
	return cfg, nil
}

// flowMapEntries reads the FlowMapEntriesEnv override.
func flowMapEntries(getenv func(string) string) (uint32, error) {
	v := getenv(FlowMapEntriesEnv)
	if v == "" {
		return defaultFlowMapEntries, nil
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil || n < minFlowMapEntries || n > maxFlowMapEntries {
		return 0, fmt.Errorf("%w: %s=%q, want an integer in [%d, %d]",
			errInvalidConfig, FlowMapEntriesEnv, v, minFlowMapEntries, maxFlowMapEntries)
	}
	return uint32(n), nil
}
