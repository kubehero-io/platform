// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
)

// scripted returns whatever samples/err the test sets.
type scripted struct {
	mu      sync.Mutex
	samples []Sample
	err     error
	calls   int
}

func (s *scripted) set(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples, s.err = []Sample{{Labels: map[string]string{"namespace": "shop"}, Value: v, Link: "/allocation?namespace=shop"}}, nil
}

func (s *scripted) Evaluate(context.Context, *Rule, time.Time) ([]Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return append([]Sample(nil), s.samples...), s.err
}

func (s *scripted) ValidateQuery(context.Context, string, string) error { return nil }

type sent struct {
	channel string
	msg     alerter.Message
}

type recorder struct {
	mu   sync.Mutex
	sent []sent
	fail map[string]bool
}

func (r *recorder) SendAll(_ context.Context, channels []string, msg alerter.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range channels {
		r.sent = append(r.sent, sent{c, msg})
		if r.fail[c] {
			return errors.New("http 500")
		}
	}
	return nil
}

func (r *recorder) WantsHeartbeat(ch string) bool { return strings.HasPrefix(ch, "alertmanager") }

func (r *recorder) take() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.sent
	r.sent = nil
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T, pending time.Duration, channels ...string) (*Engine, *MemoryStore, *scripted, *recorder, *clock, *Rule) {
	t.Helper()
	st := NewMemoryStore()
	r, err := st.UpsertRule(context.Background(), &Rule{Name: "High spend", Kind: KindCost, Query: "cost by (namespace)",
		Op: ">", Threshold: 10, PendingFor: pending, Severity: "critical", Channels: channels, Enabled: true,
		EvalInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	src, rec := &scripted{}, &recorder{}
	c := &clock{t: t0}
	e := &Engine{Store: st, Sources: src, Notifier: rec, Now: c.now, DashboardURL: "https://kubehero.example/"}
	return e, st, src, rec, c, r
}

func channelsOf(s []sent) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.channel)
	}
	return out
}

func TestEngineLifecycleNotifications(t *testing.T) {
	const slack, am = "slack://hooks.slack.com/services/T/B/SECRET", "alertmanager+http://am:9093"
	e, st, src, rec, c, r := setup(t, 2*time.Minute, slack, am)
	ctx := context.Background()

	src.set(12)
	e.EvaluateOnce(ctx, r)
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("pending must not notify: %v", channelsOf(got))
	}

	c.t = t0.Add(2 * time.Minute)
	e.EvaluateOnce(ctx, r)
	got := rec.take()
	if len(got) != 2 {
		t.Fatalf("firing must go to both channels, got %v", channelsOf(got))
	}
	for _, s := range got {
		m := s.msg
		if m.Status != alerter.StatusFiring || !strings.HasPrefix(m.Title, "[FIRING] High spend") || m.Severity != alerter.SeverityCritical ||
			m.Labels["alertname"] != "High spend" || m.Labels["namespace"] != "shop" || !m.StartsAt.Equal(t0.Add(2*time.Minute)) ||
			!strings.HasPrefix(m.URL, "https://kubehero.example/alerts?alert=alrt-") || m.Source == "" {
			t.Fatalf("firing message: %+v", m)
		}
		if !m.EndsAt.Equal(c.t.Add(4 * time.Minute)) {
			t.Fatalf("firing endsAt = %s", m.EndsAt)
		}
	}

	// Next evaluation: Alertmanager heartbeat only.
	c.t = t0.Add(3 * time.Minute)
	e.EvaluateOnce(ctx, r)
	got = rec.take()
	if len(got) != 1 || got[0].channel != am {
		t.Fatalf("heartbeat: %v", channelsOf(got))
	}

	// 4h after the firing notification: chat re-notify.
	c.t = t0.Add(2*time.Minute + RenotifyInterval)
	e.EvaluateOnce(ctx, r)
	if got = rec.take(); len(got) != 2 {
		t.Fatalf("renotify: %v", channelsOf(got))
	}

	// Silenced: state advances, nobody is told.
	if _, err := st.CreateSilence(ctx, &Silence{Matchers: map[string]string{"alertname": "High spend"},
		StartsAt: c.t, EndsAt: c.t.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	e.EvaluateOnce(ctx, r)
	if got = rec.take(); len(got) != 0 {
		t.Fatalf("silenced alert notified: %v", channelsOf(got))
	}
	alerts, _ := st.RuleAlerts(ctx, r.ID)
	if len(alerts) != 1 || alerts[0].State != StateFiring {
		t.Fatalf("silence must not change state: %+v", alerts)
	}

	// Silence over, condition clears: resolve goes out everywhere.
	c.t = c.t.Add(2 * time.Hour)
	src.set(3)
	e.EvaluateOnce(ctx, r)
	got = rec.take()
	if len(got) != 2 || got[0].msg.Status != alerter.StatusResolved || !strings.HasPrefix(got[0].msg.Title, "[RESOLVED]") ||
		!got[0].msg.EndsAt.Equal(c.t) {
		t.Fatalf("resolve: %+v", got)
	}

	// The log keeps one row per channel per delivery, channels redacted.
	log := st.Notifications()
	if len(log) != 7 {
		t.Fatalf("notification log rows = %d, want 7", len(log))
	}
	for _, n := range log {
		if strings.Contains(n.Channel, "SECRET") || !n.OK {
			t.Fatalf("log row %+v", n)
		}
	}
}

func TestEngineSilencedFiringNeverResolvesLoudly(t *testing.T) {
	e, st, src, rec, c, r := setup(t, 0, "slack://hooks.slack.com/x")
	ctx := context.Background()
	_, _ = st.CreateSilence(ctx, &Silence{Matchers: map[string]string{"namespace": "shop"}, StartsAt: t0, EndsAt: t0.Add(time.Hour)})
	src.set(50)
	e.EvaluateOnce(ctx, r)
	c.t = t0.Add(10 * time.Minute)
	src.set(1)
	e.EvaluateOnce(ctx, r)
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("a silenced alert's firing and resolve must both stay quiet: %v", channelsOf(got))
	}
}

func TestEngineEvaluationErrorKeepsState(t *testing.T) {
	e, st, src, rec, c, r := setup(t, 0, "slack://hooks.slack.com/x")
	ctx := context.Background()
	src.set(50)
	e.EvaluateOnce(ctx, r)
	rec.take()
	src.mu.Lock()
	src.err = errors.New("clickhouse: connection refused")
	src.mu.Unlock()
	c.t = t0.Add(time.Minute)
	e.EvaluateOnce(ctx, r)
	alerts, _ := st.RuleAlerts(ctx, r.ID)
	if len(alerts) != 1 || alerts[0].State != StateFiring || len(rec.take()) != 0 {
		t.Fatalf("a failed evaluation must not resolve anything: %+v", alerts)
	}
}

func TestEngineFailedDeliveryIsLoggedAndRetriedLater(t *testing.T) {
	e, st, src, rec, _, r := setup(t, 0, "slack://hooks.slack.com/x")
	rec.fail = map[string]bool{"slack://hooks.slack.com/x": true}
	src.set(50)
	e.EvaluateOnce(context.Background(), r)
	log := st.Notifications()
	if len(log) != 1 || log[0].OK || log[0].Error == "" {
		t.Fatalf("failed delivery must be logged: %+v", log)
	}
}

func TestEngineNoChannelsIsUIOnly(t *testing.T) {
	e, st, src, rec, _, r := setup(t, 0)
	src.set(50)
	e.EvaluateOnce(context.Background(), r)
	alerts, _ := st.RuleAlerts(context.Background(), r.ID)
	if len(alerts) != 1 || alerts[0].State != StateFiring || len(rec.take()) != 0 {
		t.Fatal("rules without channels track state without notifying")
	}
}

type atomicClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *atomicClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *atomicClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func TestEngineRunSeedsSchedulesAndStops(t *testing.T) {
	st := NewMemoryStore()
	src := &scripted{}
	src.set(1)
	clk := &atomicClock{t: t0}
	e := &Engine{Store: st, Sources: src, Workers: 2, Now: clk.now}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rules, _ := st.ListRules(context.Background())
		src.mu.Lock()
		calls := src.calls
		src.mu.Unlock()
		if len(rules) == len(DefaultRules()) && calls > 0 {
			break
		}
		if len(rules) == len(DefaultRules()) {
			// First evaluations are spread across each interval; jump
			// past them.
			clk.set(t0.Add(time.Hour))
		}
		if time.Now().After(deadline) {
			t.Fatalf("seeded %d rules, %d evaluations", len(rules), calls)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestDefaultRulesAreValid(t *testing.T) {
	for _, r := range DefaultRules() {
		if err := r.Normalize(nil); err != nil {
			t.Errorf("%s: %v", r.Name, err)
		}
		if r.Kind == KindLogs {
			continue
		}
		var err error
		if r.Kind == KindBudget {
			_, err = ParseBudgetQuery(r.Query)
		} else {
			_, err = ParseQuery(r.Kind, r.Query)
		}
		if err != nil {
			t.Errorf("%s: %v", r.Name, err)
		}
	}
}

func TestEngineRetriesWhenEveryDeliveryFailed(t *testing.T) {
	e, st, src, rec, c, r := setup(t, 0, "slack://hooks.slack.com/x")
	rec.fail = map[string]bool{"slack://hooks.slack.com/x": true}
	src.set(50)
	e.EvaluateOnce(context.Background(), r)
	rec.take()
	alerts, _ := st.RuleAlerts(context.Background(), r.ID)
	if !alerts[0].LastNotifiedAt.IsZero() {
		t.Fatal("a firing nobody received must not count as notified")
	}
	rec.fail = nil
	c.t = t0.Add(time.Minute)
	e.EvaluateOnce(context.Background(), r)
	if got := rec.take(); len(got) != 1 || got[0].msg.Status != alerter.StatusFiring {
		t.Fatalf("next evaluation must retry the firing: %v", channelsOf(got))
	}
}

func TestEngineResolveReachesReceiversUnderLaterSilence(t *testing.T) {
	e, st, src, rec, c, r := setup(t, 0, "pagerduty://routing-key")
	ctx := context.Background()
	src.set(50)
	e.EvaluateOnce(ctx, r) // fires and pages
	if len(rec.take()) != 1 {
		t.Fatal("firing page")
	}
	_, _ = st.CreateSilence(ctx, &Silence{Matchers: map[string]string{"alertname": "High spend"}, StartsAt: c.t, EndsAt: c.t.Add(time.Hour)})
	c.t = t0.Add(5 * time.Minute)
	src.set(1)
	e.EvaluateOnce(ctx, r)
	got := rec.take()
	if len(got) != 1 || got[0].msg.Status != alerter.StatusResolved {
		t.Fatalf("the open incident must be resolved even under a silence: %v", channelsOf(got))
	}
}

func TestEngineLogsRepeatedErrorsOnce(t *testing.T) {
	e := &Engine{}
	if !e.shouldLogError("r", "boom") || e.shouldLogError("r", "boom") || !e.shouldLogError("r", "other") {
		t.Fatal("same error must log once, a new one again")
	}
	e.shouldLogError("r", "")
	if !e.shouldLogError("r", "other") {
		t.Fatal("a success clears the de-duplication")
	}
}
