// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"debug/elf"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeProc lays out the parts of a procfs the symbolizer reads.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	return &fakeProc{t: t, root: t.TempDir()}
}

func (fp *fakeProc) write(rel, content string) string {
	fp.t.Helper()
	p := filepath.Join(fp.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fp.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		fp.t.Fatal(err)
	}
	return p
}

// installBinary copies bin into pid's root at path and returns its inode.
func (fp *fakeProc) installBinary(pid int, path, bin string) uint64 {
	fp.t.Helper()
	data, err := os.ReadFile(bin)
	if err != nil {
		fp.t.Fatal(err)
	}
	p := fp.write(filepath.Join(fmt.Sprint(pid), "root", path), string(data))
	fi, err := os.Stat(p)
	if err != nil {
		fp.t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Ino)
}

// execMapsLine renders the maps line the kernel would show for bin's
// executable segment loaded at base.
func execMapsLine(t *testing.T, bin string, base uint64, inode uint64, path string) string {
	t.Helper()
	f, err := elf.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 {
			start := base + p.Vaddr&^0xfff
			end := base + (p.Vaddr+p.Memsz+0xfff)&^0xfff
			return fmt.Sprintf("%x-%x r-xp %08x fd:01 %d %s", start, end, p.Off&^0xfff, inode, path)
		}
	}
	t.Fatal("no executable segment")
	return ""
}

func TestSymbolizerUserStacks(t *testing.T) {
	bin := buildLinuxProgram(t, "amd64", "-buildmode=pie")
	const pid, nsPID = 4242, 7
	const base = uint64(0x5555_0000_0000) // where "the kernel" put the PIE
	fp := newFakeProc(t)
	ino := fp.installBinary(pid, "/app/symprog", bin)
	fp.write(fmt.Sprintf("%d/maps", pid), strings.Join([]string{
		execMapsLine(t, bin, base, ino, "/app/symprog"),
		"7f0000000000-7f0000010000 rwxp 00000000 00:00 0 ",
		"7ffd3b1f5000-7ffd3b1f7000 r-xp 00000000 00:00 0                          [vdso]",
	}, "\n")+"\n")
	fp.write(fmt.Sprintf("%d/status", pid), "Name:\tnode\nNSpid:\t4242\t7\n")
	fp.write(fmt.Sprintf("%d/root/tmp/perf-%d.map", pid, nsPID), "7f0000000100 80 LazyCompile:*handler /srv/app.js:10\n")

	target, targetSize := funcSym(t, bin, "main.khSymTarget")
	other, _ := funcSym(t, bin, "main.khSymOther")

	s := newSymbolizer(fp.root, discardLog)
	opens := 0
	s.openMapped = func(root string, p uint32, m *mapping) (*os.File, error) {
		opens++
		return openMappedFile(root, p, m)
	}
	s.beginDrain()
	got := s.userFrames(pid, []uint64{
		base + target + targetSize/2, // leaf: symbolized as is
		base + other + 1,             // return address: call site is one byte back
		0x7f0000000140,               // JIT code, named by the perf map
		0x7ffd3b1f5100,               // vDSO
		0x6000_0000_0000,             // not mapped
	})
	want := []string{"main.khSymTarget", "main.khSymOther", "LazyCompile:*handler /srv/app.js:10", "[vdso]", unknownFrame}
	if !slices.Equal(got, want) {
		t.Fatalf("frames = %q\nwant     %q", got, want)
	}

	// The next drain re-reads maps but not the binary.
	s.beginDrain()
	if got := s.userFrames(pid, []uint64{base + target}); got[0] != "main.khSymTarget" {
		t.Fatalf("cached lookup = %q", got)
	}
	if opens != 1 {
		t.Errorf("binary opened %d times, want 1 (cached by dev+inode)", opens)
	}

	// JIT maps grow; a changed file is re-read on the next drain.
	fp.write(fmt.Sprintf("%d/root/tmp/perf-%d.map", pid, nsPID),
		"7f0000000100 80 LazyCompile:*handler /srv/app.js:10\n7f0000000200 40 LazyCompile:*later /srv/app.js:20\n")
	s.beginDrain()
	if got := s.userFrames(pid, []uint64{0x7f0000000210}); got[0] != "LazyCompile:*later /srv/app.js:20" {
		t.Errorf("perf map not refreshed: %q", got)
	}
}

func TestSymbolizerRefusesUnsafeFiles(t *testing.T) {
	bin := buildLinuxProgram(t, "amd64")
	fp := newFakeProc(t)

	// pid 1: the mapped path escapes the process root through a symlink.
	outside := fp.write("outside/app/symprog", mustRead(t, bin))
	if err := os.MkdirAll(filepath.Join(fp.root, "1", "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(fp.root, "1", "root", "app")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(outside)
	fp.write("1/maps", execMapsLine(t, bin, 0, uint64(fi.Sys().(*syscall.Stat_t).Ino), "/app/symprog")+"\n")

	// pid 2: the file at the path is not the mapped one (inode differs).
	ino := fp.installBinary(2, "/app/symprog", bin)
	fp.write("2/maps", execMapsLine(t, bin, 0, ino+1, "/app/symprog")+"\n")

	// pid 3: deleted file, reachable only through map_files (absent here).
	fp.write("3/maps", strings.Replace(execMapsLine(t, bin, 0, 77, "/app/symprog"), "/app/symprog", "/app/symprog (deleted)", 1)+"\n")

	target, _ := funcSym(t, bin, "main.khSymTarget")
	s := newSymbolizer(fp.root, discardLog)
	s.beginDrain()
	for _, pid := range []uint32{1, 2, 3} {
		if got := s.userFrames(pid, []uint64{target}); got[0] != unknownFrame {
			t.Errorf("pid %d: frame = %q, want %q", pid, got[0], unknownFrame)
		}
	}
	// A process that exited (no maps) yields unknown frames, not errors.
	if got := s.userFrames(99, []uint64{target, target}); !slices.Equal(got, []string{unknownFrame, unknownFrame}) {
		t.Errorf("exited process: %q", got)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSymbolizerKernelStacks(t *testing.T) {
	fp := newFakeProc(t)
	fp.write("kallsyms", kallsymsFixture)
	s := newSymbolizer(fp.root, discardLog)
	s.beginDrain()
	got := s.kernelFrames([]uint64{
		0xffff800080012100, // leaf, inside __schedule
		0xffff800080013001, // return address right after the first byte of schedule
		0xffff800080013000, // return address at schedule's first byte: the call was in __schedule
		0x1000,             // garbage
	})
	want := []string{"__schedule [k]", "schedule [k]", "__schedule [k]", unknownFrame}
	if !slices.Equal(got, want) {
		t.Fatalf("frames = %q, want %q", got, want)
	}

	// Without a readable kallsyms every kernel frame is unknown.
	s2 := newSymbolizer(t.TempDir(), discardLog)
	s2.beginDrain()
	if got := s2.kernelFrames([]uint64{0xffff800080012100}); got[0] != unknownFrame {
		t.Errorf("no kallsyms: %q", got)
	}
}

func TestCallSite(t *testing.T) {
	if callSite(0x1000, 0) != 0x1000 || callSite(0x1000, 1) != 0xfff || callSite(0, 3) != 0 {
		t.Error("callSite must only adjust return addresses (depth > 0)")
	}
}
