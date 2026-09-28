// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package logs tails container logs on the node — the Promtail / Loki
// agent replacement. It reads /var/log/pods/<ns>_<pod>_<uid>/<container>/
// <restart>.log for the pods on this node, parses the CRI and docker
// json-file formats (reassembling partial lines), detects level and
// trace id, attaches pod metadata, and ships batches through the
// telemetry queue.
//
// Delivery guarantees rest on three pieces:
//
//   - Files are identified by inode, not name. The kubelet rotates by
//     renaming <n>.log aside and having the runtime reopen a fresh one;
//     the tailer keeps reading the renamed file through its open
//     descriptor to EOF, then moves to the new inode. copytruncate-style
//     truncation (size < offset) restarts the file at 0.
//   - Offsets are checkpointed ({path, inode, offset}, atomically, every
//     few seconds and on shutdown) only up to what has left the pipeline
//     — see tracker in positions.go. A clean restart neither loses nor
//     repeats lines; a crash may repeat the last in-flight seconds.
//   - Start position: on the very first start (no checkpoint) existing
//     files begin at their end (--logs-from=end) so a new install
//     doesn't flood the backend with history; files the checkpoint
//     doesn't know on a restart, and files appearing later, begin at 0
//     because every line in them is new.
//
// Protection: a token bucket per container (dropped lines are counted),
// 64 KiB line cap, 1 MiB / 1 s batches, and the queue's byte budget
// (drop oldest) bound memory no matter how noisy a pod is. The
// collector's own namespace is excluded by default so shipping errors
// can't feed back into more log lines.
package logs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
	"github.com/kubehero-io/platform/services/collector/internal/ship"
)

// Config tunes the tailer.
type Config struct {
	Root              string   // /var/log/pods
	PositionsFile     string   // /var/lib/kubehero/log-positions.json
	FromStart         bool     // --logs-from=start: first start reads existing files from 0
	RateLimit         float64  // lines/s per container
	Burst             int      // token bucket burst
	ExcludeNamespaces []string // never tailed
	PodLabels         []string // pod labels copied onto every entry
	ClusterID         string
	NodeName          string

	BatchBytes         int           // flush at this many body bytes (1 MiB)
	BatchInterval      time.Duration // or after this long (1s)
	PollInterval       time.Duration // read cadence (250ms)
	RescanInterval     time.Duration // directory re-walk (10s)
	CheckpointInterval time.Duration // positions save (5s)
	MaxLineBytes       int           // line cap (64 KiB)
	Logger             *slog.Logger
}

func (c *Config) defaults() {
	if c.Root == "" {
		c.Root = "/var/log/pods"
	}
	if c.RateLimit <= 0 {
		c.RateLimit = 2000
	}
	if c.Burst <= 0 {
		c.Burst = 4000
	}
	if c.BatchBytes <= 0 {
		c.BatchBytes = 1 << 20
	}
	if c.BatchInterval <= 0 {
		c.BatchInterval = time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.RescanInterval <= 0 {
		c.RescanInterval = 10 * time.Second
	}
	if c.CheckpointInterval <= 0 {
		c.CheckpointInterval = 5 * time.Second
	}
	if c.MaxLineBytes <= 0 {
		c.MaxLineBytes = 64 << 10
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Pods resolves pod metadata for log labels.
type Pods interface {
	PodByUID(uid string) (*corev1.Pod, bool)
	Node(name string) *corev1.Node
}

// Owners resolves pods to workloads.
type Owners interface {
	Resolve(ctx context.Context, p *corev1.Pod) kube.Workload
}

// Emit hands one batch to the ship queue. done must be called exactly
// once with the batch's outcome.
type Emit func(req *kuberov1.IngestLogsRequest, done func(ship.Outcome))

const (
	readChunk     = 256 << 10 // per file per read
	pollBudget    = 16 << 20  // bytes read per poll across all files
	entryOverhead = 96        // rough proto size of an entry besides its body
)

// Tailer tails the node's container logs. Run and Checkpoint are its
// public surface; everything else runs on Run's goroutine.
type Tailer struct {
	cfg    Config
	pods   Pods
	owners Owners
	emit   Emit
	log    *slog.Logger
	now    func() time.Time

	watcher    *fsnotify.Watcher
	watched    map[string]bool
	files      map[string]*tailFile // by id (path#inode)
	current    map[string]*tailFile // by path: the live (non-rotated) file
	containers map[string]*container
	excluded   map[string]bool
	labelKeys  [][2]string // pod label → entry label

	saved        map[string][]Position
	hadPositions bool
	commits      *tracker
	lastSaved    string
	saveWarned   bool

	batch        *batch
	seq          uint64
	buf          []byte
	ctx          context.Context
	missingGrace time.Duration
}

type container struct {
	dir                       string
	namespace, pod, uid, name string
	limiter                   *rate.Limiter
	ref                       *kuberov1.PodRef  // replaced, never mutated
	labels                    map[string]string // replaced, never mutated
	resolved                  bool
	lastResolve               time.Time
	dropped                   int
	lastDropLog               time.Time
}

type tailFile struct {
	id       string
	path     string
	ino      uint64
	f        *os.File
	c        *container
	offset   int64 // next unread byte
	partials [2]partial
	skipLine bool // discarding the tail of an over-long raw line
	rotated  bool // final (renamed away, deleted, or resumed rotated): drain, then close
	// missingSince is when the path was first seen absent.
	missingSince time.Time
}

type partial struct {
	data      []byte
	start     int64 // offset of the first fragment; -1 when empty
	ts        time.Time
	truncated bool
}

type batch struct {
	seq     uint64
	entries []*kuberov1.LogEntry
	bytes   int
	started time.Time
	files   map[string]bool
}

// New builds a Tailer.
func New(cfg Config, pods Pods, owners Owners, emit Emit) *Tailer {
	cfg.defaults()
	t := &Tailer{
		cfg: cfg, pods: pods, owners: owners, emit: emit, log: cfg.Logger.With("component", "logs"), now: time.Now,
		watched:      map[string]bool{},
		files:        map[string]*tailFile{},
		current:      map[string]*tailFile{},
		containers:   map[string]*container{},
		excluded:     map[string]bool{},
		commits:      newTracker(),
		buf:          make([]byte, readChunk),
		ctx:          context.Background(),
		missingGrace: defaultMissingGrace,
	}
	for _, ns := range cfg.ExcludeNamespaces {
		if ns = strings.TrimSpace(ns); ns != "" {
			t.excluded[ns] = true
		}
	}
	for _, l := range cfg.PodLabels {
		if l = strings.TrimSpace(l); l != "" {
			t.labelKeys = append(t.labelKeys, [2]string{l, sanitizeLabel(l)})
		}
	}
	return t
}

// Run tails until ctx ends, then flushes its last batch into the queue
// and closes files. Call Checkpoint after the queue has drained.
func (t *Tailer) Run(ctx context.Context) {
	t.ctx = ctx
	byPath, existed, err := loadPositions(t.cfg.PositionsFile)
	if err != nil {
		t.log.Warn("log positions unreadable — treating as a restart without checkpoint", "file", t.cfg.PositionsFile, "err", err)
	}
	t.saved, t.hadPositions = byPath, existed

	if w, err := fsnotify.NewBufferedWatcher(4096); err != nil {
		t.log.Warn("fsnotify unavailable — discovering log files by periodic rescan only", "err", err)
	} else {
		t.watcher = w
		defer func() { _ = w.Close() }()
	}
	t.rescan(true)
	t.saved = nil // only consulted for files present at startup

	poll := time.NewTicker(t.cfg.PollInterval)
	defer poll.Stop()
	rescan := time.NewTicker(t.cfg.RescanInterval)
	defer rescan.Stop()
	ckpt := time.NewTicker(t.cfg.CheckpointInterval)
	defer ckpt.Stop()

	var events <-chan fsnotify.Event
	var errs <-chan error
	if t.watcher != nil {
		events, errs = t.watcher.Events, t.watcher.Errors
	}
	for {
		select {
		case <-ctx.Done():
			t.flush()
			t.closeAll()
			return
		case ev, ok := <-events:
			if ok {
				t.onEvent(ev)
			}
		case err, ok := <-errs:
			if ok {
				if errors.Is(err, fsnotify.ErrEventOverflow) {
					t.rescan(false)
				} else {
					t.log.Debug("fsnotify error", "err", err)
				}
			}
		case <-poll.C:
			t.readAll()
			if t.batch != nil && t.now().Sub(t.batch.started) >= t.cfg.BatchInterval {
				t.flush()
			}
		case <-rescan.C:
			t.rescan(false)
		case <-ckpt.C:
			t.Checkpoint()
		}
	}
}

// Checkpoint persists committed offsets if they changed. Safe to call
// from any goroutine.
func (t *Tailer) Checkpoint() {
	if t.cfg.PositionsFile == "" {
		return
	}
	ps := t.commits.snapshot()
	key := positionsKey(ps)
	if key == t.lastSaved {
		return
	}
	if err := savePositions(t.cfg.PositionsFile, ps); err != nil {
		if !t.saveWarned {
			t.saveWarned = true
			t.log.Warn("cannot write log positions — a restart may re-read or skip recent lines", "file", t.cfg.PositionsFile, "err", err)
		}
		return
	}
	t.lastSaved = key
}

func positionsKey(ps []Position) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.Path)
		b.WriteByte('#')
		b.WriteString(strconv.FormatUint(p.Inode, 10))
		b.WriteByte('@')
		b.WriteString(strconv.FormatInt(p.Offset, 10))
		b.WriteByte(';')
	}
	return b.String()
}

// ── discovery ─────────────────────────────────────────────────────────

func (t *Tailer) watch(dir string) {
	if t.watcher == nil || t.watched[dir] {
		return
	}
	if err := t.watcher.Add(dir); err == nil {
		t.watched[dir] = true
	}
}

// rescan walks root → pod dirs → container dirs → *.log files.
func (t *Tailer) rescan(initial bool) {
	podDirs, err := os.ReadDir(t.cfg.Root)
	if err != nil {
		if initial {
			t.log.Info("log root not readable — log tailing idle until it appears", "root", t.cfg.Root, "err", err)
		}
		return
	}
	t.watch(t.cfg.Root)
	seenDirs := map[string]bool{t.cfg.Root: true}
	for _, pd := range podDirs {
		if !pd.IsDir() {
			continue
		}
		ns, _, _, ok := parsePodDir(pd.Name())
		if !ok || t.excluded[ns] {
			continue
		}
		podDir := filepath.Join(t.cfg.Root, pd.Name())
		seenDirs[podDir] = true
		t.watch(podDir)
		cdirs, err := os.ReadDir(podDir)
		if err != nil {
			continue
		}
		for _, cd := range cdirs {
			if !cd.IsDir() {
				continue
			}
			cdir := filepath.Join(podDir, cd.Name())
			seenDirs[cdir] = true
			t.watch(cdir)
			t.scanContainer(cdir, initial)
		}
	}
	for dir := range t.watched {
		if !seenDirs[dir] {
			delete(t.watched, dir) // inotify drops watches of removed dirs itself
		}
	}
	for _, c := range t.containers {
		if !c.resolved && t.now().Sub(c.lastResolve) >= t.cfg.RescanInterval {
			t.resolve(c)
		}
	}
	metrics.LogFiles.With().Set(float64(len(t.files)))
}

func (t *Tailer) scanContainer(cdir string, initial bool) {
	entries, err := os.ReadDir(cdir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		t.track(filepath.Join(cdir, e.Name()), initial)
	}
	if initial {
		t.resumeRotated(cdir)
	}
}

func (t *Tailer) onEvent(ev fsnotify.Event) {
	if !ev.Has(fsnotify.Create) {
		return // writes are picked up by polling; renames/removes by stat
	}
	rel, err := filepath.Rel(t.cfg.Root, ev.Name)
	if err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if ns, _, _, ok := parsePodDir(parts[0]); !ok || t.excluded[ns] {
		return
	}
	switch len(parts) {
	case 1, 2: // new pod dir or container dir: walk it
		fi, err := os.Stat(ev.Name)
		if err != nil || !fi.IsDir() {
			return
		}
		t.watch(ev.Name)
		if len(parts) == 2 {
			t.scanContainer(ev.Name, false)
			return
		}
		if cdirs, err := os.ReadDir(ev.Name); err == nil {
			for _, cd := range cdirs {
				if cd.IsDir() {
					cdir := filepath.Join(ev.Name, cd.Name())
					t.watch(cdir)
					t.scanContainer(cdir, false)
				}
			}
		}
	case 3:
		if strings.HasSuffix(ev.Name, ".log") {
			t.track(ev.Name, false)
		}
	}
}

// parsePodDir splits <namespace>_<pod>_<uid>. Namespace and pod names
// are DNS labels/subdomains, so they never contain '_'.
func parsePodDir(name string) (ns, pod, uid string, ok bool) {
	parts := strings.Split(name, "_")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// track starts tailing path if its current inode isn't tracked yet.
func (t *Tailer) track(path string, initial bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	ino := inode(fi)
	id := fileID(path, ino)
	if _, ok := t.files[id]; ok {
		return
	}
	if old := t.current[path]; old != nil && old.ino != ino {
		old.rotated = true // drained and closed by the read loop
		metrics.LogRotations.With("rotated").Inc()
	}

	offset := int64(0)
	switch saved, known := t.saved[path]; {
	case known:
		for _, p := range saved {
			if p.Inode == ino {
				offset = p.Offset
				if offset > fi.Size() {
					offset = 0 // truncated while we were down
				}
			}
		}
		// Different inode: the file was rotated while we were down and
		// this one is entirely new → 0 (resumeRotated drains the old).
	case initial && !t.hadPositions && !t.cfg.FromStart:
		offset = lineAlignedEnd(path, fi.Size())
	}
	t.open(path, filepath.Dir(path), ino, offset, false)
}

// resumeRotated finds files the checkpoint remembers by inode whose name
// has moved (rotated while the collector was down) and drains them from
// their saved offset.
func (t *Tailer) resumeRotated(cdir string) {
	for path, saved := range t.saved {
		if filepath.Dir(path) != cdir {
			continue
		}
		for _, p := range saved {
			if _, ok := t.files[fileID(path, p.Inode)]; ok {
				continue
			}
			found := t.findInode(cdir, path, p.Inode)
			if found == "" {
				metrics.LogRotations.With("lost_while_down").Inc()
				t.log.Warn("a checkpointed log file was compressed or deleted while the collector was down — any unread tail is lost",
					"path", path, "offset", p.Offset)
				continue
			}
			// Checkpointed under the name it has now, so a crash while
			// draining resumes it the same way.
			t.open(found, cdir, p.Inode, p.Offset, true)
		}
	}
}

func (t *Tailer) findInode(cdir, path string, ino uint64) string {
	dirs := []string{cdir}
	if target, err := filepath.EvalSymlinks(path); err == nil && filepath.Dir(target) != cdir {
		dirs = append(dirs, filepath.Dir(target)) // docker json-file symlinks
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".gz") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(full); err == nil && inode(fi) == ino {
				return full
			}
		}
	}
	return ""
}

// lineAlignedEnd returns the offset just past the last newline before
// size, so tailing "from the end" never starts mid-line.
func lineAlignedEnd(path string, size int64) int64 {
	if size == 0 {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return size
	}
	defer f.Close()
	n := int64(64 << 10)
	if n > size {
		n = size
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, size-n); err != nil && !errors.Is(err, io.EOF) {
		return size
	}
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		return size - n + int64(i) + 1
	}
	return size
}

func fileID(path string, ino uint64) string { return path + "#" + strconv.FormatUint(ino, 10) }

// open starts tailing path at offset. cdir is the container's log
// directory, which differs from path's directory for a rotated docker
// json-file log found through the symlink target.
func (t *Tailer) open(path, cdir string, ino uint64, offset int64, rotated bool) *tailFile {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	c := t.containerFor(cdir)
	if c == nil {
		_ = f.Close()
		return nil
	}
	tf := &tailFile{id: fileID(path, ino), path: path, ino: ino, f: f, c: c, offset: offset, rotated: rotated}
	tf.partials[0].start, tf.partials[1].start = -1, -1
	t.files[tf.id] = tf
	if !rotated {
		t.current[path] = tf
	}
	t.commits.open(tf.id, path, ino, offset)
	return tf
}

func (t *Tailer) containerFor(cdir string) *container {
	if c := t.containers[cdir]; c != nil {
		return c
	}
	rel, err := filepath.Rel(t.cfg.Root, cdir)
	if err != nil {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return nil
	}
	ns, pod, uid, ok := parsePodDir(parts[0])
	if !ok {
		return nil
	}
	c := &container{
		dir: cdir, namespace: ns, pod: pod, uid: uid, name: parts[1],
		limiter: rate.NewLimiter(rate.Limit(t.cfg.RateLimit), t.cfg.Burst),
	}
	t.containers[cdir] = c
	t.resolve(c)
	return c
}

// resolve attaches pod metadata. Until the pod shows up in the cache
// (its log dir can appear before the informer event), entries carry
// what the path says.
func (t *Tailer) resolve(c *container) {
	c.lastResolve = t.now()
	ref := &kuberov1.PodRef{Namespace: c.namespace, Pod: c.pod, Container: c.name, PodUid: c.uid, Node: t.cfg.NodeName}
	var labels map[string]string
	if p, ok := t.pods.PodByUID(c.uid); ok {
		w := kube.Workload{Name: p.Name, Kind: "Pod"}
		if t.owners != nil {
			w = t.owners.Resolve(t.ctx, p)
		}
		ref = kube.PodRefFor(p, t.pods.Node(p.Spec.NodeName), w)
		ref.Container = c.name
		if ref.Node == "" {
			ref.Node = t.cfg.NodeName
		}
		for _, k := range t.labelKeys {
			if v, ok := p.Labels[k[0]]; ok {
				if labels == nil {
					labels = map[string]string{}
				}
				labels[k[1]] = v
			}
		}
		c.resolved = true
	}
	c.ref, c.labels = ref, labels
}

// sanitizeLabel makes a pod label key a valid LogQL label name:
// app.kubernetes.io/name → app_kubernetes_io_name.
func sanitizeLabel(k string) string {
	b := []byte(k)
	for i, ch := range b {
		if !(ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (i > 0 && ch >= '0' && ch <= '9')) {
			b[i] = '_'
		}
	}
	return string(b)
}

// ── reading ───────────────────────────────────────────────────────────

func (t *Tailer) readAll() {
	// Rotated files first: their lines are older than anything in the
	// file that replaced them.
	order := make([]*tailFile, 0, len(t.files))
	for _, tf := range t.files {
		if tf.rotated {
			order = append(order, tf)
		}
	}
	for _, tf := range t.files {
		if !tf.rotated {
			order = append(order, tf)
		}
	}
	budget := pollBudget
	for progress := true; progress && budget > 0; {
		progress = false
		for _, tf := range order {
			n := t.readFile(tf, min(readChunk, budget))
			budget -= n
			if n == readChunk {
				progress = true // more is waiting; come back after the others
			}
			if budget <= 0 {
				break
			}
		}
	}
	// Files at EOF: rotation, deletion, truncation.
	for _, tf := range t.files {
		t.checkFile(tf)
	}
	metrics.LogFiles.With().Set(float64(len(t.files)))
}

// readFile consumes complete lines from tf's offset and returns the
// number of bytes read.
func (t *Tailer) readFile(tf *tailFile, budget int) int {
	buf := t.buf[:budget]
	n, err := tf.f.ReadAt(buf, tf.offset)
	if n == 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			t.log.Debug("read failed", "path", tf.path, "err", err)
		}
		return 0
	}
	metrics.LogBytesRead.With().Add(float64(n))
	data := buf[:n]
	consumed := 0
	for {
		i := bytes.IndexByte(data[consumed:], '\n')
		if i < 0 {
			break
		}
		start := tf.offset + int64(consumed)
		line := data[consumed : consumed+i]
		consumed += i + 1
		t.handleLine(tf, line, start)
	}
	if consumed == 0 && n == len(buf) && n == readChunk {
		// A raw line longer than the read buffer (a malformed file —
		// runtimes split lines at 16 KiB): take what we have as a
		// truncated line and discard the rest up to the newline.
		t.handleLine(tf, data, tf.offset)
		consumed = n
		tf.skipLine = true
		metrics.LogLinesTruncated.With().Inc()
	}
	tf.offset += int64(consumed)
	t.commits.setRead(tf.id, tf.offset, tf.partialStart())
	return n
}

func (tf *tailFile) partialStart() int64 {
	s := int64(-1)
	for _, p := range tf.partials {
		if p.start >= 0 && (s < 0 || p.start < s) {
			s = p.start
		}
	}
	return s
}

// defaultMissingGrace is how long a file's path may be absent before the
// file is considered deleted. The kubelet rotates by renaming <n>.log and
// then asking the runtime to reopen it; in between, the runtime can
// still append to the renamed file, so only a NEW inode at the path
// proves the old file is final. A path that stays missing means the
// container's logs were removed.
const defaultMissingGrace = 5 * time.Second

// checkFile handles truncation, rotation and deletion after a read pass.
func (t *Tailer) checkFile(tf *tailFile) {
	if st, err := tf.f.Stat(); err == nil && st.Size() < tf.offset {
		metrics.LogRotations.With("truncated").Inc()
		t.log.Debug("log file truncated — restarting at 0", "path", tf.path)
		tf.offset = 0
		tf.partials[0], tf.partials[1] = partial{start: -1}, partial{start: -1}
		t.commits.setRead(tf.id, 0, -1)
		return
	}
	if !tf.rotated {
		fi, err := os.Stat(tf.path)
		switch {
		case err == nil && inode(fi) == tf.ino:
			tf.missingSince = time.Time{}
			return
		case err == nil:
			// The runtime reopened: a new file lives at the path and the
			// old one will never grow again.
			tf.rotated = true
			metrics.LogRotations.With("rotated").Inc()
			defer t.track(tf.path, false)
		case errors.Is(err, fs.ErrNotExist):
			if tf.missingSince.IsZero() {
				tf.missingSince = t.now()
				return
			}
			if t.now().Sub(tf.missingSince) < t.missingGrace {
				return // keep reading through the open descriptor
			}
			tf.rotated = true
			metrics.LogRotations.With("removed").Inc()
		default:
			return
		}
	}
	t.drainAndClose(tf)
}

// drainAndClose reads a final file to its end — including a last line
// the writer never terminated — and closes it.
func (t *Tailer) drainAndClose(tf *tailFile) {
	for {
		before := tf.offset
		if t.readFile(tf, readChunk) == 0 || tf.offset == before {
			break
		}
	}
	if st, err := tf.f.Stat(); err == nil && st.Size() > tf.offset {
		rest := make([]byte, min(st.Size()-tf.offset, int64(readChunk)))
		if n, _ := tf.f.ReadAt(rest, tf.offset); n > 0 {
			t.handleLine(tf, rest[:n], tf.offset)
		}
		tf.offset = st.Size()
		t.commits.setRead(tf.id, tf.offset, tf.partialStart())
	}
	t.closeFile(tf)
}

func (t *Tailer) closeFile(tf *tailFile) {
	// Flush reassembly buffers: a partial line cut by rotation is still
	// a line.
	for i := range tf.partials {
		if p := &tf.partials[i]; p.start >= 0 {
			stream := "stdout"
			if i == 1 {
				stream = "stderr"
			}
			t.emitLine(tf, p.ts, stream, p.data, p.start, p.truncated)
			*p = partial{start: -1}
		}
	}
	_ = tf.f.Close()
	delete(t.files, tf.id)
	if t.current[tf.path] == tf {
		delete(t.current, tf.path)
	}
	t.commits.close(tf.id)
}

func (t *Tailer) closeAll() {
	for _, tf := range t.files {
		_ = tf.f.Close()
	}
}

// handleLine parses one physical line and emits complete logical lines.
func (t *Tailer) handleLine(tf *tailFile, line []byte, start int64) {
	if tf.skipLine {
		tf.skipLine = false
		return
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	rl, err := parseLine(line)
	if err != nil {
		metrics.LogLinesDropped.With("parse_error").Inc()
		return
	}
	idx := 0
	if rl.stream == "stderr" {
		idx = 1
	}
	p := &tf.partials[idx]
	if rl.partial {
		if p.start < 0 {
			p.start, p.ts, p.data, p.truncated = start, rl.ts, p.data[:0], false
		}
		p.data, p.truncated = appendCapped(p.data, rl.content, t.cfg.MaxLineBytes, p.truncated)
		return
	}
	if p.start >= 0 {
		data, truncated := appendCapped(p.data, rl.content, t.cfg.MaxLineBytes, p.truncated)
		t.emitLine(tf, p.ts, rl.stream, data, p.start, truncated)
		p.start, p.data, p.truncated = -1, data[:0], false
		return
	}
	content, truncated := rl.content, false
	if len(content) > t.cfg.MaxLineBytes {
		content, truncated = content[:t.cfg.MaxLineBytes], true
	}
	t.emitLine(tf, rl.ts, rl.stream, content, start, truncated)
}

func appendCapped(dst, src []byte, capBytes int, truncated bool) ([]byte, bool) {
	room := capBytes - len(dst)
	if room <= 0 {
		return dst, truncated || len(src) > 0
	}
	if len(src) > room {
		return append(dst, src[:room]...), true
	}
	return append(dst, src...), truncated
}

func (t *Tailer) emitLine(tf *tailFile, ts time.Time, stream string, content []byte, start int64, truncated bool) {
	metrics.LogLinesRead.With().Inc()
	c := tf.c
	if !c.limiter.Allow() {
		metrics.LogLinesDropped.With("rate_limited").Inc()
		c.dropped++
		if now := t.now(); now.Sub(c.lastDropLog) >= time.Minute {
			t.log.Warn("log rate limit exceeded — dropping lines", "namespace", c.namespace, "pod", c.pod,
				"container", c.name, "dropped", c.dropped, "limit_lines_per_sec", t.cfg.RateLimit)
			c.lastDropLog, c.dropped = now, 0
		}
		return
	}
	if truncated {
		metrics.LogLinesTruncated.With().Inc()
	}
	body := toValidUTF8(content)
	e := &kuberov1.LogEntry{
		TsUnixNano: ts.UnixNano(),
		Source:     c.ref,
		Stream:     stream,
		Level:      DetectLevel(body),
		Body:       body,
		Labels:     c.labels,
		TraceId:    DetectTraceID(body),
	}
	if t.batch == nil {
		t.seq++
		t.batch = &batch{seq: t.seq, started: t.now(), files: map[string]bool{}}
	}
	b := t.batch
	if !b.files[tf.id] {
		b.files[tf.id] = true
		t.commits.addInflight(tf.id, b.seq, start)
	}
	b.entries = append(b.entries, e)
	b.bytes += len(body) + entryOverhead
	if b.bytes >= t.cfg.BatchBytes {
		t.flush()
	}
}

// toValidUTF8 copies the line out of the read buffer as a string,
// replacing invalid UTF-8 (proto strings must be valid) and never
// splitting a rune at the truncation point.
func toValidUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

func (t *Tailer) flush() {
	b := t.batch
	if b == nil || len(b.entries) == 0 {
		t.batch = nil
		return
	}
	t.batch = nil
	ids := make([]string, 0, len(b.files))
	for id := range b.files {
		ids = append(ids, id)
	}
	req := &kuberov1.IngestLogsRequest{ClusterId: t.cfg.ClusterID, Entries: b.entries}
	seq := b.seq
	t.emit(req, func(o ship.Outcome) { t.commits.done(seq, ids, o) })
}
