// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package escalator runs the escalation plan attached to a CeilingPolicy
// once the operator's reconciler has decided the policy should fire.
//
// The actuator surface is deliberately narrow — three actions, all
// reversible within the cooldown window:
//
//   hpa.cap          — patch HorizontalPodAutoscaler.maxReplicas to
//                      floor(current * ratio). Records the previous
//                      value so `kubehero undo <audit-id>` can restore.
//
//   pod.evict        — list pods matching a label selector + post one
//                      Eviction subresource per pod. Skips
//                      system-cluster-critical and system-node-critical
//                      priority classes; honours PDBs (the eviction API
//                      will refuse if a PDB would be violated).
//
//   nodepool.cordon  — list every node carrying a `kubehero.io/nodepool`
//                      label whose value matches the spec, then patch
//                      spec.unschedulable=true on each.
//
// All three actions are no-ops if the actuator's Client is nil; this
// keeps the operator unit-testable without spinning up envtest.

package escalator

import (
	"context"
	"encoding/json"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodepoolLabelKey is the canonical label customers tag nodes with so
// the cordon action can find a nodepool. Most cloud-managed clusters
// also carry a vendor-specific label (eks.amazonaws.com/nodegroup,
// cloud.google.com/gke-nodepool, agentpool on AKS) — the cordon action
// honours this one for portability; vendor labels stay as the
// chargeback axis.
const NodepoolLabelKey = "kubehero.io/nodepool"

// systemPriorityClasses are exempt from eviction. Kubernetes itself
// reserves these values for cluster-critical workloads (CoreDNS,
// kube-proxy, controller managers, etc.). Stomping on them would brick
// the cluster well before the budget policy mattered.
var systemPriorityClasses = map[string]bool{
	"system-cluster-critical": true,
	"system-node-critical":    true,
}

// Actuator is a real client-go-backed implementation. controller-runtime's
// `client.Client` already wraps client-go for us — we accept whatever
// the manager passes in, including the typed scheme it was set up with.
type Actuator struct {
	Client client.Client
}

// CapHPA patches an HPA's spec.maxReplicas to floor(current * ratio).
// `ratio` is the multiplier (0..1]; the typical "cap at 50%" ceiling
// maps to ratio=0.5. Returns the previous spec as JSON so the audit
// trail can replay it during `kubehero undo`.
func (a *Actuator) CapHPA(
	ctx context.Context,
	namespace, name string,
	ratio float32,
) (previousSpec []byte, err error) {
	if a == nil || a.Client == nil {
		return nil, nil
	}
	if ratio <= 0 || ratio > 1 {
		return nil, fmt.Errorf("ratio must be in (0, 1]; got %v", ratio)
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, hpa); err != nil {
		return nil, fmt.Errorf("get HPA %s/%s: %w", namespace, name, err)
	}

	prev, err := json.Marshal(hpa.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal HPA spec: %w", err)
	}

	current := hpa.Spec.MaxReplicas
	if current <= 0 {
		current = 1
	}
	target := int32(float32(current) * ratio)
	if target < 1 {
		target = 1
	}
	if target == current {
		return prev, nil
	}

	patched := hpa.DeepCopy()
	patched.Spec.MaxReplicas = target
	if err := a.Client.Patch(ctx, patched, client.MergeFrom(hpa)); err != nil {
		return prev, fmt.Errorf("patch HPA %s/%s: %w", namespace, name, err)
	}
	return prev, nil
}

// EvictPods lists pods matching the label selector and creates an
// Eviction subresource per pod. The Eviction API is the cluster's
// preferred way to remove pods because it respects PodDisruptionBudgets
// — if removing a pod would violate a PDB, kube-apiserver returns 429
// and we skip that pod.
//
// Returns the names of pods successfully evicted. Pods carrying a
// system priority class are skipped silently so an aggressive policy
// can't take down the cluster's own control surface.
func (a *Actuator) EvictPods(
	ctx context.Context,
	namespace string,
	selector map[string]string,
) (evicted []string, err error) {
	if a == nil || a.Client == nil {
		return nil, nil
	}
	var list corev1.PodList
	if err := a.Client.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(selector)},
	); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	for i := range list.Items {
		p := &list.Items[i]
		if systemPriorityClasses[p.Spec.PriorityClassName] {
			continue
		}
		ev := &policyv1.Eviction{
			ObjectMeta: p.ObjectMeta, // namespace + name carry over
		}
		// SubResourceCreate goes to the /eviction endpoint, which is
		// the API server's PDB-aware path. controller-runtime's client
		// exposes this via the SubResource("eviction") helper.
		if err := a.Client.SubResource("eviction").Create(ctx, p, ev); err != nil {
			// PDB-violating evictions return 429 — we surface this as
			// "skipped" rather than fatal so the runner keeps going.
			continue
		}
		evicted = append(evicted, p.Name)
	}
	return evicted, nil
}

// CordonNodepool lists nodes carrying NodepoolLabelKey=<value> and
// patches spec.unschedulable=true on each. Returns the names of nodes
// successfully cordoned.
//
// This is reversible — a follow-up `kubehero undo` calls the inverse
// operation (uncordon) using the previousSpec captured here.
func (a *Actuator) CordonNodepool(
	ctx context.Context,
	nodepoolValue string,
) (cordoned []string, previousSpec []byte, err error) {
	if a == nil || a.Client == nil {
		return nil, nil, nil
	}
	if nodepoolValue == "" {
		return nil, nil, fmt.Errorf("nodepool value is required")
	}

	var list corev1.NodeList
	if err := a.Client.List(ctx, &list,
		client.MatchingLabels{NodepoolLabelKey: nodepoolValue},
	); err != nil {
		return nil, nil, fmt.Errorf("list nodes: %w", err)
	}

	// Snapshot the schedulable state of every targeted node so we can
	// restore on undo.
	prevState := map[string]bool{}
	for _, n := range list.Items {
		prevState[n.Name] = n.Spec.Unschedulable
	}
	prev, _ := json.Marshal(prevState)

	for i := range list.Items {
		n := &list.Items[i]
		if n.Spec.Unschedulable {
			continue
		}
		patched := n.DeepCopy()
		patched.Spec.Unschedulable = true
		if err := a.Client.Patch(ctx, patched, client.MergeFrom(n)); err != nil {
			continue
		}
		cordoned = append(cordoned, n.Name)
	}
	return cordoned, prev, nil
}
