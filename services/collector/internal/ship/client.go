// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package ship is the collector's only path to the control plane. It
// wraps the generated Connect clients (protobuf binary, gzip-compressed
// requests, bearer token, collector User-Agent) and puts every signal
// behind a bounded in-memory queue with bounded, jittered retries.
//
// The queue is the outage story: when the control plane is down or slow,
// batches accumulate up to a fixed item/byte budget and the oldest are
// dropped (and counted) beyond it, so a long outage costs a bounded
// amount of memory and a burst of lost-but-accounted data — never an
// OOM-killed DaemonSet on every node at once. Retries are safe because
// every ingest RPC is idempotent on the server (re-sent windows dedupe).
package ship

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

// Options configures the control-plane clients.
type Options struct {
	// URL is the control plane's base URL (http:// or https://).
	URL string
	// Token is sent as "Authorization: Bearer <token>". Never logged.
	Token     string
	UserAgent string
	// HTTPClient overrides the default transport (tests).
	HTTPClient *http.Client
}

// Client bundles the generated clients the collector uses.
type Client struct {
	ControlPlane kuberov1connect.ControlPlaneServiceClient
	Telemetry    kuberov1connect.TelemetryServiceClient
}

// NewClient builds Connect clients for the control plane.
func NewClient(opts Options) (*Client, error) {
	if opts.URL == "" {
		return nil, errors.New("control plane url is empty")
	}
	if !strings.HasPrefix(opts.URL, "http://") && !strings.HasPrefix(opts.URL, "https://") {
		return nil, errors.New("control plane url must start with http:// or https://")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = NewHTTPClient()
	}
	clientOpts := []connect.ClientOption{
		connect.WithSendGzip(),
		connect.WithInterceptors(headerInterceptor(opts.Token, opts.UserAgent)),
	}
	return &Client{
		ControlPlane: kuberov1connect.NewControlPlaneServiceClient(hc, opts.URL, clientOpts...),
		Telemetry:    kuberov1connect.NewTelemetryServiceClient(hc, opts.URL, clientOpts...),
	}, nil
}

// NewPricingClient builds a PricingService client (no auth: the pricing
// engine is an in-cluster service without principals).
func NewPricingClient(url, userAgent string) (kuberov1connect.PricingServiceClient, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, errors.New("pricing engine url must start with http:// or https://")
	}
	return kuberov1connect.NewPricingServiceClient(NewHTTPClient(), url,
		connect.WithInterceptors(headerInterceptor("", userAgent))), nil
}

// NewHTTPClient is the shared transport: bounded dials and handshakes,
// a small idle pool (one collector talks to one control plane), HTTP/2
// negotiated on TLS. Call deadlines come from per-call contexts, not a
// client-wide timeout, so a 30s profile upload and a 5s cost batch can
// share it.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
}

func headerInterceptor(token, userAgent string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().IsClient {
				if token != "" {
					req.Header().Set("Authorization", "Bearer "+token)
				}
				if userAgent != "" {
					req.Header().Set("User-Agent", userAgent)
				}
			}
			return next(ctx, req)
		}
	}
}

// ── senders: adapt each RPC to the queue's Sender shape ───────────────

// SendPodCost ships an IngestPodCostRequest.
func (c *Client) SendPodCost(ctx context.Context, req *kuberov1.IngestPodCostRequest) (int, int, error) {
	resp, err := c.ControlPlane.IngestPodCost(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, 0, err
	}
	return int(resp.Msg.GetWritten()), int(resp.Msg.GetDropped()), nil
}

// SendUsage ships an IngestUsageRequest.
func (c *Client) SendUsage(ctx context.Context, req *kuberov1.IngestUsageRequest) (int, int, error) {
	resp, err := c.Telemetry.IngestUsage(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, 0, err
	}
	return int(resp.Msg.GetAccepted()), int(resp.Msg.GetDropped()), nil
}

// SendEvents ships an IngestEventsRequest.
func (c *Client) SendEvents(ctx context.Context, req *kuberov1.IngestEventsRequest) (int, int, error) {
	resp, err := c.Telemetry.IngestEvents(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, 0, err
	}
	return int(resp.Msg.GetAccepted()), int(resp.Msg.GetDropped()), nil
}

// SendLogs ships an IngestLogsRequest.
func (c *Client) SendLogs(ctx context.Context, req *kuberov1.IngestLogsRequest) (int, int, error) {
	resp, err := c.Telemetry.IngestLogs(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, 0, err
	}
	return int(resp.Msg.GetAccepted()), int(resp.Msg.GetDropped()), nil
}

// SendProfiles ships an IngestProfilesRequest.
func (c *Client) SendProfiles(ctx context.Context, req *kuberov1.IngestProfilesRequest) (int, int, error) {
	resp, err := c.Telemetry.IngestProfiles(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, 0, err
	}
	return int(resp.Msg.GetAccepted()), int(resp.Msg.GetDropped()), nil
}

// SendFlows ships an IngestFlowsRequest.
func (c *Client) SendFlows(ctx context.Context, req *kuberov1.IngestFlowsRequest) (int, int, error) {
	resp, err := c.Telemetry.IngestFlows(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, 0, err
	}
	return int(resp.Msg.GetAccepted()), int(resp.Msg.GetDropped()), nil
}
