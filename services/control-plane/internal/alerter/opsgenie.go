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

// OpsGenie creates an alert via the OpsGenie REST API.
//
// Channel form: `opsgenie://<api-key>[?team=<team>&priority=P2&region=eu]`
//
// `region=eu` switches the endpoint to api.eu.opsgenie.com. Severity maps
// to OpsGenie priority: info→P5, warning→P3, critical→P1 (overridable
// via the `priority` query param).
type OpsGenie struct {
	// Endpoint overrides the alerts API URL. Empty means: pick by region
	// query param. Tests inject an httptest URL.
	Endpoint string
}

const (
	opsGenieDefaultEndpoint = "https://api.opsgenie.com/v2/alerts"
	opsGenieEUEndpoint      = "https://api.eu.opsgenie.com/v2/alerts"
)

func NewOpsGenie() *OpsGenie { return &OpsGenie{} }

func (OpsGenie) Scheme() string { return "opsgenie" }

func (o OpsGenie) Send(ctx context.Context, channel string, m Message) error {
	_, rest, err := splitScheme(channel)
	if err != nil {
		return err
	}
	target, qs := parseQuery(rest)
	apiKey := strings.TrimSuffix(target, "/")
	if apiKey == "" {
		return fmt.Errorf("opsgenie: empty api key")
	}

	priority := qs.Get("priority")
	if priority == "" {
		priority = ogPriority(m.Severity)
	}

	alert := map[string]any{
		"message":     truncate(m.Title, 130),
		"description": m.Body,
		"priority":    priority,
		"source":      nonEmpty(m.Source, "kubehero"),
		"alias":       m.Source, // dedup key
		"details":     m.Fields,
	}
	if team := qs.Get("team"); team != "" {
		alert["responders"] = []map[string]string{{"name": team, "type": "team"}}
	}
	if tags := qs.Get("tags"); tags != "" {
		alert["tags"] = strings.Split(tags, ",")
	}

	body, err := json.Marshal(alert)
	if err != nil {
		return err
	}

	endpoint := o.endpoint(qs.Get("region"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "GenieKey "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opsgenie: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opsgenie: http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}

func (o OpsGenie) endpoint(region string) string {
	if o.Endpoint != "" {
		return o.Endpoint
	}
	if strings.EqualFold(region, "eu") {
		return opsGenieEUEndpoint
	}
	return opsGenieDefaultEndpoint
}

func ogPriority(s Severity) string {
	switch s {
	case SeverityInfo:
		return "P5"
	case SeverityWarning:
		return "P3"
	case SeverityCritical:
		return "P1"
	}
	return "P3"
}
