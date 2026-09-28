// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// maxMappings bounds one process's executable mappings. JIT-heavy
// runtimes map many small regions, but tens of thousands of executable
// ones would indicate a corrupt read, not a real process.
const maxMappings = 65536

// mapping is one executable region of a process address space.
type mapping struct {
	start, end uint64
	offset     uint64 // file offset of start
	dev        uint64 // major<<32 | minor, as reported by the kernel
	inode      uint64
	path       string // "" for anonymous memory; "[vdso]" etc. for special regions
}

// fileBacked reports whether the mapping comes from a regular file we
// could open and read symbols from.
func (m *mapping) fileBacked() bool {
	return m.inode != 0 && strings.HasPrefix(m.path, "/")
}

// procMaps is a process's executable mappings sorted by start address.
type procMaps []mapping

func (pm procMaps) find(addr uint64) *mapping {
	i := sort.Search(len(pm), func(i int) bool { return pm[i].end > addr })
	if i < len(pm) && pm[i].start <= addr {
		return &pm[i]
	}
	return nil
}

// parseProcMaps reads /proc/<pid>/maps and keeps executable mappings:
//
//	7f3c4a200000-7f3c4a3b5000 r-xp 00028000 fd:01 1834561    /usr/lib/x86_64-linux-gnu/libc.so.6
//	7ffd3b1f5000-7ffd3b1f7000 r-xp 00000000 00:00 0          [vdso]
//
// Paths may contain spaces and a " (deleted)" suffix; both are kept
// verbatim (the suffix makes the path unopenable, which is fine: such
// files are read through /proc/<pid>/map_files instead).
func parseProcMaps(r io.Reader) (procMaps, error) {
	var out procMaps
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		m, exec, err := parseMapsLine(sc.Text())
		if err != nil {
			return nil, err
		}
		if !exec {
			continue
		}
		if len(out) >= maxMappings {
			return nil, fmt.Errorf("maps: more than %d executable mappings", maxMappings)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("maps: %w", err)
	}
	// The kernel already emits ascending order; sorting keeps find()
	// correct for any other producer (tests, fixtures).
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out, nil
}

func parseMapsLine(line string) (m mapping, exec bool, err error) {
	bad := func() (mapping, bool, error) {
		return mapping{}, false, fmt.Errorf("maps: malformed line %q", line)
	}
	rest := line
	next := func() string {
		rest = strings.TrimLeft(rest, " \t")
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			tok := rest
			rest = ""
			return tok
		}
		tok := rest[:i]
		rest = rest[i:]
		return tok
	}
	rng, perms, off, dev, ino := next(), next(), next(), next(), next()
	if ino == "" || len(perms) < 4 {
		return bad()
	}
	lo, hi, ok := strings.Cut(rng, "-")
	if !ok {
		return bad()
	}
	if m.start, err = strconv.ParseUint(lo, 16, 64); err != nil {
		return bad()
	}
	if m.end, err = strconv.ParseUint(hi, 16, 64); err != nil || m.end < m.start {
		return bad()
	}
	if m.offset, err = strconv.ParseUint(off, 16, 64); err != nil {
		return bad()
	}
	major, minor, ok := strings.Cut(dev, ":")
	if !ok {
		return bad()
	}
	maj, err1 := strconv.ParseUint(major, 16, 32)
	mnr, err2 := strconv.ParseUint(minor, 16, 32)
	if err1 != nil || err2 != nil {
		return bad()
	}
	m.dev = maj<<32 | mnr
	if m.inode, err = strconv.ParseUint(ino, 10, 64); err != nil {
		return bad()
	}
	m.path = strings.TrimLeft(rest, " \t")
	return m, perms[2] == 'x', nil
}
