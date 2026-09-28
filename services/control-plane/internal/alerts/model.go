// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package alerts is one alerting engine over every signal KubeHero
// stores: log metrics (LogQL), spend rate, budget burn, anomalies,
// network spend and cluster events.
//
// A rule's query is evaluated at its interval into labelled series;
// each series whose value satisfies `op threshold` becomes an alert
// that moves through
//
//	inactive → pending (condition true) → firing (held for pending_for)
//	         → resolved (condition false; kept 24h for the UI)
//
// Notifications go out on firing and on resolve (only if the firing
// was delivered), are repeated every 4h while an alert keeps firing,
// and are suppressed — state still advances — while a silence matches.
// Alertmanager channels additionally receive every evaluation of a
// firing alert, because Alertmanager expires alerts that stop being
// re-sent. Rules without channels are UI-only.
//
// Rules, alert state, silences and the notification log live in
// Postgres (migration 0003_alerts); without Postgres they are kept in
// memory and lost on restart (logged loudly at startup).
package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Rule kinds.
const (
	KindLogs    = "logs"
	KindCost    = "cost"
	KindBudget  = "budget"
	KindAnomaly = "anomaly"
	KindNetwork = "network"
	KindEvent   = "event"
)

var kinds = setOf(KindLogs, KindCost, KindBudget, KindAnomaly, KindNetwork, KindEvent)

// Alert states.
const (
	StatePending  = "pending"
	StateFiring   = "firing"
	StateResolved = "resolved"
)

// Tunables of the state machine.
const (
	ResolvedRetention = 24 * time.Hour
	RenotifyInterval  = 4 * time.Hour
	DefaultInterval   = time.Minute
	MinInterval       = 15 * time.Second
	MaxInterval       = time.Hour
	MaxPendingFor     = 24 * time.Hour
	MaxSeries         = 500
	maxRules          = 1000
)

// Rule is an alert rule.
type Rule struct {
	ID           string
	Name         string
	Description  string
	Kind         string
	Query        string
	Op           string
	Threshold    float64
	PendingFor   time.Duration
	Severity     string
	Channels     []string
	Labels       map[string]string
	Annotations  map[string]string
	Enabled      bool
	EvalInterval time.Duration
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CreatedBy    string
}

// Clone deep-copies a rule.
func (r *Rule) Clone() *Rule {
	c := *r
	c.Channels = append([]string(nil), r.Channels...)
	c.Labels = cloneMap(r.Labels)
	c.Annotations = cloneMap(r.Annotations)
	return &c
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var (
	// Names become the alertname label and appear in notifications:
	// allow readable punctuation, never quotes, braces or backslashes.
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.:\-/$%(),+<>=×]{0,127}$`)
	labelKeyRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)
	ops        = setOf(">", ">=", "<", "<=", "==", "!=")
	severities = setOf("info", "warn", "critical")
)

// ChannelValidator checks a channel string (alerter.Router.Validate).
type ChannelValidator interface {
	Validate(channel string) error
}

// Normalize fills defaults and validates everything except the query
// (whose check needs the evaluation sources; see Engine.ValidateQuery).
func (r *Rule) Normalize(channels ChannelValidator) error {
	r.Name = strings.TrimSpace(r.Name)
	if !nameRE.MatchString(r.Name) {
		return errors.New("name must be 1–128 characters: letters, digits, spaces and _ . : - / $ % ( ) , + < > = ×")
	}
	if len(r.Description) > 2048 {
		return errors.New("description longer than 2048 bytes")
	}
	r.Kind = strings.ToLower(strings.TrimSpace(r.Kind))
	if !kinds[r.Kind] {
		return fmt.Errorf("kind must be one of logs, cost, budget, anomaly, network, event; got %q", r.Kind)
	}
	r.Query = strings.TrimSpace(r.Query)
	if r.Query == "" || len(r.Query) > maxQueryLen {
		return fmt.Errorf("query must be 1–%d bytes", maxQueryLen)
	}
	if !ops[r.Op] {
		return fmt.Errorf("op must be one of > >= < <= == !=; got %q", r.Op)
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
		return errors.New("threshold must be a finite number")
	}
	if r.PendingFor < 0 || r.PendingFor > MaxPendingFor {
		return fmt.Errorf("pending_for must be within 0..%s", MaxPendingFor)
	}
	if r.EvalInterval == 0 {
		r.EvalInterval = DefaultInterval
	}
	if r.EvalInterval < MinInterval || r.EvalInterval > MaxInterval {
		return fmt.Errorf("eval_interval must be within %s..%s", MinInterval, MaxInterval)
	}
	r.Severity = strings.ToLower(strings.TrimSpace(r.Severity))
	if r.Severity == "" {
		r.Severity = "warn"
	}
	if !severities[r.Severity] {
		return fmt.Errorf("severity must be info, warn or critical; got %q", r.Severity)
	}
	if len(r.Channels) > 10 {
		return errors.New("at most 10 channels")
	}
	var chans []string
	for _, c := range r.Channels {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if channels != nil {
			if err := channels.Validate(c); err != nil {
				return err
			}
		}
		chans = append(chans, c)
	}
	r.Channels = chans
	if len(r.Labels) > 20 {
		return errors.New("at most 20 labels")
	}
	for k, v := range r.Labels {
		if !labelKeyRE.MatchString(k) || k == "alertname" || len(v) > 256 {
			return fmt.Errorf("invalid label %q (names [a-zA-Z_][a-zA-Z0-9_]*, not alertname; values ≤ 256 bytes)", k)
		}
	}
	if len(r.Annotations) > 10 {
		return errors.New("at most 10 annotations")
	}
	for k, v := range r.Annotations {
		if !labelKeyRE.MatchString(k) || len(v) > 4096 {
			return fmt.Errorf("invalid annotation %q (names [a-zA-Z_][a-zA-Z0-9_]*, values ≤ 4096 bytes)", k)
		}
	}
	return nil
}

// ParseDuration reads "5m", "90s", "1h", "1d" ("" = 0).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return d, nil
}

// FormatDuration renders a duration the way users write it.
func FormatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// Compare applies op.
func Compare(v float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return v > threshold
	case ">=":
		return v >= threshold
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	case "==":
		return v == threshold
	case "!=":
		return v != threshold
	}
	return false
}

// Sample is one evaluated series.
type Sample struct {
	Labels map[string]string
	Value  float64
	Link   string // optional deep link (anomalies)
}

// Alert is the state of one (rule, series).
type Alert struct {
	ID             string
	RuleID         string
	Fingerprint    string
	Labels         map[string]string // series labels + rule labels + alertname + severity
	State          string
	Value          float64
	Summary        string
	Description    string
	StartedAt      time.Time
	FiredAt        time.Time
	ResolvedAt     time.Time
	LastEvalAt     time.Time
	LastNotifiedAt time.Time // last firing notification delivered to chat channels
	LinkPath       string
	// Silenced is computed at evaluation/list time; not persisted.
	Silenced bool
}

// Fingerprint identifies a series by its labels.
func Fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(labels[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// AlertID is stable per (rule, series).
func AlertID(ruleID, fingerprint string) string {
	sum := sha256.Sum256([]byte(ruleID + "|" + fingerprint))
	return "alrt-" + hex.EncodeToString(sum[:8])
}

// Silence suppresses notifications for matching alerts.
type Silence struct {
	ID        string
	Matchers  map[string]string
	StartsAt  time.Time
	EndsAt    time.Time
	CreatedBy string
	Comment   string
	CreatedAt time.Time
}

// Active reports whether the silence applies at t.
func (s *Silence) Active(t time.Time) bool { return !t.Before(s.StartsAt) && t.Before(s.EndsAt) }

// Matches reports whether every matcher equals the alert's label; the
// alert labels carry alertname = rule name.
func (s *Silence) Matches(labels map[string]string) bool {
	if len(s.Matchers) == 0 {
		return false
	}
	for k, v := range s.Matchers {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Validate checks a silence before it is stored.
func (s *Silence) Validate(now time.Time) error {
	if len(s.Matchers) == 0 || len(s.Matchers) > 20 {
		return errors.New("a silence needs 1–20 matchers")
	}
	for k, v := range s.Matchers {
		if !labelKeyRE.MatchString(k) || v == "" || len(v) > 256 {
			return fmt.Errorf("invalid matcher %q", k)
		}
	}
	if s.StartsAt.IsZero() {
		s.StartsAt = now
	}
	if !s.EndsAt.After(s.StartsAt) {
		return errors.New("ends_at must be after starts_at")
	}
	if s.EndsAt.Sub(s.StartsAt) > 90*24*time.Hour {
		return errors.New("a silence can last at most 90 days")
	}
	if !s.EndsAt.After(now) {
		return errors.New("ends_at is in the past")
	}
	if len(s.Comment) > 1024 {
		return errors.New("comment longer than 1024 bytes")
	}
	return nil
}

// ─── annotation templates ────────────────────────────────────────────

var (
	tmplRE   = regexp.MustCompile(`\{\{\s*(\$value|\$labels\.([a-zA-Z_][a-zA-Z0-9_]*))\s*(?:\|\s*printf\s+"([^"]*)"\s*)?\}\}`)
	printfRE = regexp.MustCompile(`^%[-+ #0]?\d{0,2}(\.\d{1,2})?[fgeGEdsv]$`)
)

// Render expands {{ $value }}, {{ $labels.name }} and
// {{ $value | printf "%.2f" }} — deliberately not text/template: rule
// text is user input and must not be able to call functions.
func Render(tmpl string, value float64, labels map[string]string) string {
	return tmplRE.ReplaceAllStringFunc(tmpl, func(m string) string {
		sub := tmplRE.FindStringSubmatch(m)
		format := sub[3]
		if sub[1] == "$value" {
			if format != "" && printfRE.MatchString(format) {
				if strings.HasSuffix(format, "d") {
					return fmt.Sprintf(format, int64(math.Round(value)))
				}
				if strings.HasSuffix(format, "s") || strings.HasSuffix(format, "v") {
					return fmt.Sprintf(format, formatValue(value))
				}
				return fmt.Sprintf(format, value)
			}
			return formatValue(value)
		}
		return labels[sub[2]]
	})
}

func formatValue(v float64) string {
	if math.Abs(v) >= 1e6 || (v != 0 && math.Abs(v) < 1e-3) {
		return strconv.FormatFloat(v, 'g', 4, 64)
	}
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

func sortStrings(s []string) { sort.Strings(s) }
