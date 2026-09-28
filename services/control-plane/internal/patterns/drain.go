// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package patterns clusters log lines into templates with Drain (He et
// al., "Drain: An Online Log Parsing Approach with Fixed Depth Tree",
// ICWS 2017), so a million lines read as twenty patterns with counts.
//
// Each line is tokenised on whitespace and its variable-looking tokens
// are masked (numbers, hex ids, UUIDs, IPs, timestamps, durations and
// sizes). A fixed-depth tree routes the line — first by token count,
// then by its first few tokens (tokens with digits or masks share a
// wildcard branch, and a node with MaxChildren children sends new
// tokens there too) — to a small list of candidate clusters. The most
// similar candidate (share of positions with equal tokens) absorbs the
// line if the similarity reaches SimThreshold, turning differing
// positions into wildcards; otherwise the line starts a new cluster.
// Memory is bounded: past MaxClusters the least recently used cluster
// is evicted. Output templates render every mask and wildcard as <_>,
// Loki's convention.
package patterns

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Config tunes the miner. Zero values take the defaults in brackets.
type Config struct {
	Depth        int     // tree depth incl. root and length level [4]
	SimThreshold float64 // [0.4]
	MaxChildren  int     // per tree node [100]
	MaxClusters  int     // [1000]
	MaxTokens    int     // longer lines keep the head, the tail becomes one wildcard [128]
}

func (c Config) withDefaults() Config {
	if c.Depth < 3 {
		c.Depth = 4
	}
	if c.SimThreshold <= 0 {
		c.SimThreshold = 0.4
	}
	if c.MaxChildren <= 0 {
		c.MaxChildren = 100
	}
	if c.MaxClusters <= 0 {
		c.MaxClusters = 1000
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 128
	}
	return c
}

const (
	wildcard = "<*>"
	outParam = "<_>"
)

type cluster struct {
	id       int
	tokens   []string
	count    int64
	levels   map[string]int64
	sample   string
	trend    []int64
	lastUsed int64
	leaf     *node
}

type node struct {
	children map[string]*node
	clusters []*cluster
}

func newNode() *node { return &node{children: map[string]*node{}} }

// Drain is an online log clusterer. Not safe for concurrent use.
type Drain struct {
	cfg      Config
	root     *node
	clusters map[int]*cluster
	nextID   int
	tick     int64
	buckets  int
	lines    int64
}

// New returns a miner; buckets is the length of each pattern's trend.
func New(cfg Config, buckets int) *Drain {
	return &Drain{cfg: cfg.withDefaults(), root: newNode(), clusters: map[int]*cluster{}, buckets: buckets}
}

// Lines is the number of lines trained so far.
func (d *Drain) Lines() int64 { return d.lines }

// Train adds one line. level is its severity (may be empty) and bucket
// its trend bucket (ignored when out of range). weight scales the
// counts (1 unless the caller sampled lines).
func (d *Drain) Train(line, level string, bucket int, weight int64) {
	if weight <= 0 {
		weight = 1
	}
	d.lines++
	d.tick++
	tokens := d.tokenize(line)
	c := d.search(tokens)
	if c == nil {
		c = d.create(tokens, line)
	} else {
		for i := range c.tokens {
			if c.tokens[i] != tokens[i] {
				c.tokens[i] = wildcard
			}
		}
	}
	c.count += weight
	c.lastUsed = d.tick
	if level != "" {
		c.levels[level] += weight
	}
	if bucket >= 0 && bucket < len(c.trend) {
		c.trend[bucket] += weight
	}
}

func (d *Drain) tokenize(line string) []string {
	fields := strings.Fields(line)
	if len(fields) > d.cfg.MaxTokens {
		fields = append(fields[:d.cfg.MaxTokens-1], wildcard)
	}
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = Mask(f)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func isRoutingWildcard(tok string) bool {
	if strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">") {
		return true
	}
	for _, r := range tok {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// leafFor walks (and with create, builds) the tree path for tokens.
func (d *Drain) leafFor(tokens []string, create bool) *node {
	lenKey := string(rune(len(tokens))) // token counts are small; one rune is a unique key
	cur, ok := d.root.children[lenKey]
	if !ok {
		if !create {
			return nil
		}
		cur = newNode()
		d.root.children[lenKey] = cur
	}
	levels := d.cfg.Depth - 2
	for i := 0; i < levels && i < len(tokens); i++ {
		tok := tokens[i]
		if isRoutingWildcard(tok) {
			tok = wildcard
		}
		next, ok := cur.children[tok]
		if !ok {
			if tok != wildcard {
				if w, has := cur.children[wildcard]; has && !create {
					cur = w
					continue
				}
				if len(cur.children) >= d.cfg.MaxChildren {
					tok = wildcard
					next, ok = cur.children[tok]
				}
			}
			if !ok {
				if !create {
					return nil
				}
				next = newNode()
				cur.children[tok] = next
			}
		}
		cur = next
	}
	return cur
}

func (d *Drain) search(tokens []string) *cluster {
	leaf := d.leafFor(tokens, false)
	if leaf == nil {
		return nil
	}
	var best *cluster
	bestSim, bestParams := -1.0, -1
	for _, c := range leaf.clusters {
		sim, params := similarity(c.tokens, tokens)
		if sim > bestSim || sim == bestSim && params > bestParams {
			best, bestSim, bestParams = c, sim, params
		}
	}
	if best != nil && bestSim >= d.cfg.SimThreshold {
		return best
	}
	return nil
}

// similarity is Drain's simSeq: the share of positions whose tokens
// are equal, not counting positions the template already generalised.
func similarity(tmpl, tokens []string) (float64, int) {
	if len(tmpl) != len(tokens) {
		return 0, 0
	}
	same, params := 0, 0
	for i, t := range tmpl {
		if t == wildcard {
			params++
			continue
		}
		if t == tokens[i] {
			same++
		}
	}
	return float64(same) / float64(len(tmpl)), params
}

func (d *Drain) create(tokens []string, line string) *cluster {
	if len(d.clusters) >= d.cfg.MaxClusters {
		d.evict()
	}
	d.nextID++
	c := &cluster{
		id:     d.nextID,
		tokens: append([]string(nil), tokens...),
		levels: map[string]int64{},
		sample: line,
		trend:  make([]int64, d.buckets),
	}
	leaf := d.leafFor(tokens, true)
	c.leaf = leaf
	leaf.clusters = append(leaf.clusters, c)
	d.clusters[c.id] = c
	return c
}

func (d *Drain) evict() {
	var victim *cluster
	for _, c := range d.clusters {
		if victim == nil || c.lastUsed < victim.lastUsed {
			victim = c
		}
	}
	if victim == nil {
		return
	}
	delete(d.clusters, victim.id)
	cs := victim.leaf.clusters
	for i, c := range cs {
		if c == victim {
			victim.leaf.clusters = append(cs[:i], cs[i+1:]...)
			break
		}
	}
}

// Pattern is one mined template with its statistics.
type Pattern struct {
	Pattern string
	Count   int64
	Level   string // dominant level ("" when no line had one)
	Levels  map[string]int64
	Sample  string
	Trend   []int64
}

// Patterns returns the templates, merged when they render the same,
// ordered by count (then pattern).
func (d *Drain) Patterns() []Pattern {
	merged := map[string]*Pattern{}
	var order []string
	ids := make([]int, 0, len(d.clusters))
	for id := range d.clusters {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		c := d.clusters[id]
		text := render(c.tokens)
		p, ok := merged[text]
		if !ok {
			p = &Pattern{Pattern: text, Sample: c.sample, Levels: map[string]int64{}, Trend: make([]int64, d.buckets)}
			merged[text] = p
			order = append(order, text)
		}
		p.Count += c.count
		for l, n := range c.levels {
			p.Levels[l] += n
		}
		for i, n := range c.trend {
			p.Trend[i] += n
		}
	}
	out := make([]Pattern, 0, len(merged))
	for _, text := range order {
		p := merged[text]
		var best int64
		for l, n := range p.Levels {
			if n > best || n == best && l < p.Level {
				p.Level, best = l, n
			}
		}
		out = append(out, *p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Pattern < out[j].Pattern
	})
	return out
}

// render joins tokens, showing every mask and wildcard as <_> and
// collapsing runs of them into one.
func render(tokens []string) string {
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = maskToken.ReplaceAllString(t, outParam)
		if t == outParam && len(parts) > 0 && parts[len(parts)-1] == outParam {
			continue
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

var maskToken = regexp.MustCompile(`<[A-Z*]>`)

// ─── masking ─────────────────────────────────────────────────────────────

var (
	reUUID     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reIPv4     = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}(:\d{1,5})?(/\d{1,2})?$`)
	reIPv6     = regexp.MustCompile(`^\[?[0-9a-fA-F]{0,4}(:[0-9a-fA-F]{0,4}){2,7}\]?(:\d{1,5})?$`)
	reNumber   = regexp.MustCompile(`^[-+]?(\d+(\.\d*)?|\.\d+)([eE][-+]?\d+)?$`)
	reHexNum   = regexp.MustCompile(`^0[xX][0-9a-fA-F]+$`)
	reHexID    = regexp.MustCompile(`^[0-9a-fA-F]{8,}$`)
	reDuration = regexp.MustCompile(`^[-+]?(\d+(\.\d+)?(ns|us|µs|ms|s|m|h|d))+$`)
	reSize     = regexp.MustCompile(`^\d+(\.\d+)?([kKmMgGtTpP]i?[bB]?|[bB]|%)$`)
	reDate     = regexp.MustCompile(`^\d{4}[-/]\d{2}[-/]\d{2}([T ]\d{2}:\d{2}(:\d{2}([.,]\d+)?)?(Z|[+-]\d{2}:?\d{2})?)?$`)
	reTime     = regexp.MustCompile(`^\d{1,2}:\d{2}(:\d{2}([.,]\d+)?)?(Z|[+-]\d{2}:?\d{2})?$`)
)

// Mask replaces a variable-looking token (or the value of a key=value
// token) with a typed mask, keeping surrounding punctuation:
// "(took", "123ms)," → "(took", "<D>),".
func Mask(tok string) string {
	start, end := 0, len(tok)
	for start < end && strings.ContainsRune(`"'([{<,;`, rune(tok[start])) {
		start++
	}
	for end > start && strings.ContainsRune(`"')]}>,;.:!?`, rune(tok[end-1])) {
		end--
	}
	core := tok[start:end]
	if core == "" {
		return tok
	}
	if m := classify(core); m != "" {
		return tok[:start] + m + tok[end:]
	}
	// key=value (logfmt, query strings): mask the value only.
	if i := strings.IndexByte(core, '='); i > 0 && i < len(core)-1 {
		return tok[:start] + core[:i+1] + Mask(core[i+1:]) + tok[end:]
	}
	if looksLikeID(core) {
		return tok[:start] + "<H>" + tok[end:]
	}
	return tok
}

func classify(s string) string {
	switch {
	case reNumber.MatchString(s) || reHexNum.MatchString(s):
		return "<N>"
	case reUUID.MatchString(s):
		return "<U>"
	case reIPv4.MatchString(s) || strings.Count(s, ":") >= 2 && reIPv6.MatchString(s) && strings.ContainsAny(s, "0123456789abcdefABCDEF"):
		return "<I>"
	case reDate.MatchString(s) || reTime.MatchString(s):
		return "<T>"
	case reDuration.MatchString(s) || reSize.MatchString(s):
		return "<D>"
	case reHexID.MatchString(s) && strings.ContainsAny(s, "0123456789"):
		return "<H>"
	}
	return ""
}

// looksLikeID: long tokens mixing letters and at least three digits
// (request ids, pod-template hashes like api-7d9f8b6c5-x2x4z).
func looksLikeID(s string) bool {
	if len(s) < 8 {
		return false
	}
	digits, letters := 0, 0
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLetter(r):
			letters++
		}
	}
	return digits >= 3 && letters > 0
}
