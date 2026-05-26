// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package costmodel computes the canonical cost-per-pod-per-second given
// node pricing and the pod's share of that node's resources.
package costmodel

import "math"

// NodePrice is the quoted per-hour cost of the node running a pod.
type NodePrice struct {
	PerHourUSD float64
	CPUMillis  int64 // total allocatable CPU in millicores
	MemBytes   int64 // total allocatable memory in bytes
}

// PodShare represents the pod's share of a node's resources.
type PodShare struct {
	CPUMillis int64
	MemBytes  int64
}

// PodCostPerHour returns the pod's share of the node's per-hour price,
// blended 50/50 between CPU share and memory share. Returns 0 if either
// allocatable dimension is zero to avoid divide-by-zero.
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
	return math.Round(PodCostPerHour(node, pod)/3600*1e10) / 1e10
}
