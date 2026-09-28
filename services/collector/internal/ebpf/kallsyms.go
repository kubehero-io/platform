// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// maxKernelSymbols bounds the kallsyms table. Distribution kernels with
// every module loaded carry ~250k text symbols; this leaves headroom
// while capping memory at a few tens of MB even for pathological input.
const maxKernelSymbols = 1 << 20

// symTable is a sorted, compact address -> name index. Names live in one
// shared blob so a table of a few hundred thousand symbols is two slices
// plus one string, not hundreds of thousands of GC-visible objects.
type symTable struct {
	addrs []uint64 // sorted ascending, unique
	ends  []uint64 // exclusive end; 0 = unknown size (extends to the next symbol)
	offs  []uint32 // name i is blob[offs[i]:offs[i+1]]
	blob  string
}

// maxUnsizedSpan caps how far past an unsized symbol (all of kallsyms,
// hand-written assembly in ELF files) an address may fall and still be
// attributed to it. Without it, an address in a module loaded after the
// table was built would resolve to whatever symbol sits last below it.
const maxUnsizedSpan = 1 << 20

// lookup returns the symbol containing addr. A nil table (a binary or
// process without symbols) finds nothing.
func (t *symTable) lookup(addr uint64) (string, bool) {
	if t == nil {
		return "", false
	}
	n := len(t.addrs)
	i := sort.Search(n, func(i int) bool { return t.addrs[i] > addr }) - 1
	if i < 0 {
		return "", false
	}
	if end := t.ends[i]; end != 0 {
		if addr >= end {
			return "", false
		}
	} else if addr-t.addrs[i] >= maxUnsizedSpan {
		return "", false
	}
	return t.blob[t.offs[i]:t.offs[i+1]], true
}

func (t *symTable) len() int {
	if t == nil {
		return 0
	}
	return len(t.addrs)
}

// symbol is a builder-side entry; rank breaks ties between aliases at the
// same address (lower wins).
type symbol struct {
	addr, size uint64
	name       string
	rank       int
}

// newSymTable sorts syms, keeps one name per address (lowest rank, then
// shortest name — aliases like malloc/__libc_malloc collapse to the
// public one) and packs the result.
func newSymTable(syms []symbol) *symTable {
	sort.Slice(syms, func(i, j int) bool {
		a, b := syms[i], syms[j]
		if a.addr != b.addr {
			return a.addr < b.addr
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if len(a.name) != len(b.name) {
			return len(a.name) < len(b.name)
		}
		return a.name < b.name
	})
	t := &symTable{
		addrs: make([]uint64, 0, len(syms)),
		ends:  make([]uint64, 0, len(syms)),
		offs:  make([]uint32, 0, len(syms)+1),
	}
	var blob strings.Builder
	for i, s := range syms {
		if i > 0 && s.addr == syms[i-1].addr {
			continue
		}
		if blob.Len()+len(s.name) > 1<<31 {
			break // offsets are uint32; unreachable with the entry caps
		}
		end := uint64(0)
		if s.size > 0 {
			end = s.addr + s.size
		}
		t.addrs = append(t.addrs, s.addr)
		t.ends = append(t.ends, end)
		t.offs = append(t.offs, uint32(blob.Len()))
		blob.WriteString(s.name)
	}
	t.offs = append(t.offs, uint32(blob.Len()))
	t.blob = blob.String()
	return t
}

// errKallsymsRestricted means every address read as zero: kptr_restrict
// hides kernel pointers from this process (it lacks CAP_SYSLOG, or
// kptr_restrict=2). Kernel frames then stay unsymbolized.
var errKallsymsRestricted = errors.New("kallsyms addresses are hidden (kptr_restrict)")

// parseKallsyms reads /proc/kallsyms-formatted text and keeps function
// symbols (types t/T and weak w/W):
//
//	ffffffff81000000 T _stext
//	ffffffffc0a01000 t nf_hook_slow	[nf_tables]
func parseKallsyms(r io.Reader) (*symTable, error) {
	var syms []symbol
	sawNonZero := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		addrStr, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		typ, rest, ok := strings.Cut(rest, " ")
		if !ok || len(typ) != 1 {
			continue
		}
		name, _, _ := strings.Cut(rest, "\t") // drop "\t[module]"
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		addr, err := strconv.ParseUint(addrStr, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("kallsyms: bad address in %q", line)
		}
		if addr != 0 {
			sawNonZero = true
		}
		rank := 0
		switch typ[0] {
		case 'T':
		case 't':
			rank = 1
		case 'W', 'w':
			rank = 2
		default:
			continue
		}
		if addr == 0 {
			continue
		}
		if len(syms) >= maxKernelSymbols {
			return nil, fmt.Errorf("kallsyms: more than %d text symbols", maxKernelSymbols)
		}
		syms = append(syms, symbol{addr: addr, name: name, rank: rank})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("kallsyms: %w", err)
	}
	if !sawNonZero {
		return nil, errKallsymsRestricted
	}
	return newSymTable(syms), nil
}
