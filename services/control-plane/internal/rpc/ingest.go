// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/batcher"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/telemetry"
)

// Per-request caps for IngestPodCost. A collector reports one node's
// pods every scan (a few hundred at most); bigger batches are a client
// bug and are refused whole rather than half-written.
const (
	maxPodCostSamples = 20_000
	maxNodeSamples    = 1_000
)

// ingestPodCost implements IngestPodCost (see its doc in control.go).
//
// Cluster attribution per sample: the sample's own cluster, else the
// request's cluster_id, else the cluster of the enrollment token the
// caller authenticated with. A cluster-scoped token can only write its
// own cluster: a request naming another cluster is refused, and
// samples naming another cluster are dropped.
func (c *ControlPlane) ingestPodCost(
	ctx context.Context,
	req *connect.Request[kuberov1.IngestPodCostRequest],
) (*connect.Response[kuberov1.IngestPodCostResponse], error) {
	if err := auth.Require(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	in, nodes := req.Msg.GetSamples(), req.Msg.GetNodes()
	switch {
	case len(in) > maxPodCostSamples:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("at most %d samples per request, got %d", maxPodCostSamples, len(in)))
	case len(nodes) > maxNodeSamples:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("at most %d node samples per request, got %d", maxNodeSamples, len(nodes)))
	case len(in)+len(nodes) == 0:
		return connect.NewResponse(&kuberov1.IngestPodCostResponse{}), nil
	}
	defaultCluster, err := telemetry.ResolveCluster(ctx, req.Msg.GetClusterId())
	if err != nil {
		return nil, err
	}
	if c.PodCost == nil {
		// Stub mode: accept the batch so the kind-demo + unit-test
		// paths don't fail when ClickHouse isn't wired, but report every
		// row as dropped so the caller can tell.
		return connect.NewResponse(&kuberov1.IngestPodCostResponse{
			Written: 0,
			Dropped: int32(len(in) + len(nodes)),
		}), nil
	}

	scoped := auth.PrincipalFromContext(ctx).ClusterID
	dropped := 0
	samples := make([]clickhouse.Sample, 0, len(in))
	for _, s := range in {
		cluster := defaultCluster
		if own := strings.TrimSpace(s.GetCluster()); own != "" {
			resolved, err := telemetry.ResolveCluster(ctx, own)
			if err != nil || (scoped != "" && resolved != scoped) {
				dropped++
				continue
			}
			cluster = resolved
		}
		samples = append(samples, podSample(cluster, s))
	}
	nodeRows := make([]clickhouse.NodeSample, 0, len(nodes))
	for _, n := range nodes {
		nodeRows = append(nodeRows, nodeSample(defaultCluster, n))
	}

	res, err := c.PodCost.Write(ctx, samples, nodeRows)
	if err != nil {
		code := connect.CodeUnavailable // ClickHouse unreachable: the collector retries
		if errors.Is(err, batcher.ErrFull) {
			code = connect.CodeResourceExhausted
		}
		return nil, connect.NewError(code, fmt.Errorf("clickhouse write: %w", err))
	}
	return connect.NewResponse(&kuberov1.IngestPodCostResponse{
		Written: int32(res.Written),
		Dropped: int32(res.Dropped + dropped),
	}), nil
}

func podSample(cluster string, s *kuberov1.PodCostSample) clickhouse.Sample {
	return clickhouse.Sample{
		OrgID:         "default", // resolved per-cluster once orgs are wired
		ClusterID:     cluster,
		Node:          s.GetNode(),
		Namespace:     s.GetNamespace(),
		Pod:           s.GetPod(),
		Team:          s.GetTeam(),
		CostCenter:    s.GetCostCenter(),
		Nodepool:      s.GetNodepool(),
		Cloud:         strings.ToLower(s.GetCloud()),
		Region:        s.GetRegion(),
		Zone:          s.GetZone(),
		SKU:           s.GetSku(),
		Lifecycle:     s.GetLifecycle(),
		GPUKind:       s.GetGpuKind(),
		Workload:      s.GetWorkload(),
		WorkloadKind:  s.GetWorkloadKind(),
		CPUMilli:      s.GetCpuMillicores(),
		MemBytes:      s.GetMemBytes(),
		CPUUsageMilli: s.GetCpuUsageMillicores(),
		MemUsageBytes: s.GetMemUsageBytes(),
		GPUUtilPct:    s.GetGpuUtilPct(),
		GPUCount:      s.GetGpuCount(),
		CostUSDSec:    s.GetCostUsdSec(),
		RecoverUSD:    s.GetRecoverableUsdSec(),
		CPUCostUSDSec: s.GetCpuCostUsdSec(),
		RAMCostUSDSec: s.GetRamCostUsdSec(),
		GPUCostUSDSec: s.GetGpuCostUsdSec(),
		IntervalSec:   s.GetIntervalSec(),
		TsUnixMS:      s.GetTsUnixMs(),
		Labels:        s.GetLabels(),
	}
}

func nodeSample(cluster string, n *kuberov1.NodeCostSample) clickhouse.NodeSample {
	return clickhouse.NodeSample{
		OrgID:         "default",
		ClusterID:     cluster,
		Node:          n.GetNode(),
		Nodepool:      n.GetNodepool(),
		Cloud:         strings.ToLower(n.GetCloud()),
		Region:        n.GetRegion(),
		Zone:          n.GetZone(),
		SKU:           n.GetSku(),
		Lifecycle:     n.GetLifecycle(),
		GPUKind:       n.GetGpuKind(),
		GPUCount:      n.GetGpuCount(),
		PricePerHour:  n.GetPricePerHour(),
		PriceSource:   n.GetPriceSource(),
		CPUAllocMilli: n.GetCpuAllocatableMillicores(),
		MemAllocBytes: n.GetMemAllocatableBytes(),
		CPUReqMilli:   n.GetCpuRequestedMillicores(),
		MemReqBytes:   n.GetMemRequestedBytes(),
		CPUUsedMilli:  n.GetCpuUsedMillicores(),
		MemUsedBytes:  n.GetMemUsedBytes(),
		CostUSDSec:    n.GetCostUsdSec(),
		IdleUSDSec:    n.GetIdleUsdSec(),
		IntervalSec:   n.GetIntervalSec(),
		TsUnixMS:      n.GetTsUnixMs(),
	}
}
