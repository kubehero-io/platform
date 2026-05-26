// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BurnRateProvider returns the current burn rate (× 1000) for a budget
// over the given window, e.g. 1500 means "spending at 1.5× budget".
//
// Implementations:
//   - The control-plane's ClickHouse-backed reader (production).
//   - A static stub that always returns 0 (for chart smoke tests, kind
//     demo, and unit tests).
//
// We keep the surface tiny so the reconciler is testable without
// pulling ClickHouse client + queries into the operator module.
type BurnRateProvider interface {
	// BurnRateMilli reads the most recent burn rate over `window`
	// (e.g. "5m", "1h") for the budget identified by namespace/name.
	// Returns 0 + nil error when no data is available — the operator
	// treats that as "below trigger".
	BurnRateMilli(ctx context.Context, namespace, name, window string) (int32, error)
}

// StubBurnRate always reports 0. The operator runs with this when the
// control-plane endpoint is unset, so policies stay observe-only and
// `helm test kubehero` does not require a live data plane.
type StubBurnRate struct{}

func (StubBurnRate) BurnRateMilli(_ context.Context, _, _, _ string) (int32, error) {
	return 0, nil
}

// FixedBurnRate is a test double — useful for table-driven reconciler
// tests where you want a specific reading deterministically.
type FixedBurnRate struct{ Value int32 }

func (f FixedBurnRate) BurnRateMilli(_ context.Context, _, _, _ string) (int32, error) {
	return f.Value, nil
}

// RPCBurnRate calls the control-plane's GetBurnRate RPC. It needs the
// monthly ceiling in dollars for the policy under evaluation; the
// reconciler computes that locally from BudgetPolicy.Spec.Ceiling and
// passes it via a closure (CeilingResolver). When the resolver returns
// 0 we shortcut to (0, nil) — operator's evaluator treats that as
// below-trigger, never tripping without a real budget.
type RPCBurnRate struct {
	Endpoint  string
	Token     string
	HTTP      *http.Client
	ClusterID string

	// CeilingResolver maps a (namespace, budgetRef) to the parsed
	// monthly USD ceiling. The reconciler injects this with a
	// k8s-client-backed function that reads the matching BudgetPolicy.
	CeilingResolver func(ctx context.Context, namespace, budgetRef string) (float64, error)
}

func (r *RPCBurnRate) BurnRateMilli(ctx context.Context, namespace, budgetRef, window string) (int32, error) {
	if r.Endpoint == "" {
		return 0, nil
	}
	if r.HTTP == nil {
		r.HTTP = &http.Client{Timeout: 5 * time.Second}
	}

	monthlyCeiling := 0.0
	if r.CeilingResolver != nil {
		v, err := r.CeilingResolver(ctx, namespace, budgetRef)
		if err != nil {
			return 0, fmt.Errorf("resolve ceiling: %w", err)
		}
		monthlyCeiling = v
	}
	if monthlyCeiling <= 0 {
		// No parseable ceiling → no meaningful burn rate.
		return 0, nil
	}

	body, err := json.Marshal(map[string]any{
		"clusterId":         r.ClusterID,
		"namespace":         namespace,
		"window":            window,
		"monthlyCeilingUsd": monthlyCeiling,
	})
	if err != nil {
		return 0, err
	}
	url := strings.TrimRight(r.Endpoint, "/") + "/kubehero.v1.ControlPlaneService/GetBurnRate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("burn-rate rpc: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("burn-rate rpc: http %d · %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		BurnRateMilli int32 `json:"burnRateMilli"`
		Available     bool  `json:"available"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if !out.Available {
		return 0, nil
	}
	return out.BurnRateMilli, nil
}

// ParseCeilingUSD turns a BudgetPolicy.Spec.Ceiling string ("$100000/mo",
// "$300/hr", "5000") into a monthly USD number. Returns 0 on failure.
func ParseCeilingUSD(s string) float64 {
	s = strings.TrimSpace(s)
	m := ceilingRE.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	raw := strings.ReplaceAll(m[1], ",", "")
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(m[2]) {
	case "hr", "hour":
		return v * 24 * 30
	default:
		return v
	}
}

var ceilingRE = regexp.MustCompile(`^\$?([0-9][0-9,]*(?:\.[0-9]+)?)(?:\s*/?\s*(mo|month|hr|hour))?$`)
