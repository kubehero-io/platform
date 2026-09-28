// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Expr is a parsed query: a *LogSelectorExpr (log query) or a
// SampleExpr (metric query). String renders canonical LogQL that
// parses back to an equal tree.
type Expr interface {
	String() string
	isExpr()
}

// SampleExpr is a metric query node.
type SampleExpr interface {
	Expr
	isSample()
}

// ─── Log queries ─────────────────────────────────────────────────────────

// MatchType is a label matcher operator.
type MatchType int

const (
	MatchEqual MatchType = iota
	MatchNotEqual
	MatchRegexp
	MatchNotRegexp
)

func (m MatchType) String() string {
	return [...]string{"=", "!=", "=~", "!~"}[m]
}

// LabelMatcher is `name op "value"`. Regular expressions are fully
// anchored, as in Loki and Prometheus.
type LabelMatcher struct {
	Name  string
	Type  MatchType
	Value string
	re    *regexp.Regexp
}

// Matches reports whether a label value satisfies the matcher (an
// absent label has value "").
func (m *LabelMatcher) Matches(v string) bool {
	switch m.Type {
	case MatchEqual:
		return v == m.Value
	case MatchNotEqual:
		return v != m.Value
	case MatchRegexp:
		return m.re.MatchString(v)
	default:
		return !m.re.MatchString(v)
	}
}

func (m *LabelMatcher) String() string {
	return m.Name + m.Type.String() + strconv.Quote(m.Value)
}

// LogSelectorExpr is a stream selector plus its pipeline.
type LogSelectorExpr struct {
	Matchers []*LabelMatcher
	Stages   []Stage
}

func (*LogSelectorExpr) isExpr() {}

func (e *LogSelectorExpr) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, m := range e.Matchers {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(m.String())
	}
	b.WriteByte('}')
	for _, s := range e.Stages {
		b.WriteByte(' ')
		b.WriteString(s.String())
	}
	return b.String()
}

// Stage is one pipeline element.
type Stage interface {
	String() string
	isStage()
}

// LineOp is a line filter operator.
type LineOp int

const (
	LineContains LineOp = iota
	LineNotContains
	LineMatch
	LineNotMatch
)

func (o LineOp) String() string { return [...]string{"|=", "!=", "|~", "!~"}[o] }

// LineFilter keeps (or drops) lines containing / matching any of its
// values: `|= "a" or "b"` keeps lines containing a or b; `!= "a" or "b"`
// drops lines containing either.
type LineFilter struct {
	Op     LineOp
	Values []string
	res    []*regexp.Regexp
}

func (*LineFilter) isStage() {}

func (f *LineFilter) String() string {
	var b strings.Builder
	b.WriteString(f.Op.String())
	for i, v := range f.Values {
		if i > 0 {
			b.WriteString(" or")
		}
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(v))
	}
	return b.String()
}

// Match reports whether the line passes the filter.
func (f *LineFilter) Match(line string) bool {
	hit := false
	for i, v := range f.Values {
		switch f.Op {
		case LineContains, LineNotContains:
			hit = strings.Contains(line, v)
		default:
			hit = f.res[i].MatchString(line)
		}
		if hit {
			break
		}
	}
	if f.Op == LineNotContains || f.Op == LineNotMatch {
		return !hit
	}
	return hit
}

// ExtractParam is `label="expression"` (json path or logfmt key).
type ExtractParam struct {
	Label string
	Expr  string
	path  []pathElem // json only
}

func (p ExtractParam) String() string {
	if p.Expr == p.Label {
		return p.Label
	}
	return p.Label + "=" + strconv.Quote(p.Expr)
}

func paramsString(ps []ExtractParam) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}

// JSONStage is `| json` (every field, nested keys joined with "_") or
// `| json label="path", …` (only the listed paths).
type JSONStage struct{ Params []ExtractParam }

func (*JSONStage) isStage() {}
func (s *JSONStage) String() string {
	if len(s.Params) == 0 {
		return "| json"
	}
	return "| json " + paramsString(s.Params)
}

// LogfmtStage is `| logfmt [--strict] [--keep-empty] [params]`.
type LogfmtStage struct {
	Strict, KeepEmpty bool
	Params            []ExtractParam
}

func (*LogfmtStage) isStage() {}
func (s *LogfmtStage) String() string {
	b := "| logfmt"
	if s.Strict {
		b += " --strict"
	}
	if s.KeepEmpty {
		b += " --keep-empty"
	}
	if len(s.Params) > 0 {
		b += " " + paramsString(s.Params)
	}
	return b
}

// RegexpStage extracts named capture groups as labels.
type RegexpStage struct {
	Pattern string
	re      *regexp.Regexp
}

func (*RegexpStage) isStage()         {}
func (s *RegexpStage) String() string { return "| regexp " + strconv.Quote(s.Pattern) }

// PatternStage is `| pattern "<ip> - <_> <status>"`.
type PatternStage struct {
	Pattern string
	parts   []patternPart
}

func (*PatternStage) isStage()         {}
func (s *PatternStage) String() string { return "| pattern " + strconv.Quote(s.Pattern) }

// UnpackStage unpacks Promtail's packed JSON lines.
type UnpackStage struct{}

func (*UnpackStage) isStage()       {}
func (*UnpackStage) String() string { return "| unpack" }

// DecolorizeStage strips ANSI color codes from the line.
type DecolorizeStage struct{}

func (*DecolorizeStage) isStage()       {}
func (*DecolorizeStage) String() string { return "| decolorize" }

// LineFormatStage rewrites the line from a Go template.
type LineFormatStage struct {
	Template string
	tmpl     *template.Template
}

func (*LineFormatStage) isStage()         {}
func (s *LineFormatStage) String() string { return "| line_format " + strconv.Quote(s.Template) }

// LabelFmt is one label_format assignment: `dst=src` renames, and
// `dst="{{…}}"` sets from a template.
type LabelFmt struct {
	Dst      string
	Src      string // rename source (when Template == "")
	Template string
	tmpl     *template.Template
}

func (f LabelFmt) String() string {
	if f.tmpl != nil {
		return f.Dst + "=" + strconv.Quote(f.Template)
	}
	return f.Dst + "=" + f.Src
}

// LabelFormatStage is `| label_format a=b, c="{{.x}}"`.
type LabelFormatStage struct{ Formats []LabelFmt }

func (*LabelFormatStage) isStage() {}
func (s *LabelFormatStage) String() string {
	parts := make([]string, len(s.Formats))
	for i, f := range s.Formats {
		parts[i] = f.String()
	}
	return "| label_format " + strings.Join(parts, ", ")
}

// LabelSel names a label, optionally only when its value matches.
type LabelSel struct {
	Name    string
	Matcher *LabelMatcher // nil = unconditional
}

func (l LabelSel) String() string {
	if l.Matcher != nil {
		return l.Matcher.String()
	}
	return l.Name
}

func selsString(ls []LabelSel) string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.String()
	}
	return strings.Join(parts, ", ")
}

// DropStage removes labels.
type DropStage struct{ Labels []LabelSel }

func (*DropStage) isStage()         {}
func (s *DropStage) String() string { return "| drop " + selsString(s.Labels) }

// KeepStage removes every label not listed.
type KeepStage struct{ Labels []LabelSel }

func (*KeepStage) isStage()         {}
func (s *KeepStage) String() string { return "| keep " + selsString(s.Labels) }

// LabelFilterStage keeps lines whose labels satisfy a filter.
type LabelFilterStage struct{ Filter LabelFilter }

func (*LabelFilterStage) isStage()         {}
func (s *LabelFilterStage) String() string { return "| " + s.Filter.String() }

// LabelFilter is a predicate over the current labels.
type LabelFilter interface {
	String() string
	isLabelFilter()
}

// BinaryLabelFilter is `a and b` / `a or b`.
type BinaryLabelFilter struct {
	And         bool
	Left, Right LabelFilter
}

func (*BinaryLabelFilter) isLabelFilter() {}
func (f *BinaryLabelFilter) String() string {
	op := " or "
	if f.And {
		op = " and "
	}
	return "(" + f.Left.String() + op + f.Right.String() + ")"
}

// StringLabelFilter applies a string matcher to a label.
type StringLabelFilter struct{ Matcher *LabelMatcher }

func (*StringLabelFilter) isLabelFilter()   {}
func (f *StringLabelFilter) String() string { return f.Matcher.String() }

// CmpOp is a numeric comparison.
type CmpOp int

const (
	CmpEq CmpOp = iota
	CmpNeq
	CmpGt
	CmpGte
	CmpLt
	CmpLte
)

func (o CmpOp) String() string { return [...]string{"==", "!=", ">", ">=", "<", "<="}[o] }

func (o CmpOp) cmp(a, b float64) bool {
	switch o {
	case CmpEq:
		return a == b
	case CmpNeq:
		return a != b
	case CmpGt:
		return a > b
	case CmpGte:
		return a >= b
	case CmpLt:
		return a < b
	}
	return a <= b
}

// ValueKind says how a numeric filter parses label values.
type ValueKind int

const (
	KindNumber ValueKind = iota
	KindDuration
	KindBytes
)

// NumericLabelFilter compares a label parsed as a number, duration
// (seconds) or byte size against a literal.
type NumericLabelFilter struct {
	Name  string
	Op    CmpOp
	Value float64 // seconds for durations, bytes for sizes
	Kind  ValueKind
	Text  string // literal as written
}

func (*NumericLabelFilter) isLabelFilter() {}
func (f *NumericLabelFilter) String() string {
	return f.Name + " " + f.Op.String() + " " + f.Text
}

// ─── Metric queries ──────────────────────────────────────────────────────

// RangeOp is a range aggregation over log lines.
type RangeOp string

const (
	RangeCount  RangeOp = "count_over_time"
	RangeRate   RangeOp = "rate"
	RangeBytes  RangeOp = "bytes_over_time"
	RangeBRate  RangeOp = "bytes_rate"
	RangeAbsent RangeOp = "absent_over_time"
)

// RangeAggExpr is `op({selector} | pipeline [range] offset o)`.
type RangeAggExpr struct {
	Op       RangeOp
	Selector *LogSelectorExpr
	Range    time.Duration
	Offset   time.Duration
}

func (*RangeAggExpr) isExpr()   {}
func (*RangeAggExpr) isSample() {}
func (e *RangeAggExpr) String() string {
	s := string(e.Op) + "(" + e.Selector.String() + " [" + FormatDuration(e.Range) + "]"
	if e.Offset != 0 {
		s += " offset " + FormatDuration(e.Offset)
	}
	return s + ")"
}

// VecOp is a vector aggregation.
type VecOp string

const (
	VecSum     VecOp = "sum"
	VecAvg     VecOp = "avg"
	VecMin     VecOp = "min"
	VecMax     VecOp = "max"
	VecCount   VecOp = "count"
	VecStddev  VecOp = "stddev"
	VecStdvar  VecOp = "stdvar"
	VecTopk    VecOp = "topk"
	VecBottomk VecOp = "bottomk"
)

// Grouping is `by (…)` or `without (…)`.
type Grouping struct {
	Without bool
	Labels  []string
}

func (g *Grouping) String() string {
	kw := "by"
	if g.Without {
		kw = "without"
	}
	return kw + " (" + strings.Join(g.Labels, ", ") + ")"
}

// VectorAggExpr aggregates series at each step.
type VectorAggExpr struct {
	Op       VecOp
	Grouping *Grouping // nil = aggregate everything into one series
	Param    int       // topk / bottomk k
	Inner    SampleExpr
}

func (*VectorAggExpr) isExpr()   {}
func (*VectorAggExpr) isSample() {}
func (e *VectorAggExpr) String() string {
	s := string(e.Op)
	if e.Grouping != nil {
		s += " " + e.Grouping.String()
	}
	s += "("
	if e.Op == VecTopk || e.Op == VecBottomk {
		s += strconv.Itoa(e.Param) + ", "
	}
	return s + e.Inner.String() + ")"
}

// BinOp is a binary operator.
type BinOp string

const (
	OpAdd    BinOp = "+"
	OpSub    BinOp = "-"
	OpMul    BinOp = "*"
	OpDiv    BinOp = "/"
	OpMod    BinOp = "%"
	OpPow    BinOp = "^"
	OpEq     BinOp = "=="
	OpNeq    BinOp = "!="
	OpGt     BinOp = ">"
	OpGte    BinOp = ">="
	OpLt     BinOp = "<"
	OpLte    BinOp = "<="
	OpAnd    BinOp = "and"
	OpOr     BinOp = "or"
	OpUnless BinOp = "unless"
)

func (o BinOp) isComparison() bool {
	switch o {
	case OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte:
		return true
	}
	return false
}

func (o BinOp) isSet() bool { return o == OpAnd || o == OpOr || o == OpUnless }

// VectorMatching is `on (…)` / `ignoring (…)`.
type VectorMatching struct {
	On     bool
	Labels []string
}

// BinOpExpr is `lhs op [bool] [on|ignoring (…)] rhs`.
type BinOpExpr struct {
	Op         BinOp
	LHS, RHS   SampleExpr
	ReturnBool bool
	Matching   *VectorMatching
}

func (*BinOpExpr) isExpr()   {}
func (*BinOpExpr) isSample() {}
func (e *BinOpExpr) String() string {
	op := string(e.Op)
	if e.ReturnBool {
		op += " bool"
	}
	if e.Matching != nil {
		kw := "ignoring"
		if e.Matching.On {
			kw = "on"
		}
		op += " " + kw + " (" + strings.Join(e.Matching.Labels, ", ") + ")"
	}
	return "(" + e.LHS.String() + " " + op + " " + e.RHS.String() + ")"
}

// LiteralExpr is a scalar number.
type LiteralExpr struct{ Value float64 }

func (*LiteralExpr) isExpr()   {}
func (*LiteralExpr) isSample() {}
func (e *LiteralExpr) String() string {
	return strconv.FormatFloat(e.Value, 'g', -1, 64)
}

// VectorExpr is `vector(s)`: one label-less series with value s.
type VectorExpr struct{ Value float64 }

func (*VectorExpr) isExpr()   {}
func (*VectorExpr) isSample() {}
func (e *VectorExpr) String() string {
	return "vector(" + strconv.FormatFloat(e.Value, 'g', -1, 64) + ")"
}

// FormatDuration renders a duration the way LogQL writes it (1h30m,
// 5m, 90s, 250ms), using d/w/y for whole days, weeks and years.
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	var b strings.Builder
	if d < 0 {
		b.WriteByte('-')
		d = -d
	}
	units := []struct {
		name string
		d    time.Duration
	}{
		{"y", 365 * 24 * time.Hour}, {"w", 7 * 24 * time.Hour}, {"d", 24 * time.Hour},
		{"h", time.Hour}, {"m", time.Minute}, {"s", time.Second}, {"ms", time.Millisecond},
		{"us", time.Microsecond}, {"ns", time.Nanosecond},
	}
	for _, u := range units {
		if n := d / u.d; n > 0 {
			b.WriteString(strconv.FormatInt(int64(n), 10))
			b.WriteString(u.name)
			d -= n * u.d
		}
	}
	return b.String()
}

// Walk visits every log selector in a metric expression.
func Walk(e Expr, fn func(*LogSelectorExpr)) {
	switch n := e.(type) {
	case *LogSelectorExpr:
		fn(n)
	case *RangeAggExpr:
		fn(n.Selector)
	case *VectorAggExpr:
		Walk(n.Inner, fn)
	case *BinOpExpr:
		Walk(n.LHS, fn)
		Walk(n.RHS, fn)
	}
}
