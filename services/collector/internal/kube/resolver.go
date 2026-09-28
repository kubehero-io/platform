// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package kube

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/ebpf"
)

// AnnotationService overrides the profile / flow "service" name of a
// pod (default: its workload).
const AnnotationService = "kubehero.io/service"

// Resolver implements ebpf.Resolver: it attributes addresses seen on
// the wire to pods, services and nodes anywhere in the cluster, and
// cgroups on this node to containers.
//
// The IP index is fed by cluster-wide pod / service / node informers —
// every node needs it, because a flow's peer can be any pod in the
// cluster. To keep that affordable on every node, the informers store
// trimmed objects (identity, owner, IPs, phase) rather than full specs,
// and workload names come from OwnerResolver.Peek (cache or naming
// heuristics, never an API read per remote pod).
//
// Lookups are precedence-ordered so address collisions resolve the
// way the traffic actually flows: pod IPs (non-hostNetwork), then
// service cluster IPs, then node addresses, then load-balancer /
// external IPs (which on ServiceLB / kube-vip setups are node IPs, and
// traffic to them lands on nodes).
type Resolver struct {
	local  *LocalCache
	owners *OwnerResolver

	factory informers.SharedInformerFactory
	synced  []cache.InformerSynced

	mu        sync.RWMutex
	podIPs    map[netip.Addr]*podEntry
	podByUID  map[types.UID][]netip.Addr
	svcIPs    map[netip.Addr]string // clusterIP → ns/name
	svcByKey  map[string][]netip.Addr
	lbIPs     map[netip.Addr]string // LB ingress / externalIPs → ns/name
	lbByKey   map[string][]netip.Addr
	nodeIPs   map[netip.Addr]string // address → node name
	nodeByKey map[string][]netip.Addr
	nodeZone  map[string]string
}

type podEntry struct {
	uid, namespace, name, node string
	workload, workloadKind     string
	team, service              string
}

var _ ebpf.Resolver = (*Resolver)(nil)

// NewResolver wires (but does not start) the cluster-wide informers.
// client may be nil, in which case only the node-local container lookups
// work (tests, or eBPF profiler without netflow).
func NewResolver(client kubernetes.Interface, local *LocalCache, owners *OwnerResolver) (*Resolver, error) {
	r := &Resolver{
		local:     local,
		owners:    owners,
		podIPs:    map[netip.Addr]*podEntry{},
		podByUID:  map[types.UID][]netip.Addr{},
		svcIPs:    map[netip.Addr]string{},
		svcByKey:  map[string][]netip.Addr{},
		lbIPs:     map[netip.Addr]string{},
		lbByKey:   map[string][]netip.Addr{},
		nodeIPs:   map[netip.Addr]string{},
		nodeByKey: map[string][]netip.Addr{},
		nodeZone:  map[string]string{},
	}
	if client == nil {
		return r, nil
	}
	r.factory = informers.NewSharedInformerFactory(client, 0)
	pods := r.factory.Core().V1().Pods().Informer()
	svcs := r.factory.Core().V1().Services().Informer()
	nodes := r.factory.Core().V1().Nodes().Informer()
	for _, inf := range []cache.SharedIndexInformer{pods, svcs, nodes} {
		if err := inf.SetTransform(skeleton); err != nil {
			return nil, err
		}
	}
	handlers := []struct {
		inf cache.SharedIndexInformer
		h   cache.ResourceEventHandlerFuncs
	}{
		{pods, cache.ResourceEventHandlerFuncs{
			AddFunc:    func(o any) { r.upsertPod(o) },
			UpdateFunc: func(_, o any) { r.upsertPod(o) },
			DeleteFunc: func(o any) { r.deletePod(o) },
		}},
		{svcs, cache.ResourceEventHandlerFuncs{
			AddFunc:    func(o any) { r.upsertService(o) },
			UpdateFunc: func(_, o any) { r.upsertService(o) },
			DeleteFunc: func(o any) { r.deleteService(o) },
		}},
		{nodes, cache.ResourceEventHandlerFuncs{
			AddFunc:    func(o any) { r.upsertNode(o) },
			UpdateFunc: func(_, o any) { r.upsertNode(o) },
			DeleteFunc: func(o any) { r.deleteNode(o) },
		}},
	}
	for _, h := range handlers {
		reg, err := h.inf.AddEventHandler(h.h)
		if err != nil {
			return nil, err
		}
		r.synced = append(r.synced, reg.HasSynced)
	}
	return r, nil
}

// Start runs the cluster-wide informers until ctx ends and waits for the
// initial list, so the first eBPF drain doesn't label every peer
// "external".
func (r *Resolver) Start(ctx context.Context) error {
	if r.factory == nil {
		return nil
	}
	r.factory.Start(ctx.Done())
	// Wait on the handler registrations, not just the stores: the index
	// is built by the handlers, which lag the store slightly.
	if !cache.WaitForCacheSync(ctx.Done(), r.synced...) {
		return errors.New("cluster-wide pod/service/node informers did not sync")
	}
	return nil
}

// LookupIP implements ebpf.Resolver.
func (r *Resolver) LookupIP(ip netip.Addr) *kuberov1.FlowEndpoint {
	ip = ip.Unmap()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.podIPs[ip]; ok {
		zone := r.nodeZone[p.node]
		name := p.workload
		if name == "" {
			name = p.name
		}
		return &kuberov1.FlowEndpoint{
			Ip:   ip.String(),
			Kind: "pod",
			Zone: zone,
			Name: name,
			Pod: &kuberov1.PodRef{
				Namespace:    p.namespace,
				Pod:          p.name,
				Workload:     p.workload,
				WorkloadKind: p.workloadKind,
				Node:         p.node,
				Team:         p.team,
				Zone:         zone,
				PodUid:       p.uid,
			},
		}
	}
	if svc, ok := r.svcIPs[ip]; ok {
		return serviceEndpoint(ip, svc)
	}
	if node, ok := r.nodeIPs[ip]; ok {
		return &kuberov1.FlowEndpoint{Ip: ip.String(), Kind: "node", Name: node, Zone: r.nodeZone[node]}
	}
	if svc, ok := r.lbIPs[ip]; ok {
		return serviceEndpoint(ip, svc)
	}
	return &kuberov1.FlowEndpoint{Ip: ip.String(), Kind: "external", Name: ip.String()}
}

func serviceEndpoint(ip netip.Addr, key string) *kuberov1.FlowEndpoint {
	name := key
	if i := strings.IndexByte(key, '/'); i >= 0 {
		name = key[i+1:]
	}
	return &kuberov1.FlowEndpoint{Ip: ip.String(), Kind: "service", Service: key, Name: name}
}

// LookupContainer implements ebpf.Resolver for containers on this node.
// Cgroup paths spell the pod UID with underscores (systemd driver); both
// spellings are accepted.
func (r *Resolver) LookupContainer(podUID, containerID string) (*kuberov1.PodRef, bool) {
	if r.local == nil {
		return nil, false
	}
	podUID = strings.ReplaceAll(podUID, "_", "-")
	containerID = StripRuntimePrefix(containerID)
	if containerID != "" {
		if p, name, ok := r.local.PodByContainerID(containerID); ok && (podUID == "" || string(p.UID) == podUID) {
			ref := PodRefFor(p, r.local.Node(p.Spec.NodeName), r.owners.Peek(p))
			ref.Container = name
			return ref, true
		}
	}
	if podUID == "" {
		return nil, false
	}
	// Container not in status yet (just started): attribute to the pod.
	p, ok := r.local.PodByUID(podUID)
	if !ok {
		return nil, false
	}
	return PodRefFor(p, r.local.Node(p.Spec.NodeName), r.owners.Peek(p)), true
}

// ServiceName implements ebpf.Resolver: the kubehero.io/service
// annotation, else the workload, else the pod name.
func (r *Resolver) ServiceName(ref *kuberov1.PodRef) string {
	if ref == nil {
		return ""
	}
	if r.local != nil {
		if p, ok := r.local.PodByName(ref.GetNamespace(), ref.GetPod()); ok {
			if s := p.Annotations[AnnotationService]; s != "" {
				return s
			}
		}
	}
	r.mu.RLock()
	for _, ip := range r.podByUID[types.UID(ref.GetPodUid())] {
		if e, ok := r.podIPs[ip]; ok && e.service != "" {
			r.mu.RUnlock()
			return e.service
		}
	}
	r.mu.RUnlock()
	if ref.GetWorkload() != "" {
		return ref.GetWorkload()
	}
	return ref.GetPod()
}

// ── index maintenance ─────────────────────────────────────────────────

func (r *Resolver) upsertPod(obj any) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	var ips []netip.Addr
	terminal := p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
	if !p.Spec.HostNetwork && !terminal && p.DeletionTimestamp == nil {
		ips = podIPs(p)
	}
	w := r.owners.Peek(p)
	e := &podEntry{
		uid: string(p.UID), namespace: p.Namespace, name: p.Name, node: p.Spec.NodeName,
		workload: w.Name, workloadKind: w.Kind, team: Team(p), service: p.Annotations[AnnotationService],
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removePodIPsLocked(p.UID)
	for _, ip := range ips {
		r.podIPs[ip] = e
	}
	if len(ips) > 0 {
		r.podByUID[p.UID] = ips
	}
}

func (r *Resolver) deletePod(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removePodIPsLocked(p.UID)
}

// removePodIPsLocked drops a pod's addresses — but only where they still
// point at that pod: an IP recycled to a new pod before the old pod's
// delete event arrives must keep its new owner.
func (r *Resolver) removePodIPsLocked(uid types.UID) {
	for _, ip := range r.podByUID[uid] {
		if e, ok := r.podIPs[ip]; ok && e.uid == string(uid) {
			delete(r.podIPs, ip)
		}
	}
	delete(r.podByUID, uid)
}

func (r *Resolver) upsertService(obj any) {
	s, ok := obj.(*corev1.Service)
	if !ok {
		return
	}
	key := s.Namespace + "/" + s.Name
	var cluster, lb []netip.Addr
	for _, raw := range append([]string{s.Spec.ClusterIP}, s.Spec.ClusterIPs...) {
		if ip, err := netip.ParseAddr(raw); err == nil {
			cluster = appendUnique(cluster, ip.Unmap())
		}
	}
	for _, raw := range s.Spec.ExternalIPs {
		if ip, err := netip.ParseAddr(raw); err == nil {
			lb = appendUnique(lb, ip.Unmap())
		}
	}
	for _, in := range s.Status.LoadBalancer.Ingress {
		if ip, err := netip.ParseAddr(in.IP); err == nil {
			lb = appendUnique(lb, ip.Unmap())
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replaceKeyed(r.svcIPs, r.svcByKey, key, cluster, key)
	replaceKeyed(r.lbIPs, r.lbByKey, key, lb, key)
}

func (r *Resolver) deleteService(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	s, ok := obj.(*corev1.Service)
	if !ok {
		return
	}
	key := s.Namespace + "/" + s.Name
	r.mu.Lock()
	defer r.mu.Unlock()
	replaceKeyed(r.svcIPs, r.svcByKey, key, nil, key)
	replaceKeyed(r.lbIPs, r.lbByKey, key, nil, key)
}

func (r *Resolver) upsertNode(obj any) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	var ips []netip.Addr
	for _, a := range n.Status.Addresses {
		if a.Type != corev1.NodeInternalIP && a.Type != corev1.NodeExternalIP {
			continue
		}
		if ip, err := netip.ParseAddr(a.Address); err == nil {
			ips = appendUnique(ips, ip.Unmap())
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replaceKeyed(r.nodeIPs, r.nodeByKey, n.Name, ips, n.Name)
	r.nodeZone[n.Name] = Zone(n)
}

func (r *Resolver) deleteNode(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	n, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replaceKeyed(r.nodeIPs, r.nodeByKey, n.Name, nil, n.Name)
	delete(r.nodeZone, n.Name)
}

// replaceKeyed swaps the addresses owned by key, removing only entries
// that still belong to key.
func replaceKeyed(index map[netip.Addr]string, byKey map[string][]netip.Addr, key string, ips []netip.Addr, value string) {
	for _, ip := range byKey[key] {
		if index[ip] == value {
			delete(index, ip)
		}
	}
	delete(byKey, key)
	for _, ip := range ips {
		index[ip] = value
	}
	if len(ips) > 0 {
		byKey[key] = ips
	}
}

func podIPs(p *corev1.Pod) []netip.Addr {
	var out []netip.Addr
	for _, pip := range p.Status.PodIPs {
		if ip, err := netip.ParseAddr(pip.IP); err == nil {
			out = appendUnique(out, ip.Unmap())
		}
	}
	if ip, err := netip.ParseAddr(p.Status.PodIP); err == nil {
		out = appendUnique(out, ip.Unmap())
	}
	return out
}

func appendUnique(s []netip.Addr, ip netip.Addr) []netip.Addr {
	for _, v := range s {
		if v == ip {
			return s
		}
	}
	return append(s, ip)
}

// skeleton reduces cluster-wide objects to the fields the index reads.
// With tens of thousands of pods per cluster and this informer running
// on every node, full objects would cost tens of MiB per collector.
func skeleton(obj any) (any, error) {
	switch o := obj.(type) {
	case *corev1.Pod:
		out := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: o.Name, Namespace: o.Namespace, UID: o.UID, ResourceVersion: o.ResourceVersion,
				Labels:          pick(o.Labels, "kubehero.io/team", "pod-template-hash"),
				Annotations:     pick(o.Annotations, AnnotationService),
				OwnerReferences: o.OwnerReferences, DeletionTimestamp: o.DeletionTimestamp,
			},
			Spec: corev1.PodSpec{NodeName: o.Spec.NodeName, HostNetwork: o.Spec.HostNetwork},
			Status: corev1.PodStatus{
				Phase: o.Status.Phase, PodIP: o.Status.PodIP, PodIPs: o.Status.PodIPs,
			},
		}
		return out, nil
	case *corev1.Service:
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: o.Name, Namespace: o.Namespace, UID: o.UID, ResourceVersion: o.ResourceVersion},
			Spec: corev1.ServiceSpec{
				ClusterIP: o.Spec.ClusterIP, ClusterIPs: o.Spec.ClusterIPs, ExternalIPs: o.Spec.ExternalIPs,
			},
			Status: corev1.ServiceStatus{LoadBalancer: o.Status.LoadBalancer},
		}, nil
	case *corev1.Node:
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: o.Name, UID: o.UID, ResourceVersion: o.ResourceVersion,
				Labels: pick(o.Labels, "topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone"),
			},
			Status: corev1.NodeStatus{Addresses: o.Status.Addresses},
		}, nil
	}
	return obj, nil
}

func pick(m map[string]string, keys ...string) map[string]string {
	var out map[string]string
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if out == nil {
				out = make(map[string]string, len(keys))
			}
			out[k] = v
		}
	}
	return out
}
