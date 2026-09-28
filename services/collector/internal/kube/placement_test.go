// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(labels map[string]string, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: labels},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

func TestCloud(t *testing.T) {
	cases := []struct {
		name       string
		providerID string
		labels     map[string]string
		want       string
	}{
		{"aws provider id", "aws:///us-east-1a/i-0abc", nil, "aws"},
		{"gce provider id", "gce://proj/europe-west4-a/gke-node", nil, "gcp"},
		{"azure provider id", "azure:///subscriptions/x/resourceGroups/y", nil, "azure"},
		{"eks label fallback", "", map[string]string{"eks.amazonaws.com/nodegroup": "ng"}, "aws"},
		{"gke label fallback", "", map[string]string{"cloud.google.com/gke-nodepool": "p"}, "gcp"},
		{"aks label fallback", "", map[string]string{"kubernetes.azure.com/agentpool": "a"}, "azure"},
		{"kind / on-prem", "kind://docker/kind/kind-control-plane", map[string]string{"kubernetes.io/os": "linux"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Cloud(node(c.labels, c.providerID)); got != c.want {
				t.Errorf("Cloud = %q, want %q", got, c.want)
			}
		})
	}
	if Cloud(nil) != "" {
		t.Error("nil node must be unknown cloud")
	}
}

func TestLifecycle(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   string
	}{
		{nil, "on-demand"},
		{map[string]string{"eks.amazonaws.com/capacityType": "ON_DEMAND"}, "on-demand"},
		{map[string]string{"eks.amazonaws.com/capacityType": "SPOT"}, "spot"},
		{map[string]string{"karpenter.sh/capacity-type": "spot"}, "spot"},
		{map[string]string{"karpenter.sh/capacity-type": "reserved"}, "reserved"},
		{map[string]string{"cloud.google.com/gke-spot": "true"}, "spot"},
		{map[string]string{"cloud.google.com/gke-preemptible": "true"}, "spot"},
		{map[string]string{"kubernetes.azure.com/scalesetpriority": "spot"}, "spot"},
		{map[string]string{"kubehero.io/lifecycle": "Savings-Plan", "karpenter.sh/capacity-type": "spot"}, "savings-plan"},
	}
	for _, c := range cases {
		if got := Lifecycle(node(c.labels, "")); got != c.want {
			t.Errorf("Lifecycle(%v) = %q, want %q", c.labels, got, c.want)
		}
	}
}

func TestPlacementLabels(t *testing.T) {
	n := node(map[string]string{
		"topology.kubernetes.io/region":            "us-east-1",
		"failure-domain.beta.kubernetes.io/zone":   "us-east-1b",
		"beta.kubernetes.io/instance-type":         "p4d.24xlarge",
		"eks.amazonaws.com/nodegroup":              "gpu",
		"k8s.amazonaws.com/accelerator":            "nvidia-tesla-a100",
		"nvidia.com/gpu.product":                   "NVIDIA-A100-SXM4-40GB",
		"cloud.google.com/gke-nodepool":            "ignored-when-eks-present",
		"karpenter.k8s.aws/instance-gpu-name":      "a100",
		"kubernetes.azure.com/accelerator":         "nvidia",
		"cloud.google.com/gke-accelerator":         "nvidia-tesla-a100",
		"node.kubernetes.io/instance-type-ignored": "x",
	}, "")
	if Region(n) != "us-east-1" || Zone(n) != "us-east-1b" || SKU(n) != "p4d.24xlarge" {
		t.Fatalf("region/zone/sku = %q/%q/%q", Region(n), Zone(n), SKU(n))
	}
	if Nodepool(n) != "gpu" {
		t.Fatalf("nodepool = %q", Nodepool(n))
	}
	// GPU feature discovery wins over cloud labels: it names the exact SKU.
	if GPUKind(n) != "NVIDIA-A100-SXM4-40GB" {
		t.Fatalf("gpu kind = %q", GPUKind(n))
	}
	n.Status.Allocatable = corev1.ResourceList{
		"nvidia.com/gpu":      resource.MustParse("8"),
		corev1.ResourceCPU:    resource.MustParse("95690m"),
		corev1.ResourceMemory: resource.MustParse("1100Gi"),
	}
	if NodeGPUs(n) != 8 || AllocatableCPUMillis(n) != 95690 || AllocatableMemBytes(n) != 1100<<30 {
		t.Fatalf("allocatable = %d gpus, %dm, %d bytes", NodeGPUs(n), AllocatableCPUMillis(n), AllocatableMemBytes(n))
	}
}

func container(name, cpu, mem string, extra ...string) corev1.Container {
	req := corev1.ResourceList{}
	if cpu != "" {
		req[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		req[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	c := corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: req}}
	for i := 0; i+1 < len(extra); i += 2 {
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Limits[corev1.ResourceName(extra[i])] = resource.MustParse(extra[i+1])
	}
	return c
}

func TestPodRequests(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	sidecar := func(c corev1.Container) corev1.Container { c.RestartPolicy = &always; return c }
	mi := int64(1 << 20)
	cases := []struct {
		name string
		spec corev1.PodSpec
		want Resources
	}{
		{
			name: "sum of app containers",
			spec: corev1.PodSpec{Containers: []corev1.Container{container("a", "250m", "64Mi"), container("b", "750m", "192Mi")}},
			want: Resources{CPUMillis: 1000, MemBytes: 256 * mi},
		},
		{
			// A heavyweight migration init container dominates the
			// pod's reservation even though it exits.
			name: "init container larger than app",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{container("migrate", "2", "1Gi")},
				Containers:     []corev1.Container{container("app", "500m", "256Mi")},
			},
			want: Resources{CPUMillis: 2000, MemBytes: 1024 * mi},
		},
		{
			// Native sidecars (restartPolicy Always) add to the app sum
			// and to every later init container.
			name: "native sidecar",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					sidecar(container("proxy", "100m", "128Mi")),
					container("init", "1", "64Mi"),
				},
				Containers: []corev1.Container{container("app", "500m", "256Mi")},
			},
			want: Resources{CPUMillis: 1100, MemBytes: 384 * mi},
		},
		{
			name: "runtime class overhead",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{container("app", "1", "1Gi")},
				Overhead:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("160Mi")},
			},
			want: Resources{CPUMillis: 1250, MemBytes: 1184 * mi},
		},
		{
			name: "pod-level resources override container sums",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{container("a", "100m", "64Mi"), container("b", "", "")},
				Resources:  &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
			},
			want: Resources{CPUMillis: 2000, MemBytes: 64 * mi},
		},
		{
			// Extended resources may be set as limits only.
			name: "gpu from limits",
			spec: corev1.PodSpec{Containers: []corev1.Container{container("train", "8", "64Gi", "nvidia.com/gpu", "2")}},
			want: Resources{CPUMillis: 8000, MemBytes: 64 << 30, GPUs: 2},
		},
		{
			name: "best effort",
			spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x"}}},
			want: Resources{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PodRequests(&corev1.Pod{Spec: c.spec}); got != c.want {
				t.Errorf("PodRequests = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestContainerLimits(t *testing.T) {
	c := container("a", "1", "1Gi", "cpu", "2", "memory", "2Gi", "amd.com/gpu", "1")
	if got := ContainerLimits(&c); got != (Resources{CPUMillis: 2000, MemBytes: 2 << 30, GPUs: 1}) {
		t.Fatalf("limits = %+v", got)
	}
	if got := ContainerRequests(&c); got != (Resources{CPUMillis: 1000, MemBytes: 1 << 30, GPUs: 1}) {
		t.Fatalf("requests = %+v", got)
	}
}

func TestStripRuntimePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"containerd://abc123": "abc123",
		"cri-o://def":         "def",
		"docker://0f0f":       "0f0f",
		"bare":                "bare",
		"":                    "",
	} {
		if got := StripRuntimePrefix(in); got != want {
			t.Errorf("StripRuntimePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTeamAndCostCenter(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "edge"}}
	if Team(p) != "edge" || CostCenter(p) != "" {
		t.Fatalf("defaults: team=%q cc=%q", Team(p), CostCenter(p))
	}
	p.Labels = map[string]string{"kubehero.io/team": "payments", "kubehero.io/cost-center": "cc-42"}
	if Team(p) != "payments" || CostCenter(p) != "cc-42" {
		t.Fatalf("labels: team=%q cc=%q", Team(p), CostCenter(p))
	}
}
