// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package kube holds the collector's Kubernetes-facing plumbing: API
// client construction, the node-local informer cache every collector
// runs, workload (owner) resolution, placement helpers that read cloud /
// zone / nodepool labels, and the cluster-wide IP index the eBPF flow
// attribution needs.
//
// The central design rule is node locality. The collector is a
// DaemonSet, so anything that scales with cluster size on every node
// multiplies by the node count: listing every pod from every collector
// is N× the apiserver load and, worse, N× the reported cost. The local
// cache therefore watches only the pods bound to this node
// (fieldSelector spec.nodeName=<node>) and only this node's Node object.
// Cluster-wide watches exist only where they are unavoidable (eBPF peer
// attribution) or run on the elected leader alone.
package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds a clientset: in-cluster config first (the DaemonSet
// path), then the given kubeconfig / $KUBECONFIG / ~/.kube/config for
// local development.
func NewClient(kubeconfig, userAgent string) (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if kubeconfig != "" {
			rules.ExplicitPath = kubeconfig
		}
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and no usable kubeconfig: %w", err)
		}
	}
	cfg.UserAgent = userAgent
	// The collector's steady state is watches; the budget only matters
	// for bursts (owner lookups after a rollout, leader relists). Keep
	// it modest — it is multiplied by the node count.
	cfg.QPS, cfg.Burst = 10, 20
	return kubernetes.NewForConfig(cfg)
}

const (
	indexUID         = "uid"
	indexContainerID = "containerID"

	tombstoneTTL = 10 * time.Minute
	maxTombstone = 4096
)

// LocalCache is the node-scoped view: pods bound to this node and this
// node's Node object. With an empty node name (local development
// against a kubeconfig) it watches every pod and node instead.
type LocalCache struct {
	nodeName string
	log      *slog.Logger

	podFactory  informers.SharedInformerFactory
	nodeFactory informers.SharedInformerFactory
	pods        cache.SharedIndexInformer
	nodes       cache.SharedIndexInformer

	mu         sync.Mutex
	tombstones map[types.UID]tombstone
	now        func() time.Time
}

type tombstone struct {
	pod     *corev1.Pod
	expires time.Time
}

// NewLocalCache wires (but does not start) the informers.
func NewLocalCache(client kubernetes.Interface, nodeName string, log *slog.Logger) (*LocalCache, error) {
	if log == nil {
		log = slog.Default()
	}
	c := &LocalCache{
		nodeName:   nodeName,
		log:        log,
		tombstones: map[types.UID]tombstone{},
		now:        time.Now,
	}
	podOpts, nodeOpts := []informers.SharedInformerOption{}, []informers.SharedInformerOption{}
	if nodeName != "" {
		podOpts = append(podOpts, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
		}))
		nodeOpts = append(nodeOpts, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", nodeName).String()
		}))
	}
	// No periodic resync: every consumer reads the lister on its own
	// cadence, so replaying the store through handlers buys nothing.
	c.podFactory = informers.NewSharedInformerFactoryWithOptions(client, 0, podOpts...)
	c.nodeFactory = informers.NewSharedInformerFactoryWithOptions(client, 0, nodeOpts...)
	c.pods = c.podFactory.Core().V1().Pods().Informer()
	c.nodes = c.nodeFactory.Core().V1().Nodes().Informer()

	if err := c.pods.SetTransform(trimPod); err != nil {
		return nil, err
	}
	if err := c.nodes.SetTransform(trimObject); err != nil {
		return nil, err
	}
	if err := c.pods.AddIndexers(cache.Indexers{
		indexUID:         podUIDs,
		indexContainerID: containerIDs,
	}); err != nil {
		return nil, err
	}
	if _, err := c.pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: c.onPodDelete,
	}); err != nil {
		return nil, err
	}
	return c, nil
}

// NodeName is the node this cache is scoped to ("" = all nodes).
func (c *LocalCache) NodeName() string { return c.nodeName }

// Start runs the informers until ctx is cancelled.
func (c *LocalCache) Start(ctx context.Context) {
	c.podFactory.Start(ctx.Done())
	c.nodeFactory.Start(ctx.Done())
}

// WaitForSync blocks until both informers have listed once or ctx ends.
func (c *LocalCache) WaitForSync(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), c.pods.HasSynced, c.nodes.HasSynced) {
		return errors.New("local informer cache did not sync")
	}
	return nil
}

// Pods returns every pod in the cache (all phases). The objects are
// shared with the informer and must not be mutated.
func (c *LocalCache) Pods() []*corev1.Pod {
	items := c.pods.GetStore().List()
	out := make([]*corev1.Pod, 0, len(items))
	for _, it := range items {
		if p, ok := it.(*corev1.Pod); ok {
			out = append(out, p)
		}
	}
	return out
}

// PodByUID finds a live pod by API UID or static-pod config hash — or
// one deleted in the last few minutes, so log lines flushed after a pod
// is gone still get their labels.
func (c *LocalCache) PodByUID(uid string) (*corev1.Pod, bool) {
	if objs, err := c.pods.GetIndexer().ByIndex(indexUID, uid); err == nil && len(objs) > 0 {
		if p, ok := objs[0].(*corev1.Pod); ok {
			return p, true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tombstones[types.UID(uid)]; ok && c.now().Before(t.expires) {
		return t.pod, true
	}
	return nil, false
}

// PodByName looks a pod up by namespace/name.
func (c *LocalCache) PodByName(namespace, name string) (*corev1.Pod, bool) {
	obj, ok, err := c.pods.GetStore().GetByKey(namespace + "/" + name)
	if err != nil || !ok {
		return nil, false
	}
	p, ok := obj.(*corev1.Pod)
	return p, ok
}

// PodByContainerID finds the pod running a container (runtime prefix
// already stripped) and returns the container's name.
func (c *LocalCache) PodByContainerID(id string) (*corev1.Pod, string, bool) {
	objs, err := c.pods.GetIndexer().ByIndex(indexContainerID, id)
	if err != nil || len(objs) == 0 {
		return nil, "", false
	}
	p, ok := objs[0].(*corev1.Pod)
	if !ok {
		return nil, "", false
	}
	for _, st := range allStatuses(p) {
		if StripRuntimePrefix(st.ContainerID) == id {
			return p, st.Name, true
		}
	}
	return p, "", true
}

// Node returns a node by name (nil when unknown).
func (c *LocalCache) Node(name string) *corev1.Node {
	obj, ok, err := c.nodes.GetStore().GetByKey(name)
	if err != nil || !ok {
		return nil
	}
	n, _ := obj.(*corev1.Node)
	return n
}

// Nodes returns every node in the cache (just this one in DaemonSet mode).
func (c *LocalCache) Nodes() []*corev1.Node {
	items := c.nodes.GetStore().List()
	out := make([]*corev1.Node, 0, len(items))
	for _, it := range items {
		if n, ok := it.(*corev1.Node); ok {
			out = append(out, n)
		}
	}
	return out
}

func (c *LocalCache) onPodDelete(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.tombstones) >= maxTombstone {
		for uid, t := range c.tombstones {
			if now.After(t.expires) {
				delete(c.tombstones, uid)
			}
		}
		if len(c.tombstones) >= maxTombstone {
			// Churn beyond the bound: forget arbitrary entries rather
			// than grow. Losing labels on a few late lines is the cheap
			// failure mode.
			for uid := range c.tombstones {
				delete(c.tombstones, uid)
				if len(c.tombstones) < maxTombstone/2 {
					break
				}
			}
		}
	}
	c.tombstones[p.UID] = tombstone{pod: p, expires: now.Add(tombstoneTTL)}
}

// AnnotationConfigHash is set on mirror pods of static pods. The kubelet
// names the static pod's cgroup and /var/log/pods directory after this
// hash, not after the mirror pod's API UID.
const AnnotationConfigHash = "kubernetes.io/config.hash"

// podUIDs indexes a pod under its API UID and, for static pods, the
// config hash the node-level artefacts use.
func podUIDs(obj any) ([]string, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	keys := []string{string(p.UID)}
	if h := p.Annotations[AnnotationConfigHash]; h != "" && h != string(p.UID) {
		keys = append(keys, h)
	}
	return keys, nil
}

// MatchesUID reports whether uid (from a cgroup path or log directory)
// identifies p: its API UID or, for a static pod, its config hash.
func MatchesUID(p *corev1.Pod, uid string) bool {
	return uid == string(p.UID) || (uid != "" && p.Annotations[AnnotationConfigHash] == uid)
}

func containerIDs(obj any) ([]string, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	var out []string
	for _, st := range allStatuses(p) {
		if id := StripRuntimePrefix(st.ContainerID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// allStatuses concatenates app, init and ephemeral container statuses.
func allStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0,
		len(p.Status.ContainerStatuses)+len(p.Status.InitContainerStatuses)+len(p.Status.EphemeralContainerStatuses))
	out = append(out, p.Status.ContainerStatuses...)
	out = append(out, p.Status.InitContainerStatuses...)
	out = append(out, p.Status.EphemeralContainerStatuses...)
	return out
}

// trimObject drops managedFields and the kubectl last-applied blob —
// typically the bulk of an object's size and never read here.
func trimObject(obj any) (any, error) {
	if m, ok := obj.(metav1.Object); ok {
		m.SetManagedFields(nil)
		if a := m.GetAnnotations(); a != nil {
			if _, ok := a[corev1.LastAppliedConfigAnnotation]; ok {
				delete(a, corev1.LastAppliedConfigAnnotation)
			}
		}
	}
	return obj, nil
}

func trimPod(obj any) (any, error) {
	obj, _ = trimObject(obj)
	if p, ok := obj.(*corev1.Pod); ok {
		// Env and volume mounts can be large (and may carry inline
		// config); nothing in the collector reads them.
		for i := range p.Spec.Containers {
			p.Spec.Containers[i].Env = nil
			p.Spec.Containers[i].VolumeMounts = nil
		}
		for i := range p.Spec.InitContainers {
			p.Spec.InitContainers[i].Env = nil
			p.Spec.InitContainers[i].VolumeMounts = nil
		}
		p.Spec.Volumes = nil
	}
	return obj, nil
}
