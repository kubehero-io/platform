// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
)

// Query is a parsed cost / network / events / anomaly expression:
//
//	metric [ '{' label op "value" (',' …)* '}' ] [ '[' duration ']' ] [ by '(' label, … ')' ]
//
// with op one of = != =~ !~ (regexes are fully anchored, RE2), e.g.
//
//	cost{namespace="ml-inference"}
//	cost by (team)
//	network{namespace="edge", egress="true"} by (workload)
//	events{kind="oom_killed", namespace="prod"}[10m] by (workload)
//	anomaly{kind="spend"}
type Query struct {
	Metric   string
	Matchers []Matcher
	Range    time.Duration // 0 = the metric's default
	By       []string
}

// Matcher is one label constraint.
type Matcher struct {
	Label string
	Op    chsql.MatchOp
	Value string
}

// Metric names per rule kind.
var kindMetric = map[string]string{
	KindCost:    "cost",
	KindNetwork: "network",
	KindEvent:   "events",
	KindAnomaly: "anomaly",
}

// labelSets whitelists the labels each metric can filter / group on.
var labelSets = map[string]map[string]bool{
	"cost": setOf("cluster", "namespace", "workload", "workload_kind", "team", "cost_center", "nodepool",
		"zone", "region", "cloud", "node", "pod", "lifecycle"),
	"network": setOf("cluster", "namespace", "workload", "zone", "dst_namespace", "dst_workload", "dst_kind",
		"egress", "cross_zone"),
	"events":  setOf("kind", "cluster", "namespace", "workload", "pod", "container", "node", "reason", "severity"),
	"anomaly": setOf("kind", "cluster", "namespace", "workload", "severity"),
}

func setOf(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

const (
	maxQueryLen  = 4096
	maxMatchers  = 20
	maxGroupBy   = 8
	maxRange     = 24 * time.Hour
	defaultRange = 15 * time.Minute // cost / network rate window
	eventsRange  = 10 * time.Minute
)

// ParseQuery parses and validates q for the rule kind.
func ParseQuery(kind, q string) (*Query, error) {
	metric, ok := kindMetric[kind]
	if !ok {
		return nil, fmt.Errorf("kind %q has no expression grammar", kind)
	}
	if len(q) > maxQueryLen {
		return nil, fmt.Errorf("query longer than %d bytes", maxQueryLen)
	}
	p := &parser{src: q}
	out, err := p.parse()
	if err != nil {
		return nil, err
	}
	if out.Metric != metric {
		return nil, fmt.Errorf("kind %s expects a %s{…} expression, got %q", kind, metric, out.Metric)
	}
	allowed := labelSets[metric]
	for _, m := range out.Matchers {
		if !allowed[m.Label] {
			return nil, fmt.Errorf("label %q is not available on %s (use one of %s)", m.Label, metric, keysOf(allowed))
		}
		if (m.Label == "egress" || m.Label == "cross_zone") && metric == "network" {
			if m.Op != chsql.OpEq && m.Op != chsql.OpNeq {
				return nil, fmt.Errorf("%s supports = and != only", m.Label)
			}
			if _, err := parseBool(m.Value); err != nil {
				return nil, fmt.Errorf("%s must be true or false", m.Label)
			}
		}
		if m.Op == chsql.OpRe || m.Op == chsql.OpNre {
			if _, err := regexp.Compile("^(?:" + m.Value + ")$"); err != nil {
				return nil, fmt.Errorf("label %s: invalid regex: %v", m.Label, err)
			}
		}
	}
	for _, g := range out.By {
		if !allowed[g] {
			return nil, fmt.Errorf("cannot group %s by %q (use one of %s)", metric, g, keysOf(allowed))
		}
	}
	if metric == "anomaly" && (len(out.By) > 0 || out.Range > 0) {
		return nil, errors.New("anomaly{…} yields one series per anomaly: no range or by (…)")
	}
	if out.Range > maxRange {
		return nil, fmt.Errorf("range longer than %s", maxRange)
	}
	if out.Range == 0 {
		switch metric {
		case "events":
			out.Range = eventsRange
		case "cost", "network":
			out.Range = defaultRange
		}
	}
	return out, nil
}

func keysOf(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sortStrings(ks)
	return strings.Join(ks, ", ")
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean: %q", s)
}

// ─── lexer / parser ──────────────────────────────────────────────────

type parser struct {
	src string
	pos int
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("at position %d: %s", p.pos+1, fmt.Sprintf(format, args...))
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) && strings.IndexByte(" \t\r\n", p.src[p.pos]) >= 0 {
		p.pos++
	}
}

func (p *parser) peek() byte {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) expect(c byte) error {
	if p.peek() != c {
		return p.errorf("expected %q, got %s", c, p.describe())
	}
	p.pos++
	return nil
}

func (p *parser) describe() string {
	if p.pos >= len(p.src) {
		return "end of query"
	}
	end := min(p.pos+10, len(p.src))
	return strconv.Quote(p.src[p.pos:end])
}

func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isIdent(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

func (p *parser) ident() (string, error) {
	p.skipSpace()
	start := p.pos
	if p.pos >= len(p.src) || !isIdentStart(p.src[p.pos]) {
		return "", p.errorf("expected a name, got %s", p.describe())
	}
	for p.pos < len(p.src) && isIdent(p.src[p.pos]) {
		p.pos++
	}
	return p.src[start:p.pos], nil
}

func (p *parser) str() (string, error) {
	if p.peek() != '"' {
		return "", p.errorf("expected a double-quoted value, got %s", p.describe())
	}
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '"':
			p.pos++
			return sb.String(), nil
		case '\\':
			if p.pos+1 >= len(p.src) {
				return "", p.errorf("unterminated escape")
			}
			p.pos++
			switch e := p.src[p.pos]; e {
			case '"', '\\':
				sb.WriteByte(e)
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			default:
				// Keep regex escapes like \d intact.
				sb.WriteByte('\\')
				sb.WriteByte(e)
			}
		default:
			sb.WriteByte(c)
		}
		p.pos++
	}
	return "", p.errorf("unterminated string")
}

var durRE = regexp.MustCompile(`^(\d+)(s|m|h|d)$`)

func (p *parser) duration() (time.Duration, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != ']' {
		p.pos++
	}
	raw := strings.TrimSpace(p.src[start:p.pos])
	m := durRE.FindStringSubmatch(raw)
	if m == nil {
		p.pos = start
		return 0, p.errorf("expected a duration like 5m, 1h or 1d, got %q", raw)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	d := time.Duration(n) * unit
	if d <= 0 {
		return 0, p.errorf("range must be positive")
	}
	return d, nil
}

func (p *parser) parse() (*Query, error) {
	q := &Query{}
	var err error
	if q.Metric, err = p.ident(); err != nil {
		return nil, err
	}
	if p.peek() == '{' {
		p.pos++
		for p.peek() != '}' {
			if len(q.Matchers) > 0 {
				if err := p.expect(','); err != nil {
					return nil, err
				}
				if p.peek() == '}' { // trailing comma
					break
				}
			}
			var m Matcher
			if m.Label, err = p.ident(); err != nil {
				return nil, err
			}
			p.skipSpace()
			switch {
			case strings.HasPrefix(p.src[p.pos:], "=~"):
				m.Op, p.pos = chsql.OpRe, p.pos+2
			case strings.HasPrefix(p.src[p.pos:], "!~"):
				m.Op, p.pos = chsql.OpNre, p.pos+2
			case strings.HasPrefix(p.src[p.pos:], "!="):
				m.Op, p.pos = chsql.OpNeq, p.pos+2
			case strings.HasPrefix(p.src[p.pos:], "="):
				m.Op, p.pos = chsql.OpEq, p.pos+1
			default:
				return nil, p.errorf("expected =, !=, =~ or !~ after %q, got %s", m.Label, p.describe())
			}
			if m.Value, err = p.str(); err != nil {
				return nil, err
			}
			if len(m.Value) > 512 {
				return nil, p.errorf("value for %q longer than 512 bytes", m.Label)
			}
			q.Matchers = append(q.Matchers, m)
			if len(q.Matchers) > maxMatchers {
				return nil, p.errorf("at most %d matchers", maxMatchers)
			}
		}
		if err := p.expect('}'); err != nil {
			return nil, err
		}
	}
	if p.peek() == '[' {
		p.pos++
		if q.Range, err = p.duration(); err != nil {
			return nil, err
		}
		if err := p.expect(']'); err != nil {
			return nil, err
		}
	}
	p.skipSpace()
	if p.pos < len(p.src) {
		kw, err := p.ident()
		if err != nil || kw != "by" {
			return nil, p.errorf("expected by (…) or end of query")
		}
		if err := p.expect('('); err != nil {
			return nil, err
		}
		for p.peek() != ')' {
			if len(q.By) > 0 {
				if err := p.expect(','); err != nil {
					return nil, err
				}
			}
			l, err := p.ident()
			if err != nil {
				return nil, err
			}
			q.By = append(q.By, l)
			if len(q.By) > maxGroupBy {
				return nil, p.errorf("at most %d grouping labels", maxGroupBy)
			}
		}
		if err := p.expect(')'); err != nil {
			return nil, err
		}
	}
	if p.peek() != 0 {
		return nil, p.errorf("unexpected %s", p.describe())
	}
	return q, nil
}

// BudgetQuery is a kind=budget query: a BudgetPolicy name ("*" = every
// BudgetPolicy) and the burn-rate window, e.g. "prod-monthly[1h]".
type BudgetQuery struct {
	Policy string
	Window time.Duration
}

var policyNameRE = regexp.MustCompile(`^(\*|[a-z0-9]([-a-z0-9.]*[a-z0-9])?)$`)

// DefaultBudgetWindow is the burn-rate window when none is given.
const DefaultBudgetWindow = time.Hour

// ParseBudgetQuery parses "name", "name[30m]", "*", "*[1h]".
func ParseBudgetQuery(q string) (*BudgetQuery, error) {
	q = strings.TrimSpace(q)
	out := &BudgetQuery{Window: DefaultBudgetWindow}
	if i := strings.IndexByte(q, '['); i >= 0 {
		if !strings.HasSuffix(q, "]") {
			return nil, errors.New("budget window must look like name[1h]")
		}
		p := &parser{src: q[i+1 : len(q)-1]}
		d, err := p.duration()
		if err != nil {
			return nil, fmt.Errorf("budget window: %w", err)
		}
		if d < 5*time.Minute || d > 7*24*time.Hour {
			return nil, errors.New("budget window must be between 5m and 7d")
		}
		out.Window = d
		q = strings.TrimSpace(q[:i])
	}
	if len(q) > 253 || !policyNameRE.MatchString(q) {
		return nil, fmt.Errorf("budget query must be a BudgetPolicy name (or *), got %q", q)
	}
	out.Policy = q
	return out, nil
}
