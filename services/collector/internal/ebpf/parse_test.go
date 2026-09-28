// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseCPUList(t *testing.T) {
	tests := []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{"0-3\n", []int{0, 1, 2, 3}, false},
		{"0", []int{0}, false},
		{"0,2-3,7", []int{0, 2, 3, 7}, false},
		{"", nil, true},
		{"3-1", nil, true},
		{"a-b", nil, true},
		{"-1", nil, true},
		{"0-99999", nil, true},
	}
	for _, tt := range tests {
		got, err := parseCPUList(tt.in)
		if (err != nil) != tt.wantErr || !slices.Equal(got, tt.want) {
			t.Errorf("parseCPUList(%q) = %v, %v; want %v, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

const kallsymsFixture = `ffff800080010000 T _stext
ffff800080010000 t __pi__stext
ffff800080010000 T __irqentry_text_start
ffff800080010100 T do_one_initcall
ffff800080010400 t run_init_process
ffff800080010800 D some_data
ffff800080011000 W weak_func
ffff800080012000 T __schedule
ffff800080013000 T schedule
ffff8000c0a01000 t nf_hook_slow	[nf_tables]
ffff8000c0a02000 t nft_do_chain	[nf_tables]
`

func TestParseKallsyms(t *testing.T) {
	tab, err := parseKallsyms(strings.NewReader(kallsymsFixture))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		addr uint64
		want string
		ok   bool
	}{
		{0xffff800080010000, "_stext", true}, // alias set: global, then shortest name
		{0xffff800080010050, "_stext", true},
		{0xffff800080010100, "do_one_initcall", true},
		{0xffff8000800103ff, "do_one_initcall", true},
		{0xffff800080010900, "run_init_process", true}, // data symbols are not functions
		{0xffff800080011010, "weak_func", true},
		{0xffff800080012fff, "__schedule", true},
		{0xffff800080013004, "schedule", true},
		{0xffff8000c0a01010, "nf_hook_slow", true}, // module suffix stripped
		{0xffff8000c0a02010, "nft_do_chain", true},
		{0xffff80008000ffff, "", false},                  // below the first symbol
		{0xffff8000c0a02000 + maxUnsizedSpan, "", false}, // far past the last symbol
	}
	for _, tt := range tests {
		got, ok := tab.lookup(tt.addr)
		if got != tt.want || ok != tt.ok {
			t.Errorf("lookup(%#x) = %q, %v; want %q, %v", tt.addr, got, ok, tt.want, tt.ok)
		}
	}
	if tab.len() != 8 {
		t.Errorf("kept %d symbols, want 8 unique text addresses", tab.len())
	}
}

func TestParseKallsymsRestricted(t *testing.T) {
	in := "0000000000000000 T _stext\n0000000000000000 T schedule\n"
	if _, err := parseKallsyms(strings.NewReader(in)); !errors.Is(err, errKallsymsRestricted) {
		t.Fatalf("err = %v, want errKallsymsRestricted", err)
	}
}

func TestParseKallsymsBadAddress(t *testing.T) {
	if _, err := parseKallsyms(strings.NewReader("zzzz T foo\n")); err == nil {
		t.Fatal("expected error")
	}
}

const mapsFixture = `00400000-00452000 r-xp 00000000 fd:01 1234 /usr/bin/app
00651000-00652000 rw-p 00051000 fd:01 1234 /usr/bin/app
01ad2000-01af3000 rw-p 00000000 00:00 0                                  [heap]
7f3c4a200000-7f3c4a228000 r--p 00000000 fd:01 1834561                    /usr/lib/x86_64-linux-gnu/libc.so.6
7f3c4a228000-7f3c4a3b5000 r-xp 00028000 fd:01 1834561                    /usr/lib/x86_64-linux-gnu/libc.so.6
7f3c4a500000-7f3c4a510000 rwxp 00000000 00:00 0
7f3c4a600000-7f3c4a601000 r-xp 00000000 00:1f 99 /opt/my app/lib with spaces.so
7f3c4a700000-7f3c4a701000 r-xp 00001000 00:1f 100 /tmp/old.so (deleted)
7ffd3b1f5000-7ffd3b1f7000 r-xp 00000000 00:00 0                          [vdso]
ffffffffff600000-ffffffffff601000 --xp 00000000 00:00 0                  [vsyscall]
`

func TestParseProcMaps(t *testing.T) {
	pm, err := parseProcMaps(strings.NewReader(mapsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(pm) != 7 {
		t.Fatalf("got %d executable mappings, want 7: %+v", len(pm), pm)
	}
	tests := []struct {
		addr       uint64
		path       string
		fileBacked bool
		offset     uint64
		dev        uint64
		inode      uint64
	}{
		{0x401234, "/usr/bin/app", true, 0, 0xfd<<32 | 1, 1234},
		{0x7f3c4a300000, "/usr/lib/x86_64-linux-gnu/libc.so.6", true, 0x28000, 0xfd<<32 | 1, 1834561},
		{0x7f3c4a500010, "", false, 0, 0, 0},
		{0x7f3c4a600010, "/opt/my app/lib with spaces.so", true, 0, 0x1f, 99},
		{0x7f3c4a700010, "/tmp/old.so (deleted)", true, 0x1000, 0x1f, 100},
		{0x7ffd3b1f5100, "[vdso]", false, 0, 0, 0},
	}
	for _, tt := range tests {
		m := pm.find(tt.addr)
		if m == nil {
			t.Errorf("find(%#x) = nil", tt.addr)
			continue
		}
		if m.path != tt.path || m.fileBacked() != tt.fileBacked || m.offset != tt.offset || m.dev != tt.dev || m.inode != tt.inode {
			t.Errorf("find(%#x) = %+v (fileBacked=%v), want path %q fileBacked %v offset %#x dev %#x inode %d",
				tt.addr, *m, m.fileBacked(), tt.path, tt.fileBacked, tt.offset, tt.dev, tt.inode)
		}
	}
	for _, addr := range []uint64{0x3fffff, 0x651500 /* rw-p */, 0x7f3c4a210000 /* r--p */, 0x7f3c4a3b5000 /* end is exclusive */} {
		if m := pm.find(addr); m != nil {
			t.Errorf("find(%#x) = %+v, want nil", addr, *m)
		}
	}
}

func TestParseProcMapsMalformed(t *testing.T) {
	for _, line := range []string{
		"zzzz-0000 r-xp 00000000 00:00 0 x",
		"00400000 r-xp 00000000 00:00 0 x",
		"00500000-00400000 r-xp 00000000 00:00 0 x",
		"00400000-00500000 r-xp 00000000 0000 0 x",
		"00400000-00500000 r-xp",
	} {
		if _, err := parseProcMaps(strings.NewReader(line + "\n")); err == nil {
			t.Errorf("expected error for %q", line)
		}
	}
}

func TestParsePerfMap(t *testing.T) {
	in := strings.Join([]string{
		"3ef414a0 5c LazyCompile:~foo /app/index.js:1",
		"0x7f0000001000 0x40 Lcom/example/Hot;::run",
		"7f0000002000 20 Interpreter",
		"garbage",
		"7f0000003000 zz bad size",
		"7f0000004000 0 zero size",
		"7f0000002000 20 Interpreter2", // recompiled at a reused address
		"7f0000005000 10 torn",         // no trailing newline, like a concurrent append
	}, "\n")
	tab, err := parsePerfMap(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		addr uint64
		want string
		ok   bool
	}{
		{0x3ef414a0, "LazyCompile:~foo /app/index.js:1", true},
		{0x3ef414fb, "LazyCompile:~foo /app/index.js:1", true},
		{0x3ef414fc, "", false},
		{0x7f0000001020, "Lcom/example/Hot;::run", true},
		{0x7f0000002010, "Interpreter2", true},
		{0x7f0000003010, "", false},
		{0x7f0000005004, "torn", true},
	}
	for _, tt := range tests {
		got, ok := tab.lookup(tt.addr)
		if got != tt.want || ok != tt.ok {
			t.Errorf("lookup(%#x) = %q, %v; want %q, %v", tt.addr, got, ok, tt.want, tt.ok)
		}
	}
}

func TestLRU(t *testing.T) {
	c := newLRU[string, int](2)
	c.put("a", 1)
	c.put("b", 2)
	if v, ok := c.get("a"); !ok || v != 1 { // a is now most recent
		t.Fatalf("get(a) = %v, %v", v, ok)
	}
	c.put("c", 3) // evicts b
	if _, ok := c.get("b"); ok {
		t.Error("b should have been evicted")
	}
	c.put("a", 10) // update in place
	if v, _ := c.get("a"); v != 10 || c.len() != 2 {
		t.Errorf("a = %d, len = %d", v, c.len())
	}
	if newLRU[int, int](0).cap != 1 {
		t.Error("capacity must be at least 1")
	}
}
