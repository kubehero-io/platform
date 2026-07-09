// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package source pulls the advisor's inputs from the control-plane's
// READ-ONLY Connect-JSON RPCs. The advisor never calls mutation RPCs
// (RegisterCluster, AppendAuditEntry, IngestPodCost, ...) and never
// talks to the Kubernetes API — this package is the only data-in path.
package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Snapshot is the compact view of a cluster (or the fleet) the brains
// reason over. Field tags mirror the control-plane's Connect-JSON wire
// names (protojson camelCase).
type Snapshot struct {
	// Origin is "control-plane" or "demo".
	Origin    string                `json:"origin"`
	ClusterID string                `json:"clusterId,omitempty"`
	Window    string                `json:"window"`
	Clusters  []Cluster             `json:"clusters,omitempty"`
	Waste     []WasteRecommendation `json:"wasteRecommendations,omitempty"`
	Anomalies []Anomaly             `json:"anomalies,omitempty"`
	Spend     *TeamSpendReport      `json:"teamSpend,omitempty"`
	BurnRate  *BurnRate             `json:"burnRate,omitempty"`
}

type Cluster struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Cloud  string `json:"cloud"`
	Region string `json:"region"`
	Nodes  int32  `json:"nodes"`
}

type WasteRecommendation struct {
	Rank                string  `json:"rank"`
	Workload            string  `json:"workload"`
	Namespace           string  `json:"namespace"`
	Cluster             string  `json:"cluster"`
	Cloud               string  `json:"cloud"`
	Signal              string  `json:"signal"`
	RecoverableUSDMonth float64 `json:"recoverableUsdMonth"`
	Action              string  `json:"action"`
	Severity            string  `json:"severity"`
}

type Anomaly struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Title          string  `json:"title"`
	Subject        string  `json:"subject"`
	Detail         string  `json:"detail"`
	DeltaPct       float64 `json:"deltaPct"`
	ImpactUSDMonth float64 `json:"impactUsdMonth"`
	Severity       string  `json:"severity"`
	Source         string  `json:"source"`
	LinkPath       string  `json:"linkPath"`
}

type TeamSpend struct {
	Team                string  `json:"team"`
	CostCenter          string  `json:"costCenter"`
	SpendUSDMonth       float64 `json:"spendUsdMonth"`
	RecoverableUSDMonth float64 `json:"recoverableUsdMonth"`
	GPUIdleUSDMonth     float64 `json:"gpuIdleUsdMonth"`
}

type TeamSpendReport struct {
	Teams                    []TeamSpend `json:"teams"`
	FleetTotalUSDMonth       float64     `json:"fleetTotalUsdMonth"`
	FleetRecoverableUSDMonth float64     `json:"fleetRecoverableUsdMonth"`
}

type BurnRate struct {
	BurnRateMilli int32  `json:"burnRateMilli"`
	Available     bool   `json:"available"`
	Source        string `json:"source"`
}

// Source produces a Snapshot for a cluster + window.
type Source interface {
	Fetch(ctx context.Context, clusterID, window string) (*Snapshot, error)
}

// ─── control-plane backed source ─────────────────────────────────────────

// ControlPlane fetches from the control-plane over Connect-JSON (plain
// HTTP POSTs, same hand-rolled pattern as cli/kubehero/internal/client).
type ControlPlane struct {
	BaseURL string
	HTTP    *http.Client
}

func NewControlPlane(baseURL string) *ControlPlane {
	return &ControlPlane{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

const cpService = "kubehero.v1.ControlPlaneService"

func (c *ControlPlane) Fetch(ctx context.Context, clusterID, window string) (*Snapshot, error) {
	snap := &Snapshot{Origin: "control-plane", ClusterID: clusterID, Window: window}
	var errs []error

	var clusters struct {
		Clusters []Cluster `json:"clusters"`
	}
	if err := c.call(ctx, "ListClusters", map[string]any{"pageSize": 50}, &clusters); err != nil {
		errs = append(errs, err)
	} else {
		snap.Clusters = clusters.Clusters
	}

	var waste struct {
		Recommendations []WasteRecommendation `json:"recommendations"`
	}
	if err := c.call(ctx, "ListWasteRecommendations",
		map[string]any{"clusterId": clusterID, "limit": 10}, &waste); err != nil {
		errs = append(errs, err)
	} else {
		snap.Waste = waste.Recommendations
	}

	var anomalies struct {
		Anomalies []Anomaly `json:"anomalies"`
	}
	if err := c.call(ctx, "ListAnomalies",
		map[string]any{"window": window, "limit": 10}, &anomalies); err != nil {
		errs = append(errs, err)
	} else {
		snap.Anomalies = anomalies.Anomalies
	}

	var spend TeamSpendReport
	if err := c.call(ctx, "GetTeamSpend", map[string]any{"window": window}, &spend); err != nil {
		errs = append(errs, err)
	} else {
		snap.Spend = &spend
	}

	// Burn rate needs a ceiling to compare against; use the fleet total
	// as a conservative reference when we have one.
	if snap.Spend != nil && snap.Spend.FleetTotalUSDMonth > 0 {
		var br BurnRate
		if err := c.call(ctx, "GetBurnRate", map[string]any{
			"clusterId":         clusterID,
			"window":            "24h",
			"monthlyCeilingUsd": snap.Spend.FleetTotalUSDMonth,
		}, &br); err != nil {
			errs = append(errs, err)
		} else {
			snap.BurnRate = &br
		}
	}

	// Best-effort: tolerate partial failures, but if nothing at all came
	// back the control-plane is effectively unreachable.
	if snap.Clusters == nil && snap.Waste == nil && snap.Anomalies == nil && snap.Spend == nil {
		return nil, fmt.Errorf("control-plane unreachable at %s: %w", c.BaseURL, errors.Join(errs...))
	}
	return snap, nil
}

// call POSTs a Connect-JSON unary request, mirroring the CLI's client.
func (c *ControlPlane) call(ctx context.Context, method string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/" + cpService + "/" + method
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Connect-Protocol-Version", "1")

	res, err := c.HTTP.Do(hreq)
	if err != nil {
		return fmt.Errorf("rpc %s: %w", method, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 400 {
		return fmt.Errorf("rpc %s: %s · %s", method, res.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
