// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package backend is the advisor's only window onto cluster data: the
// control plane's READ-ONLY Connect RPCs, behind one interface so the
// briefing snapshot, the investigation tools and the rules investigator
// all read through the same code — and so demo mode can swap in
// fixtures without touching any of them.
//
// Deliberately absent: every mutation RPC (RegisterCluster,
// AppendAuditEntry, ArmPolicy, IngestPodCost, UpsertAlertRule,
// CreateSilence, ...). The advisor proposes; it never acts.
package backend

import (
	"context"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

// Backend is the read-only data surface. Implementations must be safe
// for concurrent use (the LLM tool runner executes tools in parallel).
type Backend interface {
	// Origin labels where the data comes from: "control-plane" or "demo".
	Origin() string

	ListClusters(context.Context, *kuberov1.ListClustersRequest) (*kuberov1.ListClustersResponse, error)
	ListWasteRecommendations(context.Context, *kuberov1.ListWasteRecommendationsRequest) (*kuberov1.ListWasteRecommendationsResponse, error)
	ListAnomalies(context.Context, *kuberov1.ListAnomaliesRequest) (*kuberov1.ListAnomaliesResponse, error)
	GetTeamSpend(context.Context, *kuberov1.GetTeamSpendRequest) (*kuberov1.GetTeamSpendResponse, error)
	GetBurnRate(context.Context, *kuberov1.GetBurnRateRequest) (*kuberov1.GetBurnRateResponse, error)
	GetWorkload(context.Context, *kuberov1.GetWorkloadRequest) (*kuberov1.GetWorkloadResponse, error)
	ListCapacityDemands(context.Context, *kuberov1.ListCapacityDemandsRequest) (*kuberov1.ListCapacityDemandsResponse, error)

	GetAllocation(context.Context, *kuberov1.GetAllocationRequest) (*kuberov1.GetAllocationResponse, error)
	GetCostTimeseries(context.Context, *kuberov1.GetCostTimeseriesRequest) (*kuberov1.GetCostTimeseriesResponse, error)
	ListRightsizing(context.Context, *kuberov1.ListRightsizingRequest) (*kuberov1.ListRightsizingResponse, error)
	GetEfficiency(context.Context, *kuberov1.GetEfficiencyRequest) (*kuberov1.GetEfficiencyResponse, error)

	QueryLogs(context.Context, *kuberov1.QueryLogsRequest) (*kuberov1.QueryLogsResponse, error)
	GetLogVolume(context.Context, *kuberov1.GetLogVolumeRequest) (*kuberov1.GetLogVolumeResponse, error)
	GetLogPatterns(context.Context, *kuberov1.GetLogPatternsRequest) (*kuberov1.GetLogPatternsResponse, error)

	ListProfileTargets(context.Context, *kuberov1.ListProfileTargetsRequest) (*kuberov1.ListProfileTargetsResponse, error)
	GetFlamegraph(context.Context, *kuberov1.GetFlamegraphRequest) (*kuberov1.GetFlamegraphResponse, error)
	GetTopFunctions(context.Context, *kuberov1.GetTopFunctionsRequest) (*kuberov1.GetTopFunctionsResponse, error)

	GetServiceMap(context.Context, *kuberov1.GetServiceMapRequest) (*kuberov1.GetServiceMapResponse, error)
	ListNetworkCosts(context.Context, *kuberov1.ListNetworkCostsRequest) (*kuberov1.ListNetworkCostsResponse, error)

	ListAlerts(context.Context, *kuberov1.ListAlertsRequest) (*kuberov1.ListAlertsResponse, error)
}

const (
	// callTimeout bounds one RPC. Investigations run many calls under a
	// 120s budget; one wedged endpoint must not eat all of it.
	callTimeout = 15 * time.Second
	// maxResponseBytes caps a single response held in advisor memory.
	maxResponseBytes = 8 << 20
)

// Connect talks to the control plane with the generated Connect clients
// (Connect-JSON on the wire, same as the CLI and the operator).
type Connect struct {
	control  kuberov1connect.ControlPlaneServiceClient
	cost     kuberov1connect.CostServiceClient
	logs     kuberov1connect.LogsServiceClient
	profiles kuberov1connect.ProfilesServiceClient
	network  kuberov1connect.NetworkServiceClient
	alerts   kuberov1connect.AlertsServiceClient
}

var _ Backend = (*Connect)(nil)

// NewConnect builds the production backend. token, when non-empty, is
// sent as "Authorization: Bearer <token>" on every call (the control
// plane requires it when KUBEHERO_REQUIRE_AUTH is on).
func NewConnect(baseURL, token string) *Connect {
	hc := &http.Client{
		Timeout:   callTimeout,
		Transport: bearerTransport{token: token, inner: http.DefaultTransport},
	}
	base := strings.TrimRight(baseURL, "/")
	opts := []connect.ClientOption{connect.WithProtoJSON(), connect.WithReadMaxBytes(maxResponseBytes)}
	return &Connect{
		control:  kuberov1connect.NewControlPlaneServiceClient(hc, base, opts...),
		cost:     kuberov1connect.NewCostServiceClient(hc, base, opts...),
		logs:     kuberov1connect.NewLogsServiceClient(hc, base, opts...),
		profiles: kuberov1connect.NewProfilesServiceClient(hc, base, opts...),
		network:  kuberov1connect.NewNetworkServiceClient(hc, base, opts...),
		alerts:   kuberov1connect.NewAlertsServiceClient(hc, base, opts...),
	}
}

type bearerTransport struct {
	token string
	inner http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.inner.RoundTrip(req)
}

func (*Connect) Origin() string { return "control-plane" }

// unary adapts a generated client method to the plain request/response
// shape of Backend.
func unary[Req, Res any](
	ctx context.Context,
	call func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error),
	req *Req,
) (*Res, error) {
	res, err := call(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (c *Connect) ListClusters(ctx context.Context, r *kuberov1.ListClustersRequest) (*kuberov1.ListClustersResponse, error) {
	return unary(ctx, c.control.ListClusters, r)
}

func (c *Connect) ListWasteRecommendations(ctx context.Context, r *kuberov1.ListWasteRecommendationsRequest) (*kuberov1.ListWasteRecommendationsResponse, error) {
	return unary(ctx, c.control.ListWasteRecommendations, r)
}

func (c *Connect) ListAnomalies(ctx context.Context, r *kuberov1.ListAnomaliesRequest) (*kuberov1.ListAnomaliesResponse, error) {
	return unary(ctx, c.control.ListAnomalies, r)
}

func (c *Connect) GetTeamSpend(ctx context.Context, r *kuberov1.GetTeamSpendRequest) (*kuberov1.GetTeamSpendResponse, error) {
	return unary(ctx, c.control.GetTeamSpend, r)
}

func (c *Connect) GetBurnRate(ctx context.Context, r *kuberov1.GetBurnRateRequest) (*kuberov1.GetBurnRateResponse, error) {
	return unary(ctx, c.control.GetBurnRate, r)
}

func (c *Connect) GetWorkload(ctx context.Context, r *kuberov1.GetWorkloadRequest) (*kuberov1.GetWorkloadResponse, error) {
	return unary(ctx, c.control.GetWorkload, r)
}

func (c *Connect) ListCapacityDemands(ctx context.Context, r *kuberov1.ListCapacityDemandsRequest) (*kuberov1.ListCapacityDemandsResponse, error) {
	return unary(ctx, c.control.ListCapacityDemands, r)
}

func (c *Connect) GetAllocation(ctx context.Context, r *kuberov1.GetAllocationRequest) (*kuberov1.GetAllocationResponse, error) {
	return unary(ctx, c.cost.GetAllocation, r)
}

func (c *Connect) GetCostTimeseries(ctx context.Context, r *kuberov1.GetCostTimeseriesRequest) (*kuberov1.GetCostTimeseriesResponse, error) {
	return unary(ctx, c.cost.GetCostTimeseries, r)
}

func (c *Connect) ListRightsizing(ctx context.Context, r *kuberov1.ListRightsizingRequest) (*kuberov1.ListRightsizingResponse, error) {
	return unary(ctx, c.cost.ListRightsizing, r)
}

func (c *Connect) GetEfficiency(ctx context.Context, r *kuberov1.GetEfficiencyRequest) (*kuberov1.GetEfficiencyResponse, error) {
	return unary(ctx, c.cost.GetEfficiency, r)
}

func (c *Connect) QueryLogs(ctx context.Context, r *kuberov1.QueryLogsRequest) (*kuberov1.QueryLogsResponse, error) {
	return unary(ctx, c.logs.QueryLogs, r)
}

func (c *Connect) GetLogVolume(ctx context.Context, r *kuberov1.GetLogVolumeRequest) (*kuberov1.GetLogVolumeResponse, error) {
	return unary(ctx, c.logs.GetLogVolume, r)
}

func (c *Connect) GetLogPatterns(ctx context.Context, r *kuberov1.GetLogPatternsRequest) (*kuberov1.GetLogPatternsResponse, error) {
	return unary(ctx, c.logs.GetLogPatterns, r)
}

func (c *Connect) ListProfileTargets(ctx context.Context, r *kuberov1.ListProfileTargetsRequest) (*kuberov1.ListProfileTargetsResponse, error) {
	return unary(ctx, c.profiles.ListProfileTargets, r)
}

func (c *Connect) GetFlamegraph(ctx context.Context, r *kuberov1.GetFlamegraphRequest) (*kuberov1.GetFlamegraphResponse, error) {
	return unary(ctx, c.profiles.GetFlamegraph, r)
}

func (c *Connect) GetTopFunctions(ctx context.Context, r *kuberov1.GetTopFunctionsRequest) (*kuberov1.GetTopFunctionsResponse, error) {
	return unary(ctx, c.profiles.GetTopFunctions, r)
}

func (c *Connect) GetServiceMap(ctx context.Context, r *kuberov1.GetServiceMapRequest) (*kuberov1.GetServiceMapResponse, error) {
	return unary(ctx, c.network.GetServiceMap, r)
}

func (c *Connect) ListNetworkCosts(ctx context.Context, r *kuberov1.ListNetworkCostsRequest) (*kuberov1.ListNetworkCostsResponse, error) {
	return unary(ctx, c.network.ListNetworkCosts, r)
}

func (c *Connect) ListAlerts(ctx context.Context, r *kuberov1.ListAlertsRequest) (*kuberov1.ListAlertsResponse, error) {
	return unary(ctx, c.alerts.ListAlerts, r)
}
