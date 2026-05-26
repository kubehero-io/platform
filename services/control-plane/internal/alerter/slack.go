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

// Slack posts to an incoming webhook in Block Kit format. The channel
// string is the webhook URL with `slack://` swapped in for `https://`,
// e.g. `slack://hooks.slack.com/services/T00/B00/XXX`.
//
// For local tests, `slack://127.0.0.1:PORT/hook` is rewritten to http://
// so an httptest.Server can receive the payload without TLS.
type Slack struct{}

func NewSlack() *Slack { return &Slack{} }

func (Slack) Scheme() string { return "slack" }

func (s Slack) Send(ctx context.Context, channel string, m Message) error {
	endpoint, err := slackEndpoint(channel)
	if err != nil {
		return err
	}

	body, err := json.Marshal(slackPayload(m))
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("slack: http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}

func slackEndpoint(channel string) (string, error) {
	scheme := "https"
	if _, rest, err := splitScheme(channel); err == nil && isLocalHost(rest) {
		scheme = "http"
	}
	return rewriteScheme(channel, scheme)
}

func isLocalHost(rest string) bool {
	for _, p := range []string{"localhost", "127.", "[::1]", "::1"} {
		if strings.HasPrefix(rest, p) {
			return true
		}
	}
	return false
}

// slackPayload renders a Block Kit message. We keep the layout small so
// it stays readable in busy channels and on mobile.
func slackPayload(m Message) any {
	emoji := ":large_blue_circle:"
	switch m.Severity {
	case SeverityWarning:
		emoji = ":warning:"
	case SeverityCritical:
		emoji = ":rotating_light:"
	}

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": emoji + " " + truncate(m.Title, 140), "emoji": true},
		},
	}
	if m.Body != "" && m.Body != m.Title {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": truncate(m.Body, 2900)},
		})
	}
	if len(m.Fields) > 0 {
		fields := make([]map[string]any, 0, len(m.Fields))
		for k, v := range m.Fields {
			fields = append(fields, map[string]any{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*%s*\n%s", k, v),
			})
		}
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	}
	ctxBits := []map[string]any{
		{"type": "mrkdwn", "text": fmt.Sprintf("severity · *%s*", m.Severity)},
	}
	if m.Source != "" {
		ctxBits = append(ctxBits, map[string]any{"type": "mrkdwn", "text": "source · *" + m.Source + "*"})
	}
	if m.URL != "" {
		ctxBits = append(ctxBits, map[string]any{"type": "mrkdwn", "text": "<" + m.URL + "|open in dashboard>"})
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": ctxBits})

	return map[string]any{
		"text":   m.Title,
		"blocks": blocks,
	}
}
