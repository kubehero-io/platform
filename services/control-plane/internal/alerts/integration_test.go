// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package alerts

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/pgtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

func TestPGStore(t *testing.T) {
	db := pgtest.Open(t, "alerts_store")
	ctx := context.Background()
	st := &PGStore{DB: db}

	seeded, err := st.SeedDefaults(ctx, DefaultRules())
	if err != nil || !seeded {
		t.Fatalf("first seed: %v %v", seeded, err)
	}
	rules, _ := st.ListRules(ctx)
	if len(rules) != len(DefaultRules()) {
		t.Fatalf("seeded %d rules", len(rules))
	}
	for _, r := range rules {
		if err := st.DeleteRule(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if again, err := st.SeedDefaults(ctx, DefaultRules()); err != nil || again {
		t.Fatal("seeding must happen once, ever — deleted defaults stay deleted")
	}

	r, err := st.UpsertRule(ctx, &Rule{Name: "High spend", Kind: KindCost, Query: "cost by (namespace)", Op: ">", Threshold: 1.5,
		PendingFor: 90 * time.Second, Severity: "critical", Channels: []string{"slack://hooks.slack.com/x"},
		Labels: map[string]string{"team": "finops"}, Annotations: map[string]string{"summary": "{{ $value }}"},
		Enabled: true, EvalInterval: 30 * time.Second, CreatedBy: "admin@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRule(ctx, r.ID)
	if err != nil || got.PendingFor != 90*time.Second || got.EvalInterval != 30*time.Second || got.Labels["team"] != "finops" ||
		got.Channels[0] != "slack://hooks.slack.com/x" || got.CreatedBy != "admin@example.com" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	got.Threshold = 2
	upd, err := st.UpsertRule(ctx, got)
	if err != nil || upd.Threshold != 2 || !upd.UpdatedAt.After(upd.CreatedAt.Add(-time.Second)) {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if _, err := st.UpsertRule(ctx, &Rule{Name: "High spend", Kind: KindCost, Query: "cost", Op: ">", Severity: "warn", EvalInterval: time.Minute}); err != ErrNameTaken {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := st.GetRule(ctx, "not-a-uuid"); err != ErrNotFound {
		t.Fatal("bad id must be NotFound")
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	res := Step(r, nil, []Sample{{Labels: map[string]string{"namespace": "shop"}, Value: 3, Link: "/x"}}, now)
	res.Alerts[0].LastNotifiedAt = now
	if err := st.SaveAlerts(ctx, r.ID, res.Alerts, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.RuleAlerts(ctx, r.ID)
	if err != nil || len(loaded) != 1 || loaded[0].Labels["namespace"] != "shop" || !loaded[0].StartedAt.Equal(now) ||
		!loaded[0].LastNotifiedAt.Equal(now) || loaded[0].LinkPath != "/x" || !loaded[0].FiredAt.IsZero() {
		t.Fatalf("alert round trip: %+v %v", loaded, err)
	}
	if err := st.SaveAlerts(ctx, r.ID, nil, []string{loaded[0].ID}); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.ListAlerts(ctx); len(all) != 0 {
		t.Fatal("delete")
	}

	sil, err := st.CreateSilence(ctx, &Silence{Matchers: map[string]string{"alertname": "High spend"}, StartsAt: now,
		EndsAt: now.Add(time.Hour), CreatedBy: "a", Comment: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := st.ListSilences(ctx, false, now); len(active) != 1 || active[0].Matchers["alertname"] != "High spend" {
		t.Fatalf("silences: %+v", active)
	}
	if err := st.ExpireSilence(ctx, sil.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if active, _ := st.ListSilences(ctx, false, now.Add(2*time.Minute)); len(active) != 0 {
		t.Fatal("expired silence still active")
	}
	if err := st.RecordNotifications(ctx, []Notification{{At: now, AlertID: "a1", RuleID: r.ID, Event: "firing",
		Channel: "slack://hooks.slack.com/…", OK: true}}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRule(ctx, r.ID); err != nil {
		t.Fatal(err)
	}

	// Leadership: one holder at a time; released on shutdown.
	a, b := &PGLeader{DB: db}, &PGLeader{DB: db}
	if !a.IsLeader(ctx) || b.IsLeader(ctx) || !a.IsLeader(ctx) {
		t.Fatal("exactly one leader")
	}
	a.Release()
	if !b.IsLeader(ctx) {
		t.Fatal("leadership must pass on release")
	}
	b.Release()
}

type hook struct {
	mu     sync.Mutex
	bodies []alerter.WebhookPayload
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var p alerter.WebhookPayload
	_ = json.Unmarshal(b, &p)
	h.mu.Lock()
	h.bodies = append(h.bodies, p)
	h.mu.Unlock()
}

func (h *hook) take() []alerter.WebhookPayload {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.bodies
	h.bodies = nil
	return out
}

func TestRuleFiresAndResolvesEndToEnd(t *testing.T) {
	ch := chtest.Open(t, "alerts")
	pg := pgtest.Open(t, "alerts_e2e")
	ctx := context.Background()
	now := time.Now().UTC()

	// shop spends $0.001/s = $3.60/h over the last 20 minutes.
	var rows [][]any
	for ts := now.Add(-20 * time.Minute); ts.Before(now); ts = ts.Add(10 * time.Second) {
		rows = append(rows, []any{ts.UnixMilli(), "default", "c1", "n1", "shop", "api-1", "payments", "", "general", "aws",
			"us-east-1", "m7i.large", "on-demand", "", uint32(500), uint64(1 << 30), float32(0), 0.001, 0.0, float32(10),
			"api", "Deployment", "z1", uint32(0), uint64(0), 0.0006, 0.0004})
	}
	chtest.Insert(t, ch, "pod_cost_1s", []string{"ts", "org_id", "cluster_id", "node", "namespace", "pod", "team",
		"cost_center", "nodepool", "cloud", "region", "sku", "lifecycle", "gpu_kind", "cpu_millicores", "mem_bytes",
		"gpu_util_pct", "cost_usd_sec", "recoverable_usd_sec", "interval_sec", "workload", "workload_kind", "zone",
		"cpu_usage_millicores", "mem_usage_bytes", "cpu_cost_usd_sec", "ram_cost_usd_sec"}, rows)
	chtest.Insert(t, ch, "cluster_events", []string{"ts", "org_id", "cluster_id", "kind", "severity", "namespace",
		"workload", "pod", "container", "node", "reason", "message", "attributes", "count"}, [][]any{
		{now.Add(-3 * time.Minute), "default", "c1", "oom_killed", "warn", "shop", "api", "api-1", "app", "n1", "OOMKilled", "", map[string]string{}, uint32(2)},
		{now.Add(-2 * time.Minute), "default", "c1", "oom_killed", "warn", "shop", "api", "api-1", "app", "n1", "OOMKilled", "", map[string]string{}, uint32(1)},
		{now.Add(-2 * time.Minute), "default", "c1", "crash_loop", "warn", "shop", "api", "api-1", "app", "n1", "BackOff", "", map[string]string{}, uint32(1)},
	})

	h := &hook{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	channel := "webhook+http://" + strings.TrimPrefix(srv.URL, "http://") + "/hook?token=s3cret"

	st := &PGStore{DB: pg}
	src := &StoreSources{CH: ch, BurnRate: &clickhouse.BurnRateProvider{DB: ch}, Policies: &store.PoliciesPG{DB: pg}}

	// Per-kind queries against real ClickHouse.
	cost, err := src.Evaluate(ctx, &Rule{Kind: KindCost, Query: `cost{namespace="shop"} by (team)`}, now)
	if err != nil || len(cost) != 1 || cost[0].Labels["team"] != "payments" || math.Abs(cost[0].Value-3.6) > 0.01 {
		t.Fatalf("cost rate: %+v %v", cost, err)
	}
	ev, err := src.Evaluate(ctx, &Rule{Kind: KindEvent, Query: `events{kind="oom_killed"}[10m] by (namespace, workload)`}, now)
	if err != nil || len(ev) != 1 || ev[0].Value != 3 || ev[0].Link != "/overview" {
		t.Fatalf("events: %+v %v", ev, err)
	}
	none, err := src.Evaluate(ctx, &Rule{Kind: KindEvent, Query: `events{kind=~"crash.*",namespace!="shop"}`}, now)
	if err != nil || len(none) != 1 || none[0].Value != 0 {
		t.Fatalf("ungrouped empty count must be one zero series: %+v %v", none, err)
	}
	if _, err := src.Evaluate(ctx, &Rule{Kind: KindBudget, Query: "missing-policy"}, now); err == nil {
		t.Fatal("unknown BudgetPolicy must be an error")
	}

	rule, err := st.UpsertRule(ctx, &Rule{Name: "Shop spend", Kind: KindCost, Query: `cost{namespace="shop"}`, Op: ">",
		Threshold: 1, Severity: "critical", Channels: []string{channel}, Enabled: true, EvalInterval: time.Minute,
		Annotations: map[string]string{"summary": "shop at ${{ $value }}/h"}})
	if err != nil {
		t.Fatal(err)
	}
	clk := &atomicClock{t: now}
	e := &Engine{Store: st, Sources: src, Notifier: alerter.NewRouter(), Now: clk.now}

	// Evaluation 1: $3.60/h > $1 → fires (pending_for 0) → webhook.
	e.EvaluateOnce(ctx, rule)
	got := h.take()
	if len(got) != 1 || got[0].Status != "firing" || got[0].Labels["alertname"] != "Shop spend" ||
		!strings.Contains(got[0].Title, "shop at $3.6/h") || got[0].Severity != alerter.SeverityCritical {
		t.Fatalf("firing webhook: %+v", got)
	}
	alerts, _ := st.RuleAlerts(ctx, rule.ID)
	if len(alerts) != 1 || alerts[0].State != StateFiring || alerts[0].LastNotifiedAt.IsZero() {
		t.Fatalf("state after eval 1: %+v", alerts)
	}

	// Evaluation 2, an hour later: no spend in the window → resolved.
	clk.set(now.Add(time.Hour))
	e.EvaluateOnce(ctx, rule)
	got = h.take()
	if len(got) != 1 || got[0].Status != "resolved" || got[0].EndsAt == "" {
		t.Fatalf("resolve webhook: %+v", got)
	}
	alerts, _ = st.RuleAlerts(ctx, rule.ID)
	if len(alerts) != 1 || alerts[0].State != StateResolved {
		t.Fatalf("state after eval 2: %+v", alerts)
	}

	// Both deliveries are logged, the channel redacted.
	var n int
	var leaked bool
	rs, err := pg.QueryContext(ctx, `SELECT channel, event, ok FROM alert_notifications ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var events []string
	for rs.Next() {
		var c, event string
		var ok bool
		if err := rs.Scan(&c, &event, &ok); err != nil {
			t.Fatal(err)
		}
		n++
		events = append(events, event)
		leaked = leaked || strings.Contains(c, "s3cret") || !ok
	}
	if n != 2 || leaked || events[0] != "firing" || events[1] != "resolved" {
		t.Fatalf("notification log: %d rows %v leaked=%v", n, events, leaked)
	}
}
