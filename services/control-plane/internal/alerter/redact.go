// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// keyedSchemes carry a credential as the whole target.
var keyedSchemes = map[string]bool{"pagerduty": true, "opsgenie": true}

// RedactChannel renders a channel string safe to list or log. URL
// channels keep scheme and host ("slack://hooks.slack.com/…"); keyed
// channels keep the last four characters ("pagerduty://…3f9a").
func RedactChannel(ch string) string {
	ch = strings.TrimSpace(ch)
	scheme, rest, err := splitScheme(ch)
	if err != nil {
		return "…"
	}
	if keyedSchemes[scheme] {
		key, _ := parseQuery(rest)
		key = strings.TrimSuffix(key, "/")
		if len(key) > 8 {
			return scheme + "://…" + key[len(key)-4:]
		}
		return scheme + "://…"
	}
	u, err := url.Parse("x://" + rest)
	if err != nil || u.Host == "" {
		return scheme + "://…"
	}
	out := scheme + "://" + u.Host
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		out += "/…"
	}
	return out
}

// IsRedacted reports whether a channel string is a RedactChannel
// rendering (so an API can keep the stored secret on round-trip).
func IsRedacted(ch string) bool { return strings.Contains(ch, "…") }

// sanitize strips URLs (which carry webhook secrets) out of transport
// errors: net/http's *url.Error prints the full request URL.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		host := ""
		if u, perr := url.Parse(ue.URL); perr == nil {
			host = u.Host
		}
		return fmt.Errorf("%s %s: %w", strings.ToLower(ue.Op), host, ue.Err)
	}
	return err
}

// postJSON POSTs body as JSON and fails on non-2xx. The response body
// is read (bounded) for the error message; errors never contain the
// endpoint's path or query.
func postJSON(ctx context.Context, endpoint string, body any, header http.Header) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return sanitize(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kubehero-alerter")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return sanitize(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

// httpEndpoint turns "<scheme>://<rest>" into a real URL with the given
// transport scheme, validating that there is a host.
func httpEndpoint(channel, transport string) (string, error) {
	_, rest, err := splitScheme(channel)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(transport + "://" + rest)
	if err != nil || u.Host == "" {
		return "", errors.New("target must be host[:port][/path]")
	}
	return u.String(), nil
}

// urlTarget validates a URL-style target (for Router.Validate).
func urlTarget(rest string) error {
	u, err := url.Parse("https://" + rest)
	if err != nil || u.Host == "" {
		return errors.New("target must be host[:port][/path]")
	}
	return nil
}

// transportFor maps "x+http"/"x+https" schemes to their transport;
// bare schemes use https except for loopback test hosts.
func transportFor(scheme, rest string) string {
	switch {
	case strings.HasSuffix(scheme, "+http"):
		return "http"
	case strings.HasSuffix(scheme, "+https"):
		return "https"
	case isLocalHost(rest):
		return "http"
	}
	return "https"
}
