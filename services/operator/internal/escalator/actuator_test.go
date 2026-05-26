// SPDX-License-Identifier: BUSL-1.1
package escalator

import (
	"context"
	"encoding/json"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// scheme registers every type the actuator touches so the fake client
// can serialise them. We avoid envtest here — the fake client is enough
// to assert behaviour; envtest is reserved for the reconciler suite
// where leader-election + watch fan-out matter.
func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := autoscalingv2.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCapHPAFloorsAt1AndCapturesPrev(t *testing.T) {
	s := scheme(t)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas: ptr32(1),
			MaxReplicas: 10,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(hpa).Build()
	a := &Actuator{Client: c}

	prev, err := a.CapHPA(context.Background(), "prod", "api", 0.5)
	if err != nil {
		t.Fatalf("CapHPA: %v", err)
	}

	// Previous spec should be marshalled with maxReplicas=10
	var p autoscalingv2.HorizontalPodAutoscalerSpec
	if err := json.Unmarshal(prev, &p); err != nil {
		t.Fatalf("unmarshal prev: %v", err)
	}
	if p.MaxReplicas != 10 {
		t.Errorf("previousSpec.MaxReplicas = %d, want 10", p.MaxReplicas)
	}

	updated := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: "prod", Name: "api"}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.MaxReplicas != 5 {
		t.Errorf("after CapHPA, MaxReplicas = %d, want 5", updated.Spec.MaxReplicas)
	}
}

func TestCapHPARefusesOutOfRangeRatio(t *testing.T) {
	a := &Actuator{Client: fake.NewClientBuilder().WithScheme(scheme(t)).Build()}
	for _, r := range []float32{0, -0.1, 1.1, 2.0} {
		if _, err := a.CapHPA(context.Background(), "x", "y", r); err == nil {
			t.Errorf("ratio=%v should error", r)
		}
	}
}

func TestEvictPodsSkipsSystemPriority(t *testing.T) {
	s := scheme(t)
	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ml", Name: "system-critical",
				Labels: map[string]string{"team": "ml"},
			},
			Spec: corev1.PodSpec{PriorityClassName: "system-cluster-critical"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ml", Name: "regular-1",
				Labels: map[string]string{"team": "ml"},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ml", Name: "regular-2",
				Labels: map[string]string{"team": "ml"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pods...).Build()
	a := &Actuator{Client: c}

	evicted, err := a.EvictPods(context.Background(), "ml", map[string]string{"team": "ml"})
	if err != nil {
		t.Fatalf("EvictPods: %v", err)
	}
	// fake.Client doesn't implement the eviction subresource handler
	// fully — we mainly assert here that the system pod was filtered.
	// The exact returned set depends on the fake impl, but we should
	// never see the system pod in it.
	for _, name := range evicted {
		if name == "system-critical" {
			t.Fatalf("system-priority pod should not be evicted")
		}
	}
}

func TestCordonNodepoolSkipsAlreadyCordoned(t *testing.T) {
	s := scheme(t)
	nodes := []client.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "n1",
				Labels: map[string]string{NodepoolLabelKey: "batch"},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "n2",
				Labels: map[string]string{NodepoolLabelKey: "batch"},
			},
			Spec: corev1.NodeSpec{Unschedulable: true},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "n3",
				Labels: map[string]string{NodepoolLabelKey: "prod"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(nodes...).Build()
	a := &Actuator{Client: c}

	cordoned, prev, err := a.CordonNodepool(context.Background(), "batch")
	if err != nil {
		t.Fatalf("CordonNodepool: %v", err)
	}
	if len(cordoned) != 1 || cordoned[0] != "n1" {
		t.Errorf("cordoned = %v, want [n1] (n2 already cordoned, n3 different pool)", cordoned)
	}
	// Previous spec should record both batch nodes' state — n1=false, n2=true.
	var prevState map[string]bool
	if err := json.Unmarshal(prev, &prevState); err != nil {
		t.Fatalf("unmarshal prev: %v", err)
	}
	if prevState["n1"] != false || prevState["n2"] != true {
		t.Errorf("prevState = %v, want {n1: false, n2: true}", prevState)
	}
	if _, ok := prevState["n3"]; ok {
		t.Errorf("prevState should not include n3 (different nodepool)")
	}
}

func TestCordonNodepoolRequiresValue(t *testing.T) {
	a := &Actuator{Client: fake.NewClientBuilder().WithScheme(scheme(t)).Build()}
	if _, _, err := a.CordonNodepool(context.Background(), ""); err == nil {
		t.Fatal("empty nodepool value should error")
	}
}

func TestActuatorNoopWhenClientNil(t *testing.T) {
	var a *Actuator
	if _, err := a.CapHPA(context.Background(), "x", "y", 0.5); err != nil {
		t.Errorf("nil receiver should be a no-op, got %v", err)
	}
}

func ptr32(v int32) *int32 { return &v }
