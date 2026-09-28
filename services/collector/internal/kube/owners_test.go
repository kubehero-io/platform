// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package kube

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func ctrl(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
}

func podOwnedBy(kind, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "p-1", Labels: labels, OwnerReferences: ctrl(kind, name),
	}, Spec: corev1.PodSpec{NodeName: "node-a"}}
}

func countGets(cs *fake.Clientset, resource string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

func TestOwnerResolverFollowsOwnership(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout-7d9f8b6c5", OwnerReferences: ctrl("Deployment", "checkout")}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "canary-5f6d", OwnerReferences: ctrl("Rollout", "canary")}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bare-rs"}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "backup-28472910", OwnerReferences: ctrl("CronJob", "backup")}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "migrate-20240101"}},
	)
	r := NewOwnerResolver(cs, nil)
	cases := []struct {
		name string
		pod  *corev1.Pod
		want Workload
	}{
		{"deployment via replicaset", podOwnedBy("ReplicaSet", "checkout-7d9f8b6c5", nil), Workload{"checkout", "Deployment"}},
		{"argo rollout via replicaset", podOwnedBy("ReplicaSet", "canary-5f6d", nil), Workload{"canary", "Rollout"}},
		{"bare replicaset", podOwnedBy("ReplicaSet", "bare-rs", nil), Workload{"bare-rs", "ReplicaSet"}},
		{"cronjob via job", podOwnedBy("Job", "backup-28472910", nil), Workload{"backup", "CronJob"}},
		// Looks like a CronJob name, isn't one — the API says so.
		{"bare job", podOwnedBy("Job", "migrate-20240101", nil), Workload{"migrate-20240101", "Job"}},
		{"statefulset is direct", podOwnedBy("StatefulSet", "db", nil), Workload{"db", "StatefulSet"}},
		{"daemonset is direct", podOwnedBy("DaemonSet", "fluent-bit", nil), Workload{"fluent-bit", "DaemonSet"}},
		{"naked pod", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "debug"}}, Workload{"debug", "Pod"}},
		{
			"static pod strips node suffix",
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver-node-a", OwnerReferences: ctrl("Node", "node-a")},
				Spec:       corev1.PodSpec{NodeName: "node-a"},
			},
			Workload{"kube-apiserver", "Pod"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.Resolve(context.Background(), c.pod); got != c.want {
				t.Errorf("Resolve = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestOwnerResolverCachesPerOwner(t *testing.T) {
	cs := fake.NewSimpleClientset(&appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-abc", OwnerReferences: ctrl("Deployment", "api")},
	})
	r := NewOwnerResolver(cs, nil)
	for range 50 {
		if got := r.Resolve(context.Background(), podOwnedBy("ReplicaSet", "api-abc", nil)); got.Name != "api" {
			t.Fatalf("Resolve = %+v", got)
		}
	}
	if n := countGets(cs, "replicasets"); n != 1 {
		t.Fatalf("50 replicas must cost one GET, got %d", n)
	}
	// After the TTL the owner is re-read (ownership can change).
	now := time.Now()
	r.now = func() time.Time { return now.Add(2 * time.Hour) }
	r.Resolve(context.Background(), podOwnedBy("ReplicaSet", "api-abc", nil))
	if n := countGets(cs, "replicasets"); n != 2 {
		t.Fatalf("expected a refresh after TTL, got %d GETs", n)
	}
}

func TestOwnerResolverFallsBackOnForbidden(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "replicasets"}, "x", nil)
	})
	r := NewOwnerResolver(cs, nil)
	pod := podOwnedBy("ReplicaSet", "checkout-7d9f8b6c5", map[string]string{"pod-template-hash": "7d9f8b6c5"})
	for range 3 {
		if got := r.Resolve(context.Background(), pod); got != (Workload{"checkout", "Deployment"}) {
			t.Fatalf("heuristic = %+v", got)
		}
	}
	// The failure is negatively cached: one GET, not one per call.
	if n := countGets(cs, "replicasets"); n != 1 {
		t.Fatalf("forbidden lookups must be negatively cached, got %d GETs", n)
	}
	// Without the hash label the RS is the honest answer.
	pod2 := podOwnedBy("ReplicaSet", "other-rs", nil)
	if got := r.Resolve(context.Background(), pod2); got != (Workload{"other-rs", "ReplicaSet"}) {
		t.Fatalf("no-hash fallback = %+v", got)
	}
}

func TestOwnerResolverPeekNeverCallsAPI(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewOwnerResolver(cs, nil)
	got := r.Peek(podOwnedBy("ReplicaSet", "web-5c7b", map[string]string{"pod-template-hash": "5c7b"}))
	if got != (Workload{"web", "Deployment"}) {
		t.Fatalf("Peek = %+v", got)
	}
	if len(cs.Actions()) != 0 {
		t.Fatalf("Peek must not touch the API, got %v", cs.Actions())
	}
	// A nil resolver degrades to heuristics too.
	var nilR *OwnerResolver
	if got := nilR.Peek(podOwnedBy("Job", "j1", nil)); got != (Workload{"j1", "Job"}) {
		t.Fatalf("nil Peek = %+v", got)
	}
}

func TestOwnerResolverBoundedCache(t *testing.T) {
	r := NewOwnerResolver(nil, nil)
	r.maxEntries = 100
	for i := range 1000 {
		r.store(ownerKey{namespace: "ns", kind: "ReplicaSet", name: string(rune('a'+i%26)) + time.Duration(i).String()}, Workload{}, time.Hour)
	}
	if len(r.entries) > 100 {
		t.Fatalf("cache grew to %d entries, cap 100", len(r.entries))
	}
}
