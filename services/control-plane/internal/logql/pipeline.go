// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
)

// Labels written by stages that fail, as in Loki. A query can keep or
// discard failed lines with `| __error__ = ""`.
const (
	ErrorLabel        = "__error__"
	ErrorDetailsLabel = "__error_details__"

	errJSON        = "JSONParserErr"
	errLogfmt      = "LogfmtParserErr"
	errLabelFilter = "LabelFilterErr"
	errTemplate    = "TemplateFormatErr"
)

// maxFormattedLine caps what line_format / label_format may produce.
const maxFormattedLine = 64 << 10

// labelState is one line's labels while a pipeline runs: the stream
// labels plus whatever stages added, renamed or dropped. The stream map
// is shared and never written; the first mutation copies it.
type labelState struct {
	stream map[string]string
	cur    map[string]string // nil until the first mutation
}

func (l *labelState) get(k string) (string, bool) {
	if l.cur != nil {
		v, ok := l.cur[k]
		return v, ok
	}
	v, ok := l.stream[k]
	return v, ok
}

func (l *labelState) own() {
	if l.cur == nil {
		l.cur = make(map[string]string, len(l.stream)+8)
		for k, v := range l.stream {
			l.cur[k] = v
		}
	}
}

func (l *labelState) set(k, v string) {
	l.own()
	l.cur[k] = v
}

func (l *labelState) del(k string) {
	if _, ok := l.get(k); !ok {
		return
	}
	l.own()
	delete(l.cur, k)
}

// extract adds a parsed label; a name that is already a stream label
// gets an _extracted suffix so parsing never overwrites the stream.
func (l *labelState) extract(k, v string) {
	if _, isStream := l.stream[k]; isStream {
		k += "_extracted"
	}
	l.set(k, v)
}

func (l *labelState) hasErr() bool {
	v, ok := l.get(ErrorLabel)
	return ok && v != ""
}

func (l *labelState) setErr(kind, details string) {
	if l.hasErr() {
		return // keep the first error, as Loki does
	}
	l.set(ErrorLabel, kind)
	if details != "" {
		l.set(ErrorDetailsLabel, details)
	}
}

func (l *labelState) result() map[string]string {
	if l.cur != nil {
		return l.cur
	}
	return l.stream
}

func (l *labelState) snapshot() map[string]string {
	out := make(map[string]string, len(l.result()))
	for k, v := range l.result() {
		out[k] = v
	}
	return out
}

// Pipeline evaluates stages in Go. It is not safe for concurrent use
// (line_format keeps per-line context); build one per query.
type Pipeline struct {
	stages []runner
}

type runner interface {
	run(ts int64, line string, l *labelState) (string, bool)
}

// NewPipeline prepares stages for evaluation.
func NewPipeline(stages []Stage) *Pipeline {
	p := &Pipeline{}
	for _, s := range stages {
		switch st := s.(type) {
		case *LineFilter:
			p.stages = append(p.stages, lineFilterRunner{st})
		case *JSONStage:
			p.stages = append(p.stages, jsonRunner{st})
		case *LogfmtStage:
			p.stages = append(p.stages, logfmtRunner{st})
		case *RegexpStage:
			p.stages = append(p.stages, regexpRunner{st})
		case *PatternStage:
			p.stages = append(p.stages, patternRunner{st})
		case *UnpackStage:
			p.stages = append(p.stages, unpackRunner{})
		case *DecolorizeStage:
			p.stages = append(p.stages, decolorizeRunner{})
		case *LineFormatStage:
			p.stages = append(p.stages, newLineFormatRunner(st))
		case *LabelFormatStage:
			p.stages = append(p.stages, newLabelFormatRunner(st))
		case *DropStage:
			p.stages = append(p.stages, dropRunner{st.Labels})
		case *KeepStage:
			p.stages = append(p.stages, keepRunner{st.Labels})
		case *LabelFilterStage:
			p.stages = append(p.stages, filterRunner{st.Filter})
		}
	}
	return p
}

// Empty reports whether the pipeline has no stages.
func (p *Pipeline) Empty() bool { return p == nil || len(p.stages) == 0 }

// Process runs one line. stream must hold the line's stream labels
// (non-empty values only) and is not modified. It returns the final
// line and labels and whether the line survived the filters.
func (p *Pipeline) Process(ts int64, line string, stream map[string]string) (string, map[string]string, bool) {
	l := labelState{stream: stream}
	if p == nil {
		return line, stream, true
	}
	for _, s := range p.stages {
		var keep bool
		line, keep = s.run(ts, line, &l)
		if !keep {
			return "", nil, false
		}
	}
	return line, l.result(), true
}

type lineFilterRunner struct{ f *LineFilter }

func (r lineFilterRunner) run(_ int64, line string, _ *labelState) (string, bool) {
	return line, r.f.Match(line)
}

type filterRunner struct{ f LabelFilter }

func (r filterRunner) run(_ int64, line string, l *labelState) (string, bool) {
	return line, evalLabelFilter(r.f, l)
}

func evalLabelFilter(f LabelFilter, l *labelState) bool {
	switch n := f.(type) {
	case *BinaryLabelFilter:
		if n.And {
			return evalLabelFilter(n.Left, l) && evalLabelFilter(n.Right, l)
		}
		return evalLabelFilter(n.Left, l) || evalLabelFilter(n.Right, l)
	case *StringLabelFilter:
		v, _ := l.get(n.Matcher.Name)
		return n.Matcher.Matches(v)
	case *NumericLabelFilter:
		// Loki: a line that already failed a stage passes numeric
		// filters untouched (only string matchers on __error__ act on it).
		if l.hasErr() {
			return true
		}
		v, ok := l.get(n.Name)
		if !ok {
			return false
		}
		got, err := parseLabelNumber(v, n.Kind)
		if err != nil {
			l.setErr(errLabelFilter, err.Error())
			return true
		}
		return n.Op.cmp(got, n.Value)
	}
	return false
}

func parseLabelNumber(v string, kind ValueKind) (float64, error) {
	v = strings.TrimSpace(v)
	switch kind {
	case KindDuration:
		d, err := ParseDuration(v)
		if err != nil {
			return 0, err
		}
		return d.Seconds(), nil
	case KindBytes:
		return ParseBytes(v)
	}
	return strconv.ParseFloat(v, 64)
}

// ParseBytes parses sizes like 1024, 10KB, 1.5 MiB (units are
// case-insensitive; KB = 1000, KiB = 1024).
func ParseBytes(s string) (float64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == '-' || s[i] == '+' || s[i] == 'e' || s[i] == 'E') {
		// stop at 'e' that starts "eb"-like units: sizes never use exponents with units
		if (s[i] == 'e' || s[i] == 'E') && i+1 < len(s) && !(s[i+1] >= '0' && s[i+1] <= '9' || s[i+1] == '-' || s[i+1] == '+') {
			break
		}
		i++
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, err
	}
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	if unit == "" {
		return v, nil
	}
	mult, ok := byteUnits[unit]
	if !ok {
		return 0, &strconv.NumError{Func: "ParseBytes", Num: s, Err: strconv.ErrSyntax}
	}
	return v * mult, nil
}

// ─── json ────────────────────────────────────────────────────────────────

type jsonRunner struct{ st *JSONStage }

func (r jsonRunner) run(_ int64, line string, l *labelState) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		l.setErr(errJSON, "line is not a JSON object")
		return line, true
	}
	if len(r.st.Params) > 0 {
		var root map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &root); err != nil {
			l.setErr(errJSON, err.Error())
			return line, true
		}
		for _, p := range r.st.Params {
			if v, ok := jsonPathValue(root, p.path); ok {
				l.extract(p.Label, v)
			}
		}
		return line, true
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		l.setErr(errJSON, err.Error())
		return line, true
	}
	flattenJSON("", obj, l)
	return line, true
}

// flattenJSON extracts every scalar field, joining nested keys with
// "_" (Loki's json parser); arrays are skipped. Keys are visited in
// sorted order so collisions resolve deterministically.
func flattenJSON(prefix string, obj map[string]any, l *labelState) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := logschema.SanitizeLabelName(k)
		if name == "" {
			continue
		}
		if prefix != "" {
			name = prefix + "_" + name
		}
		switch v := obj[k].(type) {
		case map[string]any:
			flattenJSON(name, v, l)
		case string:
			l.extract(name, v)
		case json.Number:
			l.extract(name, v.String())
		case bool:
			l.extract(name, strconv.FormatBool(v))
		case nil:
			l.extract(name, "")
		}
	}
}

// jsonPathValue walks a parsed path through raw JSON and renders the
// value: strings unquoted, everything else as its JSON text.
func jsonPathValue(root map[string]json.RawMessage, path []pathElem) (string, bool) {
	var cur json.RawMessage
	for i, el := range path {
		if i == 0 {
			if el.index >= 0 {
				return "", false
			}
			v, ok := root[el.key]
			if !ok {
				return "", false
			}
			cur = v
			continue
		}
		if el.index >= 0 {
			var arr []json.RawMessage
			if json.Unmarshal(cur, &arr) != nil || el.index >= len(arr) {
				return "", false
			}
			cur = arr[el.index]
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(cur, &obj) != nil {
			return "", false
		}
		v, ok := obj[el.key]
		if !ok {
			return "", false
		}
		cur = v
	}
	cur = bytes.TrimSpace(cur)
	if len(cur) > 0 && cur[0] == '"' {
		var s string
		if json.Unmarshal(cur, &s) != nil {
			return "", false
		}
		return s, true
	}
	if string(cur) == "null" {
		return "", true
	}
	return string(cur), true
}

// ─── logfmt ──────────────────────────────────────────────────────────────

type logfmtRunner struct{ st *LogfmtStage }

func (r logfmtRunner) run(_ int64, line string, l *labelState) (string, bool) {
	want := map[string]string(nil) // logfmt key → label
	if len(r.st.Params) > 0 {
		want = make(map[string]string, len(r.st.Params))
		for _, p := range r.st.Params {
			want[p.Expr] = p.Label
		}
	}
	err := scanLogfmt(line, r.st.Strict, func(k, v string, bare bool) {
		if bare && !r.st.KeepEmpty {
			return
		}
		if want != nil {
			if label, ok := want[k]; ok {
				l.extract(label, v)
			}
			return
		}
		if name := logschema.SanitizeLabelName(k); name != "" {
			l.extract(name, v)
		}
	})
	if err != "" {
		l.setErr(errLogfmt, err)
	}
	return line, true
}

// scanLogfmt walks key=value pairs. Values may be double-quoted with
// Go escapes. A key without "=" is reported with bare=true. In strict
// mode the first malformed pair stops the scan with an error; otherwise
// malformed pairs are skipped.
func scanLogfmt(s string, strict bool, emit func(k, v string, bare bool)) string {
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' && s[i] != '\t' && s[i] != '"' {
			i++
		}
		key := s[start:i]
		if i < len(s) && s[i] == '"' || key == "" {
			if strict {
				return "unexpected '\"' or empty key at " + strconv.Itoa(i)
			}
			// skip the malformed token
			for i < len(s) && s[i] != ' ' && s[i] != '\t' {
				i++
			}
			continue
		}
		if i >= len(s) || s[i] != '=' {
			emit(key, "", true)
			continue
		}
		i++ // '='
		if i < len(s) && s[i] == '"' {
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				if strict {
					return "unterminated quoted value for key " + key
				}
				emit(key, s[i+1:], false)
				return ""
			}
			v, err := strconv.Unquote(s[i : j+1])
			if err != nil {
				v = s[i+1 : j]
			}
			emit(key, v, false)
			i = j + 1
			continue
		}
		vs := i
		for i < len(s) && s[i] != ' ' && s[i] != '\t' {
			i++
		}
		emit(key, s[vs:i], false)
	}
	return ""
}

// ─── regexp / pattern / unpack / decolorize ──────────────────────────────

type regexpRunner struct{ st *RegexpStage }

func (r regexpRunner) run(_ int64, line string, l *labelState) (string, bool) {
	m := r.st.re.FindStringSubmatchIndex(line)
	if m == nil {
		return line, true
	}
	for i, name := range r.st.re.SubexpNames() {
		if name == "" || m[2*i] < 0 {
			continue
		}
		l.extract(name, line[m[2*i]:m[2*i+1]])
	}
	return line, true
}

type patternRunner struct{ st *PatternStage }

func (r patternRunner) run(_ int64, line string, l *labelState) (string, bool) {
	matchPattern(r.st.parts, line, func(name, v string) {
		if name != "_" {
			l.extract(name, v)
		}
	})
	return line, true
}

// matchPattern follows Loki: a leading literal must be a prefix, each
// capture runs to the next occurrence of the following literal, and a
// trailing capture takes the rest. Captures found before a literal
// fails to match are still emitted.
func matchPattern(parts []patternPart, s string, emit func(name, v string)) {
	i := 0
	if len(parts) > 0 && parts[0].capture == "" {
		if !strings.HasPrefix(s, parts[0].literal) {
			return
		}
		s = s[len(parts[0].literal):]
		i = 1
	}
	for i < len(parts) {
		p := parts[i]
		if p.capture == "" { // literal after a literal cannot happen; skip defensively
			i++
			continue
		}
		if i == len(parts)-1 {
			emit(p.capture, s)
			return
		}
		lit := parts[i+1].literal
		j := strings.Index(s, lit)
		if j < 0 {
			return
		}
		emit(p.capture, s[:j])
		s = s[j+len(lit):]
		i += 2
	}
}

type unpackRunner struct{}

func (unpackRunner) run(_ int64, line string, l *labelState) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		l.setErr(errJSON, err.Error())
		return line, true
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var s string
		if json.Unmarshal(obj[k], &s) != nil {
			continue // Promtail packs string values only
		}
		if k == "_entry" {
			line = s
			continue
		}
		if name := logschema.SanitizeLabelName(k); name != "" {
			l.extract(name, s)
		}
	}
	return line, true
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

type decolorizeRunner struct{}

func (decolorizeRunner) run(_ int64, line string, _ *labelState) (string, bool) {
	if strings.IndexByte(line, 0x1b) < 0 {
		return line, true
	}
	return ansiRE.ReplaceAllString(line, ""), true
}

// ─── line_format / label_format / drop / keep ────────────────────────────

// tmplCtx carries the per-line values behind __line__ / __timestamp__.
type tmplCtx struct {
	line string
	ts   int64
}

func bindTemplate(t *template.Template) (*template.Template, *tmplCtx) {
	ctx := &tmplCtx{}
	c := template.Must(t.Clone())
	c.Funcs(template.FuncMap{
		"__line__":      func() string { return ctx.line },
		"__timestamp__": func() time.Time { return time.Unix(0, ctx.ts).UTC() },
	})
	return c, ctx
}

type lineFormatRunner struct {
	tmpl *template.Template
	ctx  *tmplCtx
	buf  *bytes.Buffer
}

func newLineFormatRunner(st *LineFormatStage) *lineFormatRunner {
	t, ctx := bindTemplate(st.tmpl)
	return &lineFormatRunner{tmpl: t, ctx: ctx, buf: &bytes.Buffer{}}
}

func (r *lineFormatRunner) run(ts int64, line string, l *labelState) (string, bool) {
	r.ctx.line, r.ctx.ts = line, ts
	out, err := execTemplate(r.tmpl, r.buf, l.result())
	if err != nil {
		l.setErr(errTemplate, err.Error())
		return line, true
	}
	return out, true
}

func execTemplate(t *template.Template, buf *bytes.Buffer, data map[string]string) (string, error) {
	buf.Reset()
	w := &limitWriter{buf: buf, max: maxFormattedLine}
	if err := t.Execute(w, data); err != nil && err != errLimit {
		return "", err
	}
	out := buf.String()
	if !utf8.ValidString(out) {
		out = strings.ToValidUTF8(out, "�")
	}
	return out, nil
}

type limitWriter struct {
	buf *bytes.Buffer
	max int
}

var errLimit = &strconv.NumError{Func: "template", Num: "output", Err: strconv.ErrRange}

func (w *limitWriter) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	if room <= 0 {
		return 0, errLimit
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		return room, errLimit
	}
	return w.buf.Write(p)
}

type labelFormatRunner struct {
	formats []LabelFmt
	tmpls   []*template.Template
	ctxs    []*tmplCtx
	buf     *bytes.Buffer
}

func newLabelFormatRunner(st *LabelFormatStage) *labelFormatRunner {
	r := &labelFormatRunner{formats: st.Formats, buf: &bytes.Buffer{}}
	for _, f := range st.Formats {
		if f.tmpl == nil {
			r.tmpls = append(r.tmpls, nil)
			r.ctxs = append(r.ctxs, nil)
			continue
		}
		t, ctx := bindTemplate(f.tmpl)
		r.tmpls = append(r.tmpls, t)
		r.ctxs = append(r.ctxs, ctx)
	}
	return r
}

func (r *labelFormatRunner) run(ts int64, line string, l *labelState) (string, bool) {
	// Loki evaluates every template against the labels as they were
	// before this stage, then applies the results.
	type change struct {
		dst, src, val string
		rename        bool
	}
	changes := make([]change, 0, len(r.formats))
	for i, f := range r.formats {
		if r.tmpls[i] == nil {
			v, _ := l.get(f.Src)
			changes = append(changes, change{dst: f.Dst, src: f.Src, val: v, rename: true})
			continue
		}
		r.ctxs[i].line, r.ctxs[i].ts = line, ts
		out, err := execTemplate(r.tmpls[i], r.buf, l.result())
		if err != nil {
			l.setErr(errTemplate, err.Error())
			continue
		}
		changes = append(changes, change{dst: f.Dst, val: out})
	}
	for _, c := range changes {
		if c.rename && c.src != c.dst {
			l.del(c.src)
		}
		l.set(c.dst, c.val)
	}
	return line, true
}

type dropRunner struct{ sels []LabelSel }

func (r dropRunner) run(_ int64, line string, l *labelState) (string, bool) {
	for _, s := range r.sels {
		v, ok := l.get(s.Name)
		if !ok {
			continue
		}
		if s.Matcher == nil || s.Matcher.Matches(v) {
			l.del(s.Name)
		}
	}
	return line, true
}

type keepRunner struct{ sels []LabelSel }

func (r keepRunner) run(_ int64, line string, l *labelState) (string, bool) {
	cur := l.result()
	var drop []string
	for k, v := range cur {
		keep := false
		for _, s := range r.sels {
			if s.Name == k && (s.Matcher == nil || s.Matcher.Matches(v)) {
				keep = true
				break
			}
		}
		if !keep {
			drop = append(drop, k)
		}
	}
	for _, k := range drop {
		l.del(k)
	}
	return line, true
}
