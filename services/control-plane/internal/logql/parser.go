// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"fmt"
	"math"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
)

// Limits on what a query may ask for.
const (
	maxRange       = 30 * 24 * time.Hour
	maxTopK        = 10_000
	maxStages      = 64
	maxMatchers    = 64
	maxLineFilters = 64
	maxRegexLen    = 4096
)

// Parse parses a log or metric query.
func Parse(q string) (Expr, error) {
	p, err := newParser(q)
	if err != nil {
		return nil, err
	}
	e, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		if t.kind == tLBracket {
			return nil, p.errorf(t, "a range like [5m] must be inside a function such as count_over_time(...)")
		}
		return nil, p.errorf(t, "unexpected %s after a complete query", t.describe())
	}
	return e, nil
}

// ParseLogSelector parses a log query (no metric functions).
func ParseLogSelector(q string) (*LogSelectorExpr, error) {
	e, err := Parse(q)
	if err != nil {
		return nil, err
	}
	sel, ok := e.(*LogSelectorExpr)
	if !ok {
		return nil, &ParseError{Line: 1, Col: 1, Msg: "expected a log query like {app=\"x\"} |= \"error\", got a metric query"}
	}
	return sel, nil
}

// ParseSampleExpr parses a metric query.
func ParseSampleExpr(q string) (SampleExpr, error) {
	e, err := Parse(q)
	if err != nil {
		return nil, err
	}
	s, ok := e.(SampleExpr)
	if !ok {
		return nil, &ParseError{Line: 1, Col: 1, Msg: "expected a metric query like count_over_time({app=\"x\"}[5m]), got a log query"}
	}
	return s, nil
}

// ParseLabels parses a Prometheus-style label set `{a="b", c="d"}`
// (Loki push stream labels). Only `=` is allowed.
func ParseLabels(s string) (map[string]string, error) {
	p, err := newParser(s)
	if err != nil {
		return nil, err
	}
	ms, err := p.parseSelector()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, p.errorf(t, "unexpected %s after the label set", t.describe())
	}
	out := make(map[string]string, len(ms))
	for _, m := range ms {
		if m.Type != MatchEqual {
			return nil, &ParseError{Line: 1, Col: 1, Msg: fmt.Sprintf("label %s: only = is allowed in a label set", m.Name)}
		}
		out[m.Name] = m.Value
	}
	return out, nil
}

type parser struct {
	src  string
	toks []token
	pos  int
}

func newParser(q string) (*parser, error) {
	toks, err := lex(q)
	if err != nil {
		return nil, err
	}
	return &parser{src: q, toks: toks}, nil
}

func (p *parser) peek() token        { return p.toks[p.pos] }
func (p *parser) peekAt(n int) token { return p.toks[min(p.pos+n, len(p.toks)-1)] }
func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}

func (p *parser) errorf(t token, format string, args ...any) *ParseError {
	return errAt(p.src, t.pos, format, args...)
}

func (p *parser) expect(k tokenKind, context string) (token, error) {
	t := p.next()
	if t.kind != k {
		return t, p.errorf(t, "expected %s %s, found %s", k, context, t.describe())
	}
	return t, nil
}

func (p *parser) isIdent(text string) bool {
	t := p.peek()
	return t.kind == tIdent && t.text == text
}

// ─── Metric / binary expressions ─────────────────────────────────────────

type binInfo struct {
	op    BinOp
	prec  int
	right bool
}

func (p *parser) binOp() (binInfo, bool) {
	t := p.peek()
	switch t.kind {
	case tAdd:
		return binInfo{OpAdd, 4, false}, true
	case tSub:
		return binInfo{OpSub, 4, false}, true
	case tMul:
		return binInfo{OpMul, 5, false}, true
	case tDiv:
		return binInfo{OpDiv, 5, false}, true
	case tMod:
		return binInfo{OpMod, 5, false}, true
	case tPow:
		return binInfo{OpPow, 6, true}, true
	case tCmpEq:
		return binInfo{OpEq, 3, false}, true
	case tNeq:
		return binInfo{OpNeq, 3, false}, true
	case tGt:
		return binInfo{OpGt, 3, false}, true
	case tGte:
		return binInfo{OpGte, 3, false}, true
	case tLt:
		return binInfo{OpLt, 3, false}, true
	case tLte:
		return binInfo{OpLte, 3, false}, true
	case tIdent:
		switch t.text {
		case "or":
			return binInfo{OpOr, 1, false}, true
		case "and":
			return binInfo{OpAnd, 2, false}, true
		case "unless":
			return binInfo{OpUnless, 2, false}, true
		}
	}
	return binInfo{}, false
}

func (p *parser) parseExpr(minPrec int) (Expr, error) {
	lhs, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		info, ok := p.binOp()
		if !ok || info.prec < minPrec {
			return lhs, nil
		}
		opTok := p.next()
		be := &BinOpExpr{Op: info.op}
		if p.isIdent("bool") {
			if !info.op.isComparison() {
				return nil, p.errorf(p.peek(), "bool modifier is only allowed on comparison operators")
			}
			p.next()
			be.ReturnBool = true
		}
		if p.isIdent("on") || p.isIdent("ignoring") {
			on := p.next().text == "on"
			labels, err := p.parseLabelList("after " + map[bool]string{true: "on", false: "ignoring"}[on])
			if err != nil {
				return nil, err
			}
			be.Matching = &VectorMatching{On: on, Labels: labels}
		}
		if p.isIdent("group_left") || p.isIdent("group_right") {
			return nil, p.errorf(p.peek(), "%s (many-to-one matching) is not supported", p.peek().text)
		}
		next := info.prec + 1
		if info.right {
			next = info.prec
		}
		rhs, err := p.parseExpr(next)
		if err != nil {
			return nil, err
		}
		l, lok := lhs.(SampleExpr)
		r, rok := rhs.(SampleExpr)
		if !lok || !rok {
			return nil, p.errorf(opTok, "operator %s needs metric queries on both sides (wrap log queries in count_over_time(...) or rate(...))", info.op)
		}
		if info.op.isSet() && (isScalar(l) || isScalar(r)) {
			return nil, p.errorf(opTok, "set operator %s is not allowed on scalars", info.op)
		}
		if info.op.isComparison() && isScalar(l) && isScalar(r) && !be.ReturnBool {
			return nil, p.errorf(opTok, "comparisons between scalars must use the bool modifier")
		}
		be.LHS, be.RHS = l, r
		lhs = be
	}
}

func isScalar(e SampleExpr) bool {
	switch n := e.(type) {
	case *LiteralExpr:
		return true
	case *BinOpExpr:
		return isScalar(n.LHS) && isScalar(n.RHS)
	}
	return false
}

func (p *parser) parseUnary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tSub, tAdd:
		p.next()
		operand, err := p.parseExpr(6) // binds looser than ^, tighter than * /
		if err != nil {
			return nil, err
		}
		s, ok := operand.(SampleExpr)
		if !ok {
			return nil, p.errorf(t, "unary %s needs a metric query", t.text)
		}
		if t.kind == tAdd {
			return s, nil
		}
		if lit, ok := s.(*LiteralExpr); ok {
			return &LiteralExpr{Value: -lit.Value}, nil
		}
		return &BinOpExpr{Op: OpMul, LHS: &LiteralExpr{Value: -1}, RHS: s}, nil
	case tNumber:
		p.next()
		return &LiteralExpr{Value: t.num}, nil
	case tLParen:
		p.next()
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tRParen, "to close '('"); err != nil {
			return nil, err
		}
		return e, nil
	case tLBrace:
		return p.parseLogExpr()
	case tIdent:
		return p.parseFunction()
	case tEOF:
		return nil, p.errorf(t, "unexpected end of query, expected a log selector like {app=\"x\"} or a metric function")
	}
	return nil, p.errorf(t, "unexpected %s, expected a log selector like {app=\"x\"} or a metric function", t.describe())
}

var rangeOps = map[string]RangeOp{
	"count_over_time": RangeCount, "rate": RangeRate, "bytes_over_time": RangeBytes,
	"bytes_rate": RangeBRate, "absent_over_time": RangeAbsent,
}

var unwrapOps = map[string]bool{
	"sum_over_time": true, "avg_over_time": true, "max_over_time": true, "min_over_time": true,
	"first_over_time": true, "last_over_time": true, "stdvar_over_time": true,
	"stddev_over_time": true, "quantile_over_time": true, "rate_counter": true,
}

var vecOps = map[string]VecOp{
	"sum": VecSum, "avg": VecAvg, "min": VecMin, "max": VecMax, "count": VecCount,
	"stddev": VecStddev, "stdvar": VecStdvar, "topk": VecTopk, "bottomk": VecBottomk,
}

func (p *parser) parseFunction() (Expr, error) {
	name := p.next()
	switch {
	case rangeOps[name.text] != "":
		return p.parseRangeAgg(name, rangeOps[name.text])
	case unwrapOps[name.text]:
		return nil, p.errorf(name, "%s needs | unwrap, which is not supported; use count_over_time, rate, bytes_over_time or bytes_rate", name.text)
	case vecOps[name.text] != "":
		return p.parseVectorAgg(name, vecOps[name.text])
	case name.text == "vector":
		if _, err := p.expect(tLParen, "after vector"); err != nil {
			return nil, err
		}
		neg := false
		if p.peek().kind == tSub {
			p.next()
			neg = true
		}
		num, err := p.expect(tNumber, "inside vector(...)")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tRParen, "to close vector("); err != nil {
			return nil, err
		}
		v := num.num
		if neg {
			v = -v
		}
		return &VectorExpr{Value: v}, nil
	case name.text == "label_replace" || name.text == "sort" || name.text == "sort_desc":
		return nil, p.errorf(name, "%s is not supported", name.text)
	}
	return nil, p.errorf(name, "unknown function %q (supported: count_over_time, rate, bytes_over_time, bytes_rate, absent_over_time, sum, avg, min, max, count, stddev, stdvar, topk, bottomk, vector)", name.text)
}

func (p *parser) parseRangeAgg(name token, op RangeOp) (Expr, error) {
	if _, err := p.expect(tLParen, "after "+name.text); err != nil {
		return nil, err
	}
	var sel *LogSelectorExpr
	var rng, offset time.Duration
	var err error
	if p.peek().kind == tLParen {
		// count_over_time(({app="x"} |= "y")[5m])
		p.next()
		inner, err := p.parseLogExpr()
		if err != nil {
			return nil, err
		}
		sel = inner
		if _, err := p.expect(tRParen, "to close the log query"); err != nil {
			return nil, err
		}
		if rng, offset, err = p.parseRange(); err != nil {
			return nil, err
		}
	} else {
		if p.peek().kind != tLBrace {
			return nil, p.errorf(p.peek(), "expected a log selector like {app=\"x\"} inside %s(...), found %s", name.text, p.peek().describe())
		}
		ms, err := p.parseSelector()
		if err != nil {
			return nil, err
		}
		sel = &LogSelectorExpr{Matchers: ms}
		if p.peek().kind == tLBracket {
			// range before the pipeline: {app="x"}[5m] | json
			if rng, offset, err = p.parseRange(); err != nil {
				return nil, err
			}
			if sel.Stages, err = p.parseStages(); err != nil {
				return nil, err
			}
		} else {
			if sel.Stages, err = p.parseStages(); err != nil {
				return nil, err
			}
			if p.peek().kind != tLBracket {
				return nil, p.errorf(p.peek(), "expected a range like [5m] in %s(...), found %s", name.text, p.peek().describe())
			}
			if rng, offset, err = p.parseRange(); err != nil {
				return nil, err
			}
		}
	}
	if _, err = p.expect(tRParen, "to close "+name.text+"("); err != nil {
		return nil, err
	}
	return &RangeAggExpr{Op: op, Selector: sel, Range: rng, Offset: offset}, nil
}

func (p *parser) parseRange() (time.Duration, time.Duration, error) {
	if _, err := p.expect(tLBracket, "to start a range"); err != nil {
		return 0, 0, err
	}
	d := p.next()
	if d.kind != tDuration {
		return 0, 0, p.errorf(d, "expected a duration like 5m inside [...], found %s", d.describe())
	}
	if d.dur <= 0 || d.dur > maxRange {
		return 0, 0, p.errorf(d, "range %s must be between 1ns and %s", d.text, FormatDuration(maxRange))
	}
	if _, err := p.expect(tRBracket, "to close the range"); err != nil {
		return 0, 0, err
	}
	var off time.Duration
	if p.isIdent("offset") {
		p.next()
		neg := false
		if p.peek().kind == tSub {
			p.next()
			neg = true
		}
		o := p.next()
		if o.kind != tDuration {
			return 0, 0, p.errorf(o, "expected a duration after offset, found %s", o.describe())
		}
		off = o.dur
		if neg {
			off = -off
		}
	}
	return d.dur, off, nil
}

func (p *parser) parseVectorAgg(name token, op VecOp) (Expr, error) {
	e := &VectorAggExpr{Op: op}
	var err error
	if p.isIdent("by") || p.isIdent("without") {
		if e.Grouping, err = p.parseGrouping(); err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(tLParen, "after "+name.text); err != nil {
		return nil, err
	}
	if op == VecTopk || op == VecBottomk {
		k := p.next()
		if k.kind != tNumber || k.num != math.Trunc(k.num) || k.num < 1 || k.num > maxTopK {
			return nil, p.errorf(k, "%s needs an integer k between 1 and %d as its first argument, found %s", name.text, maxTopK, k.describe())
		}
		e.Param = int(k.num)
		if _, err := p.expect(tComma, "after the k of "+name.text); err != nil {
			return nil, err
		}
	}
	innerTok := p.peek()
	inner, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	s, ok := inner.(SampleExpr)
	if !ok {
		return nil, p.errorf(innerTok, "%s(...) needs a metric query inside, e.g. %s(count_over_time({app=\"x\"}[5m]))", name.text, name.text)
	}
	if isScalar(s) {
		return nil, p.errorf(innerTok, "%s(...) needs a vector, not a scalar", name.text)
	}
	e.Inner = s
	if _, err := p.expect(tRParen, "to close "+name.text+"("); err != nil {
		return nil, err
	}
	if p.isIdent("by") || p.isIdent("without") {
		if e.Grouping != nil {
			return nil, p.errorf(p.peek(), "grouping given twice")
		}
		if e.Grouping, err = p.parseGrouping(); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func (p *parser) parseGrouping() (*Grouping, error) {
	kw := p.next()
	labels, err := p.parseLabelList("after " + kw.text)
	if err != nil {
		return nil, err
	}
	return &Grouping{Without: kw.text == "without", Labels: labels}, nil
}

func (p *parser) parseLabelList(context string) ([]string, error) {
	if _, err := p.expect(tLParen, context); err != nil {
		return nil, err
	}
	var out []string
	for p.peek().kind != tRParen {
		t := p.next()
		if t.kind != tIdent {
			return nil, p.errorf(t, "expected a label name, found %s", t.describe())
		}
		out = append(out, t.text)
		if p.peek().kind == tComma {
			p.next()
			continue
		}
		if p.peek().kind != tRParen {
			return nil, p.errorf(p.peek(), "expected ',' or ')' in the label list, found %s", p.peek().describe())
		}
	}
	p.next()
	return out, nil
}

// ─── Log expressions ─────────────────────────────────────────────────────

func (p *parser) parseLogExpr() (*LogSelectorExpr, error) {
	ms, err := p.parseSelector()
	if err != nil {
		return nil, err
	}
	stages, err := p.parseStages()
	if err != nil {
		return nil, err
	}
	return &LogSelectorExpr{Matchers: ms, Stages: stages}, nil
}

func (p *parser) parseSelector() ([]*LabelMatcher, error) {
	if _, err := p.expect(tLBrace, "to start a stream selector"); err != nil {
		return nil, err
	}
	var ms []*LabelMatcher
	for p.peek().kind != tRBrace {
		if len(ms) >= maxMatchers {
			return nil, p.errorf(p.peek(), "too many matchers (limit %d)", maxMatchers)
		}
		m, err := p.parseMatcher()
		if err != nil {
			return nil, err
		}
		ms = append(ms, m)
		switch p.peek().kind {
		case tComma:
			p.next()
		case tRBrace:
		default:
			return nil, p.errorf(p.peek(), "expected ',' or '}' after a matcher, found %s", p.peek().describe())
		}
	}
	p.next()
	return ms, nil
}

func (p *parser) parseMatcher() (*LabelMatcher, error) {
	name := p.next()
	if name.kind != tIdent {
		return nil, p.errorf(name, "expected a label name, found %s", name.describe())
	}
	opTok := p.next()
	var typ MatchType
	switch opTok.kind {
	case tEq:
		typ = MatchEqual
	case tNeq:
		typ = MatchNotEqual
	case tRe:
		typ = MatchRegexp
	case tNre:
		typ = MatchNotRegexp
	default:
		return nil, p.errorf(opTok, "expected one of = != =~ !~ after label %q, found %s", name.text, opTok.describe())
	}
	val := p.next()
	if val.kind != tString {
		return nil, p.errorf(val, "expected a quoted string after %s%s, found %s", name.text, opTok.text, val.describe())
	}
	return p.newMatcher(name.text, typ, val)
}

func (p *parser) newMatcher(name string, typ MatchType, val token) (*LabelMatcher, error) {
	m := &LabelMatcher{Name: name, Type: typ, Value: val.text}
	if typ == MatchRegexp || typ == MatchNotRegexp {
		re, err := compileRegex("^(?:"+val.text+")$", val.text)
		if err != nil {
			return nil, p.errorf(val, "invalid regular expression %s: %v", strconv.Quote(val.text), err)
		}
		m.re = re
	}
	return m, nil
}

// compileRegex compiles with Go's RE2 engine — the same syntax family
// ClickHouse's match() uses — and rejects oversized patterns.
func compileRegex(expr, orig string) (*regexp.Regexp, error) {
	if len(orig) > maxRegexLen {
		return nil, fmt.Errorf("pattern is longer than %d bytes", maxRegexLen)
	}
	if _, err := syntax.Parse(expr, syntax.Perl); err != nil {
		return nil, err
	}
	return regexp.Compile(expr)
}

func (p *parser) parseStages() ([]Stage, error) {
	var stages []Stage
	for {
		t := p.peek()
		var st Stage
		var err error
		switch t.kind {
		case tPipeEq, tPipeRe, tNeq, tNre:
			st, err = p.parseLineFilter()
		case tPipe:
			p.next()
			st, err = p.parseStage()
		default:
			return stages, nil
		}
		if err != nil {
			return nil, err
		}
		if len(stages) >= maxStages {
			return nil, p.errorf(t, "too many pipeline stages (limit %d)", maxStages)
		}
		stages = append(stages, st)
	}
}

func (p *parser) parseLineFilter() (Stage, error) {
	opTok := p.next()
	f := &LineFilter{}
	switch opTok.kind {
	case tPipeEq:
		f.Op = LineContains
	case tNeq:
		f.Op = LineNotContains
	case tPipeRe:
		f.Op = LineMatch
	default:
		f.Op = LineNotMatch
	}
	for {
		v := p.next()
		if v.kind == tIdent && v.text == "ip" {
			return nil, p.errorf(v, "ip(...) line filters are not supported")
		}
		if v.kind != tString {
			return nil, p.errorf(v, "expected a quoted string after %s, found %s", opTok.text, v.describe())
		}
		if f.Op == LineMatch || f.Op == LineNotMatch {
			re, err := compileRegex(v.text, v.text)
			if err != nil {
				return nil, p.errorf(v, "invalid regular expression %s: %v", strconv.Quote(v.text), err)
			}
			f.res = append(f.res, re)
		}
		f.Values = append(f.Values, v.text)
		if len(f.Values) > maxLineFilters {
			return nil, p.errorf(v, "too many alternatives in one line filter (limit %d)", maxLineFilters)
		}
		// `|= "a" or "b"`
		if p.isIdent("or") && p.peekAt(1).kind == tString {
			p.next()
			continue
		}
		return f, nil
	}
}

func (p *parser) parseStage() (Stage, error) {
	t := p.peek()
	if t.kind == tLParen {
		lf, err := p.parseLabelFilterOr()
		if err != nil {
			return nil, err
		}
		return &LabelFilterStage{Filter: lf}, nil
	}
	if t.kind != tIdent {
		return nil, p.errorf(t, "expected a pipeline stage (json, logfmt, regexp, pattern, line_format, label_format, drop, keep, or a label filter) after '|', found %s", t.describe())
	}
	// A keyword is a stage only when not used as a label filter's
	// label (`| json` vs `| json = "x"` is not a thing, but `| level`
	// alone is not a stage either).
	switch t.text {
	case "json":
		p.next()
		params, err := p.parseExtractParams(true)
		if err != nil {
			return nil, err
		}
		return &JSONStage{Params: params}, nil
	case "logfmt":
		p.next()
		st := &LogfmtStage{}
		for p.peek().kind == tSub && p.peekAt(1).kind == tSub && p.peekAt(2).kind == tIdent {
			p.next()
			p.next()
			flag := p.next()
			switch flag.text {
			case "strict":
				st.Strict = true
			case "keep":
				// --keep-empty lexes as keep - empty
				if p.peek().kind == tSub && p.peekAt(1).kind == tIdent && p.peekAt(1).text == "empty" {
					p.next()
					p.next()
					st.KeepEmpty = true
					continue
				}
				return nil, p.errorf(flag, "unknown logfmt flag --%s (want --strict or --keep-empty)", flag.text)
			default:
				return nil, p.errorf(flag, "unknown logfmt flag --%s (want --strict or --keep-empty)", flag.text)
			}
		}
		params, err := p.parseExtractParams(false)
		if err != nil {
			return nil, err
		}
		st.Params = params
		return st, nil
	case "regexp":
		p.next()
		v, err := p.expect(tString, "after regexp")
		if err != nil {
			return nil, err
		}
		re, err := compileRegex(v.text, v.text)
		if err != nil {
			return nil, p.errorf(v, "invalid regular expression %s: %v", strconv.Quote(v.text), err)
		}
		named := false
		for _, n := range re.SubexpNames() {
			if n == "" {
				continue
			}
			if !logschema.ValidLabelName(n) {
				return nil, p.errorf(v, "capture group name %q is not a valid label name", n)
			}
			named = true
		}
		if !named {
			return nil, p.errorf(v, "regexp stage needs at least one named capture group like (?P<status>\\d+)")
		}
		return &RegexpStage{Pattern: v.text, re: re}, nil
	case "pattern":
		p.next()
		v, err := p.expect(tString, "after pattern")
		if err != nil {
			return nil, err
		}
		parts, err := parsePattern(v.text)
		if err != nil {
			return nil, p.errorf(v, "invalid pattern: %v", err)
		}
		return &PatternStage{Pattern: v.text, parts: parts}, nil
	case "unpack":
		p.next()
		return &UnpackStage{}, nil
	case "decolorize":
		p.next()
		return &DecolorizeStage{}, nil
	case "unwrap":
		return nil, p.errorf(t, "unwrap is not supported; use count_over_time, rate, bytes_over_time or bytes_rate")
	case "line_format":
		p.next()
		v, err := p.expect(tString, "after line_format")
		if err != nil {
			return nil, err
		}
		tmpl, err := newTemplate("line_format", v.text)
		if err != nil {
			return nil, p.errorf(v, "invalid line_format template: %v", err)
		}
		return &LineFormatStage{Template: v.text, tmpl: tmpl}, nil
	case "label_format":
		p.next()
		return p.parseLabelFormat()
	case "drop", "keep":
		// `| drop` / `| keep` followed by a label name; `| keep = "x"`
		// would be a label filter on a label named keep.
		if nt := p.peekAt(1).kind; nt == tIdent {
			p.next()
			sels, err := p.parseLabelSels()
			if err != nil {
				return nil, err
			}
			if t.text == "drop" {
				return &DropStage{Labels: sels}, nil
			}
			return &KeepStage{Labels: sels}, nil
		}
	}
	lf, err := p.parseLabelFilterOr()
	if err != nil {
		return nil, err
	}
	return &LabelFilterStage{Filter: lf}, nil
}

// parseExtractParams parses `label="expr", label2, …` after json /
// logfmt. A bare label extracts the field of the same name.
func (p *parser) parseExtractParams(isJSON bool) ([]ExtractParam, error) {
	var out []ExtractParam
	for p.peek().kind == tIdent && (p.peekAt(1).kind == tEq || p.peekAt(1).kind == tComma || isStageEnd(p.peekAt(1).kind)) {
		name := p.next()
		param := ExtractParam{Label: name.text, Expr: name.text}
		if p.peek().kind == tEq {
			p.next()
			v, err := p.expect(tString, "after "+name.text+"=")
			if err != nil {
				return nil, err
			}
			param.Expr = v.text
		}
		if isJSON {
			path, err := parseJSONPath(param.Expr)
			if err != nil {
				return nil, p.errorf(name, "invalid json path %s: %v", strconv.Quote(param.Expr), err)
			}
			param.path = path
		}
		out = append(out, param)
		if p.peek().kind != tComma {
			break
		}
		p.next()
	}
	return out, nil
}

func isStageEnd(k tokenKind) bool {
	switch k {
	case tEOF, tPipe, tPipeEq, tPipeRe, tNeq, tNre, tRParen, tLBracket:
		return true
	}
	return false
}

func (p *parser) parseLabelFormat() (Stage, error) {
	st := &LabelFormatStage{}
	for {
		dst, err := p.expect(tIdent, "(label name) in label_format")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tEq, "after "+dst.text+" in label_format"); err != nil {
			return nil, err
		}
		v := p.next()
		switch v.kind {
		case tIdent:
			st.Formats = append(st.Formats, LabelFmt{Dst: dst.text, Src: v.text})
		case tString:
			tmpl, err := newTemplate("label_format", v.text)
			if err != nil {
				return nil, p.errorf(v, "invalid label_format template: %v", err)
			}
			st.Formats = append(st.Formats, LabelFmt{Dst: dst.text, Template: v.text, tmpl: tmpl})
		default:
			return nil, p.errorf(v, "expected a label name or a template string after %s=, found %s", dst.text, v.describe())
		}
		if p.peek().kind != tComma {
			return st, nil
		}
		p.next()
	}
}

func (p *parser) parseLabelSels() ([]LabelSel, error) {
	var out []LabelSel
	for {
		name, err := p.expect(tIdent, "(label name)")
		if err != nil {
			return nil, err
		}
		sel := LabelSel{Name: name.text}
		switch p.peek().kind {
		case tEq, tNeq, tRe, tNre:
			op := p.next()
			v, err := p.expect(tString, "after "+name.text+op.text)
			if err != nil {
				return nil, err
			}
			typ := map[tokenKind]MatchType{tEq: MatchEqual, tNeq: MatchNotEqual, tRe: MatchRegexp, tNre: MatchNotRegexp}[op.kind]
			m, err := p.newMatcher(name.text, typ, v)
			if err != nil {
				return nil, err
			}
			sel.Matcher = m
		}
		out = append(out, sel)
		if p.peek().kind != tComma {
			return out, nil
		}
		p.next()
	}
}

func (p *parser) parseLabelFilterOr() (LabelFilter, error) {
	l, err := p.parseLabelFilterAnd()
	if err != nil {
		return nil, err
	}
	for p.isIdent("or") {
		p.next()
		r, err := p.parseLabelFilterAnd()
		if err != nil {
			return nil, err
		}
		l = &BinaryLabelFilter{And: false, Left: l, Right: r}
	}
	return l, nil
}

func (p *parser) parseLabelFilterAnd() (LabelFilter, error) {
	l, err := p.parseLabelFilterAtom()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.kind == tComma || t.kind == tIdent && t.text == "and":
			p.next()
		case t.kind == tLParen || t.kind == tIdent && t.text != "or" && t.text != "offset":
			// Juxtaposition means "and": | status >= 500 method="GET"
		default:
			return l, nil
		}
		r, err := p.parseLabelFilterAtom()
		if err != nil {
			return nil, err
		}
		l = &BinaryLabelFilter{And: true, Left: l, Right: r}
	}
}

func (p *parser) parseLabelFilterAtom() (LabelFilter, error) {
	t := p.peek()
	if t.kind == tLParen {
		p.next()
		f, err := p.parseLabelFilterOr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tRParen, "to close '(' in the label filter"); err != nil {
			return nil, err
		}
		return f, nil
	}
	name := p.next()
	if name.kind != tIdent {
		return nil, p.errorf(name, "expected a label filter like status >= 500 or level=\"error\", found %s", name.describe())
	}
	if name.text == "ip" && p.peek().kind == tLParen {
		return nil, p.errorf(name, "ip(...) label filters are not supported")
	}
	op := p.next()
	val := p.peek()
	switch op.kind {
	case tEq, tNeq, tRe, tNre, tCmpEq:
		if val.kind == tString {
			p.next()
			typ := map[tokenKind]MatchType{tEq: MatchEqual, tCmpEq: MatchEqual, tNeq: MatchNotEqual, tRe: MatchRegexp, tNre: MatchNotRegexp}[op.kind]
			m, err := p.newMatcher(name.text, typ, val)
			if err != nil {
				return nil, err
			}
			return &StringLabelFilter{Matcher: m}, nil
		}
		if op.kind == tRe || op.kind == tNre {
			return nil, p.errorf(val, "expected a quoted regular expression after %s%s, found %s", name.text, op.text, val.describe())
		}
	case tGt, tGte, tLt, tLte:
	default:
		return nil, p.errorf(op, "expected a comparison (= != =~ !~ == > >= < <=) after label %q, found %s", name.text, op.describe())
	}
	p.next()
	f := &NumericLabelFilter{Name: name.text, Text: val.text}
	switch val.kind {
	case tNumber:
		f.Kind, f.Value = KindNumber, val.num
	case tDuration:
		f.Kind, f.Value = KindDuration, val.dur.Seconds()
	case tBytes:
		f.Kind, f.Value = KindBytes, val.num
	case tSub:
		// negative number
		num := p.next()
		if num.kind != tNumber {
			return nil, p.errorf(num, "expected a number after '-', found %s", num.describe())
		}
		f.Kind, f.Value, f.Text = KindNumber, -num.num, "-"+num.text
	default:
		return nil, p.errorf(val, "expected a number, duration (250ms) or size (10KB) after %s %s, found %s", name.text, op.text, val.describe())
	}
	switch op.kind {
	case tEq, tCmpEq:
		f.Op = CmpEq
	case tNeq:
		f.Op = CmpNeq
	case tGt:
		f.Op = CmpGt
	case tGte:
		f.Op = CmpGte
	case tLt:
		f.Op = CmpLt
	case tLte:
		f.Op = CmpLte
	}
	return f, nil
}

// ─── json paths and patterns ─────────────────────────────────────────────

type pathElem struct {
	key   string
	index int // -1 for object keys
}

// parseJSONPath parses Loki's json expressions: a.b.c, servers[0],
// request.headers["User-Agent"].
func parseJSONPath(s string) ([]pathElem, error) {
	var out []pathElem
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '.':
			if i == 0 || i == len(s)-1 {
				return nil, fmt.Errorf("unexpected '.' at %d", i)
			}
			i++
		case s[i] == '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unclosed '[' at %d", i)
			}
			inner := strings.TrimSpace(s[i+1 : i+end])
			if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0] {
				key := inner[1 : len(inner)-1]
				if inner[0] == '"' {
					unq, err := strconv.Unquote(inner)
					if err != nil {
						return nil, fmt.Errorf("bad quoted key %s", inner)
					}
					key = unq
				}
				out = append(out, pathElem{key: key, index: -1})
			} else {
				n, err := strconv.Atoi(inner)
				if err != nil || n < 0 {
					return nil, fmt.Errorf("array index %q is not a non-negative integer", inner)
				}
				out = append(out, pathElem{index: n})
			}
			i += end + 1
		default:
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			out = append(out, pathElem{key: s[i:j], index: -1})
			i = j
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	return out, nil
}

type patternPart struct {
	literal string
	capture string // "" for a literal part; "_" to discard
}

// parsePattern splits `<ip> - - <_> "<method> <path>"` into literals
// and captures. Two captures may not touch (nothing to split them on).
func parsePattern(s string) ([]patternPart, error) {
	var parts []patternPart
	names := map[string]bool{}
	i := 0
	var lit strings.Builder
	for i < len(s) {
		if s[i] == '<' {
			end := strings.IndexByte(s[i:], '>')
			if end > 1 {
				name := s[i+1 : i+end]
				if name == "_" || logschema.ValidLabelName(name) {
					if lit.Len() > 0 {
						parts = append(parts, patternPart{literal: lit.String()})
						lit.Reset()
					}
					if len(parts) > 0 && parts[len(parts)-1].capture != "" {
						return nil, fmt.Errorf("captures <%s> and <%s> are adjacent; put a literal between them", parts[len(parts)-1].capture, name)
					}
					if name != "_" {
						if names[name] {
							return nil, fmt.Errorf("capture <%s> appears twice", name)
						}
						names[name] = true
					}
					parts = append(parts, patternPart{capture: name})
					i += end + 1
					continue
				}
			}
		}
		lit.WriteByte(s[i])
		i++
	}
	if lit.Len() > 0 {
		parts = append(parts, patternPart{literal: lit.String()})
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("pattern has no named capture like <status>")
	}
	return parts, nil
}
