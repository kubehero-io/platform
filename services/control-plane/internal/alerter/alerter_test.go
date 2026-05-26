// SPDX-License-Identifier: BUSL-1.1
package alerter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// captureServer records every incoming request so tests can assert on
// method, path, headers, and JSON body. status controls the response
// code (default 200).
type captureServer struct {
	calls   atomic.Int32
	bodies  []map[string]any
	headers []http.Header
	paths   []string
	status  int
}

func newCaptureServer(status int) (*captureServer, *httptest.Server) {
	c := &captureServer{status: status}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		c.bodies = append(c.bodies, parsed)
		c.headers = append(c.headers, r.Header.Clone())
		c.paths = append(c.paths, r.URL.Path)
		w.WriteHeader(c.status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return c, srv
}

// ─── Slack ────────────────────────────────────────────────────────────

func TestSlackSendsBlockKitToWebhook(t *testing.T) {
	cap, srv := newCaptureServer(http.StatusOK)
	defer srv.Close()

	channel := "slack://" + strings.TrimPrefix(srv.URL, "http://") + "/services/T00/B00/XXX"
	if err := (Slack{}).Send(context.Background(), channel, Message{
		Title:    "CeilingPolicy fired",
		Body:     "burn-rate 1.4× over budget",
		Severity: SeverityCritical,
		Source:   "ceiling/prod-eu-1",
		Fields:   map[string]string{"cluster": "prod-eu-1"},
		URL:      "https://kubehero.io/clusters/prod-eu-1",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	if cap.calls.Load() != 1 {
		t.Fatalf("want 1 call, got %d", cap.calls.Load())
	}
	got := cap.bodies[0]
	if got["text"] != "CeilingPolicy fired" {
		t.Errorf("missing fallback text, got %v", got["text"])
	}
	blocks, _ := got["blocks"].([]any)
	if len(blocks) < 2 {
		t.Fatalf("expected ≥2 blocks, got %d", len(blocks))
	}
	header := blocks[0].(map[string]any)
	if header["type"] != "header" {
		t.Errorf("first block is not header: %v", header["type"])
	}
	if !strings.Contains(cap.paths[0], "/services/T00/B00/XXX") {
		t.Errorf("path lost during scheme rewrite: %q", cap.paths[0])
	}
}

func TestSlackHTTP4xxIsError(t *testing.T) {
	_, srv := newCaptureServer(http.StatusBadRequest)
	defer srv.Close()
	channel := "slack://" + strings.TrimPrefix(srv.URL, "http://") + "/x"
	err := (Slack{}).Send(context.Background(), channel, Message{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "http 400") {
		t.Fatalf("want http 400 error, got %v", err)
	}
}

// ─── PagerDuty ────────────────────────────────────────────────────────

func TestPagerDutyEnqueuesEventWithRoutingKey(t *testing.T) {
	cap, srv := newCaptureServer(http.StatusAccepted)
	defer srv.Close()

	pd := &PagerDuty{Endpoint: srv.URL}
	err := pd.Send(context.Background(), "pagerduty://routing-key-abc?priority=P2&dedup=ceiling-1", Message{
		Title:    "CeilingPolicy fired",
		Body:     "burn-rate 1.4×",
		Severity: SeverityCritical,
		Source:   "ceiling/prod-eu-1",
		URL:      "https://kubehero.io/x",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	got := cap.bodies[0]
	if got["routing_key"] != "routing-key-abc" {
		t.Errorf("routing key not preserved: %v", got["routing_key"])
	}
	if got["event_action"] != "trigger" {
		t.Errorf("expected trigger, got %v", got["event_action"])
	}
	if got["dedup_key"] != "ceiling-1" {
		t.Errorf("dedup override ignored: %v", got["dedup_key"])
	}
	payload := got["payload"].(map[string]any)
	if payload["severity"] != "critical" {
		t.Errorf("severity not mapped: %v", payload["severity"])
	}
	if payload["class"] != "P2" {
		t.Errorf("priority class missing: %v", payload["class"])
	}
}

func TestPagerDutyDefaultsDedupToSource(t *testing.T) {
	cap, srv := newCaptureServer(http.StatusAccepted)
	defer srv.Close()

	pd := &PagerDuty{Endpoint: srv.URL}
	_ = pd.Send(context.Background(), "pagerduty://k", Message{
		Title:  "t",
		Source: "policy/abc",
	})
	if cap.bodies[0]["dedup_key"] != "policy/abc" {
		t.Fatalf("dedup default wrong: %v", cap.bodies[0]["dedup_key"])
	}
}

// ─── OpsGenie ─────────────────────────────────────────────────────────

func TestOpsGenieAuthHeaderAndBody(t *testing.T) {
	cap, srv := newCaptureServer(http.StatusAccepted)
	defer srv.Close()

	og := &OpsGenie{Endpoint: srv.URL}
	err := og.Send(context.Background(), "opsgenie://api-key-xyz?team=ops&priority=P2&tags=ceiling,prod", Message{
		Title:    "CeilingPolicy fired",
		Body:     "burn-rate 1.4×",
		Severity: SeverityCritical,
		Source:   "ceiling/prod-eu-1",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	auth := cap.headers[0].Get("Authorization")
	if auth != "GenieKey api-key-xyz" {
		t.Errorf("auth header wrong: %q", auth)
	}
	got := cap.bodies[0]
	if got["priority"] != "P2" {
		t.Errorf("priority override not honoured: %v", got["priority"])
	}
	if got["alias"] != "ceiling/prod-eu-1" {
		t.Errorf("alias should default to source: %v", got["alias"])
	}
	tags, _ := got["tags"].([]any)
	if len(tags) != 2 || tags[0] != "ceiling" {
		t.Errorf("tags not parsed: %v", tags)
	}
	resp, _ := got["responders"].([]any)
	if len(resp) != 1 {
		t.Fatalf("responders missing: %v", resp)
	}
	team := resp[0].(map[string]any)
	if team["name"] != "ops" || team["type"] != "team" {
		t.Errorf("team responder wrong: %v", team)
	}
}

func TestOpsGeniePriorityFromSeverity(t *testing.T) {
	cap, srv := newCaptureServer(http.StatusAccepted)
	defer srv.Close()
	og := &OpsGenie{Endpoint: srv.URL}
	for _, c := range []struct {
		sev  Severity
		want string
	}{
		{SeverityInfo, "P5"},
		{SeverityWarning, "P3"},
		{SeverityCritical, "P1"},
	} {
		if err := og.Send(context.Background(), "opsgenie://k", Message{Title: "t", Severity: c.sev}); err != nil {
			t.Fatalf("send: %v", err)
		}
		got := cap.bodies[len(cap.bodies)-1]
		if got["priority"] != c.want {
			t.Errorf("severity %s → priority %v, want %s", c.sev, got["priority"], c.want)
		}
	}
}

// ─── Router ───────────────────────────────────────────────────────────

func TestRouterFanOut(t *testing.T) {
	slackCap, slackSrv := newCaptureServer(http.StatusOK)
	pdCap, pdSrv := newCaptureServer(http.StatusAccepted)
	ogCap, ogSrv := newCaptureServer(http.StatusAccepted)
	defer slackSrv.Close()
	defer pdSrv.Close()
	defer ogSrv.Close()

	r := NewRouter(
		Slack{},
		&PagerDuty{Endpoint: pdSrv.URL},
		&OpsGenie{Endpoint: ogSrv.URL},
	)
	channels := []string{
		"slack://" + strings.TrimPrefix(slackSrv.URL, "http://") + "/hook",
		"pagerduty://routing-1",
		"opsgenie://api-1",
	}
	if err := r.Alert(context.Background(), channels, "burn-rate 1.4×"); err != nil {
		t.Fatalf("alert: %v", err)
	}
	if slackCap.calls.Load() != 1 || pdCap.calls.Load() != 1 || ogCap.calls.Load() != 1 {
		t.Fatalf("want 1 call each, got slack=%d pd=%d og=%d",
			slackCap.calls.Load(), pdCap.calls.Load(), ogCap.calls.Load())
	}
}

func TestRouterUnknownSchemeReturnsError(t *testing.T) {
	r := NewRouter(Slack{})
	err := r.Alert(context.Background(), []string{"webex://team/general"}, "x")
	if err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("want no-provider error, got %v", err)
	}
}

func TestRouterAggregatesPartialFailures(t *testing.T) {
	okCap, okSrv := newCaptureServer(http.StatusOK)
	failCap, failSrv := newCaptureServer(http.StatusBadRequest)
	defer okSrv.Close()
	defer failSrv.Close()

	r := NewRouter(Slack{})
	err := r.Alert(context.Background(), []string{
		"slack://" + strings.TrimPrefix(okSrv.URL, "http://") + "/ok",
		"slack://" + strings.TrimPrefix(failSrv.URL, "http://") + "/bad",
	}, "x")
	if okCap.calls.Load() != 1 || failCap.calls.Load() != 1 {
		t.Fatalf("both endpoints should be hit; ok=%d fail=%d", okCap.calls.Load(), failCap.calls.Load())
	}
	if err == nil || !strings.Contains(err.Error(), "http 400") {
		t.Fatalf("want aggregated error containing http 400, got %v", err)
	}
}

func TestRouterIgnoresEmptyChannels(t *testing.T) {
	r := NewRouter(Slack{})
	if err := r.Alert(context.Background(), []string{"", "   "}, "x"); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestSplitSchemeRejectsMissingScheme(t *testing.T) {
	if _, _, err := splitScheme("hooks.slack.com/hook"); err == nil {
		t.Fatal("want error on missing scheme")
	}
	if _, _, err := splitScheme("slack://"); err == nil {
		t.Fatal("want error on empty target")
	}
}
