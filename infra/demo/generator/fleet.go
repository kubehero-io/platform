// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// The demo fleet: three clusters on three clouds with the workloads the
// docs, site and dashboard fixtures talk about. Everything here is
// synthetic but internally consistent — pod cost is priced from the
// node it runs on, idle is what's left of the node, and the stories
// (checkout retry storm, OOM-ing payments worker, idle A100s, an ETL job
// that can't schedule) show up coherently across cost, usage, logs,
// profiles, flows and events.

type cluster struct {
	ID, Cloud, Region string
	Zones             []string
	Pools             []*nodepool
	Workloads         []*workload
	Nodes             []*node
}

type nodepool struct {
	Name, SKU, Lifecycle, GPUKind string
	Count                         int
	CPU                           float64 // allocatable cores
	MemGiB                        float64
	GPUs                          int
	PricePerHour                  float64
}

type node struct {
	Name, Zone string
	Pool       *nodepool
}

type workload struct {
	Namespace, Name, Kind, Team, CostCenter string
	Replicas                                int
	CPUReq, MemReqGiB                       float64 // per replica
	CPULimit, MemLimitGiB                   float64
	GPUs                                    int
	Pool                                    string
	// Usage model: base fraction of request, diurnal swing, noise.
	CPUUse, MemUse, Diurnal, Noise float64
	// Optional behaviours.
	RetryStorm   bool // checkout-api: the incident
	OOMs         bool // payments-worker: runs out of memory
	BatchWindow  [2]int
	GPUUtil      float64
	Unschedulable bool
	pods         []*pod
}

type pod struct {
	Name, UID string
	Node      *node
	Restarts  uint32
}

// Incident timing, relative to generator start: the checkout retry
// storm began incidentAge ago and is still going.
const incidentAge = 3 * time.Hour

func gib(v float64) uint64 { return uint64(v * (1 << 30)) }

func newFleet(seed uint64) []*cluster {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	fleet := []*cluster{
		{
			ID: "eks-use1-prod", Cloud: "aws", Region: "us-east-1",
			Zones: []string{"us-east-1a", "us-east-1b", "us-east-1c"},
			Pools: []*nodepool{
				{Name: "general", SKU: "m7i.2xlarge", Lifecycle: "on-demand", Count: 6, CPU: 7.9, MemGiB: 30, PricePerHour: 0.4032},
				{Name: "spot-batch", SKU: "c7i.4xlarge", Lifecycle: "spot", Count: 2, CPU: 15.8, MemGiB: 30, PricePerHour: 0.2856},
			},
			Workloads: []*workload{
				{Namespace: "edge", Name: "frontend-gateway", Kind: "Deployment", Team: "platform", CostCenter: "cc-100", Replicas: 4, CPUReq: 2, MemReqGiB: 2, CPULimit: 4, MemLimitGiB: 4, Pool: "general", CPUUse: 0.17, MemUse: 0.35, Diurnal: 0.6, Noise: 0.08},
				{Namespace: "checkout", Name: "checkout-api", Kind: "Deployment", Team: "payments", CostCenter: "cc-210", Replicas: 6, CPUReq: 1, MemReqGiB: 1.5, CPULimit: 2, MemLimitGiB: 3, Pool: "general", CPUUse: 0.55, MemUse: 0.6, Diurnal: 0.5, Noise: 0.07, RetryStorm: true},
				{Namespace: "payments", Name: "payments-worker", Kind: "Deployment", Team: "payments", CostCenter: "cc-210", Replicas: 3, CPUReq: 0.5, MemReqGiB: 1, CPULimit: 1, MemLimitGiB: 1.25, Pool: "general", CPUUse: 0.7, MemUse: 1.12, Diurnal: 0.3, Noise: 0.05, OOMs: true},
				{Namespace: "payments", Name: "postgres", Kind: "StatefulSet", Team: "payments", CostCenter: "cc-210", Replicas: 1, CPUReq: 2, MemReqGiB: 8, CPULimit: 4, MemLimitGiB: 8, Pool: "general", CPUUse: 0.4, MemUse: 0.75, Diurnal: 0.4, Noise: 0.04},
				{Namespace: "growth", Name: "recommendation-svc", Kind: "Deployment", Team: "growth", CostCenter: "cc-330", Replicas: 2, CPUReq: 4, MemReqGiB: 8, CPULimit: 4, MemLimitGiB: 8, Pool: "general", CPUUse: 0.12, MemUse: 0.3, Diurnal: 0.3, Noise: 0.05},
				{Namespace: "data-platform", Name: "event-compactor", Kind: "CronJob", Team: "data", CostCenter: "cc-400", Replicas: 2, CPUReq: 6, MemReqGiB: 12, CPULimit: 8, MemLimitGiB: 16, Pool: "spot-batch", CPUUse: 0.8, MemUse: 0.6, Noise: 0.1, BatchWindow: [2]int{1, 4}},
			},
		},
		{
			ID: "gke-euw4-batch", Cloud: "gcp", Region: "europe-west4",
			Zones: []string{"europe-west4-a", "europe-west4-b"},
			Pools: []*nodepool{
				{Name: "batch", SKU: "n2-standard-8", Lifecycle: "on-demand", Count: 4, CPU: 7.9, MemGiB: 30, PricePerHour: 0.3886},
			},
			Workloads: []*workload{
				{Namespace: "data-platform", Name: "retrieval-indexer", Kind: "Deployment", Team: "data", CostCenter: "cc-400", Replicas: 3, CPUReq: 2, MemReqGiB: 4, CPULimit: 4, MemLimitGiB: 6, Pool: "batch", CPUUse: 0.7, MemUse: 0.65, Diurnal: 0.2, Noise: 0.06},
				{Namespace: "data-platform", Name: "etl-nightly", Kind: "CronJob", Team: "data", CostCenter: "cc-400", Replicas: 2, CPUReq: 3, MemReqGiB: 12, CPULimit: 4, MemLimitGiB: 16, Pool: "batch", CPUUse: 0.85, MemUse: 0.7, Noise: 0.08, BatchWindow: [2]int{0, 3}},
				{Namespace: "data-platform", Name: "etl-backfill", Kind: "Job", Team: "data", CostCenter: "cc-400", Replicas: 4, CPUReq: 16, MemReqGiB: 64, Pool: "batch", Unschedulable: true},
			},
		},
		{
			ID: "aks-westeu-prod-01", Cloud: "azure", Region: "westeurope",
			Zones: []string{"westeurope-1", "westeurope-2", "westeurope-3"},
			Pools: []*nodepool{
				{Name: "system", SKU: "Standard_D8s_v5", Lifecycle: "on-demand", Count: 3, CPU: 7.8, MemGiB: 28, PricePerHour: 0.384},
				{Name: "gpu-a100", SKU: "Standard_NC24ads_A100_v4", Lifecycle: "on-demand", GPUKind: "nvidia-a100-80gb", Count: 2, CPU: 23.5, MemGiB: 210, GPUs: 1, PricePerHour: 3.673},
			},
			Workloads: []*workload{
				{Namespace: "ml-inference", Name: "model-server-a100", Kind: "Deployment", Team: "ml", CostCenter: "cc-500", Replicas: 2, CPUReq: 8, MemReqGiB: 48, CPULimit: 12, MemLimitGiB: 64, GPUs: 1, Pool: "gpu-a100", CPUUse: 0.25, MemUse: 0.55, Diurnal: 0.5, Noise: 0.06, GPUUtil: 22},
				{Namespace: "ml-inference", Name: "feature-store", Kind: "Deployment", Team: "ml", CostCenter: "cc-500", Replicas: 2, CPUReq: 2, MemReqGiB: 8, CPULimit: 4, MemLimitGiB: 8, Pool: "system", CPUUse: 0.35, MemUse: 0.5, Diurnal: 0.4, Noise: 0.05},
				{Namespace: "edge", Name: "frontend-gateway", Kind: "Deployment", Team: "platform", CostCenter: "cc-100", Replicas: 3, CPUReq: 1, MemReqGiB: 1, CPULimit: 2, MemLimitGiB: 2, Pool: "system", CPUUse: 0.3, MemUse: 0.4, Diurnal: 0.6, Noise: 0.07},
			},
		},
	}
	for _, c := range fleet {
		for _, p := range c.Pools {
			for i := 0; i < p.Count; i++ {
				c.Nodes = append(c.Nodes, &node{
					Name: fmt.Sprintf("%s-%s-%d", c.ID, p.Name, i),
					Zone: c.Zones[i%len(c.Zones)],
					Pool: p,
				})
			}
		}
		for _, w := range c.Workloads {
			var poolNodes []*node
			for _, n := range c.Nodes {
				if n.Pool.Name == w.Pool {
					poolNodes = append(poolNodes, n)
				}
			}
			for i := 0; i < w.Replicas; i++ {
				w.pods = append(w.pods, &pod{
					Name: podName(w, i, r),
					UID:  fmt.Sprintf("%08x-%04x-4%03x-a%03x-%012x", r.Uint32(), r.Uint32()&0xffff, r.Uint32()&0xfff, r.Uint32()&0xfff, r.Uint64()&0xffffffffffff),
					Node: poolNodes[(i+len(w.Name))%len(poolNodes)],
				})
			}
		}
	}
	return fleet
}

func podName(w *workload, i int, r *rand.Rand) string {
	switch w.Kind {
	case "StatefulSet":
		return fmt.Sprintf("%s-%d", w.Name, i)
	case "CronJob", "Job":
		return fmt.Sprintf("%s-%d-%s", w.Name, 29000000+r.IntN(99999), randSuffix(r, 5))
	default:
		return fmt.Sprintf("%s-%s-%s", w.Name, randSuffix(r, 10), randSuffix(r, 5))
	}
}

func randSuffix(r *rand.Rand, n int) string {
	const alphabet = "bcdfghjklmnpqrstvwxz2456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

// running reports whether a workload's pods run at t (batch windows are
// hours of the day, UTC; unschedulable jobs never run).
func (w *workload) running(t time.Time) bool {
	if w.Unschedulable {
		return false
	}
	if w.BatchWindow != [2]int{} {
		h := t.UTC().Hour()
		return h >= w.BatchWindow[0] && h < w.BatchWindow[1]
	}
	return true
}

// inIncident: the checkout retry storm window.
func inIncident(t, start time.Time) bool {
	return !t.Before(start.Add(-incidentAge))
}

// usage returns cores and GiB used by one pod of w at t.
func (w *workload) usage(t, start time.Time, r *rand.Rand) (cores, memGiB float64) {
	hour := float64(t.UTC().Hour()) + float64(t.UTC().Minute())/60
	diurnal := 1 + w.Diurnal*math.Sin((hour-9)/24*2*math.Pi)
	cpu := w.CPUReq * w.CPUUse * diurnal * (1 + w.Noise*r.NormFloat64())
	mem := w.MemReqGiB * w.MemUse * (1 + 0.3*w.Noise*r.NormFloat64())
	if w.RetryStorm && inIncident(t, start) {
		cpu *= 1.75
		mem *= 1.15
	}
	if w.OOMs {
		// Sawtooth: memory creeps toward the limit and resets after an
		// OOM kill every ~2h.
		phase := math.Mod(float64(t.Unix())/7200, 1)
		mem = w.MemLimitGiB * (0.78 + 0.24*phase)
	}
	return math.Max(cpu, 0.005), math.Max(mem, 0.02)
}

// replicasAt: the HPA scales checkout-api out during the retry storm.
func (w *workload) replicasAt(t, start time.Time) int {
	if w.RetryStorm && inIncident(t, start) {
		return len(w.pods)
	}
	if w.RetryStorm {
		return len(w.pods) * 2 / 3
	}
	return len(w.pods)
}
