// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	unknownFrame = "[unknown]"
	kernelSuffix = " [k]"

	// binaryCacheSize bounds parsed symbol tables kept across drains,
	// keyed by (device, inode): a node runs many processes but far fewer
	// distinct binaries, and the hot ones stay resident.
	binaryCacheSize = 64
	// perfMapCacheSize bounds parsed JIT maps (one per process).
	perfMapCacheSize = 16
	// kallsymsTTL: kernel modules come and go, so the table is rebuilt
	// periodically rather than trusted forever.
	kallsymsTTL = 10 * time.Minute
	// maxMapsBytes bounds one /proc/<pid>/maps read.
	maxMapsBytes = 16 << 20
)

// fileID identifies a binary independently of the path it was mapped
// through (containers share image layers under different paths).
type fileID struct{ dev, inode uint64 }

type perfMapEntry struct {
	size  int64
	mtime time.Time
	table *symTable
}

// symbolizer turns raw stack addresses into frame names. It is owned by
// the profiler's drain loop and not safe for concurrent use.
type symbolizer struct {
	procRoot string
	log      *slog.Logger
	now      func() time.Time
	// openMapped opens the file behind a mapping of pid (map_files, then
	// the process root); overridable in tests.
	openMapped func(procRoot string, pid uint32, m *mapping) (*os.File, error)
	// openInRoot opens path inside pid's root directory.
	openInRoot func(procRoot string, pid uint32, path string) (*os.File, error)

	kernel      *symTable
	kernelErr   error
	kernelAt    time.Time
	loggedKsyms bool

	binaries *lru[fileID, *elfSymbols]   // nil value: unreadable / no symbols
	perfMaps *lru[uint32, *perfMapEntry] // nil value: no perf map

	// Per-drain state, reset by beginDrain: processes are re-read every
	// drain because PIDs are reused and mappings change (dlopen, JIT).
	maps       map[uint32]procMaps
	perfMapped map[uint32]bool // perf map already (re)validated this drain
}

func newSymbolizer(procRoot string, log *slog.Logger) *symbolizer {
	return &symbolizer{
		procRoot:   procRoot,
		log:        log,
		now:        time.Now,
		openMapped: openMappedFile,
		openInRoot: openFileInRoot,
		binaries:   newLRU[fileID, *elfSymbols](binaryCacheSize),
		perfMaps:   newLRU[uint32, *perfMapEntry](perfMapCacheSize),
	}
}

// beginDrain resets per-drain caches and refreshes kallsyms when stale.
func (s *symbolizer) beginDrain() {
	s.maps = make(map[uint32]procMaps)
	s.perfMapped = make(map[uint32]bool)
	if s.kernelAt.IsZero() || s.now().Sub(s.kernelAt) >= kallsymsTTL {
		s.loadKallsyms()
	}
}

func (s *symbolizer) loadKallsyms() {
	s.kernelAt = s.now()
	f, err := os.Open(filepath.Join(s.procRoot, "kallsyms"))
	if err == nil {
		var t *symTable
		t, err = parseKallsyms(f)
		_ = f.Close()
		if err == nil {
			s.kernel, s.kernelErr = t, nil
			return
		}
	}
	// Keep a previous good table rather than dropping to nothing.
	s.kernelErr = err
	if !s.loggedKsyms {
		s.loggedKsyms = true
		s.log.Warn("ebpf profiler: kernel frames will be unsymbolized", "err", err)
	}
}

// kernelFrames symbolizes a kernel stack (leaf first, as the kernel
// records it) into names suffixed " [k]".
func (s *symbolizer) kernelFrames(ips []uint64) []string {
	frames := make([]string, len(ips))
	for i, ip := range ips {
		frames[i] = unknownFrame
		if s.kernel == nil {
			continue
		}
		if name, ok := s.kernel.lookup(callSite(ip, i)); ok {
			frames[i] = name + kernelSuffix
		}
	}
	return frames
}

// userFrames symbolizes a user stack of process pid (leaf first).
func (s *symbolizer) userFrames(pid uint32, ips []uint64) []string {
	pm := s.procMaps(pid)
	frames := make([]string, len(ips))
	for i, ip := range ips {
		frames[i] = s.userFrame(pid, pm, callSite(ip, i))
	}
	return frames
}

// callSite maps a stack slot to the address to symbolize. Every slot but
// the leaf holds a return address, the instruction after the call; one
// byte back lands inside the call itself, which matters when the call is
// the last instruction of a function (noreturn calls).
func callSite(ip uint64, depth int) uint64 {
	if depth > 0 && ip > 0 {
		return ip - 1
	}
	return ip
}

func (s *symbolizer) userFrame(pid uint32, pm procMaps, addr uint64) string {
	m := pm.find(addr)
	switch {
	case m == nil:
		// Not in any executable mapping we know of: the process exited
		// or remapped since sampling.
		return unknownFrame
	case m.fileBacked():
		if name, ok := s.binary(pid, m).lookupFileOffset(addr - m.start + m.offset); ok {
			return name
		}
		return unknownFrame
	case m.path == "[vdso]":
		return "[vdso]"
	case m.path == "" || strings.HasPrefix(m.path, "[anon"):
		// Anonymous executable memory is JIT output; only the runtime's
		// own perf map can name it.
		if name, ok := s.perfMap(pid).lookup(addr); ok {
			return name
		}
	}
	return unknownFrame
}

// procMaps returns pid's executable mappings, read once per drain. A
// process that already exited yields an empty set (all frames unknown).
func (s *symbolizer) procMaps(pid uint32) procMaps {
	if pm, ok := s.maps[pid]; ok {
		return pm
	}
	var pm procMaps
	f, err := os.Open(filepath.Join(s.procRoot, strconv.FormatUint(uint64(pid), 10), "maps"))
	if err == nil {
		pm, err = parseProcMaps(io.LimitReader(f, maxMapsBytes))
		_ = f.Close()
		if err != nil {
			s.log.Debug("ebpf profiler: unreadable maps", "pid", pid, "err", err)
		}
	}
	s.maps[pid] = pm
	return pm
}

// binary returns the symbols of the file behind m, loading and caching
// them on first use. Failures are cached too, so a stripped or
// unreadable binary costs one attempt per cache lifetime, not one per
// frame.
func (s *symbolizer) binary(pid uint32, m *mapping) *elfSymbols {
	id := fileID{dev: m.dev, inode: m.inode}
	if es, ok := s.binaries.get(id); ok {
		return es
	}
	es, err := s.loadBinary(pid, m)
	if err != nil {
		s.log.Debug("ebpf profiler: no symbols", "pid", pid, "path", m.path, "err", err)
		es = nil
	}
	s.binaries.put(id, es)
	return es
}

func (s *symbolizer) loadBinary(pid uint32, m *mapping) (*elfSymbols, error) {
	f, err := s.openMapped(s.procRoot, pid, m)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := checkRegular(f, m.inode); err != nil {
		return nil, err
	}
	return readELFSymbols(f)
}

// perfMap returns pid's JIT symbol map, re-validated (size + mtime) at
// most once per drain because runtimes keep appending to it.
func (s *symbolizer) perfMap(pid uint32) *symTable {
	cached, _ := s.perfMaps.get(pid)
	if !s.perfMapped[pid] {
		s.perfMapped[pid] = true
		cached = s.refreshPerfMap(pid, cached)
		s.perfMaps.put(pid, cached)
	}
	if cached == nil {
		return nil
	}
	return cached.table
}

func (s *symbolizer) refreshPerfMap(pid uint32, cached *perfMapEntry) *perfMapEntry {
	f, err := s.openInRoot(s.procRoot, pid, fmt.Sprintf("/tmp/perf-%d.map", s.nsPID(pid)))
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	if cached != nil && cached.size == fi.Size() && cached.mtime.Equal(fi.ModTime()) {
		return cached
	}
	t, err := parsePerfMap(f)
	if err != nil {
		return nil
	}
	return &perfMapEntry{size: fi.Size(), mtime: fi.ModTime(), table: t}
}

// nsPID is pid as the process sees itself (innermost PID namespace),
// which is the number JIT runtimes put in their perf map file name.
func (s *symbolizer) nsPID(pid uint32) uint32 {
	f, err := os.Open(filepath.Join(s.procRoot, strconv.FormatUint(uint64(pid), 10), "status"))
	if err != nil {
		return pid
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "NSpid:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				if n, err := strconv.ParseUint(fields[len(fields)-1], 10, 32); err == nil {
					return uint32(n)
				}
			}
			break
		}
	}
	return pid
}

// checkRegular rejects anything but a regular file with the expected
// inode: the path may have been swapped (rename + symlink) between the
// maps read and the open, and a FIFO or device must never be read.
func checkRegular(f *os.File, inode uint64) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", f.Name())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && inode != 0 && uint64(st.Ino) != inode {
		return fmt.Errorf("%s: inode %d, mapping has %d", f.Name(), st.Ino, inode)
	}
	return nil
}

// errDeletedPath marks mappings whose file is gone from the filesystem;
// only map_files can still reach them.
var errDeletedPath = errors.New("mapped file was deleted")

// openMappedFile opens the file behind mapping m of process pid. First
// choice is the map_files magic link: it is the exact mapped file, immune
// to renames and to paths that only exist in the process's mount
// namespace (needs CAP_SYS_ADMIN or CAP_CHECKPOINT_RESTORE). Otherwise
// m.path is resolved inside the process's root.
//
// O_NONBLOCK everywhere: if a hostile process swapped the path for a FIFO
// after we read its maps, open must not block the drain loop.
func openMappedFile(procRoot string, pid uint32, m *mapping) (*os.File, error) {
	dir := filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10))
	mapFiles := filepath.Join(dir, "map_files", fmt.Sprintf("%x-%x", m.start, m.end))
	f, err := os.OpenFile(mapFiles, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		return f, nil
	}
	if strings.HasSuffix(m.path, " (deleted)") {
		return nil, errors.Join(err, errDeletedPath)
	}
	f, rootErr := openFileInRoot(procRoot, pid, m.path)
	if rootErr != nil {
		return nil, errors.Join(err, rootErr)
	}
	return f, nil
}

// openFileInRoot opens path as seen by process pid. os.Root refuses
// resolutions that escape the root, so a symlink planted in a container
// (e.g. usr -> /) cannot redirect the collector to a host file.
func openFileInRoot(procRoot string, pid uint32, path string) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10), "root"))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.OpenFile(strings.TrimPrefix(path, "/"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
