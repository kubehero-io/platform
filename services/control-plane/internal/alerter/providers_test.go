// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawServer captures raw bodies (arrays and objects alike).
type rawServer struct {
	mu      sync.Mutex
	bodies  [][]byte
	paths   []string
	headers []http.Header
}

func newRawServer(t *testing.T) (*rawServer, string) {
	t.Helper()
	rs := &rawServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rs.mu.Lock()
		rs.bodies = append(rs.bodies, b)
		rs.paths = append(rs.paths, r.URL.RequestURI())
		rs.headers = append(rs.headers, r.Header.Clone())
		rs.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return rs, strings.TrimPrefix(srv.URL, "http://")
}

var alertMsg = Message{
	Title: "[FIRING] HighSpend — shop at $12.40/h", Body: "cost{namespace=\"shop\"} = 12.40 > 10",
	Severity: SeverityCritical, Source: "alert/rule-1/abc", URL: "https://kubehero.example/alerts/abc",
	Fields:      map[string]string{"namespace": "shop", "value": "12.40"},
	Status:      StatusFiring,
	Labels:      map[string]string{"alertname": "HighSpend", "namespace": "shop", "kubehero.io/team": "payments"},
	Annotations: map[string]string{"summary": "shop spend high", "runbook_url": "https://runbooks/x"},
	StartsAt:    time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	EndsAt:      time.Date(2026, 9, 30, 12, 4, 0, 0, time.UTC),
}

func TestWebhookPayload(t *testing.T) {
	rs, host := newRawServer(t)
	if err := NewRouter().SendAll(context.Background(), []string{"webhook+http://" + host + "/hooks/kubehero?token=s3cret"}, alertMsg); err != nil {
		t.Fatal(err)
	}
	var p WebhookPayload
	if err := json.Unmarshal(rs.bodies[0], &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != "1" || p.Status != "firing" || p.Labels["alertname"] != "HighSpend" || p.StartsAt != "2026-09-30T12:00:00Z" ||
		p.Annotations["runbook_url"] == "" || p.Severity != SeverityCritical {
		t.Fatalf("payload: %+v", p)
	}
	if rs.paths[0] != "/hooks/kubehero?token=s3cret" {
		t.Fatalf("path/query must be preserved: %q", rs.paths[0])
	}
}

func TestTeamsAdaptiveCard(t *testing.T) {
	rs, host := newRawServer(t)
	if err := NewRouter().SendAll(context.Background(), []string{"teams://" + host + "/workflows/abc/triggers/manual/paths/invoke"}, alertMsg); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal(rs.bodies[0], &p)
	att := p["attachments"].([]any)[0].(map[string]any)
	if p["type"] != "message" || att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("envelope: %v", p)
	}
	card := att["content"].(map[string]any)
	body := card["body"].([]any)
	if card["type"] != "AdaptiveCard" || body[0].(map[string]any)["color"] != "Attention" || len(body) != 3 {
		t.Fatalf("card: %v", card)
	}
	if card["actions"].([]any)[0].(map[string]any)["url"] != alertMsg.URL {
		t.Fatal("deep link action missing")
	}
}

func TestDiscordEmbed(t *testing.T) {
	rs, host := newRawServer(t)
	long := alertMsg
	long.Title = strings.Repeat("x", 400)
	long.Body = "@everyone " + strings.Repeat("y", 5000)
	if err := NewRouter().SendAll(context.Background(), []string{"discord://" + host + "/api/webhooks/1/tok"}, long); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal(rs.bodies[0], &p)
	if m := p["allowed_mentions"].(map[string]any)["parse"].([]any); len(m) != 0 {
		t.Fatal("mentions must be disabled")
	}
	e := p["embeds"].([]any)[0].(map[string]any)
	if len([]rune(e["title"].(string))) > 256 || len([]rune(e["description"].(string))) > 4096 {
		t.Fatal("discord limits exceeded")
	}
	if int(e["color"].(float64)) != 0xef4444 || e["timestamp"] != "2026-09-30T12:00:00Z" {
		t.Fatalf("embed: %v", e)
	}
}

func TestAlertmanagerV2(t *testing.T) {
	rs, host := newRawServer(t)
	r := NewRouter()
	ch := "alertmanager+http://" + host + "/am"
	if !r.WantsHeartbeat(ch) || r.WantsHeartbeat("slack://hooks.slack.com/x") {
		t.Fatal("only alertmanager wants heartbeats")
	}
	if err := r.SendAll(context.Background(), []string{ch}, alertMsg); err != nil {
		t.Fatal(err)
	}
	if rs.paths[0] != "/am/api/v2/alerts" {
		t.Fatalf("path %q", rs.paths[0])
	}
	var alerts []AMAlert
	if err := json.Unmarshal(rs.bodies[0], &alerts); err != nil || len(alerts) != 1 {
		t.Fatalf("body: %s", rs.bodies[0])
	}
	a := alerts[0]
	if a.Labels["alertname"] != "HighSpend" || a.Labels["kubehero_io_team"] != "payments" || a.Labels["severity"] != "critical" {
		t.Fatalf("labels must be Prometheus-valid: %v", a.Labels)
	}
	if a.EndsAt != "2026-09-30T12:04:00Z" || a.GeneratorURL != alertMsg.URL || a.Annotations["summary"] != "shop spend high" {
		t.Fatalf("alert: %+v", a)
	}
}

func TestPagerDutyResolveAndOpsGenieClose(t *testing.T) {
	cap, srv := newCaptureServer(http.StatusAccepted)
	defer srv.Close()
	resolved := alertMsg
	resolved.Status = StatusResolved
	if err := (&PagerDuty{Endpoint: srv.URL}).Send(context.Background(), "pagerduty://rk", resolved); err != nil {
		t.Fatal(err)
	}
	if cap.bodies[0]["event_action"] != "resolve" || cap.bodies[0]["dedup_key"] != "alert/rule-1/abc" {
		t.Fatalf("pagerduty resolve: %v", cap.bodies[0])
	}
	if err := (&OpsGenie{Endpoint: srv.URL}).Send(context.Background(), "opsgenie://key", resolved); err != nil {
		t.Fatal(err)
	}
	if cap.paths[1] != "/alert/rule-1/abc/close" || cap.headers[1].Get("Authorization") != "GenieKey key" {
		t.Fatalf("opsgenie close: %q %v", cap.paths[1], cap.headers[1])
	}
}

func TestRedactChannel(t *testing.T) {
	for in, want := range map[string]string{
		"slack://hooks.slack.com/services/T00/B00/XXX":        "slack://hooks.slack.com/…",
		"webhook+https://user:pw@example.com/hook?token=abc":  "webhook+https://example.com/…",
		"teams://prod.westeurope.logic.azure.com/workflows/x": "teams://prod.westeurope.logic.azure.com/…",
		"alertmanager+http://am:9093":                         "alertmanager+http://am:9093",
		"pagerduty://0123456789abcdef0123456789abcdef":        "pagerduty://…cdef",
		"opsgenie://short?team=ops":                           "opsgenie://…",
		"nonsense":                                            "…",
	} {
		if got := RedactChannel(in); got != want {
			t.Errorf("RedactChannel(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(RedactChannel(in), "XXX") || strings.Contains(RedactChannel(in), "token") {
			t.Errorf("secret leaked from %q", in)
		}
	}
	if !IsRedacted("slack://hooks.slack.com/…") || IsRedacted("slack://hooks.slack.com/x") {
		t.Fatal("IsRedacted")
	}
}

func TestErrorsNeverLeakSecrets(t *testing.T) {
	// A closed port: the transport error would normally print the URL.
	ln := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(ln.URL, "http://")
	ln.Close()
	err := NewRouter().SendAll(context.Background(), []string{
		"webhook+http://" + host + "/secret-path?token=hunter2",
		"slack://" + host + "/services/T00/B00/SECRET",
	}, alertMsg)
	if err == nil {
		t.Fatal("want connection errors")
	}
	for _, leak := range []string{"hunter2", "secret-path", "SECRET"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
}

func TestValidate(t *testing.T) {
	r := NewRouter()
	for _, ok := range []string{"slack://hooks.slack.com/x", "pagerduty://key", "opsgenie://key?team=a",
		"webhook+https://example.com/h", "teams+https://example.com/h", "discord+https://discord.com/api/webhooks/1/2",
		"alertmanager+https://am:9093"} {
		if err := r.Validate(ok); err != nil {
			t.Errorf("Validate(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "webex://team", "slack://", "pagerduty://?x=1", "webhook+https://", "noscheme",
		"webhook+https://" + strings.Repeat("a", 2100)} {
		if err := r.Validate(bad); err == nil {
			t.Errorf("Validate(%q) should fail", bad)
		}
	}
	if err := r.Validate("webex://team/secret-room"); err == nil || strings.Contains(err.Error(), "secret-room") {
		t.Fatalf("validation errors must be redacted: %v", err)
	}
}
