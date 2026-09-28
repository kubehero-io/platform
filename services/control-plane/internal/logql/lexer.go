// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package logql implements the subset of Grafana Loki's query language
// that KubeHero serves over its ClickHouse log store: a hand-written
// lexer and recursive-descent parser producing an AST with precise
// error positions, a compiler that pushes selectors, line filters and
// label filters down into parameterised ClickHouse SQL, a Go
// evaluator for everything SQL cannot express exactly (parsers,
// templates, extracted-label filters), and the metric evaluator that
// turns bucketed counts into range vectors, aggregations and binary
// operations with Loki's (t - range, t] window semantics.
//
// Supported grammar:
//
//	log query     {matchers} [line filters | stages]…
//	matchers      label = | != | =~ | !~ "value"
//	line filters  |= != |~ !~ "value" (or "value")…
//	stages        | json [label="path", …]  | logfmt [--strict] [--keep-empty] [labels…]
//	              | regexp "(?P<name>…)"  | pattern "<a> <_> <b>"  | unpack
//	              | label filters (== != > >= < <= on numbers, durations and
//	                bytes; = != =~ !~ on strings; and / or / "," / parentheses)
//	              | line_format "{{.label}}"  | label_format a=b, c="{{.x}}"
//	              | drop a, b="v"  | keep a, b  | decolorize
//	metric query  count_over_time rate bytes_over_time bytes_rate absent_over_time
//	              over [range] (offset allowed); sum avg min max count stddev
//	              stdvar topk bottomk with by / without; vector(s); scalar
//	              and vector binary operators + - * / % ^ == != > >= < <=
//	              (bool), and or unless, on / ignoring.
//
// Not supported (clear errors): unwrap and the *_over_time functions
// that need it, group_left / group_right, label_replace.
package logql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type tokenKind int

const (
	tEOF tokenKind = iota
	tIdent
	tString
	tNumber
	tDuration
	tBytes
	tLBrace   // {
	tRBrace   // }
	tLParen   // (
	tRParen   // )
	tLBracket // [
	tRBracket // ]
	tComma    // ,
	tEq       // =
	tNeq      // !=
	tRe       // =~
	tNre      // !~
	tPipeEq   // |=
	tPipeRe   // |~
	tPipe     // |
	tGt       // >
	tGte      // >=
	tLt       // <
	tLte      // <=
	tCmpEq    // ==
	tAdd      // +
	tSub      // -
	tMul      // *
	tDiv      // /
	tMod      // %
	tPow      // ^
)

var tokenNames = map[tokenKind]string{
	tEOF: "end of query", tIdent: "identifier", tString: "string", tNumber: "number",
	tDuration: "duration", tBytes: "bytes", tLBrace: "'{'", tRBrace: "'}'", tLParen: "'('",
	tRParen: "')'", tLBracket: "'['", tRBracket: "']'", tComma: "','", tEq: "'='", tNeq: "'!='",
	tRe: "'=~'", tNre: "'!~'", tPipeEq: "'|='", tPipeRe: "'|~'", tPipe: "'|'", tGt: "'>'",
	tGte: "'>='", tLt: "'<'", tLte: "'<='", tCmpEq: "'=='", tAdd: "'+'", tSub: "'-'",
	tMul: "'*'", tDiv: "'/'", tMod: "'%'", tPow: "'^'",
}

func (k tokenKind) String() string { return tokenNames[k] }

type token struct {
	kind tokenKind
	pos  int    // byte offset of the first character
	text string // raw text; for strings the unquoted value
	num  float64
	dur  time.Duration
}

func (t token) describe() string {
	switch t.kind {
	case tEOF:
		return "end of query"
	case tString:
		return strconv.Quote(t.text)
	case tIdent, tNumber, tDuration, tBytes:
		return strconv.Quote(t.text)
	}
	return t.kind.String()
}

// ParseError is a syntax error with a 1-based line and column.
type ParseError struct {
	Line, Col int
	Msg       string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("logql: parse error at line %d, col %d: %s", e.Line, e.Col, e.Msg)
}

func errAt(src string, pos int, format string, args ...any) *ParseError {
	if pos > len(src) {
		pos = len(src)
	}
	line, col := 1, 1
	for _, r := range src[:pos] {
		if r == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return &ParseError{Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}

// maxQueryLen bounds what the lexer will look at.
const maxQueryLen = 64 << 10

func lex(src string) ([]token, error) {
	if len(src) > maxQueryLen {
		return nil, &ParseError{Line: 1, Col: 1, Msg: fmt.Sprintf("query is %d bytes, the limit is %d", len(src), maxQueryLen)}
	}
	if !utf8.ValidString(src) {
		return nil, &ParseError{Line: 1, Col: 1, Msg: "query is not valid UTF-8"}
	}
	if i := strings.IndexByte(src, 0); i >= 0 {
		return nil, errAt(src, i, "NUL byte in query")
	}
	var toks []token
	i := 0
	for {
		// whitespace and # comments
		for i < len(src) {
			c := src[i]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				i++
				continue
			}
			if c == '#' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			break
		}
		if i >= len(src) {
			toks = append(toks, token{kind: tEOF, pos: i})
			return toks, nil
		}
		start := i
		c := src[i]
		two := ""
		if i+1 < len(src) {
			two = src[i : i+2]
		}
		switch {
		case c == 0:
			return nil, errAt(src, i, "NUL byte in query")
		case two == "|=":
			toks = append(toks, token{kind: tPipeEq, pos: start, text: two})
			i += 2
		case two == "|~":
			toks = append(toks, token{kind: tPipeRe, pos: start, text: two})
			i += 2
		case two == "!=":
			toks = append(toks, token{kind: tNeq, pos: start, text: two})
			i += 2
		case two == "!~":
			toks = append(toks, token{kind: tNre, pos: start, text: two})
			i += 2
		case two == "=~":
			toks = append(toks, token{kind: tRe, pos: start, text: two})
			i += 2
		case two == "==":
			toks = append(toks, token{kind: tCmpEq, pos: start, text: two})
			i += 2
		case two == ">=":
			toks = append(toks, token{kind: tGte, pos: start, text: two})
			i += 2
		case two == "<=":
			toks = append(toks, token{kind: tLte, pos: start, text: two})
			i += 2
		case c == '|':
			toks = append(toks, token{kind: tPipe, pos: start, text: "|"})
			i++
		case c == '=':
			toks = append(toks, token{kind: tEq, pos: start, text: "="})
			i++
		case c == '>':
			toks = append(toks, token{kind: tGt, pos: start, text: ">"})
			i++
		case c == '<':
			toks = append(toks, token{kind: tLt, pos: start, text: "<"})
			i++
		case c == '{':
			toks = append(toks, token{kind: tLBrace, pos: start, text: "{"})
			i++
		case c == '}':
			toks = append(toks, token{kind: tRBrace, pos: start, text: "}"})
			i++
		case c == '(':
			toks = append(toks, token{kind: tLParen, pos: start, text: "("})
			i++
		case c == ')':
			toks = append(toks, token{kind: tRParen, pos: start, text: ")"})
			i++
		case c == '[':
			toks = append(toks, token{kind: tLBracket, pos: start, text: "["})
			i++
		case c == ']':
			toks = append(toks, token{kind: tRBracket, pos: start, text: "]"})
			i++
		case c == ',':
			toks = append(toks, token{kind: tComma, pos: start, text: ","})
			i++
		case c == '+':
			toks = append(toks, token{kind: tAdd, pos: start, text: "+"})
			i++
		case c == '-':
			toks = append(toks, token{kind: tSub, pos: start, text: "-"})
			i++
		case c == '*':
			toks = append(toks, token{kind: tMul, pos: start, text: "*"})
			i++
		case c == '/':
			toks = append(toks, token{kind: tDiv, pos: start, text: "/"})
			i++
		case c == '%':
			toks = append(toks, token{kind: tMod, pos: start, text: "%"})
			i++
		case c == '^':
			toks = append(toks, token{kind: tPow, pos: start, text: "^"})
			i++
		case c == '"' || c == '`':
			s, n, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tString, pos: start, text: s})
			i = n
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			t, n, err := lexNumber(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i = n
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			for i < len(src) && (src[i] == '_' || src[i] >= 'a' && src[i] <= 'z' || src[i] >= 'A' && src[i] <= 'Z' || src[i] >= '0' && src[i] <= '9') {
				i++
			}
			toks = append(toks, token{kind: tIdent, pos: start, text: src[start:i]})
		default:
			r, _ := utf8.DecodeRuneInString(src[i:])
			if unicode.IsPrint(r) {
				return nil, errAt(src, i, "unexpected character %q", r)
			}
			return nil, errAt(src, i, "unexpected character %U", r)
		}
	}
}

// lexString scans a double-quoted (Go escapes) or backtick (raw)
// string starting at src[i] and returns its value and the offset after
// the closing quote.
func lexString(src string, i int) (string, int, error) {
	q := src[i]
	j := i + 1
	if q == '`' {
		end := strings.IndexByte(src[j:], '`')
		if end < 0 {
			return "", 0, errAt(src, i, "unterminated raw string (missing closing `)")
		}
		return src[j : j+end], j + end + 1, nil
	}
	for j < len(src) {
		switch src[j] {
		case '\\':
			j += 2
			continue
		case '\n':
			return "", 0, errAt(src, i, "unterminated string: newline before closing quote")
		case '"':
			s, err := strconv.Unquote(src[i : j+1])
			if err != nil {
				return "", 0, errAt(src, i, "invalid escape sequence in string %s", src[i:j+1])
			}
			if strings.IndexByte(s, 0) >= 0 {
				return "", 0, errAt(src, i, "NUL byte in string")
			}
			return s, j + 1, nil
		}
		j++
	}
	return "", 0, errAt(src, i, "unterminated string (missing closing \")")
}

var durationUnits = []string{"ns", "us", "µs", "ms", "s", "m", "h", "d", "w", "y"}

// byteUnits in lower case; matched case-insensitively.
var byteUnits = map[string]float64{
	"b": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12, "pb": 1e15,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40, "pib": 1 << 50,
}

// lexNumber scans a number, duration (1h30m) or byte size (10KB).
func lexNumber(src string, i int) (token, int, error) {
	start := i
	// hex literal
	if src[i] == '0' && i+1 < len(src) && (src[i+1] == 'x' || src[i+1] == 'X') {
		j := i + 2
		for j < len(src) && isHex(src[j]) {
			j++
		}
		v, err := strconv.ParseInt(src[i+2:j], 16, 64)
		if err != nil || j == i+2 {
			return token{}, 0, errAt(src, i, "invalid hex number %q", src[i:j])
		}
		return token{kind: tNumber, pos: start, text: src[i:j], num: float64(v)}, j, nil
	}
	j := scanDecimal(src, i)
	// unit suffix?
	k := j
	for k < len(src) && (src[k] >= 'a' && src[k] <= 'z' || src[k] >= 'A' && src[k] <= 'Z' || strings.HasPrefix(src[k:], "µ")) {
		if strings.HasPrefix(src[k:], "µ") {
			k += len("µ")
		} else {
			k++
		}
	}
	if k == j {
		v, err := strconv.ParseFloat(src[i:j], 64)
		if err != nil {
			return token{}, 0, errAt(src, i, "invalid number %q", src[i:j])
		}
		return token{kind: tNumber, pos: start, text: src[i:j], num: v}, j, nil
	}
	unit := src[j:k]
	if isDurationUnit(unit) {
		// Durations may chain: 1h30m15s.
		end := k
		for end < len(src) && src[end] >= '0' && src[end] <= '9' {
			n := scanDecimal(src, end)
			u := n
			for u < len(src) && (src[u] >= 'a' && src[u] <= 'z' || strings.HasPrefix(src[u:], "µ")) {
				if strings.HasPrefix(src[u:], "µ") {
					u += len("µ")
				} else {
					u++
				}
			}
			if u == n || !isDurationUnit(src[n:u]) {
				break
			}
			end = u
		}
		text := src[start:end]
		d, err := ParseDuration(text)
		if err != nil {
			return token{}, 0, errAt(src, start, "invalid duration %q: %v", text, err)
		}
		return token{kind: tDuration, pos: start, text: text, dur: d}, end, nil
	}
	if mult, ok := byteUnits[strings.ToLower(unit)]; ok {
		v, err := strconv.ParseFloat(src[i:j], 64)
		if err != nil {
			return token{}, 0, errAt(src, i, "invalid number %q", src[i:j])
		}
		return token{kind: tBytes, pos: start, text: src[start:k], num: v * mult}, k, nil
	}
	return token{}, 0, errAt(src, j, "unknown unit %q after number (want a duration like 5m or a size like 10KB)", unit)
}

func scanDecimal(src string, i int) int {
	j := i
	for j < len(src) && src[j] >= '0' && src[j] <= '9' {
		j++
	}
	if j < len(src) && src[j] == '.' {
		j++
		for j < len(src) && src[j] >= '0' && src[j] <= '9' {
			j++
		}
	}
	// exponent: 1e3, 2.5E-2 (not a unit: e is followed by a digit or sign+digit)
	if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < len(src) && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k < len(src) && src[k] >= '0' && src[k] <= '9' {
			for k < len(src) && src[k] >= '0' && src[k] <= '9' {
				k++
			}
			j = k
		}
	}
	return j
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isDurationUnit(u string) bool {
	for _, d := range durationUnits {
		if u == d {
			return true
		}
	}
	return false
}

// ParseDuration parses Prometheus-style durations: Go units plus d
// (24h), w (7d) and y (365d), chainable (1d12h).
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	rest := s
	for rest != "" {
		j := 0
		for j < len(rest) && (rest[j] >= '0' && rest[j] <= '9' || rest[j] == '.') {
			j++
		}
		if j == 0 {
			return 0, fmt.Errorf("expected a number in %q", s)
		}
		num := rest[:j]
		k := j
		for k < len(rest) && !(rest[k] >= '0' && rest[k] <= '9') {
			k++
		}
		unit := rest[j:k]
		rest = rest[k:]
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("bad number %q", num)
		}
		var mult time.Duration
		switch unit {
		case "ns":
			mult = time.Nanosecond
		case "us", "µs":
			mult = time.Microsecond
		case "ms":
			mult = time.Millisecond
		case "s":
			mult = time.Second
		case "m":
			mult = time.Minute
		case "h":
			mult = time.Hour
		case "d":
			mult = 24 * time.Hour
		case "w":
			mult = 7 * 24 * time.Hour
		case "y":
			mult = 365 * 24 * time.Hour
		default:
			return 0, fmt.Errorf("unknown unit %q", unit)
		}
		f := v * float64(mult)
		if f > float64(1<<62) {
			return 0, fmt.Errorf("duration %q is too large", s)
		}
		total += time.Duration(f)
	}
	return total, nil
}
