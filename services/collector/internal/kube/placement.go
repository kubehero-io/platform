// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package kube

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Well-known extended resources that count as accelerators. GPU cost
// attribution (packages/cost-model) prices by count, so only resources
// that represent whole devices belong here — MIG slices and time-shared
// replicas are still advertised under nvidia.com/gpu by the device
// plugin and are counted as devices, which is how they are billed.
var gpuResources = []corev1.ResourceName{"nvidia.com/gpu", "amd.com/gpu"}

// Cloud identifies the provider from spec.providerID (aws:// gce://
// azure://), falling back to provider-specific labels. "" means
// unknown / on-prem.
func Cloud(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	id := n.Spec.ProviderID
	switch {
	case strings.HasPrefix(id, "aws://"):
		return "aws"
	case strings.HasPrefix(id, "gce://"):
		return "gcp"
	case strings.HasPrefix(id, "azure://"):
		return "azure"
	}
	for k := range n.Labels {
		switch {
		case strings.HasPrefix(k, "eks.amazonaws.com/"), strings.HasPrefix(k, "k8s.amazonaws.com/"),
			strings.HasPrefix(k, "karpenter.k8s.aws/"):
			return "aws"
		case strings.HasPrefix(k, "cloud.google.com/"):
			return "gcp"
		case strings.HasPrefix(k, "kubernetes.azure.com/"):
			return "azure"
		}
	}
	return ""
}

// Region reads topology.kubernetes.io/region (beta label fallback).
func Region(n *corev1.Node) string {
	return firstLabel(n, "topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region")
}

// Zone reads topology.kubernetes.io/zone (beta label fallback).
func Zone(n *corev1.Node) string {
	return firstLabel(n, "topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone")
}

// SKU is the instance type.
func SKU(n *corev1.Node) string {
	return firstLabel(n, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type")
}

// Nodepool reads the cloud-vendor-specific nodepool label, preferring
// KubeHero's own canonical label.
func Nodepool(n *corev1.Node) string {
	return firstLabel(n,
		"kubehero.io/nodepool",
		"eks.amazonaws.com/nodegroup",
		"cloud.google.com/gke-nodepool",
		"kubernetes.azure.com/agentpool",
		"agentpool",
		"karpenter.sh/nodepool",
		"karpenter.sh/provisioner-name",
	)
}

// Lifecycle normalises the purchase option to on-demand | spot |
// reserved. Vendors disagree on spelling (EKS says ON_DEMAND, Karpenter
// on-demand, AKS a scale-set priority), and the pricing engine keys on
// the normalised form.
func Lifecycle(n *corev1.Node) string {
	if n == nil {
		return "on-demand"
	}
	if v := n.Labels["kubehero.io/lifecycle"]; v != "" {
		return normaliseLifecycle(v)
	}
	if v := n.Labels["karpenter.sh/capacity-type"]; v != "" {
		return normaliseLifecycle(v)
	}
	if v := n.Labels["eks.amazonaws.com/capacityType"]; v != "" {
		return normaliseLifecycle(v)
	}
	if n.Labels["cloud.google.com/gke-spot"] == "true" || n.Labels["cloud.google.com/gke-preemptible"] == "true" {
		return "spot"
	}
	if strings.EqualFold(n.Labels["kubernetes.azure.com/scalesetpriority"], "spot") {
		return "spot"
	}
	return "on-demand"
}

func normaliseLifecycle(v string) string {
	v = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), "_", "-"))
	switch v {
	case "ondemand", "on-demand", "regular", "standard":
		return "on-demand"
	case "spot", "preemptible":
		return "spot"
	}
	return v
}

// GPUKind is the accelerator model advertised by GPU feature discovery
// or the cloud's node labels.
func GPUKind(n *corev1.Node) string {
	return firstLabel(n,
		"nvidia.com/gpu.product",
		"cloud.google.com/gke-accelerator",
		"k8s.amazonaws.com/accelerator",
		"karpenter.k8s.aws/instance-gpu-name",
		"kubernetes.azure.com/accelerator",
	)
}

// NodeGPUs is the node's allocatable accelerator count.
func NodeGPUs(n *corev1.Node) int64 {
	if n == nil {
		return 0
	}
	var total int64
	for _, r := range gpuResources {
		if q, ok := n.Status.Allocatable[r]; ok {
			total += q.Value()
		}
	}
	return total
}

// AllocatableCPUMillis / AllocatableMemBytes read node allocatable (0
// when unreported).
func AllocatableCPUMillis(n *corev1.Node) int64 {
	if n == nil {
		return 0
	}
	if q, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
		return q.MilliValue()
	}
	return 0
}

// AllocatableMemBytes reads node allocatable memory.
func AllocatableMemBytes(n *corev1.Node) int64 {
	if n == nil {
		return 0
	}
	if q, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
		return q.Value()
	}
	return 0
}

// Team is the kubehero.io/team label, falling back to the namespace so
// every pod lands in some chargeback bucket.
func Team(p *corev1.Pod) string {
	if t := p.Labels["kubehero.io/team"]; t != "" {
		return t
	}
	return p.Namespace
}

// CostCenter is the kubehero.io/cost-center label.
func CostCenter(p *corev1.Pod) string { return p.Labels["kubehero.io/cost-center"] }

func firstLabel(n *corev1.Node, keys ...string) string {
	if n == nil {
		return ""
	}
	for _, k := range keys {
		if v := n.Labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// ── pod resources ─────────────────────────────────────────────────────

// Resources is a pod's effective reservation.
type Resources struct {
	CPUMillis int64
	MemBytes  int64
	GPUs      int64
}

// PodRequests returns the pod's effective requests the way the
// scheduler accounts them, not a naive sum over containers:
//
//	max(Σ app containers + Σ restartable (sidecar) init containers,
//	    max over init containers of (its request + sidecars started before it))
//	+ pod overhead (RuntimeClass)
//
// Pod-level resources (spec.resources, KEP-2837) replace the container
// computation for the dimensions they set. Extended GPU resources fall
// back to limits, since a limit-only extended resource request defaults
// to its limit.
func PodRequests(p *corev1.Pod) Resources {
	var app, sidecars, initMax Resources
	for i := range p.Spec.Containers {
		app = app.add(containerRequests(&p.Spec.Containers[i]))
	}
	for i := range p.Spec.InitContainers {
		c := &p.Spec.InitContainers[i]
		r := containerRequests(c)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			// Sidecars keep running next to the app containers and
			// alongside every init container that starts after them.
			sidecars = sidecars.add(r)
			continue
		}
		initMax = initMax.max(r.add(sidecars))
	}
	out := app.add(sidecars).max(initMax)

	if pr := p.Spec.Resources; pr != nil {
		if q, ok := pr.Requests[corev1.ResourceCPU]; ok {
			out.CPUMillis = q.MilliValue()
		}
		if q, ok := pr.Requests[corev1.ResourceMemory]; ok {
			out.MemBytes = q.Value()
		}
	}
	if q, ok := p.Spec.Overhead[corev1.ResourceCPU]; ok {
		out.CPUMillis += q.MilliValue()
	}
	if q, ok := p.Spec.Overhead[corev1.ResourceMemory]; ok {
		out.MemBytes += q.Value()
	}
	return out.nonNegative()
}

// ContainerRequests returns one container's requests.
func ContainerRequests(c *corev1.Container) Resources { return containerRequests(c).nonNegative() }

// ContainerLimits returns one container's limits (0 = unlimited).
func ContainerLimits(c *corev1.Container) Resources {
	var r Resources
	if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		r.CPUMillis = q.MilliValue()
	}
	if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		r.MemBytes = q.Value()
	}
	for _, g := range gpuResources {
		if q, ok := c.Resources.Limits[g]; ok {
			r.GPUs += q.Value()
		}
	}
	return r.nonNegative()
}

func containerRequests(c *corev1.Container) Resources {
	var r Resources
	if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		r.CPUMillis = q.MilliValue()
	}
	if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		r.MemBytes = q.Value()
	}
	for _, g := range gpuResources {
		if q, ok := c.Resources.Requests[g]; ok {
			r.GPUs += q.Value()
		} else if q, ok := c.Resources.Limits[g]; ok {
			r.GPUs += q.Value()
		}
	}
	return r
}

func (r Resources) add(o Resources) Resources {
	return Resources{CPUMillis: r.CPUMillis + o.CPUMillis, MemBytes: r.MemBytes + o.MemBytes, GPUs: r.GPUs + o.GPUs}
}

func (r Resources) max(o Resources) Resources {
	return Resources{CPUMillis: max(r.CPUMillis, o.CPUMillis), MemBytes: max(r.MemBytes, o.MemBytes), GPUs: max(r.GPUs, o.GPUs)}
}

func (r Resources) nonNegative() Resources {
	return Resources{CPUMillis: max(r.CPUMillis, 0), MemBytes: max(r.MemBytes, 0), GPUs: max(r.GPUs, 0)}
}

// StripRuntimePrefix turns "containerd://<id>" into "<id>".
func StripRuntimePrefix(id string) string {
	if i := strings.Index(id, "://"); i >= 0 {
		return id[i+3:]
	}
	return id
}
