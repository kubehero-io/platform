// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package alerter delivers escalation messages to operator channels.
//
// Each policy step lists one or more channel strings; the scheme of each
// string selects a provider:
//
//	slack://hooks.slack.com/services/T00/B00/XXX   → Slack incoming webhook
//	pagerduty://<routing-key>                      → PagerDuty Events API v2
//	opsgenie://<api-key>[?team=ops&priority=P2]    → OpsGenie alerts API
//
// The Router fans out to providers in parallel and aggregates errors —
// one bad channel does not silence the others. Providers themselves are
// stateless and safe for concurrent use.
package alerter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Provider is what the Router dispatches to. Each implementation owns a
// single scheme. Send must be safe for concurrent use.
type Provider interface {
	Scheme() string
	Send(ctx context.Context, channel string, msg Message) error
}

// Message is the payload the runner hands to the alerter. We carry
// enough context for providers to render a useful incident card without
// needing to look anything else up.
type Message struct {
	Title    string            // short summary, e.g. "CeilingPolicy · prod-eu-1 · burn 1.4×"
	Body     string            // long-form details, plain text
	Severity Severity          // info · warning · critical
	Source   string            // policy kind/name, used as dedup key
	Fields   map[string]string // optional key/value pairs surfaced in cards
	URL      string            // deep link back into the dashboard
}

// Severity is normalised across providers. Each maps it to its own scale.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Router fans channels out to the matching provider. It implements the
// engine.Alerter interface (Alert(ctx, channels, message)).
type Router struct {
	providers map[string]Provider
}

// NewRouter wires the default provider set. Tests inject overrides via
// WithProvider.
func NewRouter(providers ...Provider) *Router {
	r := &Router{providers: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		r.providers[strings.ToLower(p.Scheme())] = p
	}
	return r
}

// WithProvider returns a copy with the given provider registered (or
// replaced). Useful for tests that swap a real provider for a stub.
func (r *Router) WithProvider(p Provider) *Router {
	cp := &Router{providers: make(map[string]Provider, len(r.providers)+1)}
	for k, v := range r.providers {
		cp.providers[k] = v
	}
	cp.providers[strings.ToLower(p.Scheme())] = p
	return cp
}

// Alert satisfies engine.Alerter: it accepts the channel slice from the
// policy spec plus a flat message string. The string is used as both
// title and body; richer messages should call SendAll directly.
func (r *Router) Alert(ctx context.Context, channels []string, message string) error {
	return r.SendAll(ctx, channels, Message{
		Title:    truncate(message, 120),
		Body:     message,
		Severity: SeverityCritical,
	})
}

// SendAll dispatches in parallel and joins per-channel errors.
func (r *Router) SendAll(ctx context.Context, channels []string, msg Message) error {
	if len(channels) == 0 {
		return nil
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, ch := range channels {
		ch := strings.TrimSpace(ch)
		if ch == "" {
			continue
		}
		scheme, _, err := splitScheme(ch)
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("%s: %w", ch, err))
			mu.Unlock()
			continue
		}
		p, ok := r.providers[scheme]
		if !ok {
			mu.Lock()
			errs = append(errs, fmt.Errorf("%s: no provider for scheme %q", ch, scheme))
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(p Provider, ch string) {
			defer wg.Done()
			if err := p.Send(ctx, ch, msg); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", ch, err))
				mu.Unlock()
			}
		}(p, ch)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// splitScheme parses "slack://foo/bar" into ("slack", "foo/bar"). url.Parse
// is too permissive for our taste — we want a hard error if the scheme is
// missing rather than silently routing to "".
func splitScheme(channel string) (scheme, rest string, err error) {
	idx := strings.Index(channel, "://")
	if idx <= 0 {
		return "", "", fmt.Errorf("missing scheme")
	}
	scheme = strings.ToLower(channel[:idx])
	rest = channel[idx+3:]
	if rest == "" {
		return "", "", fmt.Errorf("missing target after scheme")
	}
	return scheme, rest, nil
}

// httpClient is the default transport for all providers. We keep one
// shared client so connection pooling kicks in, with a tight timeout —
// alerters fail fast and let the caller retry on the next cooldown tick.
var httpClient = &http.Client{
	Timeout: 8 * time.Second,
}

// SetHTTPClient lets tests inject a stub. Providers created via the
// public constructors all read from this single client.
func SetHTTPClient(c *http.Client) { httpClient = c }

// rewriteScheme returns the channel URL with its scheme replaced. Useful
// for providers that piggyback on the channel string as a webhook URL
// (Slack), but expose it through a friendly scheme.
func rewriteScheme(channel, newScheme string) (string, error) {
	scheme, rest, err := splitScheme(channel)
	if err != nil {
		return "", err
	}
	_ = scheme
	return newScheme + "://" + rest, nil
}

// parseQuery strips the query off a target string and returns it as a
// url.Values. The remaining target has the query removed.
func parseQuery(target string) (string, url.Values) {
	idx := strings.Index(target, "?")
	if idx < 0 {
		return target, url.Values{}
	}
	q, _ := url.ParseQuery(target[idx+1:])
	return target[:idx], q
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
