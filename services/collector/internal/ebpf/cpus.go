// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"fmt"
	"strconv"
	"strings"
)

// maxCPUs bounds how many perf events one collector opens. Well above
// any shipping server; a corrupt cpu list must not fan out unbounded.
const maxCPUs = 8192

// parseCPUList parses the kernel's cpu list format, as found in
// /sys/devices/system/cpu/online: "0-3,5,7-8".
func parseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty cpu list")
	}
	var cpus []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil || first < 0 {
			return nil, fmt.Errorf("bad cpu list %q", s)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(hi); err != nil || last < first {
				return nil, fmt.Errorf("bad cpu list %q", s)
			}
		}
		if last >= maxCPUs || len(cpus)+(last-first+1) > maxCPUs {
			return nil, fmt.Errorf("cpu list %q exceeds %d cpus", s, maxCPUs)
		}
		for c := first; c <= last; c++ {
			cpus = append(cpus, c)
		}
	}
	return cpus, nil
}
