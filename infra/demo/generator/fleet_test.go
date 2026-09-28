// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package main

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestFleetIsDeterministic(t *testing.T) {
	a, b := newFleet(42), newFleet(42)
	for i := range a {
		for j, w := range a[i].Workloads {
			for k, p := range w.pods {
				if q := b[i].Workloads[j].pods[k]; p.Name != q.Name || p.UID != q.UID || p.Node.Name != q.Node.Name {
					t.Fatalf("pod %d/%d/%d differs: %s vs %s", i, j, k, p.Name, q.Name)
				}
			}
		}
	}
}

// Node idle + pod costs must add up to the node price, per interval.
func TestCostBatchConservesNodePrice(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	r := rand.New(rand.NewPCG(1, 2))
	for _, c := range newFleet(42) {
		states := snapshot(c, now, now, r)
		pods, nodes := costBatch(c, states, now, 15*time.Second)
		perNode := map[string]float64{}
		for _, p := range pods {
			if p.GetCostUsdSec() <= 0 {
				t.Fatalf("%s/%s: non-positive cost", c.ID, p.GetPod())
			}
			if d := p.GetCpuCostUsdSec() + p.GetRamCostUsdSec() + p.GetGpuCostUsdSec() - p.GetCostUsdSec(); math.Abs(d) > 1e-12 {
				t.Fatalf("%s/%s: split does not add up (%g)", c.ID, p.GetPod(), d)
			}
			perNode[p.GetNode()] += p.GetCostUsdSec()
		}
		for _, n := range nodes {
			got := perNode[n.GetNode()] + n.GetIdleUsdSec()
			// Idle is floored at 0: an overcommitted node can bill more
			// than its price, never less.
			if got+1e-12 < n.GetCostUsdSec() {
				t.Fatalf("%s: pods+idle %.10f < node %.10f", n.GetNode(), got, n.GetCostUsdSec())
			}
		}
	}
}

func TestIncidentShowsUpAcrossSignals(t *testing.T) {
	start := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	before, during := start.Add(-6*time.Hour), start
	r := rand.New(rand.NewPCG(3, 4))
	var eks *cluster
	for _, c := range newFleet(42) {
		if c.ID == "eks-use1-prod" {
			eks = c
		}
	}
	errLines := func(t0 time.Time) int {
		n := 0
		for _, l := range logBatch(eks, snapshot(eks, t0, start, r), t0, start, time.Minute, 1000, r) {
			if l.GetSource().GetWorkload() == "checkout-api" && l.GetLevel() != "info" {
				n++
			}
		}
		return n
	}
	if b, d := errLines(before), errLines(during); d <= b*5 {
		t.Fatalf("incident should multiply checkout warn/error lines: before %d during %d", b, d)
	}
	egress := func(t0 time.Time) uint64 {
		var total uint64
		for _, f := range flowBatch(eks, snapshot(eks, t0, start, r), t0, start, time.Minute, r) {
			if f.GetDst().GetKind() == "external" && f.GetSrc().GetName() == "checkout-api" {
				total += f.GetBytes()
			}
		}
		return total
	}
	if b, d := egress(before), egress(during); d < 3*b {
		t.Fatalf("incident should multiply checkout egress: before %d during %d", b, d)
	}
}
