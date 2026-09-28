// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Transition is a state change worth notifying about.
type Transition struct {
	Alert *Alert
	Event string // StateFiring | StateResolved
}

// StepResult is one evaluation's outcome for one rule.
type StepResult struct {
	Alerts      []*Alert // every alert still tracked (pending, firing, resolved)
	Deleted     []string // alert ids no longer tracked
	Transitions []Transition
}

// Step advances the state machine of one rule. prev are the rule's
// tracked alerts; samples the series this evaluation produced (only
// those satisfying the condition matter — a series that is false or
// absent resolves). Pure: time comes from now.
func Step(rule *Rule, prev []*Alert, samples []Sample, now time.Time) StepResult {
	byFP := make(map[string]*Alert, len(prev))
	for _, a := range prev {
		byFP[a.Fingerprint] = a
	}
	var res StepResult
	active := map[string]bool{}
	for i, s := range samples {
		if i >= MaxSeries {
			break
		}
		if !Compare(s.Value, rule.Op, rule.Threshold) {
			continue
		}
		fp := Fingerprint(s.Labels)
		if active[fp] {
			continue // duplicate series in one evaluation
		}
		active[fp] = true
		a := byFP[fp]
		if a == nil || a.State == StateResolved {
			a = &Alert{
				ID: AlertID(rule.ID, fp), RuleID: rule.ID, Fingerprint: fp,
				State: StatePending, StartedAt: now,
			}
		}
		a.Value = s.Value
		a.LastEvalAt = now
		a.Labels = alertLabels(rule, s.Labels)
		a.LinkPath = s.Link
		if a.State == StatePending && now.Sub(a.StartedAt) >= rule.PendingFor {
			a.State = StateFiring
			a.FiredAt = now
			res.Transitions = append(res.Transitions, Transition{Alert: a, Event: StateFiring})
		}
		a.Summary, a.Description = annotations(rule, a)
		res.Alerts = append(res.Alerts, a)
	}
	for _, a := range prev {
		if active[a.Fingerprint] {
			continue
		}
		switch a.State {
		case StatePending:
			// Never fired: back to inactive, nothing to announce.
			res.Deleted = append(res.Deleted, a.ID)
		case StateFiring:
			a.State = StateResolved
			a.ResolvedAt = now
			a.LastEvalAt = now
			a.Summary, a.Description = annotations(rule, a)
			res.Transitions = append(res.Transitions, Transition{Alert: a, Event: StateResolved})
			res.Alerts = append(res.Alerts, a)
		case StateResolved:
			if now.Sub(a.ResolvedAt) >= ResolvedRetention {
				res.Deleted = append(res.Deleted, a.ID)
			} else {
				res.Alerts = append(res.Alerts, a)
			}
		}
	}
	return res
}

// alertLabels: series labels, overridden by the rule's own labels,
// plus alertname and severity (Prometheus conventions).
func alertLabels(rule *Rule, series map[string]string) map[string]string {
	out := make(map[string]string, len(series)+len(rule.Labels)+2)
	for k, v := range series {
		out[k] = v
	}
	for k, v := range rule.Labels {
		out[k] = v
	}
	out["alertname"] = rule.Name
	if _, ok := rule.Labels["severity"]; !ok {
		out["severity"] = rule.Severity
	}
	return out
}

// annotations renders summary and description, defaulting to a plain
// statement of what crossed which threshold.
func annotations(rule *Rule, a *Alert) (string, string) {
	summary := rule.Annotations["summary"]
	if summary == "" {
		summary = fmt.Sprintf("%s: %s %s %s", rule.Name, formatValue(a.Value), rule.Op, formatValue(rule.Threshold))
		if l := seriesLabelText(a.Labels); l != "" {
			summary += " (" + l + ")"
		}
	}
	desc := rule.Annotations["description"]
	if desc == "" {
		desc = rule.Description
	}
	if desc == "" {
		desc = fmt.Sprintf("%s query %q evaluated to %s, threshold %s %s.", rule.Kind, rule.Query,
			formatValue(a.Value), rule.Op, formatValue(rule.Threshold))
	}
	return Render(summary, a.Value, a.Labels), Render(desc, a.Value, a.Labels)
}

// seriesLabelText renders the identifying labels for a default summary.
func seriesLabelText(labels map[string]string) string {
	var parts []string
	for k, v := range labels {
		if k == "alertname" || k == "severity" || v == "" {
			continue
		}
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
