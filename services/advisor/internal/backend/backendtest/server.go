// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package backendtest serves any backend.Backend as a fake control plane
// over real Connect handlers (the generated ones), so tests exercise the
// production client, wire format and auth header end to end.
package backendtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
)

// Server is a running fake control plane.
type Server struct {
	*httptest.Server
	mu    sync.Mutex
	calls map[string]int
}

// Calls reports how many times each RPC (by bare method name) was hit.
func (s *Server) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *Server) count(method string) {
	s.mu.Lock()
	s.calls[method]++
	s.mu.Unlock()
}

// New starts a fake control plane backed by b. When token is non-empty
// every request must carry "Authorization: Bearer <token>" or gets
// CodeUnauthenticated — the same contract as KUBEHERO_REQUIRE_AUTH.
func New(t testing.TB, b backend.Backend, token string) *Server {
	t.Helper()
	s := &Server{calls: map[string]int{}}
	h := &handlers{b: b, s: s}
	opts := connect.WithInterceptors(authInterceptor(token))
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewControlPlaneServiceHandler(h, opts))
	mux.Handle(kuberov1connect.NewCostServiceHandler(h, opts))
	mux.Handle(kuberov1connect.NewLogsServiceHandler(h, opts))
	mux.Handle(kuberov1connect.NewProfilesServiceHandler(h, opts))
	mux.Handle(kuberov1connect.NewNetworkServiceHandler(h, opts))
	mux.Handle(kuberov1connect.NewAlertsServiceHandler(h, opts))
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func authInterceptor(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" && req.Header().Get("Authorization") != "Bearer "+token {
				return nil, connect.NewError(connect.CodeUnauthenticated, nil)
			}
			return next(ctx, req)
		}
	}
}

type handlers struct {
	kuberov1connect.UnimplementedControlPlaneServiceHandler
	kuberov1connect.UnimplementedCostServiceHandler
	kuberov1connect.UnimplementedLogsServiceHandler
	kuberov1connect.UnimplementedProfilesServiceHandler
	kuberov1connect.UnimplementedNetworkServiceHandler
	kuberov1connect.UnimplementedAlertsServiceHandler
	b backend.Backend
	s *Server
}

func serve[Req, Res any](h *handlers, method string, ctx context.Context, req *connect.Request[Req],
	f func(context.Context, *Req) (*Res, error)) (*connect.Response[Res], error) {
	h.s.count(method)
	res, err := f(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (h *handlers) ListClusters(ctx context.Context, r *connect.Request[kuberov1.ListClustersRequest]) (*connect.Response[kuberov1.ListClustersResponse], error) {
	return serve(h, "ListClusters", ctx, r, h.b.ListClusters)
}

func (h *handlers) ListWasteRecommendations(ctx context.Context, r *connect.Request[kuberov1.ListWasteRecommendationsRequest]) (*connect.Response[kuberov1.ListWasteRecommendationsResponse], error) {
	return serve(h, "ListWasteRecommendations", ctx, r, h.b.ListWasteRecommendations)
}

func (h *handlers) ListAnomalies(ctx context.Context, r *connect.Request[kuberov1.ListAnomaliesRequest]) (*connect.Response[kuberov1.ListAnomaliesResponse], error) {
	return serve(h, "ListAnomalies", ctx, r, h.b.ListAnomalies)
}

func (h *handlers) GetTeamSpend(ctx context.Context, r *connect.Request[kuberov1.GetTeamSpendRequest]) (*connect.Response[kuberov1.GetTeamSpendResponse], error) {
	return serve(h, "GetTeamSpend", ctx, r, h.b.GetTeamSpend)
}

func (h *handlers) GetBurnRate(ctx context.Context, r *connect.Request[kuberov1.GetBurnRateRequest]) (*connect.Response[kuberov1.GetBurnRateResponse], error) {
	return serve(h, "GetBurnRate", ctx, r, h.b.GetBurnRate)
}

func (h *handlers) GetWorkload(ctx context.Context, r *connect.Request[kuberov1.GetWorkloadRequest]) (*connect.Response[kuberov1.GetWorkloadResponse], error) {
	return serve(h, "GetWorkload", ctx, r, h.b.GetWorkload)
}

func (h *handlers) ListCapacityDemands(ctx context.Context, r *connect.Request[kuberov1.ListCapacityDemandsRequest]) (*connect.Response[kuberov1.ListCapacityDemandsResponse], error) {
	return serve(h, "ListCapacityDemands", ctx, r, h.b.ListCapacityDemands)
}

func (h *handlers) GetAllocation(ctx context.Context, r *connect.Request[kuberov1.GetAllocationRequest]) (*connect.Response[kuberov1.GetAllocationResponse], error) {
	return serve(h, "GetAllocation", ctx, r, h.b.GetAllocation)
}

func (h *handlers) GetCostTimeseries(ctx context.Context, r *connect.Request[kuberov1.GetCostTimeseriesRequest]) (*connect.Response[kuberov1.GetCostTimeseriesResponse], error) {
	return serve(h, "GetCostTimeseries", ctx, r, h.b.GetCostTimeseries)
}

func (h *handlers) ListRightsizing(ctx context.Context, r *connect.Request[kuberov1.ListRightsizingRequest]) (*connect.Response[kuberov1.ListRightsizingResponse], error) {
	return serve(h, "ListRightsizing", ctx, r, h.b.ListRightsizing)
}

func (h *handlers) GetEfficiency(ctx context.Context, r *connect.Request[kuberov1.GetEfficiencyRequest]) (*connect.Response[kuberov1.GetEfficiencyResponse], error) {
	return serve(h, "GetEfficiency", ctx, r, h.b.GetEfficiency)
}

func (h *handlers) QueryLogs(ctx context.Context, r *connect.Request[kuberov1.QueryLogsRequest]) (*connect.Response[kuberov1.QueryLogsResponse], error) {
	return serve(h, "QueryLogs", ctx, r, h.b.QueryLogs)
}

func (h *handlers) GetLogVolume(ctx context.Context, r *connect.Request[kuberov1.GetLogVolumeRequest]) (*connect.Response[kuberov1.GetLogVolumeResponse], error) {
	return serve(h, "GetLogVolume", ctx, r, h.b.GetLogVolume)
}

func (h *handlers) GetLogPatterns(ctx context.Context, r *connect.Request[kuberov1.GetLogPatternsRequest]) (*connect.Response[kuberov1.GetLogPatternsResponse], error) {
	return serve(h, "GetLogPatterns", ctx, r, h.b.GetLogPatterns)
}

func (h *handlers) ListProfileTargets(ctx context.Context, r *connect.Request[kuberov1.ListProfileTargetsRequest]) (*connect.Response[kuberov1.ListProfileTargetsResponse], error) {
	return serve(h, "ListProfileTargets", ctx, r, h.b.ListProfileTargets)
}

func (h *handlers) GetFlamegraph(ctx context.Context, r *connect.Request[kuberov1.GetFlamegraphRequest]) (*connect.Response[kuberov1.GetFlamegraphResponse], error) {
	return serve(h, "GetFlamegraph", ctx, r, h.b.GetFlamegraph)
}

func (h *handlers) GetTopFunctions(ctx context.Context, r *connect.Request[kuberov1.GetTopFunctionsRequest]) (*connect.Response[kuberov1.GetTopFunctionsResponse], error) {
	return serve(h, "GetTopFunctions", ctx, r, h.b.GetTopFunctions)
}

func (h *handlers) GetServiceMap(ctx context.Context, r *connect.Request[kuberov1.GetServiceMapRequest]) (*connect.Response[kuberov1.GetServiceMapResponse], error) {
	return serve(h, "GetServiceMap", ctx, r, h.b.GetServiceMap)
}

func (h *handlers) ListNetworkCosts(ctx context.Context, r *connect.Request[kuberov1.ListNetworkCostsRequest]) (*connect.Response[kuberov1.ListNetworkCostsResponse], error) {
	return serve(h, "ListNetworkCosts", ctx, r, h.b.ListNetworkCosts)
}

func (h *handlers) ListAlerts(ctx context.Context, r *connect.Request[kuberov1.ListAlertsRequest]) (*connect.Response[kuberov1.ListAlertsResponse], error) {
	return serve(h, "ListAlerts", ctx, r, h.b.ListAlerts)
}
