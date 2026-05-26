// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PagerDuty triggers an alert via the Events API v2.
//
// Channel form: `pagerduty://<routing-key>[?dedup=<key>&priority=P2]`
//
// Severity is mapped: info→info, warning→warning, critical→critical.
// The dedup key defaults to Message.Source so repeated firings of the
// same policy collapse into a single PagerDuty incident.
type PagerDuty struct {
	// Endpoint overrides the Events API URL. Defaults to the public
	// endpoint; tests inject an httptest URL.
	Endpoint string
}

const pagerDutyDefaultEndpoint = "https://events.pagerduty.com/v2/enqueue"

func NewPagerDuty() *PagerDuty { return &PagerDuty{Endpoint: pagerDutyDefaultEndpoint} }

func (PagerDuty) Scheme() string { return "pagerduty" }

func (p PagerDuty) Send(ctx context.Context, channel string, m Message) error {
	_, rest, err := splitScheme(channel)
	if err != nil {
		return err
	}
	target, qs := parseQuery(rest)
	routingKey := strings.TrimSuffix(target, "/")
	if routingKey == "" {
		return fmt.Errorf("pagerduty: empty routing key")
	}

	dedup := qs.Get("dedup")
	if dedup == "" {
		dedup = m.Source
	}
	severity := pdSeverity(m.Severity)

	payload := map[string]any{
		"routing_key":  routingKey,
		"event_action": "trigger",
		"dedup_key":    dedup,
		"payload": map[string]any{
			"summary":   truncate(m.Title, 1024),
			"source":    nonEmpty(m.Source, "kubehero"),
			"severity":  severity,
			"component": "kubehero-control-plane",
			"custom_details": map[string]any{
				"body":   m.Body,
				"fields": m.Fields,
			},
		},
	}
	if m.URL != "" {
		payload["client"] = "KubeHero Dashboard"
		payload["client_url"] = m.URL
	}
	if prio := qs.Get("priority"); prio != "" {
		payload["payload"].(map[string]any)["class"] = prio
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = pagerDutyDefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pagerduty: http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}

func pdSeverity(s Severity) string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityCritical:
		return "critical"
	}
	return "warning"
}

func nonEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
