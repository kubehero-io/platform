// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package kube

import (
	corev1 "k8s.io/api/core/v1"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// PodRefFor builds the telemetry PodRef for a pod (container unset).
// n may be nil when the node isn't known; zone is then empty.
func PodRefFor(p *corev1.Pod, n *corev1.Node, w Workload) *kuberov1.PodRef {
	return &kuberov1.PodRef{
		Namespace:    p.Namespace,
		Pod:          p.Name,
		Workload:     w.Name,
		WorkloadKind: w.Kind,
		Node:         p.Spec.NodeName,
		Team:         Team(p),
		Zone:         Zone(n),
		PodUid:       string(p.UID),
	}
}
