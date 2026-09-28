// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
)

// Notifier delivers messages (alerter.Router).
type Notifier interface {
	SendAll(ctx context.Context, channels []string, msg alerter.Message) error
	WantsHeartbeat(channel string) bool
}

// Leader gates evaluation to one replica (PGLeader); nil = always.
type Leader interface {
	IsLeader(ctx context.Context) bool
}

// Engine evaluates enabled rules on their intervals and notifies.
type Engine struct {
	Store    Store
	Sources  Sources
	Notifier Notifier // nil = UI-only
	Leader   Leader
	Log      *slog.Logger
	// DashboardURL prefixes alert deep links in notifications
	// (KUBEHERO_DASHBOARD_URL); empty = no link.
	DashboardURL string
	Workers      int // default 4
	Now          func() time.Time

	mu       sync.Mutex
	rules    []*Rule
	next     map[string]time.Time
	running  map[string]bool
	reload   chan struct{}
	leader   bool
	leaderAt time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// Reload asks the loop to re-read rules (after an upsert/delete).
func (e *Engine) Reload() {
	e.mu.Lock()
	ch := e.reload
	e.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run evaluates until ctx ends. It seeds the default rules on first
// boot and re-reads rules every 30s (or on Reload).
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	e.reload = make(chan struct{}, 1)
	e.next = map[string]time.Time{}
	e.running = map[string]bool{}
	e.mu.Unlock()

	if seeded, err := e.Store.SeedDefaults(ctx, DefaultRules()); err != nil {
		e.log().Warn("alerts: seeding default rules failed", "err", err)
	} else if seeded {
		e.log().Info("alerts: seeded the default rule set (UI-only until channels are added)", "rules", len(DefaultRules()))
	}
	if !e.Store.Persistent() {
		e.log().Warn("alerts: Postgres not configured — alert rules, state and silences are kept in memory and lost on restart")
	}
	e.loadRules(ctx)

	workers := e.Workers
	if workers <= 0 {
		workers = 4
	}
	jobs := make(chan *Rule, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				e.evaluate(ctx, r)
				e.mu.Lock()
				delete(e.running, r.ID)
				e.mu.Unlock()
			}
		}()
	}
	tick := time.NewTicker(time.Second)
	refresh := time.NewTicker(30 * time.Second)
	prune := time.NewTicker(time.Hour)
	defer func() {
		tick.Stop()
		refresh.Stop()
		prune.Stop()
		close(jobs)
		wg.Wait()
		if l, ok := e.Leader.(interface{ Release() }); ok {
			l.Release()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.reload:
			e.loadRules(ctx)
		case <-refresh.C:
			e.loadRules(ctx)
		case <-prune.C:
			if p, ok := e.Store.(interface {
				PruneNotifications(context.Context, time.Duration) error
			}); ok {
				if err := p.PruneNotifications(ctx, 30*24*time.Hour); err != nil {
					e.log().Warn("alerts: pruning notification log failed", "err", err)
				}
			}
		case <-tick.C:
			if !e.isLeader(ctx) {
				continue
			}
			for _, r := range e.due() {
				select {
				case jobs <- r:
				default:
					// Workers saturated: retry on the next tick.
					e.mu.Lock()
					delete(e.running, r.ID)
					e.next[r.ID] = e.now()
					e.mu.Unlock()
				}
			}
		}
	}
}

// isLeader re-checks leadership every 15s.
func (e *Engine) isLeader(ctx context.Context) bool {
	if e.Leader == nil {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.leaderAt) < 15*time.Second {
		return e.leader
	}
	was := e.leader
	e.leader, e.leaderAt = e.Leader.IsLeader(ctx), time.Now()
	if e.leader != was {
		e.log().Info("alerts: leadership changed", "leader", e.leader)
	}
	return e.leader
}

func (e *Engine) loadRules(ctx context.Context) {
	rules, err := e.Store.ListRules(ctx)
	if err != nil {
		e.log().Warn("alerts: loading rules failed", "err", err)
		return
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	keep := map[string]bool{}
	e.rules = e.rules[:0]
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		keep[r.ID] = true
		e.rules = append(e.rules, r)
		if _, ok := e.next[r.ID]; !ok {
			// Spread first evaluations over the interval, stably per
			// rule, so a restart doesn't evaluate everything at once.
			h := fnv.New32a()
			_, _ = h.Write([]byte(r.ID))
			e.next[r.ID] = now.Add(time.Duration(h.Sum32()%1000) * r.EvalInterval / 1000)
		}
	}
	for id := range e.next {
		if !keep[id] {
			delete(e.next, id)
		}
	}
}

// due returns rules whose next evaluation has come and schedules the
// one after: interval ± up to 5% jitter (capped at 5s).
func (e *Engine) due() []*Rule {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []*Rule
	for _, r := range e.rules {
		if e.running[r.ID] || now.Before(e.next[r.ID]) {
			continue
		}
		e.running[r.ID] = true
		jitter := min(r.EvalInterval/20, 5*time.Second)
		var j time.Duration
		if jitter > 0 {
			j = time.Duration(rand.Int64N(int64(2*jitter))) - jitter
		}
		e.next[r.ID] = now.Add(r.EvalInterval + j)
		out = append(out, r)
	}
	return out
}

// EvaluateOnce runs one rule synchronously (tests, TestAlertRule uses
// Sources directly instead).
func (e *Engine) EvaluateOnce(ctx context.Context, r *Rule) { e.evaluate(ctx, r) }

func (e *Engine) evaluate(ctx context.Context, r *Rule) {
	now := e.now()
	timeout := min(r.EvalInterval, 30*time.Second)
	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	samples, err := e.Sources.Evaluate(ectx, r, now)
	if err != nil {
		// Keep state untouched: a transient store error must not
		// resolve (and page "resolved" for) every firing alert.
		e.log().Warn("alerts: rule evaluation failed", "rule", r.Name, "kind", r.Kind, "err", err)
		return
	}
	prev, err := e.Store.RuleAlerts(ctx, r.ID)
	if err != nil {
		e.log().Warn("alerts: loading alert state failed", "rule", r.Name, "err", err)
		return
	}
	res := Step(r, prev, samples, now)
	silences, err := e.Store.ListSilences(ctx, false, now)
	if err != nil {
		e.log().Warn("alerts: loading silences failed; notifying as if none were active", "err", err)
	}
	log := e.notify(ctx, r, res, silences, now)
	if err := e.Store.SaveAlerts(ctx, r.ID, res.Alerts, res.Deleted); err != nil {
		e.log().Warn("alerts: saving alert state failed", "rule", r.Name, "err", err)
	}
	if len(log) > 0 {
		if err := e.Store.RecordNotifications(ctx, log); err != nil {
			e.log().Warn("alerts: recording notifications failed", "err", err)
		}
	}
}

func silenced(a *Alert, silences []*Silence, now time.Time) bool {
	for _, s := range silences {
		if s.Active(now) && s.Matches(a.Labels) {
			return true
		}
	}
	return false
}

// notify decides and sends this evaluation's notifications, updating
// LastNotifiedAt, and returns the delivery log.
func (e *Engine) notify(ctx context.Context, r *Rule, res StepResult, silences []*Silence, now time.Time) []Notification {
	resolvedNow := map[string]bool{}
	for _, t := range res.Transitions {
		if t.Event == StateResolved {
			resolvedNow[t.Alert.ID] = true
		}
	}
	var chat, beat []string
	for _, ch := range r.Channels {
		if e.Notifier != nil && e.Notifier.WantsHeartbeat(ch) {
			beat = append(beat, ch)
		} else {
			chat = append(chat, ch)
		}
	}
	var log []Notification
	for _, a := range res.Alerts {
		a.Silenced = silenced(a, silences, now)
		if e.Notifier == nil || len(r.Channels) == 0 || a.Silenced {
			continue
		}
		switch {
		case a.State == StateFiring:
			event := ""
			switch {
			case a.LastNotifiedAt.IsZero():
				event = "firing"
			case now.Sub(a.LastNotifiedAt) >= RenotifyInterval:
				event = "renotify"
			}
			if event != "" && len(chat) > 0 {
				log = append(log, e.send(ctx, r, a, chat, event, now)...)
			}
			if len(beat) > 0 {
				ev := event
				if ev == "" {
					ev = "heartbeat"
				}
				log = append(log, e.send(ctx, r, a, beat, ev, now)...)
			}
			if event != "" {
				a.LastNotifiedAt = now
			}
		case resolvedNow[a.ID] && !a.LastNotifiedAt.IsZero():
			// Resolve only what was announced: a silenced firing never
			// reached anyone, so its resolve shouldn't either.
			log = append(log, e.send(ctx, r, a, r.Channels, "resolved", now)...)
		}
	}
	return log
}

// send delivers one alert to channels in parallel, one log row each.
func (e *Engine) send(ctx context.Context, r *Rule, a *Alert, channels []string, event string, now time.Time) []Notification {
	msg := e.message(r, a, event, now)
	out := make([]Notification, len(channels))
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Add(1)
		go func(i int, ch string) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			err := e.Notifier.SendAll(sctx, []string{ch}, msg)
			n := Notification{At: now, AlertID: a.ID, RuleID: r.ID, Event: event, Channel: alerter.RedactChannel(ch), OK: err == nil}
			if err != nil {
				n.Error = err.Error() // alerter errors are already redacted
				e.log().Warn("alerts: notification failed", "rule", r.Name, "alert", a.ID, "channel", n.Channel, "err", err)
			}
			out[i] = n
		}(i, ch)
	}
	wg.Wait()
	return out
}

var severityMap = map[string]alerter.Severity{"info": alerter.SeverityInfo, "warn": alerter.SeverityWarning, "critical": alerter.SeverityCritical}

// message renders one alert for the alerter.
func (e *Engine) message(r *Rule, a *Alert, event string, now time.Time) alerter.Message {
	status := alerter.StatusFiring
	prefix := "FIRING"
	if event == "resolved" {
		status, prefix = alerter.StatusResolved, "RESOLVED"
	}
	sev := severityMap[a.Labels["severity"]]
	if sev == "" {
		sev = severityMap[r.Severity]
	}
	fields := map[string]string{"value": formatValue(a.Value), "rule": r.Name, "kind": r.Kind}
	keys := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(fields) >= 20 {
			break
		}
		if k != "alertname" && k != "severity" {
			fields[k] = a.Labels[k]
		}
	}
	ann := cloneMap(r.Annotations)
	if ann == nil {
		ann = map[string]string{}
	}
	for k, v := range ann {
		ann[k] = Render(v, a.Value, a.Labels)
	}
	ann["summary"], ann["description"] = a.Summary, a.Description
	endsAt := now.Add(4 * max(r.EvalInterval, time.Minute))
	if status == alerter.StatusResolved {
		endsAt = a.ResolvedAt
	}
	started := a.FiredAt
	if started.IsZero() {
		started = a.StartedAt
	}
	link := ""
	if e.DashboardURL != "" {
		link = strings.TrimSuffix(e.DashboardURL, "/") + "/alerts?alert=" + a.ID
	}
	return alerter.Message{
		Title:       fmt.Sprintf("[%s] %s — %s", prefix, r.Name, a.Summary),
		Body:        a.Description,
		Severity:    sev,
		Source:      "kubehero/alert/" + a.ID,
		Fields:      fields,
		URL:         link,
		Status:      status,
		Labels:      cloneMap(a.Labels),
		Annotations: ann,
		StartsAt:    started,
		EndsAt:      endsAt,
	}
}
