// SPDX-License-Identifier: Apache-2.0
package ingest

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeNode builds a Node with allocatable cpu/memory + a node hourly
// price annotation so the cost math is deterministic.
func fakeNode(t *testing.T, hourlyUSD string, cpuCores int64, memGiB int64) *corev1.Node {
	t.Helper()
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n1",
			Annotations: map[string]string{
				"kubehero.io/node-hourly-usd": hourlyUSD,
			},
			Labels: map[string]string{
				"node.kubernetes.io/instance-type":  "m5.4xlarge",
				"topology.kubernetes.io/region":     "us-east-1",
				"kubehero.io/nodepool":              "platform",
			},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(cpuCores, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memGiB*1024*1024*1024, resource.BinarySI),
			},
		},
	}
	return n
}

func TestAttributePodSplitsCPUAndMemoryCost(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ml",
			Name:      "model-server-a100-0",
			Labels: map[string]string{
				"kubehero.io/team":        "ml-platform",
				"kubehero.io/cost-center": "research",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "n1",
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("16Gi"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// $1.00 / hr node, 16 cores, 64 GiB allocatable.
	// CPU rate: $1.00/16 = $0.0625 / core / hr
	// Mem rate: ($1.00 × 0.40) / 64 = $0.00625 / GiB / hr
	// Pod request: 4 cores × $0.0625 = $0.25 / hr
	//            + 16 GiB × $0.00625 = $0.10 / hr
	// Total: $0.35 / hr → $0.35/3600 ≈ $9.722e-5 / sec
	n := fakeNode(t, "1.00", 16, 64)
	out := attributePod(pod, n, 5*time.Second)

	cost, _ := out["costUsdSec"].(float64)
	if cost <= 0 {
		t.Fatalf("expected positive cost, got %v", cost)
	}
	wantHourly := 0.35
	wantSec := wantHourly / 3600
	if delta := cost - wantSec; delta > 1e-9 || delta < -1e-9 {
		t.Errorf("cost %v vs expected %v (delta %v)", cost, wantSec, delta)
	}
	if out["team"] != "ml-platform" {
		t.Errorf("team label not propagated: %v", out["team"])
	}
	if out["nodepool"] != "platform" {
		t.Errorf("nodepool label not propagated: %v", out["nodepool"])
	}
	if out["sku"] != "m5.4xlarge" {
		t.Errorf("sku label not propagated: %v", out["sku"])
	}
}

func TestAttributePodFallsBackToNamespaceTeam(t *testing.T) {
	// No team label — defaults to namespace.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "edge", Name: "api-1"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					},
				},
			}},
		},
	}
	out := attributePod(pod, nil, 5*time.Second)
	if out["team"] != "edge" {
		t.Fatalf("team should default to namespace, got %v", out["team"])
	}
}

func TestNodepoolReadsCloudVendorLabels(t *testing.T) {
	cases := []struct {
		name string
		labels map[string]string
		want string
	}{
		{"kubehero own", map[string]string{"kubehero.io/nodepool": "ml-a100"}, "ml-a100"},
		{"eks", map[string]string{"eks.amazonaws.com/nodegroup": "spot-batch"}, "spot-batch"},
		{"gke", map[string]string{"cloud.google.com/gke-nodepool": "default"}, "default"},
		{"aks", map[string]string{"agentpool": "user1"}, "user1"},
		{"karpenter", map[string]string{"karpenter.sh/nodepool": "spot"}, "spot"},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: c.labels}}
			if got := nodepoolOf(n); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestNodeHourlyUSDFallsBackWhenAnnotationMissing(t *testing.T) {
	// 4 cores, 16 GiB allocatable, no annotation.
	n := fakeNode(t, "", 4, 16)
	delete(n.Annotations, "kubehero.io/node-hourly-usd")
	got := nodeHourlyUSD(n)
	// 4 × 0.04 + 16 × 0.005 = 0.24
	want := 0.24
	if delta := got - want; delta > 1e-9 || delta < -1e-9 {
		t.Fatalf("fallback nodeHourlyUSD = %v, want %v", got, want)
	}
}
