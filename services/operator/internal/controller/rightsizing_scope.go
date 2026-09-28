// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// maxWorkloadsPerPolicy bounds one evaluation's memory and API cost.
// Beyond it the policy still evaluates the first N (sorted by namespace)
// and says so in its Ready condition.
const maxWorkloadsPerPolicy = 5000

// systemNamespaces are left out of a catch-all ({}) namespaceSelector —
// resizing the cluster's own control surface should always be an
// explicit choice.
var systemNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// vpaListGVK is the VerticalPodAutoscaler list kind. The VPA CRD is
// optional; the operator reads it unstructured so it needs no VPA Go
// module and tolerates the CRD's absence.
var vpaListGVK = schema.GroupVersionKind{
	Group:   "autoscaling.k8s.io",
	Version: "v1",
	Kind:    "VerticalPodAutoscalerList",
}

// ClusterInScope evaluates a policy's clusterSelector against this
// cluster's labels. A nil selector matches every cluster.
func ClusterInScope(sel *metav1.LabelSelector, clusterLabels map[string]string) (bool, error) {
	if sel == nil {
		return true, nil
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false, fmt.Errorf("invalid clusterSelector: %w", err)
	}
	return s.Matches(labels.Set(clusterLabels)), nil
}

// ParseClusterLabels turns "env=prod,cloud=aws" into a label map and
// adds kubehero.io/cluster-id so a clusterSelector can pin one cluster.
func ParseClusterLabels(raw, clusterID string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if clusterID != "" {
		out["kubehero.io/cluster-id"] = clusterID
	}
	return out
}

// namespacesInScope resolves the namespaceSelector. nil selects nothing
// (policies must opt namespaces in explicitly); {} selects every
// non-system namespace. Terminating namespaces and namespaces annotated
// kubehero.io/rightsizing=disabled are skipped.
func namespacesInScope(ctx context.Context, r client.Reader, sel *metav1.LabelSelector) ([]string, error) {
	if sel == nil {
		return nil, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, fmt.Errorf("invalid namespaceSelector: %w", err)
	}
	var list corev1.NamespaceList
	if err := r.List(ctx, &list, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	catchAll := selector.Empty()
	var out []string
	for i := range list.Items {
		ns := &list.Items[i]
		switch {
		case ns.DeletionTimestamp != nil:
		case catchAll && systemNamespaces[ns.Name]:
		case disabled(ns.Annotations):
		default:
			out = append(out, ns.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func disabled(annotations map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(annotations[AnnotationRightsizing]), "disabled")
}

// excluded reports whether spec.exclude names w (as "name" or
// "namespace/name").
func excluded(exclude []string, ns, name string) bool {
	for _, e := range exclude {
		e = strings.TrimSpace(e)
		if e == name || e == ns+"/"+name {
			return true
		}
	}
	return false
}

// listWorkloads returns the Deployments and StatefulSets of ns that the
// policy may consider, minus exclusions and opted-out workloads.
func listWorkloads(ctx context.Context, r client.Reader, ns string, exclude []string) ([]workload, error) {
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", ns, err)
	}
	var sts appsv1.StatefulSetList
	if err := r.List(ctx, &sts, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list statefulsets in %s: %w", ns, err)
	}
	out := make([]workload, 0, len(deps.Items)+len(sts.Items))
	for i := range deps.Items {
		d := &deps.Items[i]
		if d.DeletionTimestamp != nil || excluded(exclude, ns, d.Name) || disabled(d.Annotations) {
			continue
		}
		out = append(out, workloadFromDeployment(d))
	}
	for i := range sts.Items {
		s := &sts.Items[i]
		if s.DeletionTimestamp != nil || excluded(exclude, ns, s.Name) || disabled(s.Annotations) {
			continue
		}
		out = append(out, workloadFromStatefulSet(s))
	}
	return out, nil
}

// vpaLookup lists VerticalPodAutoscalers per namespace, remembering
// within one evaluation that the VPA API is not installed (the common,
// harmless case) so absent CRDs cost one discovery miss, not one per
// namespace.
type vpaLookup struct {
	reader client.Reader
	absent bool // VPA CRD not installed; skip further lookups
}

// forNamespace returns VPA targets in ns keyed by "Kind/name". A missing
// VPA CRD yields an empty map; any other failure is returned so apply
// mode fails closed (it cannot prove no VPA manages the container).
func (v *vpaLookup) forNamespace(ctx context.Context, ns string) (map[string][]vpaTarget, error) {
	out := map[string][]vpaTarget{}
	if v.absent {
		return out, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vpaListGVK)
	if err := v.reader.List(ctx, list, client.InNamespace(ns)); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			v.absent = true
			return out, nil
		}
		return nil, err
	}
	for _, item := range list.Items {
		kind, _, _ := unstructured.NestedString(item.Object, "spec", "targetRef", "kind")
		name, _, _ := unstructured.NestedString(item.Object, "spec", "targetRef", "name")
		if kind == "" || name == "" {
			continue
		}
		t := vpaTarget{Name: item.GetName(), OptedOut: map[string]bool{}}
		policies, _, _ := unstructured.NestedSlice(item.Object, "spec", "resourcePolicy", "containerPolicies")
		for _, p := range policies {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			cn, _ := pm["containerName"].(string)
			mode, _ := pm["mode"].(string)
			if cn != "" && mode == "Off" {
				t.OptedOut[cn] = true
			}
		}
		key := kind + "/" + name
		out[key] = append(out[key], t)
	}
	return out, nil
}

// hpaResources maps "Kind/name" → the utilization-scaled resources of
// every HPA in ns (see autoscalerInfo.HPAResources for the key shape).
func hpaResources(ctx context.Context, r client.Reader, ns string) (map[string]map[string]string, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		if meta.IsNoMatchError(err) {
			return map[string]map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]map[string]string{}
	for i := range list.Items {
		h := &list.Items[i]
		key := h.Spec.ScaleTargetRef.Kind + "/" + h.Spec.ScaleTargetRef.Name
		if out[key] == nil {
			out[key] = map[string]string{}
		}
		if len(h.Spec.Metrics) == 0 {
			// autoscaling/v2 defaults to 80% average CPU utilization.
			out[key]["cpu"] = h.Name
			continue
		}
		for _, m := range h.Spec.Metrics {
			switch {
			case m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil &&
				m.Resource.Target.Type == autoscalingv2.UtilizationMetricType:
				out[key][string(m.Resource.Name)] = h.Name
			case m.Type == autoscalingv2.ContainerResourceMetricSourceType && m.ContainerResource != nil &&
				m.ContainerResource.Target.Type == autoscalingv2.UtilizationMetricType:
				out[key][m.ContainerResource.Container+"/"+string(m.ContainerResource.Name)] = h.Name
			}
		}
	}
	return out, nil
}
