// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

// RightsizingSource is where the RightsizingPolicy reconciler gets its
// per-container recommendations. The operator never computes them
// itself: the control plane owns the usage history (ClickHouse) and the
// percentile math, and the operator only decides whether it is SAFE to
// act on what comes back.
type RightsizingSource interface {
	ListRightsizing(ctx context.Context, req *kuberov1.ListRightsizingRequest) (*kuberov1.ListRightsizingResponse, error)
}

const (
	// rightsizingTimeout bounds one ListRightsizing round trip. A policy
	// evaluates every 10 minutes, so a slow control plane just delays
	// one namespace — it never wedges the work queue.
	rightsizingTimeout = 10 * time.Second

	// rightsizingMaxResponseBytes caps what one namespace's
	// recommendations may occupy in operator memory.
	rightsizingMaxResponseBytes = 16 << 20
)

// ControlPlaneRightsizing calls CostService.ListRightsizing over Connect
// JSON with the generated client — the same wire shape and bearer-token
// handling as the burn-rate provider.
type ControlPlaneRightsizing struct {
	client kuberov1connect.CostServiceClient
}

// NewControlPlaneRightsizing builds the production source. baseURL is the
// control plane's HTTP endpoint (CONTROL_PLANE_URL); token, when set, is
// sent as a Bearer Authorization header.
func NewControlPlaneRightsizing(baseURL, token string) *ControlPlaneRightsizing {
	httpClient := &http.Client{
		Timeout:   rightsizingTimeout,
		Transport: bearerTransport{token: token, inner: http.DefaultTransport},
	}
	return &ControlPlaneRightsizing{
		client: kuberov1connect.NewCostServiceClient(
			httpClient,
			strings.TrimRight(baseURL, "/"),
			connect.WithProtoJSON(),
			connect.WithReadMaxBytes(rightsizingMaxResponseBytes),
		),
	}
}

func (c *ControlPlaneRightsizing) ListRightsizing(
	ctx context.Context,
	req *kuberov1.ListRightsizingRequest,
) (*kuberov1.ListRightsizingResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, rightsizingTimeout)
	defer cancel()
	resp, err := c.client.ListRightsizing(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, fmt.Errorf("control-plane ListRightsizing: %w", err)
	}
	return resp.Msg, nil
}
