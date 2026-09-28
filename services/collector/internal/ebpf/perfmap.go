// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// Limits for /tmp/perf-<pid>.map files. JIT runtimes append to them for
// the life of the process (V8 with --perf-basic-prof, the JVM with
// perf-map-agent, .NET with DOTNET_PerfMapEnabled, Python 3.12's
// -X perf), so a long-running process can grow one without bound.
const (
	maxPerfMapBytes   = 64 << 20
	maxPerfMapEntries = 1 << 20
)

// parsePerfMap reads the perf JIT map format, one symbol per line:
//
//	START SIZE symbolname
//
// with START and SIZE in hex, with or without a 0x prefix. Malformed
// lines are skipped rather than failing the whole file: writers append
// concurrently and the last line may be torn.
func parsePerfMap(r io.Reader) (*symTable, error) {
	var syms []symbol
	sc := bufio.NewScanner(io.LimitReader(r, maxPerfMapBytes))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() && len(syms) < maxPerfMapEntries {
		line := strings.TrimSpace(sc.Text())
		startStr, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		sizeStr, name, ok := strings.Cut(strings.TrimLeft(rest, " "), " ")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			continue
		}
		start, err1 := strconv.ParseUint(strings.TrimPrefix(startStr, "0x"), 16, 64)
		size, err2 := strconv.ParseUint(strings.TrimPrefix(sizeStr, "0x"), 16, 64)
		if err1 != nil || err2 != nil || size == 0 || start+size < start {
			continue
		}
		// Later lines describe code JIT-ed more recently at a reused
		// address; the lower rank wins in newSymTable.
		syms = append(syms, symbol{addr: start, size: size, name: name, rank: -len(syms)})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return newSymTable(syms), nil
}
