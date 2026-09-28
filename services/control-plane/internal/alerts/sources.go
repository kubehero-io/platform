// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/budget"
	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/cost"
	"github.com/kubehero-io/platform/services/control-plane/internal/insights"
	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

// Sources turns a rule into samples.
type Sources interface {
	Evaluate(ctx context.Context, r *Rule, now time.Time) ([]Sample, error)
	ValidateQuery(ctx context.Context, kind, query string) error
}

// BurnRater is clickhouse.BurnRateProvider's surface.
type BurnRater interface {
	Compute(ctx context.Context, clusterID, namespace, window string, monthlyCeilingUSD float64) (clickhouse.Reading, error)
}

// SpendDetector is clickhouse.SpendAnomalyProvider's surface.
type SpendDetector interface {
	Detect(ctx context.Context, baseline time.Duration) ([]clickhouse.SpendAnomaly, error)
	EffectiveThreshold() float64
}

// StoreSources evaluates rules against ClickHouse, the LogQL engine,
// the anomaly detectors and the Postgres policy mirror. Any of them may
// be nil; rules that need a missing one evaluate to an error.
type StoreSources struct {
	CH       *sql.DB
	Logs     signals.LogMetricQuerier
	Policies store.PolicyLister
	BurnRate BurnRater
	Spend    SpendDetector
	Insights *insights.Engine
	Clusters *clusters.Resolver
	// Lag keeps the newest, possibly still-arriving seconds out of rate
	// windows (default 30s).
	Lag     time.Duration
	Timeout time.Duration
}

// ErrNoClickHouse: the rule needs ClickHouse, which is not configured.
var ErrNoClickHouse = errors.New("ClickHouse is not configured")

func (s *StoreSources) lag() time.Duration {
	if s.Lag > 0 {
		return s.Lag
	}
	return 30 * time.Second
}

func (s *StoreSources) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 30 * time.Second
}

// ValidateQuery checks a query without side effects. LogQL is checked
// by running it over the last minute when the logs engine is present.
func (s *StoreSources) ValidateQuery(ctx context.Context, kind, query string) error {
	switch kind {
	case KindLogs:
		if s.Logs == nil {
			return nil // accepted; evaluation reports the missing engine
		}
		qctx, cancel := context.WithTimeout(ctx, s.timeout())
		defer cancel()
		now := time.Now()
		if _, err := s.Logs.QueryMetric(qctx, query, now.Add(-time.Minute), now, time.Minute); err != nil {
			return fmt.Errorf("LogQL: %w", err)
		}
		return nil
	case KindBudget:
		_, err := ParseBudgetQuery(query)
		return err
	}
	_, err := ParseQuery(kind, query)
	return err
}

// Evaluate runs one rule's query at now.
func (s *StoreSources) Evaluate(ctx context.Context, r *Rule, now time.Time) ([]Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	switch r.Kind {
	case KindLogs:
		return s.logs(ctx, r.Query, now)
	case KindBudget:
		bq, err := ParseBudgetQuery(r.Query)
		if err != nil {
			return nil, err
		}
		return s.budgets(ctx, bq, now)
	}
	q, err := ParseQuery(r.Kind, r.Query)
	if err != nil {
		return nil, err
	}
	if q.Metric == "anomaly" {
		return s.anomalies(ctx, q, now)
	}
	if s.CH == nil {
		return nil, ErrNoClickHouse
	}
	switch q.Metric {
	case "cost":
		return s.cost(ctx, q, now)
	case "network":
		return s.network(ctx, q, now)
	case "events":
		return s.events(ctx, q, now)
	}
	return nil, fmt.Errorf("unsupported metric %q", q.Metric)
}

func (s *StoreSources) logs(ctx context.Context, query string, now time.Time) ([]Sample, error) {
	if s.Logs == nil {
		return nil, errors.New("the logs engine is unavailable (ClickHouse not configured)")
	}
	series, err := s.Logs.QueryMetric(ctx, query, now.Add(-time.Minute), now, time.Minute)
	if err != nil {
		return nil, err
	}
	link := "/logs?query=" + url.QueryEscape(query)
	out := make([]Sample, 0, len(series))
	for _, sr := range series {
		v, ok := sr.Last()
		if !ok || math.IsNaN(v) {
			continue
		}
		out = append(out, Sample{Labels: cloneMap(sr.Labels), Value: v, Link: link})
	}
	return out, nil
}

// columnMaps map query labels onto table columns (all whitelisted).
var (
	costColumns = map[string]string{"cluster": "cluster_id", "namespace": "namespace", "workload": "workload",
		"workload_kind": "workload_kind", "team": "team", "cost_center": "cost_center", "nodepool": "nodepool",
		"zone": "zone", "region": "region", "cloud": "cloud", "node": "node", "pod": "pod", "lifecycle": "lifecycle"}
	networkColumns = map[string]string{"cluster": "cluster_id", "namespace": "src_namespace", "workload": "src_workload",
		"zone": "src_zone", "dst_namespace": "dst_namespace", "dst_workload": "dst_workload", "dst_kind": "dst_kind",
		"egress": "egress", "cross_zone": "cross_zone"}
	eventColumns = map[string]string{"kind": "kind", "cluster": "cluster_id", "namespace": "namespace",
		"workload": "workload", "pod": "pod", "container": "container", "node": "node", "reason": "reason",
		"severity": "severity"}
)

// where compiles matchers; cluster equality expands to every alias of
// the named cluster, booleans compare as 0/1.
func (s *StoreSources) where(ctx context.Context, w *chsql.Where, cols map[string]string, ms []Matcher) error {
	snap := s.Clusters.Snapshot(ctx)
	for _, m := range ms {
		col := chsql.Ident(cols[m.Label])
		switch {
		case m.Label == "cluster" && (m.Op == chsql.OpEq || m.Op == chsql.OpNeq):
			al := snap.Aliases(m.Value)
			if m.Op == chsql.OpEq {
				w.Add(col+" IN (?)", al)
			} else {
				w.Add(col+" NOT IN (?)", al)
			}
		case col == "egress" || col == "cross_zone":
			b, _ := parseBool(m.Value)
			v := uint8(0)
			if b {
				v = 1
			}
			if m.Op == chsql.OpEq {
				w.Add(col+" = ?", v)
			} else {
				w.Add(col+" != ?", v)
			}
		default:
			if err := w.Matcher(col, m.Op, m.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// groupExprs renders grouping columns as g0, g1, ….
func groupExprs(cols map[string]string, by []string) (sel []string, names []string) {
	for i, l := range by {
		col := chsql.Ident(cols[l])
		expr := col
		if col == "egress" || col == "cross_zone" {
			expr = "if(" + col + " = 1, 'true', 'false')"
		}
		sel = append(sel, fmt.Sprintf("%s AS g%d", expr, i))
		names = append(names, fmt.Sprintf("g%d", i))
	}
	return sel, names
}

// scanSamples reads (g0..gn, value) rows into samples.
func (s *StoreSources) scanSamples(ctx context.Context, query string, args []any, by []string, scale float64, link func(map[string]string) string) ([]Sample, error) {
	rows, err := s.CH.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	snap := s.Clusters.Snapshot(ctx)
	var out []Sample
	for rows.Next() {
		vals := make([]string, len(by))
		dest := make([]any, 0, len(by)+1)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var v float64
		dest = append(dest, &v)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		labels := make(map[string]string, len(by))
		for i, l := range by {
			if l == "cluster" {
				vals[i] = snap.Display(vals[i])
			}
			labels[l] = vals[i]
		}
		sm := Sample{Labels: labels, Value: v * scale}
		if link != nil {
			sm.Link = link(labels)
		}
		out = append(out, sm)
		if len(out) > MaxSeries {
			break
		}
	}
	return out, rows.Err()
}

func linkWith(path string, labels map[string]string, keys ...string) string {
	v := url.Values{}
	for _, k := range keys {
		if labels[k] != "" {
			v.Set(k, labels[k])
		}
	}
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// cost: spend rate in $/hour over the range (default 15m).
func (s *StoreSources) cost(ctx context.Context, q *Query, now time.Time) ([]Sample, error) {
	end := now.Add(-s.lag())
	start := end.Add(-q.Range)
	var w chsql.Where
	w.Add("ts >= ? AND ts < ?", start.UnixMilli(), end.UnixMilli())
	if err := s.where(ctx, &w, costColumns, q.Matchers); err != nil {
		return nil, err
	}
	sel, names := groupExprs(costColumns, q.By)
	query := "SELECT " + strings.Join(append(sel, "sum(cost_usd_sec * interval_sec)"), ", ") +
		" FROM pod_cost_1s WHERE " + w.SQL()
	if len(names) > 0 {
		query += " GROUP BY " + strings.Join(names, ", ")
	}
	query += fmt.Sprintf(" LIMIT %d", MaxSeries+1)
	return s.scanSamples(ctx, query, w.Args(), q.By, 3600/q.Range.Seconds(), func(l map[string]string) string {
		return linkWith("/allocation", l, "cluster", "namespace", "workload", "team")
	})
}

// network: network spend rate in $/hour over the range, each flow pair
// counted from one observation point (cost.PreferredDirectionSQL).
func (s *StoreSources) network(ctx context.Context, q *Query, now time.Time) ([]Sample, error) {
	end := now.Add(-s.lag())
	start := end.Add(-q.Range)
	var w chsql.Where
	w.Add("ts >= toDateTime(?) AND ts < toDateTime(?)", start.Unix(), end.Unix())
	if err := s.where(ctx, &w, networkColumns, q.Matchers); err != nil {
		return nil, err
	}
	sel, names := groupExprs(networkColumns, q.By)
	pair := []string{"src_namespace", "src_workload", "dst_kind", "dst_namespace", "dst_workload", "dst_service", "dst_name"}
	inner := "SELECT " + strings.Join(append(append([]string{}, sel...),
		cost.PreferredDirectionSQL+" AS use_in",
		"sumIf(cost_usd, direction = 'ingress') AS c_in",
		"sumIf(cost_usd, direction != 'ingress') AS c_eg"), ", ") +
		" FROM net_flows WHERE " + w.SQL() +
		" GROUP BY " + strings.Join(append(append([]string{}, names...), pair...), ", ")
	query := "SELECT " + strings.Join(append(append([]string{}, names...), "sum(if(use_in, c_in, c_eg))"), ", ") +
		" FROM (" + inner + ")"
	if len(names) > 0 {
		query += " GROUP BY " + strings.Join(names, ", ")
	}
	query += fmt.Sprintf(" LIMIT %d", MaxSeries+1)
	return s.scanSamples(ctx, query, w.Args(), q.By, 3600/q.Range.Seconds(), func(l map[string]string) string {
		return linkWith("/network", l, "cluster", "namespace")
	})
}

// events: count of cluster events in the range (default 10m).
func (s *StoreSources) events(ctx context.Context, q *Query, now time.Time) ([]Sample, error) {
	end := now.Add(-s.lag())
	start := end.Add(-q.Range)
	var w chsql.Where
	w.Add("ts >= fromUnixTimestamp64Milli(toInt64(?)) AND ts < fromUnixTimestamp64Milli(toInt64(?))", start.UnixMilli(), end.UnixMilli())
	if err := s.where(ctx, &w, eventColumns, q.Matchers); err != nil {
		return nil, err
	}
	sel, names := groupExprs(eventColumns, q.By)
	query := "SELECT " + strings.Join(append(sel, "toFloat64(sum(greatest(count, 1)))"), ", ") +
		" FROM cluster_events WHERE " + w.SQL()
	if len(names) > 0 {
		query += " GROUP BY " + strings.Join(names, ", ")
	}
	query += fmt.Sprintf(" LIMIT %d", MaxSeries+1)
	return s.scanSamples(ctx, query, w.Args(), q.By, 1, func(l map[string]string) string {
		if l["namespace"] != "" && l["workload"] != "" && l["cluster"] != "" {
			return fmt.Sprintf("/workloads/%s/%s/%s", l["cluster"], l["namespace"], l["workload"])
		}
		return "/overview"
	})
}

// anomalies: one series per detected anomaly, value = $/mo impact
// (spend) or $/mo exposure (capacity, logs).
func (s *StoreSources) anomalies(ctx context.Context, q *Query, now time.Time) ([]Sample, error) {
	if s.Spend == nil && s.Insights == nil {
		return nil, ErrNoClickHouse
	}
	var all []Sample
	add := func(kind, cluster, ns, wl, sev string, v float64, link string) {
		all = append(all, Sample{Labels: map[string]string{"kind": kind, "cluster": cluster, "namespace": ns,
			"workload": wl, "severity": sev}, Value: v, Link: link})
	}
	wlLink := func(c, ns, wl string) string { return fmt.Sprintf("/workloads/%s/%s/%s", c, ns, wl) }
	if s.Spend != nil {
		hits, err := s.Spend.Detect(ctx, 24*time.Hour)
		if err != nil {
			return nil, err
		}
		th := s.Spend.EffectiveThreshold()
		snap := s.Clusters.Snapshot(ctx)
		for _, h := range hits {
			sev := "warn"
			switch {
			case h.Z >= 2*th:
				sev = "critical"
			case h.Z < 0:
				sev = "info"
			}
			c := snap.Display(h.ClusterID)
			add("spend", c, h.Namespace, h.Workload, sev, math.Abs(h.ImpactUSDMonth), wlLink(c, h.Namespace, h.Workload))
		}
	}
	if s.Insights != nil {
		bursts, err := s.Insights.OOMBursts(ctx, now, insights.DefaultOOMBurst)
		if err != nil {
			return nil, err
		}
		for _, b := range bursts {
			sev := "warn"
			if b.Kills >= 10 {
				sev = "critical"
			}
			add("capacity", b.Cluster, b.Namespace, b.Workload, sev, b.ExposureUSDMonth, wlLink(b.Cluster, b.Namespace, b.Workload))
		}
		spikes, err := s.Insights.LogErrorSpikes(ctx, now, 24*time.Hour, 0)
		if err != nil {
			return nil, err
		}
		for _, sp := range spikes {
			sel := fmt.Sprintf(`{cluster=%q,namespace=%q,workload=%q,level=~"error|fatal|critical|panic"}`, sp.Cluster, sp.Namespace, sp.Workload)
			add("logs", sp.Cluster, sp.Namespace, sp.Workload, "warn", sp.ExposureUSDMonth, "/logs?query="+url.QueryEscape(sel))
		}
	}
	var out []Sample
	for _, sm := range all {
		if matchAll(q.Matchers, sm.Labels) {
			out = append(out, sm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out, nil
}

// matchAll evaluates matchers in Go (anchored RE2, like the SQL path).
func matchAll(ms []Matcher, labels map[string]string) bool {
	for _, m := range ms {
		v := labels[m.Label]
		switch m.Op {
		case chsql.OpEq:
			if v != m.Value {
				return false
			}
		case chsql.OpNeq:
			if v == m.Value {
				return false
			}
		case chsql.OpRe, chsql.OpNre:
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil || re.MatchString(v) != (m.Op == chsql.OpRe) {
				return false
			}
		}
	}
	return true
}

// budgets: burn-rate multiple per BudgetPolicy (1.0 = spending exactly
// at the monthly ceiling), over the query window.
func (s *StoreSources) budgets(ctx context.Context, bq *BudgetQuery, now time.Time) ([]Sample, error) {
	if s.Policies == nil {
		return nil, errors.New("budget rules need Postgres (the BudgetPolicy mirror) to be configured")
	}
	if s.CH == nil {
		return nil, ErrNoClickHouse
	}
	rows, err := s.Policies.ListAll(ctx, "")
	if err != nil {
		return nil, err
	}
	snap := s.Clusters.Snapshot(ctx)
	var out []Sample
	found := false
	for _, p := range rows {
		if p.Kind != "BudgetPolicy" || (bq.Policy != "*" && p.Name != bq.Policy) {
			continue
		}
		found = true
		spec, err := budget.ParseSpec(p.SpecJSON)
		if err != nil {
			continue
		}
		ceiling, ok := budget.ParseCeiling(spec.Ceiling)
		if !ok {
			continue
		}
		ns, _ := spec.Namespaces()
		v, err := s.burnMultiple(ctx, snap.Aliases(p.ClusterID), ns, bq.Window, ceiling, now)
		if err != nil {
			return nil, err
		}
		out = append(out, Sample{Labels: map[string]string{"policy": p.Name, "cluster": snap.Display(p.ClusterID)},
			Value: v, Link: "/budgets"})
	}
	if !found && bq.Policy != "*" {
		return nil, fmt.Errorf("BudgetPolicy %q not found", bq.Policy)
	}
	return out, nil
}

// burnMultiple: monthly-equivalent spend over the window ÷ ceiling. A
// single-namespace scope goes through clickhouse.BurnRateProvider (the
// operator's own reading); cluster-wide and multi-namespace scopes,
// which that provider can't express, use the same math directly.
func (s *StoreSources) burnMultiple(ctx context.Context, aliases, ns []string, window time.Duration, ceiling float64, now time.Time) (float64, error) {
	if len(ns) == 1 && s.BurnRate != nil {
		for _, c := range aliases {
			r, err := s.BurnRate.Compute(ctx, c, ns[0], window.String(), ceiling)
			if errors.Is(err, clickhouse.ErrUnavailable) {
				continue
			}
			if err != nil {
				return 0, err
			}
			return float64(r.BurnRateMilli) / 1000, nil
		}
		return 0, nil // no spend recorded in the window
	}
	if ns != nil && len(ns) == 0 {
		return 0, nil // selector matches no namespace
	}
	var w chsql.Where
	w.Add("ts >= ? AND ts < ?", now.Add(-window).UnixMilli(), now.UnixMilli())
	w.In("cluster_id", aliases)
	w.In("namespace", ns)
	var spent sql.NullFloat64
	if err := s.CH.QueryRowContext(ctx, "SELECT sum(cost_usd_sec * interval_sec) FROM pod_cost_1s WHERE "+w.SQL(),
		w.Args()...).Scan(&spent); err != nil {
		return 0, err
	}
	monthly := spent.Float64 / window.Seconds() * 86400 * 30
	return monthly / ceiling, nil
}
