// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package rpc builds the generated Connect clients the signal commands
// (logs, profile, cost, network, alerts, ask, mcp) use. The older
// hand-rolled JSON client in internal/client keeps serving the original
// fleet commands; both send the same bearer token and honour --insecure.
package rpc

import (
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/kubehero-io/platform/cli/kubehero/internal/config"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

// UnaryTimeout bounds one request/response call; streams (TailLogs,
// InvestigateStream) are bounded by their context instead.
const UnaryTimeout = 30 * time.Second

// maxResponseBytes caps a single response message.
const maxResponseBytes = 32 << 20

// ErrNoEndpoint is returned when no control plane is configured.
var ErrNoEndpoint = errors.New("no endpoint configured · run: kubehero auth login --endpoint=...")

// Clients is every read surface the CLI talks to.
type Clients struct {
	Control  kuberov1connect.ControlPlaneServiceClient
	Cost     kuberov1connect.CostServiceClient
	Logs     kuberov1connect.LogsServiceClient
	Profiles kuberov1connect.ProfilesServiceClient
	Network  kuberov1connect.NetworkServiceClient
	Alerts   kuberov1connect.AlertsServiceClient
	Advisor  kuberov1connect.AdvisorServiceClient

	// HTTP is the plain client (bearer + TLS settings) for non-RPC
	// endpoints such as the FOCUS CSV export.
	HTTP     *http.Client
	Endpoint string
}

// New builds clients from the resolved config.
func New(cfg *config.Config) (*Clients, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, ErrNoEndpoint
	}
	base := strings.TrimRight(cfg.Endpoint, "/")
	advisor := base
	if cfg.AdvisorEndpoint != "" {
		advisor = strings.TrimRight(cfg.AdvisorEndpoint, "/")
	}
	transport := &bearerTransport{token: cfg.Token, inner: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.Insecure}, //nolint:gosec // opt-in dev flag
		MaxIdleConns:        20,
		MaxConnsPerHost:     10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}}
	// No client-level timeout: it would cut long-lived streams. Unary
	// callers wrap their context with UnaryTimeout.
	hc := &http.Client{Transport: transport}
	opts := []connect.ClientOption{connect.WithProtoJSON(), connect.WithReadMaxBytes(maxResponseBytes)}
	return &Clients{
		Control:  kuberov1connect.NewControlPlaneServiceClient(hc, base, opts...),
		Cost:     kuberov1connect.NewCostServiceClient(hc, base, opts...),
		Logs:     kuberov1connect.NewLogsServiceClient(hc, base, opts...),
		Profiles: kuberov1connect.NewProfilesServiceClient(hc, base, opts...),
		Network:  kuberov1connect.NewNetworkServiceClient(hc, base, opts...),
		Alerts:   kuberov1connect.NewAlertsServiceClient(hc, base, opts...),
		Advisor:  kuberov1connect.NewAdvisorServiceClient(hc, advisor, opts...),
		HTTP:     hc,
		Endpoint: base,
	}, nil
}

type bearerTransport struct {
	token string
	inner http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.inner.RoundTrip(req)
}
