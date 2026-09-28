// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
)

// SQL generation. Every value — label values, line filter patterns,
// regexes, map keys, times — travels as a `?` argument; the only
// identifiers in the SQL text come from logschema's fixed column list.
// Regexes are validated with Go's RE2 before they reach ClickHouse's
// RE2-based match(). (One known divergence: ClickHouse matches bytes,
// Go matches UTF-8 runes, so `.` differs on non-ASCII text.)

// sqlPart is a SQL fragment with its arguments in placeholder order.
type sqlPart struct {
	sql  string
	args []any
}

func and(parts []sqlPart) sqlPart {
	switch len(parts) {
	case 0:
		return sqlPart{sql: "1"}
	case 1:
		return parts[0]
	}
	var out sqlPart
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = "(" + p.sql + ")"
		out.args = append(out.args, p.args...)
	}
	out.sql = strings.Join(texts, " AND ")
	return out
}

func or(parts []sqlPart) sqlPart {
	if len(parts) == 1 {
		return parts[0]
	}
	var out sqlPart
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = "(" + p.sql + ")"
		out.args = append(out.args, p.args...)
	}
	out.sql = strings.Join(texts, " OR ")
	return out
}

func not(p sqlPart) sqlPart { return sqlPart{sql: "NOT (" + p.sql + ")", args: p.args} }

// labelExpr is the SQL expression holding a stream label.
func labelExpr(name string) sqlPart {
	if col, ok := logschema.Column(name); ok {
		return sqlPart{sql: col}
	}
	return sqlPart{sql: "labels[?]", args: []any{name}}
}

// escapeLike escapes LIKE wildcards so the value matches literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const regexMeta = `\.+*?()[]{}^$|`

// literalAlternation recognises `a|b|c` where every branch is a plain
// literal (what Grafana sends for multi-value variables) so it can
// become an IN list.
func literalAlternation(re string) ([]string, bool) {
	parts := strings.Split(re, "|")
	for _, p := range parts {
		if strings.ContainsAny(p, strings.TrimSuffix(regexMeta, "|")) {
			return nil, false
		}
	}
	return parts, true
}

func isLiteral(re string) bool { return !strings.ContainsAny(re, regexMeta) }

func matcherSQL(m *LabelMatcher) sqlPart {
	col := labelExpr(m.Name)
	with := func(sql string, args ...any) sqlPart {
		return sqlPart{sql: sql, args: append(append([]any{}, col.args...), args...)}
	}
	positive := func() sqlPart {
		if m.Value == ".*" {
			return sqlPart{sql: "1"}
		}
		if m.Value == ".+" {
			return with(col.sql + " != ''")
		}
		if lits, ok := literalAlternation(m.Value); ok {
			ph := strings.TrimSuffix(strings.Repeat("?, ", len(lits)), ", ")
			args := make([]any, len(lits))
			for i, l := range lits {
				args[i] = l
			}
			if len(lits) == 1 {
				return with(col.sql+" = ?", args...)
			}
			return with(col.sql+" IN ("+ph+")", args...)
		}
		return with("match("+col.sql+", ?)", "^(?:"+m.Value+")$")
	}
	switch m.Type {
	case MatchEqual:
		return with(col.sql+" = ?", m.Value)
	case MatchNotEqual:
		return with(col.sql+" != ?", m.Value)
	case MatchRegexp:
		return positive()
	}
	p := positive()
	if p.sql == "1" {
		return sqlPart{sql: "0"}
	}
	return not(p)
}

// asciiLiteralFold recognises `(?i)literal` with an ASCII literal.
func asciiLiteralFold(re string) (string, bool) {
	rest, ok := strings.CutPrefix(re, "(?i)")
	if !ok || rest == "" || !isLiteral(rest) {
		return "", false
	}
	for _, r := range rest {
		if r > unicode.MaxASCII {
			return "", false
		}
	}
	return rest, true
}

// lineFilterSQL is exact for the line as stored (body).
func lineFilterSQL(f *LineFilter) sqlPart {
	var alts []sqlPart
	for _, v := range f.Values {
		switch f.Op {
		case LineContains, LineNotContains:
			if v == "" {
				alts = append(alts, sqlPart{sql: "1"})
				continue
			}
			alts = append(alts, sqlPart{sql: "like(body, ?)", args: []any{"%" + escapeLike(v) + "%"}})
		default:
			switch lit, fold := asciiLiteralFold(v); {
			case v == "":
				alts = append(alts, sqlPart{sql: "1"})
			case isLiteral(v):
				alts = append(alts, sqlPart{sql: "like(body, ?)", args: []any{"%" + escapeLike(v) + "%"}})
			case fold:
				alts = append(alts, sqlPart{sql: "ilike(body, ?)", args: []any{"%" + escapeLike(lit) + "%"}})
			default:
				alts = append(alts, sqlPart{sql: "match(body, ?)", args: []any{v}})
			}
		}
	}
	p := or(alts)
	if f.Op == LineNotContains || f.Op == LineNotMatch {
		return not(p)
	}
	return p
}

// Plan is a log query split between ClickHouse and Go.
type Plan struct {
	// Where is a SQL predicate over the logs table (no org / time
	// terms; callers add those). Rows it rejects can never survive the
	// pipeline, so it is always safe to apply.
	Where sqlPart
	// Stages is what must still run in Go, in order; empty when Where
	// alone decides the result exactly.
	Stages []Stage
	// RollupOK: the query is a bare selector over labels that
	// log_volume_1m keeps, so volume / count / bytes can come from the
	// rollup.
	RollupOK bool
}

// Exact reports whether SQL alone decides which lines match, with the
// stored line and stream labels unchanged.
func (p *Plan) Exact() bool { return len(p.Stages) == 0 }

// WhereSQL returns the predicate text and arguments.
func (p *Plan) WhereSQL() (string, []any) { return p.Where.sql, p.Where.args }

// PlanSelector decides what can be pushed into SQL:
//
//   - matchers are always exact;
//   - the leading run of line filters and stream-label filters (before
//     any parser or label-changing stage) is exact and leaves Go;
//   - after that, every stage stays in Go, but line filters that still
//     see the stored line (no line_format / decolorize / unpack yet) are
//     also pushed as SQL prefilters, and so are necessary conditions for
//     string equality on labels parsed by a single json / logfmt stage
//     (JSONExtractString or the literal key=value text in the body).
func PlanSelector(sel *LogSelectorExpr) *Plan {
	var where []sqlPart
	for _, m := range sel.Matchers {
		where = append(where, matcherSQL(m))
	}
	rollupOK := len(sel.Stages) == 0
	for _, m := range sel.Matchers {
		if !logschema.IsRollupLabel(m.Name) {
			rollupOK = false
		}
	}

	i := 0
	for ; i < len(sel.Stages); i++ {
		switch st := sel.Stages[i].(type) {
		case *LineFilter:
			where = append(where, lineFilterSQL(st))
			continue
		case *LabelFilterStage:
			if p, ok := streamFilterSQL(st.Filter); ok {
				where = append(where, p)
				continue
			}
		}
		break
	}
	rest := sel.Stages[i:]

	bodyIntact := true
	var parsers []Stage
	labelsTouched := false
	for _, s := range rest {
		switch st := s.(type) {
		case *LineFilter:
			if bodyIntact {
				where = append(where, lineFilterSQL(st))
			}
		case *LineFormatStage, *DecolorizeStage:
			bodyIntact = false
		case *UnpackStage:
			bodyIntact = false
			labelsTouched = true
		case *JSONStage, *LogfmtStage, *RegexpStage, *PatternStage:
			parsers = append(parsers, st)
		case *LabelFormatStage, *DropStage, *KeepStage:
			labelsTouched = true
		case *LabelFilterStage:
			if labelsTouched {
				continue
			}
			var parser Stage
			if len(parsers) == 1 {
				parser = parsers[0]
			}
			if p, ok := prefilterSQL(st.Filter, parser, len(parsers)); ok {
				where = append(where, p)
			}
		}
	}
	return &Plan{Where: and(where), Stages: rest, RollupOK: rollupOK}
}

// streamFilterSQL is exact for label filters that only read stream
// labels (before any parser): string matchers. Numeric comparisons
// have Loki's error-label semantics and stay in Go.
func streamFilterSQL(f LabelFilter) (sqlPart, bool) {
	switch n := f.(type) {
	case *StringLabelFilter:
		if strings.HasPrefix(n.Matcher.Name, "__") {
			// __error__ does not exist before a parser ran.
			if n.Matcher.Matches("") {
				return sqlPart{sql: "1"}, true
			}
			return sqlPart{sql: "0"}, true
		}
		return matcherSQL(n.Matcher), true
	case *BinaryLabelFilter:
		l, lok := streamFilterSQL(n.Left)
		r, rok := streamFilterSQL(n.Right)
		if !lok || !rok {
			return sqlPart{}, false
		}
		if n.And {
			return and([]sqlPart{l, r}), true
		}
		return or([]sqlPart{l, r}), true
	}
	return sqlPart{}, false
}

// safeLiteral: characters JSON and logfmt writers never escape, so the
// value appears verbatim in the line.
func safeLiteral(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.:/@+-", c) >= 0) {
			return false
		}
	}
	return true
}

func looksNumeric(v string) bool {
	_, err := strconv.ParseFloat(v, 64)
	return err == nil
}

// prefilterSQL derives a necessary (not sufficient) SQL condition for a
// label filter that runs after parsers. ok=false means no useful
// condition. The Go pipeline still evaluates the filter exactly.
func prefilterSQL(f LabelFilter, parser Stage, nParsers int) (sqlPart, bool) {
	switch n := f.(type) {
	case *BinaryLabelFilter:
		l, lok := prefilterSQL(n.Left, parser, nParsers)
		r, rok := prefilterSQL(n.Right, parser, nParsers)
		if n.And {
			switch {
			case lok && rok:
				return and([]sqlPart{l, r}), true
			case lok:
				return l, true
			case rok:
				return r, true
			}
			return sqlPart{}, false
		}
		if lok && rok {
			return or([]sqlPart{l, r}), true
		}
		return sqlPart{}, false
	case *StringLabelFilter:
		m := n.Matcher
		if m.Type != MatchEqual || m.Value == "" || strings.HasPrefix(m.Name, "__") {
			return sqlPart{}, false
		}
		// A parser never overwrites a present stream label (it writes
		// <name>_extracted instead), so either the stream label equals
		// the value, or it is absent and a parser produced the value.
		stream := labelExpr(m.Name)
		streamEq := sqlPart{sql: stream.sql + " = ?", args: append(append([]any{}, stream.args...), m.Value)}
		streamAbsent := sqlPart{sql: stream.sql + " = ''", args: append([]any{}, stream.args...)}
		var fromBody sqlPart
		switch st := parser.(type) {
		case *JSONStage:
			path := jsonPathFor(st, m.Name)
			if path == nil || looksNumeric(m.Value) || m.Value == "true" || m.Value == "false" {
				fromBody = sqlPart{sql: "1"}
				break
			}
			// JSONExtractString unescapes strings exactly as Go does.
			sql := "JSONExtractString(body" + strings.Repeat(", ?", len(path)) + ") = ?"
			args := make([]any, 0, len(path)+1)
			for _, p := range path {
				args = append(args, p)
			}
			fromBody = sqlPart{sql: sql, args: append(args, m.Value)}
		case *LogfmtStage:
			key := logfmtKeyFor(st, m.Name)
			if key == "" || !safeLiteral(m.Value) || !safeLiteral(key) {
				fromBody = sqlPart{sql: "1"}
				break
			}
			fromBody = or([]sqlPart{
				{sql: "position(body, ?) > 0", args: []any{key + "=" + m.Value}},
				{sql: "position(body, ?) > 0", args: []any{key + `="` + m.Value + `"`}},
			})
		default:
			if nParsers == 0 {
				return sqlPart{}, false
			}
			fromBody = sqlPart{sql: "1"}
		}
		if fromBody.sql == "1" {
			return or([]sqlPart{streamEq, streamAbsent}), true
		}
		return or([]sqlPart{streamEq, and([]sqlPart{streamAbsent, fromBody})}), true
	}
	return sqlPart{}, false
}

// jsonPathFor returns the JSON keys that produce label `name`, or nil
// when that is ambiguous (flattened names containing "_" could come
// from several paths) or involves array indexes.
func jsonPathFor(st *JSONStage, name string) []string {
	if len(st.Params) == 0 {
		if strings.Contains(name, "_") {
			return nil
		}
		return []string{name}
	}
	for _, p := range st.Params {
		if p.Label != name {
			continue
		}
		keys := make([]string, 0, len(p.path))
		for _, el := range p.path {
			if el.index >= 0 {
				return nil
			}
			keys = append(keys, el.key)
		}
		return keys
	}
	return nil
}

func logfmtKeyFor(st *LogfmtStage, name string) string {
	if len(st.Params) == 0 {
		if strings.Contains(name, "_") {
			return "" // could be a sanitised key like a.b or a-b
		}
		return name
	}
	for _, p := range st.Params {
		if p.Label == name {
			return p.Expr
		}
	}
	return ""
}
