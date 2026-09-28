// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// symProgSource is compiled for Linux by the tests below; its two
// noinline functions are the symbolization targets.
const symProgSource = `package main

import "os"

//go:noinline
func khSymTarget(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += i * i
	}
	return s
}

//go:noinline
func khSymOther(n int) int { return n * 3 }

func main() {
	os.Exit(khSymTarget(len(os.Args)) + khSymOther(1) - 3)
}
`

var (
	buildMu    sync.Mutex
	buildDir   string // removed by TestMain
	buildCache = map[string]string{}
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// buildLinuxProgram compiles symProgSource for linux/arch with extra
// `go build` flags and returns the binary's path. Results are cached per
// test binary run.
func buildLinuxProgram(t testing.TB, arch string, flags ...string) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not in PATH")
	}
	key := arch + " " + strings.Join(flags, " ")
	buildMu.Lock()
	defer buildMu.Unlock()
	if p, ok := buildCache[key]; ok {
		return p
	}
	if buildDir == "" {
		d, err := os.MkdirTemp("", "kh-symprog-")
		if err != nil {
			t.Fatal(err)
		}
		buildDir = d
	}
	dir := filepath.Join(buildDir, strings.NewReplacer(" ", "_", "=", "_", "-", "").Replace(key))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module symprog\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(symProgSource), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "symprog")
	args := append([]string{"build", "-o", out}, flags...)
	cmd := exec.Command(goBin, append(args, ".")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %v: %v\n%s", flags, err, b)
	}
	buildCache[key] = out
	return out
}

// funcSym returns the address and size of a function from a binary's
// .symtab, read independently of the code under test.
func funcSym(t testing.TB, path, name string) (addr, size uint64) {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		if s.Name == name {
			return s.Value, s.Size
		}
	}
	t.Fatalf("%s not in %s", name, path)
	return 0, 0
}

// fileOffsetOf converts a link-time address into its file offset using
// the binary's PT_LOAD headers (the inverse of elfSymbols.vaddr).
func fileOffsetOf(t testing.TB, path string, vaddr uint64) uint64 {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && vaddr >= p.Vaddr && vaddr < p.Vaddr+p.Filesz {
			return vaddr - p.Vaddr + p.Off
		}
	}
	t.Fatalf("%#x not in any PT_LOAD of %s", vaddr, path)
	return 0
}

func readSymbolsFile(t testing.TB, path string) *elfSymbols {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	es, err := readELFSymbols(f)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestELFSymbolization(t *testing.T) {
	variants := []struct {
		name       string
		flags      []string
		refFlags   []string // unstripped twin providing reference addresses
		wantSource string
	}{
		{"exe", nil, nil, "symtab"},
		{"exe stripped", []string{"-ldflags=-s -w"}, nil, "gopclntab"},
		{"pie", []string{"-buildmode=pie"}, []string{"-buildmode=pie"}, "symtab"},
		{"pie stripped", []string{"-buildmode=pie", "-ldflags=-s -w"}, []string{"-buildmode=pie"}, "gopclntab"},
	}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, v := range variants {
			t.Run(arch+" "+v.name, func(t *testing.T) {
				bin := buildLinuxProgram(t, arch, v.flags...)
				ref := buildLinuxProgram(t, arch, v.refFlags...)
				es := readSymbolsFile(t, bin)
				if es.source != v.wantSource {
					t.Fatalf("source = %q, want %q", es.source, v.wantSource)
				}
				for _, fn := range []string{"main.khSymTarget", "main.khSymOther"} {
					addr, size := funcSym(t, ref, fn)
					if size == 0 {
						t.Fatalf("%s has no size", fn)
					}
					for _, a := range []uint64{addr, addr + size/2, addr + size - 1} {
						off := fileOffsetOf(t, bin, a)
						if got, ok := es.lookupFileOffset(off); !ok || got != fn {
							t.Errorf("%s at %#x (file offset %#x) = %q, %v", fn, a, off, got, ok)
						}
						if va, ok := es.vaddr(off); !ok || va != a {
							t.Errorf("vaddr(%#x) = %#x, %v; want %#x", off, va, ok, a)
						}
					}
					if got, _ := es.table.lookup(addr - 1); got == fn {
						t.Errorf("address before %s resolved to it", fn)
					}
				}
			})
		}
	}
}

func TestReadELFSymbolsRejectsGarbage(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     nil,
		"not elf":   []byte("#!/bin/sh\necho hi\n"),
		"truncated": append([]byte("\x7fELF\x02\x01\x01"), make([]byte, 20)...),
	} {
		if _, err := readELFSymbols(strings.NewReader(string(data))); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
