// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package costmodel computes the canonical cost-per-pod-per-second given
// node pricing and the pod's share of that node's resources. The
// collector, control-plane and CLI all price through this package so a
// pod costs the same number of dollars no matter who asks.
//
// The model is deliberately simple and auditable:
//
//   - CPU-only nodes: the node's hourly price is split 50/50 between
//     CPU and memory; a pod pays price/2 × its CPU share plus price/2 ×
//     its memory share (PodCostPerHour).
//   - GPU nodes: accelerators dominate the bill of GPU instance types
//     (an 8×A100 node's price is mostly the A100s), so GPUPriceShare of
//     the price is attributed to GPUs by count requested and the rest is
//     split 50/50 CPU/memory as above (PodCostBreakdown).
//
// Shares are computed against allocatable capacity, not raw capacity:
// the kubelet's system reservation is overhead the scheduler never hands
// to pods, so it lands in the node's idle cost rather than inflating
// every pod's share.
package costmodel

import "math"

// GPUPriceShare is the fraction of a GPU node's hourly price attributed
// to its accelerators. Published on-demand prices put accelerators at
// roughly 65–85% of GPU instance cost (e.g. p4d/p5, a2/a3, ND A100 v4
// vs. the equivalent CPU/memory-only family), so 0.7 is a conservative
// single number that keeps CPU/memory of GPU nodes from looking absurdly
// expensive while charging GPU tenants for what they actually occupy.
const GPUPriceShare = 0.7

// NodePrice is the quoted per-hour cost of the node running a pod.
type NodePrice struct {
	PerHourUSD float64
	CPUMillis  int64 // total allocatable CPU in millicores
	MemBytes   int64 // total allocatable memory in bytes
	// GPUs is the node's allocatable accelerator count (nvidia.com/gpu +
	// amd.com/gpu). 0 means a CPU-only node — or a GPU node whose device
	// plugin isn't advertising devices, which is priced as CPU-only
	// because no pod can be scheduled onto the accelerators anyway.
	GPUs int64
}

// PodShare represents the pod's share of a node's resources.
type PodShare struct {
	CPUMillis int64
	MemBytes  int64
	GPUs      int64 // accelerators requested
}

// PodCostPerHour returns the pod's share of the node's per-hour price,
// blended 50/50 between CPU share and memory share. Returns 0 if either
// allocatable dimension is zero to avoid divide-by-zero. It ignores
// GPUs; GPU-aware callers use PodCostBreakdown, which is identical to
// this function on CPU-only nodes.
func PodCostPerHour(node NodePrice, pod PodShare) float64 {
	if node.CPUMillis == 0 || node.MemBytes == 0 {
		return 0
	}
	cpuShare := float64(pod.CPUMillis) / float64(node.CPUMillis)
	memShare := float64(pod.MemBytes) / float64(node.MemBytes)
	blended := (cpuShare + memShare) / 2
	return node.PerHourUSD * blended
}

// PodCostPerSecond is PodCostPerHour / 3600, rounded to 10 decimals.
func PodCostPerSecond(node NodePrice, pod PodShare) float64 {
	return HourlyToPerSecond(PodCostPerHour(node, pod))
}

// HourlyToPerSecond converts a $/hour figure into $/second with the same
// 10-decimal rounding PodCostPerSecond applies, so per-resource parts
// and totals round identically.
func HourlyToPerSecond(usdPerHour float64) float64 {
	return math.Round(usdPerHour/3600*1e10) / 1e10
}

// PodCostBreakdown splits a pod's hourly cost into CPU, memory and GPU
// parts (OpenCost-style allocation).
//
// On a CPU-only node (node.GPUs == 0) the result is exactly
// PodCostPerHour split into its two halves: cpu = price/2 × cpuShare,
// ram = price/2 × memShare, gpu = 0 — so totals are unchanged from the
// pre-GPU model.
//
// On a GPU node, GPUPriceShare of the price is spread across the node's
// accelerators by count (gpu = price × GPUPriceShare × pod.GPUs /
// node.GPUs) and the remaining (1 − GPUPriceShare) is split 50/50
// CPU/memory by share. A pod that occupies the same fraction f of every
// dimension therefore pays exactly f × price, and pods that together
// request the whole node sum to the whole price; whatever nobody
// requests is the node's idle cost.
//
// Shares are not clamped: callers pass non-negative requests/usage.
// When CPU or memory allocatable is zero the node can't be sized, so the
// CPU and memory parts are 0 (the GPU part, which only depends on GPU
// counts, is still attributed on GPU nodes).
func PodCostBreakdown(node NodePrice, pod PodShare) (cpuUSDPerHour, ramUSDPerHour, gpuUSDPerHour float64) {
	cpuPool, ramPool := node.PerHourUSD/2, node.PerHourUSD/2
	if node.GPUs > 0 {
		gpuPool := node.PerHourUSD * GPUPriceShare
		rest := node.PerHourUSD - gpuPool
		cpuPool, ramPool = rest/2, rest/2
		gpuUSDPerHour = gpuPool * float64(pod.GPUs) / float64(node.GPUs)
	}
	if node.CPUMillis == 0 || node.MemBytes == 0 {
		return 0, 0, gpuUSDPerHour
	}
	cpuUSDPerHour = cpuPool * float64(pod.CPUMillis) / float64(node.CPUMillis)
	ramUSDPerHour = ramPool * float64(pod.MemBytes) / float64(node.MemBytes)
	return cpuUSDPerHour, ramUSDPerHour, gpuUSDPerHour
}

// UtilizationBlend returns the billable share for a pod given what it
// requested and what it measurably used: per dimension, the max of the
// two. Requests floor the bill — the scheduler reserved that capacity
// whether the pod used it or not — while measured usage above requests
// bills the overage a burstable pod squats on. Callers without a
// measurement (kubelet stats unavailable) should bill `requested`
// directly rather than blending against a zero measurement. GPUs are
// not measured today, so their billable count is the request.
func UtilizationBlend(requested, measured PodShare) PodShare {
	return PodShare{
		CPUMillis: max(requested.CPUMillis, measured.CPUMillis),
		MemBytes:  max(requested.MemBytes, measured.MemBytes),
		GPUs:      max(requested.GPUs, measured.GPUs),
	}
}

// UnusedShare is the part of a pod's reservation it did not use:
// per dimension max(0, requested − measured). Priced through
// PodCostBreakdown it is the pod's recoverable cost — what rightsizing
// the requests down to observed usage would free. GPUs are excluded
// (0): without accelerator utilisation telemetry an idle-looking GPU
// can't be told apart from one whose work we don't observe.
func UnusedShare(requested, measured PodShare) PodShare {
	return PodShare{
		CPUMillis: max(0, requested.CPUMillis-measured.CPUMillis),
		MemBytes:  max(0, requested.MemBytes-measured.MemBytes),
	}
}
