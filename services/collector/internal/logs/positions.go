// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package logs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/kubehero-io/platform/services/collector/internal/ship"
)

// Position is one file's checkpoint: everything before Offset in the
// file with this inode has been shipped (or deliberately dropped).
type Position struct {
	Path   string `json:"path"`
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

type positionsDoc struct {
	Version int        `json:"version"`
	Files   []Position `json:"files"`
}

// loadPositions reads the checkpoint file. existed reports whether a
// checkpoint was found — the tailer uses it to tell a first-ever start
// (begin new files at the end) from a restart (files unknown to the
// checkpoint appeared while we were down: read them from the start).
func loadPositions(path string) (byPath map[string][]Position, existed bool, err error) {
	byPath = map[string][]Position{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return byPath, false, nil
	}
	if err != nil {
		return byPath, false, err
	}
	var doc positionsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A corrupt checkpoint is treated as present-but-empty: new
		// files are read from the start rather than skipped.
		return byPath, true, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, p := range doc.Files {
		if p.Path == "" || p.Offset < 0 {
			continue
		}
		byPath[p.Path] = append(byPath[p.Path], p)
	}
	return byPath, true, nil
}

// savePositions writes the checkpoint atomically: a temp file in the
// same directory, fsync, rename over the old one. A crash leaves either
// the old or the new checkpoint, never a torn one.
func savePositions(path string, ps []Position) error {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Path != ps[j].Path {
			return ps[i].Path < ps[j].Path
		}
		return ps[i].Inode < ps[j].Inode
	})
	raw, err := json.Marshal(positionsDoc{Version: 1, Files: ps})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // persist the rename; best effort
		_ = d.Close()
	}
	return nil
}

// ── commit tracking ───────────────────────────────────────────────────

// tracker decides, per file, the offset that is safe to checkpoint:
// the lowest of (a) the read position, (b) the start of a partial CRI
// line still being reassembled, and (c) the first line of every batch
// still in flight. Batches complete out of order (the ship queue drops
// the oldest queued batch on overflow while an older one is retried),
// so a simple "last completed" offset could skip unshipped lines.
//
// Outcomes: Sent and Dropped release a batch; Abandoned (still queued at
// shutdown) keeps it pinned, so its lines are re-read after restart —
// at-least-once on a crash or a failed final flush, exactly-once on a
// clean restart with a reachable control plane.
type tracker struct {
	mu    sync.Mutex
	files map[string]*fileCommit
}

type fileCommit struct {
	path     string
	ino      uint64
	read     int64
	partial  int64 // -1: none
	inflight map[uint64]int64
	closed   bool
	pinned   bool // an abandoned batch holds this file's offset
}

func newTracker() *tracker { return &tracker{files: map[string]*fileCommit{}} }

func (t *tracker) open(id, path string, ino uint64, offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.files[id] = &fileCommit{path: path, ino: ino, read: offset, partial: -1, inflight: map[uint64]int64{}}
}

func (t *tracker) setRead(id string, offset, partial int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fc := t.files[id]; fc != nil {
		fc.read, fc.partial = offset, partial
	}
}

func (t *tracker) addInflight(id string, seq uint64, start int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fc := t.files[id]; fc != nil {
		if _, ok := fc.inflight[seq]; !ok {
			fc.inflight[seq] = start
		}
	}
}

// done settles a batch for every file it carried lines from.
func (t *tracker) done(seq uint64, ids []string, o ship.Outcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		fc := t.files[id]
		if fc == nil {
			continue
		}
		if o == ship.Abandoned {
			fc.pinned = true
			continue
		}
		delete(fc.inflight, seq)
		t.gcLocked(id, fc)
	}
}

// close marks a file drained; it leaves the checkpoint once nothing of
// it is in flight.
func (t *tracker) close(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fc := t.files[id]; fc != nil {
		fc.closed = true
		fc.partial = -1
		t.gcLocked(id, fc)
	}
}

func (t *tracker) gcLocked(id string, fc *fileCommit) {
	if fc.closed && len(fc.inflight) == 0 {
		delete(t.files, id)
	}
}

func (t *tracker) committed(fc *fileCommit) int64 {
	c := fc.read
	if fc.partial >= 0 && fc.partial < c {
		c = fc.partial
	}
	for _, start := range fc.inflight {
		if start < c {
			c = start
		}
	}
	return c
}

// snapshot is the checkpoint content.
func (t *tracker) snapshot() []Position {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Position, 0, len(t.files))
	for _, fc := range t.files {
		out = append(out, Position{Path: fc.path, Inode: fc.ino, Offset: t.committed(fc)})
	}
	return out
}

func (t *tracker) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.files)
}
