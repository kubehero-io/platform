// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerter

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ─── Generic webhook ─────────────────────────────────────────────────

// Webhook POSTs a stable JSON document to any HTTP endpoint:
// `webhook+https://example.com/hook` (or `webhook+http://` for an
// in-cluster receiver). URL userinfo becomes HTTP basic auth.
//
// Body (version 1):
//
//	{"version":"1","status":"firing","title":…,"body":…,"severity":…,
//	 "source":…,"url":…,"labels":{…},"annotations":{…},"fields":{…},
//	 "startsAt":"RFC3339","endsAt":"RFC3339"}
type Webhook struct{ scheme string }

// NewWebhook returns the provider for webhook+https or webhook+http.
func NewWebhook(scheme string) *Webhook { return &Webhook{scheme: scheme} }

func (w *Webhook) Scheme() string { return w.scheme }

func (w *Webhook) validate(rest string) error { return urlTarget(rest) }

// WebhookPayload is the documented JSON body.
type WebhookPayload struct {
	Version     string            `json:"version"`
	Status      string            `json:"status"`
	Title       string            `json:"title"`
	Body        string            `json:"body,omitempty"`
	Severity    Severity          `json:"severity"`
	Source      string            `json:"source,omitempty"`
	URL         string            `json:"url,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
	StartsAt    string            `json:"startsAt,omitempty"`
	EndsAt      string            `json:"endsAt,omitempty"`
}

func (w *Webhook) Send(ctx context.Context, channel string, m Message) error {
	_, rest, err := splitScheme(channel)
	if err != nil {
		return err
	}
	endpoint, err := httpEndpoint(channel, transportFor(w.scheme, rest))
	if err != nil {
		return err
	}
	status := m.Status
	if status == "" {
		status = StatusFiring
	}
	return postJSON(ctx, endpoint, WebhookPayload{
		Version: "1", Status: status, Title: m.Title, Body: m.Body, Severity: m.Severity, Source: m.Source,
		URL: m.URL, Labels: m.Labels, Annotations: m.Annotations, Fields: m.Fields,
		StartsAt: rfc(m.StartsAt), EndsAt: rfc(m.EndsAt),
	}, nil)
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ─── Microsoft Teams ─────────────────────────────────────────────────

// Teams posts an Adaptive Card to a Teams Workflows ("Post to a channel
// when a webhook request is received") URL: `teams://<host>/<path>` or
// `teams+https://<host>/<path>`.
type Teams struct{ scheme string }

// NewTeams returns the provider for teams or teams+https.
func NewTeams(scheme string) *Teams { return &Teams{scheme: scheme} }

func (t *Teams) Scheme() string { return t.scheme }

func (t *Teams) validate(rest string) error { return urlTarget(rest) }

func (t *Teams) Send(ctx context.Context, channel string, m Message) error {
	_, rest, err := splitScheme(channel)
	if err != nil {
		return err
	}
	endpoint, err := httpEndpoint(channel, transportFor(t.scheme, rest))
	if err != nil {
		return err
	}
	return postJSON(ctx, endpoint, teamsPayload(m), nil)
}

func teamsPayload(m Message) map[string]any {
	color := "Default"
	switch {
	case m.Resolved():
		color = "Good"
	case m.Severity == SeverityCritical:
		color = "Attention"
	case m.Severity == SeverityWarning:
		color = "Warning"
	}
	body := []any{map[string]any{
		"type": "TextBlock", "size": "Medium", "weight": "Bolder", "wrap": true, "color": color,
		"text": truncate(m.Title, 300),
	}}
	if m.Body != "" && m.Body != m.Title {
		body = append(body, map[string]any{"type": "TextBlock", "wrap": true, "text": truncate(m.Body, 4000)})
	}
	if facts := sortedFacts(m.Fields, 20); len(facts) > 0 {
		fs := make([]any, 0, len(facts))
		for _, f := range facts {
			fs = append(fs, map[string]any{"title": truncate(f[0], 100), "value": truncate(f[1], 500)})
		}
		body = append(body, map[string]any{"type": "FactSet", "facts": fs})
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
	if m.URL != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open in KubeHero", "url": m.URL}}
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card,
		}},
	}
}

func sortedFacts(fields map[string]string, max int) [][2]string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		if len(out) >= max {
			break
		}
		out = append(out, [2]string{k, fields[k]})
	}
	return out
}

// ─── Discord ─────────────────────────────────────────────────────────

// Discord posts an embed to a channel webhook:
// `discord+https://discord.com/api/webhooks/<id>/<token>`. Mentions are
// disabled so alert text can never ping @everyone.
type Discord struct{ scheme string }

// NewDiscord returns the provider for discord or discord+https.
func NewDiscord(scheme string) *Discord { return &Discord{scheme: scheme} }

func (d *Discord) Scheme() string { return d.scheme }

func (d *Discord) validate(rest string) error { return urlTarget(rest) }

func (d *Discord) Send(ctx context.Context, channel string, m Message) error {
	_, rest, err := splitScheme(channel)
	if err != nil {
		return err
	}
	endpoint, err := httpEndpoint(channel, transportFor(d.scheme, rest))
	if err != nil {
		return err
	}
	return postJSON(ctx, endpoint, discordPayload(m), nil)
}

func discordPayload(m Message) map[string]any {
	color := 0x3b82f6 // info blue
	switch {
	case m.Resolved():
		color = 0x22c55e
	case m.Severity == SeverityCritical:
		color = 0xef4444
	case m.Severity == SeverityWarning:
		color = 0xf59e0b
	}
	embed := map[string]any{
		"title":       truncate(m.Title, 256),
		"description": truncate(m.Body, 4096),
		"color":       color,
	}
	if m.URL != "" {
		embed["url"] = m.URL
	}
	ts := m.StartsAt
	if m.Resolved() && !m.EndsAt.IsZero() {
		ts = m.EndsAt
	}
	if !ts.IsZero() {
		embed["timestamp"] = ts.UTC().Format(time.RFC3339)
	}
	if facts := sortedFacts(m.Fields, 25); len(facts) > 0 {
		fs := make([]any, 0, len(facts))
		for _, f := range facts {
			fs = append(fs, map[string]any{"name": truncate(f[0], 256), "value": truncate(nonEmpty(f[1], "—"), 1024), "inline": true})
		}
		embed["fields"] = fs
	}
	footer := "KubeHero"
	if m.Source != "" {
		footer += " · " + m.Source
	}
	embed["footer"] = map[string]any{"text": truncate(footer, 2048)}
	return map[string]any{
		"username":         "KubeHero",
		"embeds":           []any{embed},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

// ─── Alertmanager ────────────────────────────────────────────────────

// Alertmanager pushes alerts to an Alertmanager (or anything speaking
// its v2 API, e.g. Grafana, Keep): `alertmanager+https://am:9093[/prefix]`
// or `alertmanager+http://…` → POST <base>/api/v2/alerts.
//
// Alertmanager keeps state itself and expires alerts whose endsAt
// passes, so firing alerts must be re-sent every evaluation (Heartbeat)
// with endsAt pushed forward; a resolve sends endsAt = resolve time.
type Alertmanager struct{ scheme string }

// NewAlertmanager returns the provider for alertmanager+https / +http.
func NewAlertmanager(scheme string) *Alertmanager { return &Alertmanager{scheme: scheme} }

func (a *Alertmanager) Scheme() string { return a.scheme }

func (a *Alertmanager) Heartbeat() bool { return true }

func (a *Alertmanager) validate(rest string) error { return urlTarget(rest) }

// AMAlert is one element of POST /api/v2/alerts.
type AMAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	StartsAt     string            `json:"startsAt,omitempty"`
	EndsAt       string            `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

func (a *Alertmanager) Send(ctx context.Context, channel string, m Message) error {
	_, rest, err := splitScheme(channel)
	if err != nil {
		return err
	}
	base, err := httpEndpoint(channel, transportFor(a.scheme, rest))
	if err != nil {
		return err
	}
	return postJSON(ctx, strings.TrimSuffix(base, "/")+"/api/v2/alerts", []AMAlert{amAlert(m)}, http.Header{})
}

var labelNameRE = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// amLabel makes a Prometheus-valid label name.
func amLabel(k string) string {
	k = labelNameRE.ReplaceAllString(k, "_")
	if k == "" || (k[0] >= '0' && k[0] <= '9') {
		k = "_" + k
	}
	return k
}

func amAlert(m Message) AMAlert {
	labels := map[string]string{}
	for k, v := range m.Labels {
		labels[amLabel(k)] = v
	}
	if labels["alertname"] == "" {
		labels["alertname"] = nonEmpty(m.Source, "KubeHero")
	}
	if labels["severity"] == "" && m.Severity != "" {
		labels["severity"] = string(m.Severity)
	}
	ann := map[string]string{}
	for k, v := range m.Annotations {
		ann[amLabel(k)] = v
	}
	if ann["summary"] == "" && m.Title != "" {
		ann["summary"] = m.Title
	}
	if ann["description"] == "" && m.Body != "" && m.Body != m.Title {
		ann["description"] = m.Body
	}
	out := AMAlert{Labels: labels, Annotations: ann, StartsAt: rfc(m.StartsAt), GeneratorURL: m.URL}
	if !m.EndsAt.IsZero() {
		out.EndsAt = rfc(m.EndsAt)
	}
	return out
}

// ─── shared ──────────────────────────────────────────────────────────

func (s Slack) validate(rest string) error { return urlTarget(rest) }

func (p PagerDuty) validate(rest string) error {
	key, _ := parseQuery(rest)
	if strings.TrimSuffix(key, "/") == "" {
		return fmt.Errorf("empty routing key")
	}
	return nil
}

func (o OpsGenie) validate(rest string) error {
	key, _ := parseQuery(rest)
	if strings.TrimSuffix(key, "/") == "" {
		return fmt.Errorf("empty api key")
	}
	return nil
}
