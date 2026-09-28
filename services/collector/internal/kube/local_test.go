// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package kube

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type runtimeObject = runtime.Object

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func localPod(name, uid, containerID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: name, UID: types.UID(uid),
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
			Annotations:   map[string]string{corev1.LastAppliedConfigAnnotation: "{huge}", "keep": "me"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "SECRET", Value: "x"}}}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: containerID}},
		},
	}
}

func startLocal(t *testing.T, cs *fake.Clientset, nodeName string) (*LocalCache, context.CancelFunc) {
	t.Helper()
	lc, err := NewLocalCache(cs, nodeName, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Start(ctx)
	if err := lc.WaitForSync(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	return lc, cancel
}

// The DaemonSet must only ever list its own node's pods and its own
// Node — this is the N× cost / N² apiserver-load fix.
func TestLocalCacheScopesListsToNode(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var mu sync.Mutex
	selectors := map[string]string{}
	cs.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtimeObject, error) {
		mu.Lock()
		defer mu.Unlock()
		if la, ok := a.(k8stesting.ListAction); ok {
			selectors[a.GetResource().Resource] = la.GetListRestrictions().Fields.String()
		}
		return false, nil, nil
	})
	_, cancel := startLocal(t, cs, "node-a")
	defer cancel()
	mu.Lock()
	defer mu.Unlock()
	if selectors["pods"] != "spec.nodeName=node-a" {
		t.Errorf("pod list field selector = %q", selectors["pods"])
	}
	if selectors["nodes"] != "metadata.name=node-a" {
		t.Errorf("node list field selector = %q", selectors["nodes"])
	}
}

func TestLocalCacheAllNodesModeHasNoSelector(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var mu sync.Mutex
	var podSelector *string
	cs.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtimeObject, error) {
		mu.Lock()
		defer mu.Unlock()
		s := a.(k8stesting.ListAction).GetListRestrictions().Fields.String()
		podSelector = &s
		return false, nil, nil
	})
	_, cancel := startLocal(t, cs, "")
	defer cancel()
	mu.Lock()
	defer mu.Unlock()
	if podSelector == nil || *podSelector != "" {
		t.Fatalf("all-nodes mode must list without a field selector, got %v", podSelector)
	}
}

func TestLocalCacheLookupsAndTrimming(t *testing.T) {
	cs := fake.NewSimpleClientset(
		localPod("checkout-1", "uid-1", "containerd://aaa111"),
		localPod("checkout-2", "uid-2", "cri-o://bbb222"),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
	)
	lc, cancel := startLocal(t, cs, "node-a")
	defer cancel()

	if got := len(lc.Pods()); got != 2 {
		t.Fatalf("pods = %d", got)
	}
	p, ok := lc.PodByUID("uid-1")
	if !ok || p.Name != "checkout-1" {
		t.Fatalf("PodByUID = %v, %v", p, ok)
	}
	if len(p.ManagedFields) != 0 || p.Annotations[corev1.LastAppliedConfigAnnotation] != "" || p.Annotations["keep"] != "me" {
		t.Fatalf("transform must strip managedFields + last-applied only: %+v", p.ObjectMeta)
	}
	if p.Spec.Containers[0].Env != nil {
		t.Fatal("transform must drop container env (may hold inline secrets)")
	}
	p2, name, ok := lc.PodByContainerID("bbb222")
	if !ok || p2.Name != "checkout-2" || name != "app" {
		t.Fatalf("PodByContainerID = %v %q %v", p2, name, ok)
	}
	if _, ok := lc.PodByName("shop", "checkout-2"); !ok {
		t.Fatal("PodByName miss")
	}
	if lc.Node("node-a") == nil || len(lc.Nodes()) != 1 {
		t.Fatal("node not cached")
	}

	// Deleted pods stay resolvable by UID for late log lines.
	if err := cs.CoreV1().Pods("shop").Delete(context.Background(), "checkout-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "pod delete", func() bool { return len(lc.Pods()) == 1 })
	if p, ok := lc.PodByUID("uid-1"); !ok || p.Name != "checkout-1" {
		t.Fatalf("tombstone lookup = %v, %v", p, ok)
	}
	lc.now = func() time.Time { return time.Now().Add(tombstoneTTL + time.Minute) }
	if _, ok := lc.PodByUID("uid-1"); ok {
		t.Fatal("tombstone must expire")
	}
}

// ── Resolver ──────────────────────────────────────────────────────────

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestResolverLookupIP(t *testing.T) {
	r, err := NewResolver(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.upsertNode(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"topology.kubernetes.io/zone": "us-east-1a"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.1.10"},
			{Type: corev1.NodeExternalIP, Address: "54.1.2.3"},
			{Type: corev1.NodeHostName, Address: "ip-10-0-1-10"},
		}},
	})
	r.upsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "checkout-7d9f8b6c5-x2x4q", UID: "u1",
			Labels:          map[string]string{"pod-template-hash": "7d9f8b6c5", "kubehero.io/team": "payments"},
			OwnerReferences: ctrl("ReplicaSet", "checkout-7d9f8b6c5"),
		},
		Spec:   corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "10.244.1.5"}, {IP: "fd00::5"}}},
	})
	// hostNetwork pods share the node IP; they must not claim it.
	r.upsertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-proxy-x", UID: "u2"},
		Spec:       corev1.PodSpec{NodeName: "node-a", HostNetwork: true},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.10"},
	})
	r.upsertService(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.20", ClusterIPs: []string{"10.96.0.20", "fd00:96::20"}},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "34.5.6.7"}, {IP: "10.0.1.10"}}}},
	})
	r.upsertService(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "headless"},
		Spec:       corev1.ServiceSpec{ClusterIP: "None"},
	})

	cases := []struct {
		ip, kind, name, service, zone string
	}{
		{"10.244.1.5", "pod", "checkout", "", "us-east-1a"},
		{"fd00::5", "pod", "checkout", "", "us-east-1a"},
		{"::ffff:10.244.1.5", "pod", "checkout", "", "us-east-1a"}, // v4-mapped
		{"10.96.0.20", "service", "checkout", "shop/checkout", ""},
		{"fd00:96::20", "service", "checkout", "shop/checkout", ""},
		{"34.5.6.7", "service", "checkout", "shop/checkout", ""},
		// Node address wins over a ServiceLB ingress on the same IP.
		{"10.0.1.10", "node", "node-a", "", "us-east-1a"},
		{"54.1.2.3", "node", "node-a", "", "us-east-1a"},
		{"1.1.1.1", "external", "1.1.1.1", "", ""},
	}
	for _, c := range cases {
		ep := r.LookupIP(mustAddr(c.ip))
		if ep.GetKind() != c.kind || ep.GetName() != c.name || ep.GetService() != c.service || ep.GetZone() != c.zone {
			t.Errorf("LookupIP(%s) = kind %q name %q service %q zone %q; want %q %q %q %q",
				c.ip, ep.GetKind(), ep.GetName(), ep.GetService(), ep.GetZone(), c.kind, c.name, c.service, c.zone)
		}
		if ep.GetIp() == "" {
			t.Errorf("LookupIP(%s) left ip empty", c.ip)
		}
	}
	ref := r.LookupIP(mustAddr("10.244.1.5")).GetPod()
	if ref.GetWorkload() != "checkout" || ref.GetWorkloadKind() != "Deployment" || ref.GetTeam() != "payments" ||
		ref.GetNode() != "node-a" || ref.GetPodUid() != "u1" || ref.GetNamespace() != "shop" {
		t.Fatalf("pod ref = %+v", ref)
	}
}

// An IP recycled to a new pod before the old pod's delete event arrives
// keeps its new owner; a pod that finished releases its IP.
func TestResolverIPReuse(t *testing.T) {
	r, _ := NewResolver(nil, nil, nil)
	mk := func(name, uid string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, UID: types.UID(uid)},
			Status:     corev1.PodStatus{Phase: phase, PodIP: "10.244.9.9"},
		}
	}
	old, repl := mk("old", "u-old", corev1.PodRunning), mk("new", "u-new", corev1.PodRunning)
	r.upsertPod(old)
	r.upsertPod(repl)
	r.deletePod(old)
	if got := r.LookupIP(mustAddr("10.244.9.9")).GetPod().GetPod(); got != "new" {
		t.Fatalf("recycled IP owner = %q, want new", got)
	}
	r.upsertPod(mk("new", "u-new", corev1.PodSucceeded))
	if got := r.LookupIP(mustAddr("10.244.9.9")).GetKind(); got != "external" {
		t.Fatalf("finished pod must release its IP, got kind %q", got)
	}
}

func TestResolverServiceAndNodeDelete(t *testing.T) {
	r, _ := NewResolver(nil, nil, nil)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.1.1"}}
	r.upsertService(svc)
	svc2 := svc.DeepCopy()
	svc2.Spec.ClusterIP = "10.96.1.2"
	r.upsertService(svc2) // IP change: old address released
	if r.LookupIP(mustAddr("10.96.1.1")).GetKind() != "external" || r.LookupIP(mustAddr("10.96.1.2")).GetKind() != "service" {
		t.Fatal("service IP update not applied")
	}
	r.deleteService(svc2)
	if r.LookupIP(mustAddr("10.96.1.2")).GetKind() != "external" {
		t.Fatal("service delete not applied")
	}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}}}
	r.upsertNode(n)
	r.deleteNode(n)
	if r.LookupIP(mustAddr("10.0.0.1")).GetKind() != "external" {
		t.Fatal("node delete not applied")
	}
}

func TestResolverContainerLookupAndServiceName(t *testing.T) {
	p := localPod("checkout-7d9f8b6c5-x2x4q", "11111111-2222-3333-4444-555555555555", "containerd://"+"abcdef")
	p.Labels = map[string]string{"pod-template-hash": "7d9f8b6c5"}
	p.OwnerReferences = ctrl("ReplicaSet", "checkout-7d9f8b6c5")
	annotated := localPod("worker-0", "u-worker", "containerd://ffff")
	annotated.Annotations[AnnotationService] = "billing-worker"
	cs := fake.NewSimpleClientset(p, annotated, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a", Labels: map[string]string{"topology.kubernetes.io/zone": "z1"},
	}})
	lc, cancel := startLocal(t, cs, "node-a")
	defer cancel()
	r, err := NewResolver(nil, lc, NewOwnerResolver(nil, nil))
	if err != nil {
		t.Fatal(err)
	}

	// systemd cgroup driver spells the UID with underscores.
	ref, ok := r.LookupContainer("11111111_2222_3333_4444_555555555555", "abcdef")
	if !ok || ref.GetContainer() != "app" || ref.GetWorkload() != "checkout" || ref.GetZone() != "z1" {
		t.Fatalf("LookupContainer = %+v, %v", ref, ok)
	}
	// Unknown container ID of a known pod: attributed to the pod.
	ref, ok = r.LookupContainer("11111111-2222-3333-4444-555555555555", "not-in-status-yet")
	if !ok || ref.GetPod() != "checkout-7d9f8b6c5-x2x4q" || ref.GetContainer() != "" {
		t.Fatalf("pod-only lookup = %+v, %v", ref, ok)
	}
	// System cgroups are not Kubernetes containers.
	if _, ok := r.LookupContainer("", "kubelet"); ok {
		t.Fatal("non-kubernetes cgroup must not resolve")
	}
	// Container ID of another pod with a mismatching UID: rejected.
	if _, ok := r.LookupContainer("u-other", "ffff"); ok {
		t.Fatal("uid mismatch must not resolve")
	}

	if got := r.ServiceName(ref); got != "checkout" {
		t.Fatalf("ServiceName = %q, want workload", got)
	}
	aref, _ := r.LookupContainer("u-worker", "ffff")
	if got := r.ServiceName(aref); got != "billing-worker" {
		t.Fatalf("ServiceName with annotation = %q", got)
	}
}

// End to end through real (fake-backed) informers.
func TestResolverInformers(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "cart-0", UID: "u-cart", OwnerReferences: ctrl("StatefulSet", "cart")},
			Spec:       corev1.PodSpec{NodeName: "node-b"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.2.7"},
		},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "cart"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.3.3"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"topology.kubernetes.io/zone": "z2"}},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.2.2"}}},
		},
	)
	r, err := NewResolver(cs, nil, NewOwnerResolver(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ep := r.LookupIP(mustAddr("10.244.2.7"))
	if ep.GetKind() != "pod" || ep.GetName() != "cart" || ep.GetZone() != "z2" || ep.GetPod().GetWorkloadKind() != "StatefulSet" {
		t.Fatalf("pod endpoint = %+v", ep)
	}
	if r.LookupIP(mustAddr("10.96.3.3")).GetService() != "shop/cart" || r.LookupIP(mustAddr("10.0.2.2")).GetName() != "node-b" {
		t.Fatal("service/node not indexed from informers")
	}
	// Live updates flow through.
	if _, err := cs.CoreV1().Pods("shop").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "cart-1", UID: "u-cart-1", OwnerReferences: ctrl("StatefulSet", "cart")},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.2.8"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "new pod indexed", func() bool { return r.LookupIP(mustAddr("10.244.2.8")).GetKind() == "pod" })
}

// Static pods: the kubelet names cgroups and /var/log/pods directories
// after the config hash, not the mirror pod's API UID.
func TestStaticPodResolvesByConfigHash(t *testing.T) {
	mirror := localPod("kube-proxy-node-a", "mirror-uid", "containerd://cafe01")
	mirror.Annotations[AnnotationConfigHash] = "0123456789abcdef0123456789abcdef"
	mirror.OwnerReferences = ctrl("Node", "node-a")
	cs := fake.NewSimpleClientset(mirror, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	lc, cancel := startLocal(t, cs, "node-a")
	defer cancel()
	if p, ok := lc.PodByUID("0123456789abcdef0123456789abcdef"); !ok || p.Name != "kube-proxy-node-a" {
		t.Fatalf("PodByUID(config hash) = %v, %v", p, ok)
	}
	r, _ := NewResolver(nil, lc, NewOwnerResolver(nil, nil))
	ref, ok := r.LookupContainer("0123456789abcdef0123456789abcdef", "cafe01")
	if !ok || ref.GetContainer() != "app" || ref.GetWorkload() != "kube-proxy" {
		t.Fatalf("static pod container lookup = %+v, %v", ref, ok)
	}
	if ref, ok := r.LookupContainer("0123456789abcdef0123456789abcdef", ""); !ok || ref.GetPod() != "kube-proxy-node-a" {
		t.Fatalf("static pod-level cgroup lookup = %+v, %v", ref, ok)
	}
}
