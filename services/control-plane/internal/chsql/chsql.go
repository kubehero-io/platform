// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package chsql is the one place the query engines assemble ClickHouse
// SQL. The rule it enforces: user input only ever reaches the server as
// a bound parameter (clickhouse-go escapes positional `?` values,
// including slices expanded for IN lists and map keys). Column and
// expression text comes from compile-time whitelists; Ident panics if a
// non-identifier ever slips into that path, so a future bug fails
// loudly in tests instead of becoming an injection.
package chsql

import (
	"fmt"
	"regexp"
	"strings"
)

var identRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Ident returns s if it is a plain lower-case identifier, else panics.
// Only call it with whitelisted constants.
func Ident(s string) string {
	if !identRE.MatchString(s) {
		panic(fmt.Sprintf("chsql: %q is not a safe identifier", s))
	}
	return s
}

// Where accumulates AND-ed conditions and their parameters in order.
type Where struct {
	conds []string
	args  []any
}

// Add appends a condition containing exactly as many `?` placeholders
// as args.
func (w *Where) Add(cond string, args ...any) {
	if n := strings.Count(cond, "?"); n != len(args) {
		panic(fmt.Sprintf("chsql: condition %q has %d placeholders, %d args", cond, n, len(args)))
	}
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

// In appends `expr IN (?)` for a non-empty value list (no-op when
// empty — "no filter", never "match nothing").
func (w *Where) In(expr string, values []string) {
	if len(values) == 0 {
		return
	}
	if len(values) == 1 {
		w.Add(expr+" = ?", values[0])
		return
	}
	w.Add(expr+" IN (?)", values)
}

// SQL renders "cond1 AND cond2 …" ("1" when empty, so it can always
// follow WHERE).
func (w *Where) SQL() string {
	if len(w.conds) == 0 {
		return "1"
	}
	return strings.Join(w.conds, " AND ")
}

// Args returns the parameters in placeholder order.
func (w *Where) Args() []any { return append([]any(nil), w.args...) }

// Clone copies the builder so a base filter can be extended per query.
func (w *Where) Clone() *Where {
	return &Where{conds: append([]string(nil), w.conds...), args: append([]any(nil), w.args...)}
}

// MatchOp is a Prometheus-style label matcher operator.
type MatchOp string

const (
	OpEq  MatchOp = "="
	OpNeq MatchOp = "!="
	OpRe  MatchOp = "=~"
	OpNre MatchOp = "!~"
)

// Matcher compiles one label matcher against a whitelisted column
// expression. Regexes are anchored like PromQL's (ClickHouse match()
// is unanchored) and validated as RE2 — the dialect match() speaks —
// before they're sent.
func (w *Where) Matcher(expr string, op MatchOp, value string) error {
	switch op {
	case OpEq:
		w.Add(expr+" = ?", value)
	case OpNeq:
		w.Add(expr+" != ?", value)
	case OpRe, OpNre:
		anchored := "^(?:" + value + ")$"
		if _, err := regexp.Compile(anchored); err != nil {
			return fmt.Errorf("invalid regex %q: %w", value, err)
		}
		if op == OpRe {
			w.Add("match("+expr+", ?)", anchored)
		} else {
			w.Add("NOT match("+expr+", ?)", anchored)
		}
	default:
		return fmt.Errorf("unknown matcher operator %q", op)
	}
	return nil
}

// labelKeyRE is the Kubernetes label-key grammar (optional DNS prefix
// + name), which is also what pod_metadata.labels keys hold.
var labelKeyRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// ValidLabelKey reports whether k is a syntactically valid Kubernetes
// label key (≤ 317 bytes: 253 prefix + "/" + 63 name).
func ValidLabelKey(k string) bool {
	if k == "" || len(k) > 317 {
		return false
	}
	if i := strings.LastIndexByte(k, '/'); i >= 0 && len(k)-i-1 > 63 {
		return false
	}
	return labelKeyRE.MatchString(k)
}
