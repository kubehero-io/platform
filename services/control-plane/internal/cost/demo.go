// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package cost

import (
	"context"
	"hash/fnv"
	"math"
	"time"
)

// The demo fleet: three clouds, the same cluster and workload names the
// other demo fixtures use. Rates are $/hour for all replicas together;
// the generator spreads them over pods, nodes and zones so every
// aggregation (down to pod, node and container) has something honest-
// looking to show. Everything the demo returns is labelled source=demo.
type demoWorkload struct {
	cluster, namespace, name, kind string
	team, costCenter, nodepool     string
	lifecycle                      string
	replicas                       int
	usdHour                        float64 // compute $/h, all replicas
	gpuShare                       float64 // share of compute $ that is GPU
	cpuReq, cpuUse                 float64 // cores per pod
	memReq, memUse                 float64 // GiB per pod
	gpus                           float64 // per pod
	egressHour, crossZoneHour      float64 // network $/h
	logGBHour                      float64
	containers                     []containerShare
	labels                         map[string]string
}

type demoCluster struct {
	name, cloud, region string
	zones               []string
	idleShare           float64 // idle / node cost
}

var demoClusters = []demoCluster{
	{"eks-use1-prod", "aws", "us-east-1", []string{"us-east-1a", "us-east-1b", "us-east-1c"}, 0.14},
	{"gke-usc1-prod", "gcp", "us-central1", []string{"us-central1-a", "us-central1-b"}, 0.22},
	{"aks-westeu-prod-01", "azure", "westeurope", []string{"westeurope-1", "westeurope-2", "westeurope-3"}, 0.18},
}

var appSidecar = []containerShare{{Name: "app", CPU: 0.85, RAM: 0.8}, {Name: "istio-proxy", CPU: 0.15, RAM: 0.2}}
var single = func(n string) []containerShare { return []containerShare{{Name: n, CPU: 1, RAM: 1}} }

var demoWorkloads = []demoWorkload{
	{cluster: "eks-use1-prod", namespace: "ml-inference", name: "embedder", kind: "Deployment", team: "ml-inference", costCenter: "ml-platform", nodepool: "gpu-l4", lifecycle: "on-demand",
		replicas: 4, usdHour: 41, gpuShare: 0.7, cpuReq: 8, cpuUse: 3.1, memReq: 32, memUse: 21, gpus: 1, logGBHour: 0.4, containers: single("triton"), labels: map[string]string{"app": "embedder", "env": "prod"}},
	{cluster: "eks-use1-prod", namespace: "retrieval", name: "vectordb-ingress", kind: "Deployment", team: "retrieval", costCenter: "ml-platform", nodepool: "general", lifecycle: "on-demand",
		replicas: 6, usdHour: 18.4, cpuReq: 16, cpuUse: 0.41, memReq: 32, memUse: 1.4, egressHour: 0.35, crossZoneHour: 0.42, logGBHour: 0.9, containers: appSidecar, labels: map[string]string{"app": "vectordb", "env": "prod"}},
	{cluster: "eks-use1-prod", namespace: "retrieval", name: "retrieval-indexer", kind: "StatefulSet", team: "retrieval", costCenter: "ml-platform", nodepool: "memory-opt", lifecycle: "on-demand",
		replicas: 3, usdHour: 11.9, cpuReq: 4, cpuUse: 2.2, memReq: 64, memUse: 6, crossZoneHour: 0.18, logGBHour: 0.3, containers: single("indexer"), labels: map[string]string{"app": "indexer", "env": "prod"}},
	{cluster: "eks-use1-prod", namespace: "payments", name: "ledger", kind: "Deployment", team: "payments", costCenter: "fintech", nodepool: "general", lifecycle: "on-demand",
		replicas: 4, usdHour: 2.6, cpuReq: 1, cpuUse: 0.7, memReq: 0.75, memUse: 0.95, egressHour: 0.05, logGBHour: 0.2, containers: appSidecar, labels: map[string]string{"app": "ledger", "env": "prod"}},
	{cluster: "eks-use1-prod", namespace: "data", name: "queue-consumer", kind: "Deployment", team: "data", costCenter: "analytics", nodepool: "spot", lifecycle: "spot",
		replicas: 8, usdHour: 3.6, cpuReq: 1, cpuUse: 0.05, memReq: 2, memUse: 0.3, logGBHour: 0.05, containers: single("consumer"), labels: map[string]string{"app": "queue-consumer", "env": "prod"}},
	{cluster: "eks-use1-prod", namespace: "kube-system", name: "coredns", kind: "Deployment", team: "platform", costCenter: "platform", nodepool: "general", lifecycle: "on-demand",
		replicas: 2, usdHour: 0.6, cpuReq: 0.25, cpuUse: 0.08, memReq: 0.25, memUse: 0.06, logGBHour: 0.02, containers: single("coredns"), labels: map[string]string{"k8s-app": "kube-dns"}},
	{cluster: "eks-use1-prod", namespace: "monitoring", name: "prometheus", kind: "StatefulSet", team: "platform", costCenter: "platform", nodepool: "general", lifecycle: "on-demand",
		replicas: 2, usdHour: 1.9, cpuReq: 2, cpuUse: 0.9, memReq: 8, memUse: 5.5, logGBHour: 0.05, containers: single("prometheus"), labels: map[string]string{"app": "prometheus"}},

	{cluster: "gke-usc1-prod", namespace: "edge", name: "frontend-gateway", kind: "Deployment", team: "edge", costCenter: "platform", nodepool: "general", lifecycle: "on-demand",
		replicas: 8, usdHour: 14.2, cpuReq: 8, cpuUse: 1.6, memReq: 2, memUse: 0.6, egressHour: 2.1, crossZoneHour: 0.3, logGBHour: 1.4, containers: appSidecar, labels: map[string]string{"app": "frontend", "env": "prod"}},
	{cluster: "gke-usc1-prod", namespace: "checkout", name: "checkout", kind: "Deployment", team: "checkout", costCenter: "commerce", nodepool: "general", lifecycle: "on-demand",
		replicas: 5, usdHour: 3.3, cpuReq: 1, cpuUse: 0.6, memReq: 1, memUse: 0.7, egressHour: 0.4, crossZoneHour: 0.12, logGBHour: 0.35, containers: appSidecar, labels: map[string]string{"app": "checkout", "env": "prod"}},
	{cluster: "gke-usc1-prod", namespace: "checkout", name: "cart", kind: "Deployment", team: "checkout", costCenter: "commerce", nodepool: "general", lifecycle: "on-demand",
		replicas: 5, usdHour: 1.1, cpuReq: 0.25, cpuUse: 0.45, memReq: 0.25, memUse: 0.28, crossZoneHour: 0.06, logGBHour: 0.1, containers: appSidecar, labels: map[string]string{"app": "cart", "env": "prod"}},
	{cluster: "gke-usc1-prod", namespace: "kube-system", name: "kube-dns", kind: "Deployment", team: "platform", costCenter: "platform", nodepool: "general", lifecycle: "on-demand",
		replicas: 2, usdHour: 0.5, cpuReq: 0.26, cpuUse: 0.05, memReq: 0.1, memUse: 0.05, containers: single("kubedns"), labels: map[string]string{"k8s-app": "kube-dns"}},

	{cluster: "aks-westeu-prod-01", namespace: "ml-inference", name: "model-server-a100", kind: "Deployment", team: "ml-inference", costCenter: "ml-platform", nodepool: "gpu-a100", lifecycle: "on-demand",
		replicas: 4, usdHour: 62, gpuShare: 0.8, cpuReq: 12, cpuUse: 2.2, memReq: 96, memUse: 40, gpus: 2, egressHour: 0.2, logGBHour: 0.3, containers: single("tritonserver"), labels: map[string]string{"app": "model-server", "env": "prod"}},
	{cluster: "aks-westeu-prod-01", namespace: "edge", name: "api-ingress", kind: "Deployment", team: "edge", costCenter: "platform", nodepool: "general", lifecycle: "on-demand",
		replicas: 6, usdHour: 4.7, cpuReq: 4, cpuUse: 0.7, memReq: 1, memUse: 0.4, egressHour: 0.9, crossZoneHour: 0.08, logGBHour: 0.8, containers: appSidecar, labels: map[string]string{"app": "api-ingress", "env": "prod"}},
	{cluster: "aks-westeu-prod-01", namespace: "kube-system", name: "konnectivity-agent", kind: "DaemonSet", team: "platform", costCenter: "platform", nodepool: "general", lifecycle: "on-demand",
		replicas: 6, usdHour: 0.7, cpuReq: 0.05, cpuUse: 0.02, memReq: 0.05, memUse: 0.03, containers: single("agent"), labels: map[string]string{"k8s-app": "konnectivity-agent"}},
	{cluster: "aks-westeu-prod-01", namespace: "monitoring", name: "otel-collector", kind: "DaemonSet", team: "platform", costCenter: "platform", nodepool: "general", lifecycle: "on-demand",
		replicas: 6, usdHour: 1.5, cpuReq: 0.2, cpuUse: 0.12, memReq: 0.4, memUse: 0.3, logGBHour: 0.1, containers: single("otelcol"), labels: map[string]string{"app": "otel-collector"}},
}

const demoGiB = 1 << 30

// demoSeasonality is a deterministic weekly + daily shape so demo time
// series look like a real fleet (weekday peaks, overnight dips).
func demoSeasonality(t time.Time) float64 {
	day := float64(t.Weekday())
	hour := float64(t.Hour()) + float64(t.Minute())/60
	return 1 + 0.08*math.Sin(2*math.Pi*(day-1)/7) + 0.05*math.Sin(2*math.Pi*(hour-8)/24)
}

func demoClusterInfo(name string) demoCluster {
	for _, c := range demoClusters {
		if c.name == name {
			return c
		}
	}
	return demoCluster{name: name}
}

// demoPodName mimics a Deployment pod: <name>-<template hash>-<rand5>
// (StatefulSets get ordinals) drawn from Kubernetes' safe alphabet.
func demoPodName(w demoWorkload, i int) string {
	if w.kind == "StatefulSet" {
		return w.name + "-" + itoa(i)
	}
	const alpha = "bcdfghjklmnpqrstvwxz2456789"
	h := fnv.New64a()
	_, _ = h.Write([]byte(w.cluster + "/" + w.namespace + "/" + w.name))
	x := h.Sum64()
	pick := func(n int, seed uint64) string {
		b := make([]byte, n)
		for j := range b {
			seed = seed*6364136223846793005 + 1442695040888963407
			b[j] = alpha[(seed>>33)%uint64(len(alpha))]
		}
		return string(b)
	}
	if w.kind == "DaemonSet" {
		return w.name + "-" + pick(5, x+uint64(i)*7919)
	}
	return w.name + "-" + pick(9, x) + "-" + pick(5, x+uint64(i)*7919)
}

// demoAllocator serves allocations from the demo fleet through the
// real aggregation path.
type demoAllocator struct{}

func (demoAllocator) Allocate(_ context.Context, q AllocationQuery) ([]AllocSet, error) {
	bws, _, err := q.buckets()
	if err != nil {
		return nil, err
	}
	end := q.Window.QueryEnd()
	bws = clampBuckets(bws, end)
	shared := map[string]bool{}
	for _, ns := range q.SharedNamespaces {
		shared[ns] = true
	}
	p := &plan{q: q, containerDim: -1, shared: shared}
	for i, d := range q.Dims {
		if d.Name == DimContainer {
			p.containerDim = i
		}
	}

	var rows []fineRow
	totals := map[clusterKey]*clusterTotals{}
	nodeCost := map[clusterKey]float64{}
	filtered := false
	for _, f := range q.Filters {
		if f.Dim.Name != DimCluster {
			filtered = true
		}
	}
	for b, bw := range bws {
		hours := bw.End.Sub(bw.Start).Hours()
		if hours <= 0 {
			continue
		}
		season := demoSeasonality(bw.Start.Add(bw.End.Sub(bw.Start) / 2))
		for _, w := range demoWorkloads {
			c := demoClusterInfo(w.cluster)
			if q.ClusterID != "" && q.ClusterID != c.name {
				continue
			}
			for i := 0; i < w.replicas; i++ {
				rec := demoRecord(w, c, i)
				r := demoFine(w, rec, b, bw, hours*season/float64(w.replicas))
				if !passesClusterFilter(q.Filters, c.name) {
					continue
				}
				k := clusterKey{b, c.name}
				ct := totals[k]
				if ct == nil {
					ct = &clusterTotals{}
					totals[k] = ct
				}
				if shared[w.namespace] {
					ct.Shared += r.M.Cost
				} else {
					ct.NonShared += r.M.Cost
				}
				nodeCost[k] += r.M.Cost / (1 - c.idleShare)
				if !demoMatches(q.Filters, rec) {
					continue
				}
				r.Vals = demoVals(q.Dims, rec)
				rows = append(rows, r)
			}
		}
	}
	if p.containerDim >= 0 || hasContainerFilter(q.Filters) {
		shares := map[wlID][]containerShare{}
		for _, w := range demoWorkloads {
			shares[wlID{w.cluster, w.namespace, w.name}] = w.containers
		}
		var keep map[string]bool
		for _, f := range q.Filters {
			if f.Dim.Name == DimContainer {
				keep = map[string]bool{}
				for _, v := range f.Values {
					keep[v] = true
				}
			}
		}
		rows = splitContainers(rows, shares, p.containerDim, keep)
	}
	in := aggInput{Dims: q.Dims, Rows: rows, Buckets: bws, Shared: shared,
		IncludeIdle: q.IncludeIdle, ShareIdle: q.ShareIdle, NodeCost: nodeCost}
	if filtered {
		in.Totals = totals
	}
	if !q.IncludeIdle && q.ShareIdle == ShareIdleNone {
		in.NodeCost = nil
	}
	return aggregate(in), nil
}

func hasContainerFilter(fs []Filter) bool {
	for _, f := range fs {
		if f.Dim.Name == DimContainer {
			return true
		}
	}
	return false
}

func passesClusterFilter(fs []Filter, cluster string) bool {
	for _, f := range fs {
		if f.Dim.Name == DimCluster && !contains(f.Values, cluster) {
			return false
		}
	}
	return true
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// demoRecord is one demo pod's dimension values.
func demoRecord(w demoWorkload, c demoCluster, i int) map[string]string {
	zone := ""
	if len(c.zones) > 0 {
		zone = c.zones[i%len(c.zones)]
	}
	rec := map[string]string{
		DimCluster: c.name, DimNamespace: w.namespace, DimWorkload: w.name,
		DimController: w.kind + "/" + w.name, DimWorkloadKind: w.kind,
		DimPod: demoPodName(w, i), DimNodepool: w.nodepool, DimTeam: w.team,
		DimCostCenter: w.costCenter, DimZone: zone, DimCloud: c.cloud, DimRegion: c.region,
		DimLifecycle: w.lifecycle,
		DimNode:      w.nodepool + "-" + zone + "-" + itoa(i%3),
	}
	for k, v := range w.labels {
		rec[labelPrefix+k] = v
	}
	return rec
}

func demoFine(w demoWorkload, rec map[string]string, b int, bw bucketWindow, podHours float64) fineRow {
	r := fineRow{Bucket: b, Cluster: rec[DimCluster], Namespace: w.namespace, Workload: w.name,
		Start: bw.Start.Unix(), End: bw.End.Unix()}
	m := &r.M
	m.Cost = w.usdHour * podHours
	m.GPUCost = m.Cost * w.gpuShare
	m.CPUCost = (m.Cost - m.GPUCost) * 0.6
	m.RAMCost = m.Cost - m.GPUCost - m.CPUCost
	sec := podHours * 3600
	m.CPUReqCS, m.CPUUseCS = w.cpuReq*sec, w.cpuUse*sec
	m.CPUAllocCS = math.Max(w.cpuReq, w.cpuUse) * sec
	m.RAMReqBS, m.RAMUseBS = w.memReq*demoGiB*sec, w.memUse*demoGiB*sec
	m.RAMAllocBS = math.Max(w.memReq, w.memUse) * demoGiB * sec
	m.GPUSec = w.gpus * sec
	m.PodSec = sec
	eff := (m.CPUEfficiency() + m.RAMEfficiency()) / 2
	if eff < 1 {
		m.Recoverable = (m.CPUCost + m.RAMCost) * (1 - eff) * 0.6
	}
	m.NetInternetCost = w.egressHour * podHours
	m.NetCrossZoneCost = w.crossZoneHour * podHours
	m.NetCost = m.NetInternetCost + m.NetCrossZoneCost
	m.LogBytes = w.logGBHour * podHours * 1e9
	return r
}

func demoMatches(fs []Filter, rec map[string]string) bool {
	for _, f := range fs {
		if f.Dim.Name == DimCluster || f.Dim.Name == DimContainer {
			continue
		}
		if !contains(f.Values, rec[f.Dim.Name]) {
			return false
		}
	}
	return true
}

func demoVals(dims []Dim, rec map[string]string) []string {
	out := make([]string, len(dims))
	for i, d := range dims {
		out[i] = rec[d.Name]
	}
	return out
}

// DemoClusters lists the demo fleet's clusters (cloud, region).
func DemoClusters() [][3]string {
	out := make([][3]string, len(demoClusters))
	for i, c := range demoClusters {
		out[i] = [3]string{c.name, c.cloud, c.region}
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
