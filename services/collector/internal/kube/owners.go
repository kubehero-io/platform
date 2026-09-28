// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package kube

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Workload is a pod's top-level owner: the Deployment behind a
// ReplicaSet, the CronJob behind a Job, or the StatefulSet / DaemonSet /
// custom controller that owns the pod directly.
type Workload struct {
	Name string
	Kind string // Deployment | StatefulSet | DaemonSet | Job | CronJob | ReplicaSet | Pod | <custom kind>
}

// OwnerResolver maps pods to workloads. Two ownership hops need an API
// read (ReplicaSet → Deployment, Job → CronJob); results are cached per
// direct owner so a node with 100 replicas of one Deployment does one
// GET, not 100, and a steady-state collector does almost none.
//
// When the read fails (RBAC gap, owner already deleted, API timeout) we
// fall back to naming heuristics that are right for the overwhelmingly
// common case — a ReplicaSet named <deployment>-<pod-template-hash> —
// and cache that answer briefly so a broken RBAC setup doesn't turn
// into a GET per pod per scan.
type OwnerResolver struct {
	client kubernetes.Interface
	log    *slog.Logger
	now    func() time.Time

	ttl, negTTL time.Duration
	maxEntries  int
	timeout     time.Duration

	mu      sync.Mutex
	entries map[ownerKey]ownerEntry
	warned  bool
}

type ownerKey struct{ namespace, kind, name string }

type ownerEntry struct {
	w       Workload
	expires time.Time
}

// NewOwnerResolver builds a resolver. client may be nil (tests, or
// callers that only want heuristics).
func NewOwnerResolver(client kubernetes.Interface, log *slog.Logger) *OwnerResolver {
	if log == nil {
		log = slog.Default()
	}
	return &OwnerResolver{
		client:     client,
		log:        log,
		now:        time.Now,
		ttl:        time.Hour,
		negTTL:     time.Minute,
		maxEntries: 20_000,
		timeout:    5 * time.Second,
		entries:    map[ownerKey]ownerEntry{},
	}
}

// Resolve returns the pod's workload, reading ReplicaSets / Jobs through
// the API on a cache miss.
func (r *OwnerResolver) Resolve(ctx context.Context, p *corev1.Pod) Workload {
	return r.resolve(ctx, p, true)
}

// Peek resolves without any API call: cache hit, else heuristics. Used
// on hot paths over cluster-wide pods (eBPF flow attribution), where a
// GET per remote pod would reintroduce the N² load the node-local
// design removes.
func (r *OwnerResolver) Peek(p *corev1.Pod) Workload {
	return r.resolve(context.Background(), p, false)
}

func (r *OwnerResolver) resolve(ctx context.Context, p *corev1.Pod, fetch bool) Workload {
	ref := metav1.GetControllerOf(p)
	if r == nil {
		// No resolver wired (tests, minimal setups): heuristics only.
		if ref == nil && len(p.OwnerReferences) > 0 {
			ref = &p.OwnerReferences[0]
		}
		if ref == nil {
			return Workload{Name: p.Name, Kind: "Pod"}
		}
		if ref.Kind == "Node" {
			return Workload{Name: strings.TrimSuffix(p.Name, "-"+p.Spec.NodeName), Kind: "Pod"}
		}
		return heuristic(p, ref)
	}
	if ref == nil {
		if len(p.OwnerReferences) == 0 {
			return Workload{Name: p.Name, Kind: "Pod"}
		}
		ref = &p.OwnerReferences[0]
	}
	switch ref.Kind {
	case "ReplicaSet", "Job":
		// Needs one more hop; handled below.
	case "Node":
		// Static (mirror) pods are named <manifest-name>-<node-name>;
		// strip the node so every node's kube-proxy is one workload.
		return Workload{Name: strings.TrimSuffix(p.Name, "-"+p.Spec.NodeName), Kind: "Pod"}
	default:
		return Workload{Name: ref.Name, Kind: ref.Kind}
	}

	key := ownerKey{namespace: p.Namespace, kind: ref.Kind, name: ref.Name}
	if w, ok := r.cached(key); ok {
		return w
	}
	if !fetch || r.client == nil {
		return heuristic(p, ref)
	}

	w, err := r.fetch(ctx, key)
	if err != nil {
		w = heuristic(p, ref)
		r.store(key, w, r.negTTL)
		r.warnOnce(key, err)
		return w
	}
	r.store(key, w, r.ttl)
	return w
}

func (r *OwnerResolver) fetch(ctx context.Context, key ownerKey) (Workload, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var owner *metav1.OwnerReference
	switch key.kind {
	case "ReplicaSet":
		rs, err := r.client.AppsV1().ReplicaSets(key.namespace).Get(ctx, key.name, metav1.GetOptions{})
		if err != nil {
			return Workload{}, err
		}
		owner = metav1.GetControllerOf(rs)
	case "Job":
		job, err := r.client.BatchV1().Jobs(key.namespace).Get(ctx, key.name, metav1.GetOptions{})
		if err != nil {
			return Workload{}, err
		}
		owner = metav1.GetControllerOf(job)
	}
	if owner == nil {
		// A bare ReplicaSet / Job is its own workload.
		return Workload{Name: key.name, Kind: key.kind}, nil
	}
	return Workload{Name: owner.Name, Kind: owner.Kind}, nil
}

// heuristic names the workload without the API. Deployments name their
// ReplicaSets <deployment>-<pod-template-hash> and stamp the hash on
// every pod, which makes the strip unambiguous; anything else stays at
// the direct owner — honest rather than guessed (a Job named
// "migrate-20240101" must not become CronJob "migrate").
func heuristic(p *corev1.Pod, ref *metav1.OwnerReference) Workload {
	if ref.Kind == "ReplicaSet" {
		if h := p.Labels["pod-template-hash"]; h != "" && strings.HasSuffix(ref.Name, "-"+h) {
			return Workload{Name: strings.TrimSuffix(ref.Name, "-"+h), Kind: "Deployment"}
		}
	}
	return Workload{Name: ref.Name, Kind: ref.Kind}
}

func (r *OwnerResolver) cached(key ownerKey) (Workload, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok || r.now().After(e.expires) {
		return Workload{}, false
	}
	return e.w, true
}

func (r *OwnerResolver) store(key ownerKey, w Workload, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if len(r.entries) >= r.maxEntries {
		// Evict expired entries first; if everything is live (a very
		// large namespace churn), drop the soonest-to-expire tenth.
		for k, e := range r.entries {
			if now.After(e.expires) {
				delete(r.entries, k)
			}
		}
		if len(r.entries) >= r.maxEntries {
			evictSoonest(r.entries, r.maxEntries/10+1)
		}
	}
	r.entries[key] = ownerEntry{w: w, expires: now.Add(ttl)}
}

func evictSoonest(m map[ownerKey]ownerEntry, n int) {
	type kv struct {
		k ownerKey
		t time.Time
	}
	all := make([]kv, 0, len(m))
	for k, e := range m {
		all = append(all, kv{k, e.expires})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, v := range all[:min(n, len(all))] {
		delete(m, v.k)
	}
}

func (r *OwnerResolver) warnOnce(key ownerKey, err error) {
	r.mu.Lock()
	first := !r.warned
	r.warned = true
	r.mu.Unlock()
	if first {
		r.log.Warn("owner lookup failed — naming workloads by heuristic (check get on replicasets/jobs in the collector ClusterRole)",
			"kind", key.kind, "namespace", key.namespace, "name", key.name, "err", err)
	}
}
