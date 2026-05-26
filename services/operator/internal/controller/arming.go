// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The annotation set by `kubehero cap --arm` (or the dashboard). When present
// and equal to "true", the resource is considered armed and the reconciler
// may proceed with enforcement steps. Missing or any other value means the
// reconciler observes but does not act.
const AnnotationArmed = "kubehero.kubehero.io/armed"

// Condition types surfaced on every KubeHero CRD.
const (
	ConditionReady    = "Ready"
	ConditionArmed    = "Armed"
	ConditionTripped  = "Tripped"
	ReasonArmedOK     = "OperatorArmed"
	ReasonNotArmed    = "RequiresHumanArm"
	ReasonObserveOnly = "AdvisoryPolicy"
	ReasonReconciled  = "SpecReconciled"
	ReasonWaitingArm  = "WaitingForArm"
	// CeilingPolicy-specific reasons
	ReasonBurnRateOK       = "BurnRateBelowTrigger"
	ReasonBurnRateExceeded = "BurnRateExceeded"
	ReasonBurnRateUnknown  = "BurnRateUnavailable"
)

// IsArmed reports whether a reconciler should proceed with enforcement.
// It respects the human-arm requirement: if humanArm is true (or nil —
// which defaults to true), the resource must carry the arming annotation.
func IsArmed(obj client.Object, humanArm *bool) bool {
	required := true
	if humanArm != nil {
		required = *humanArm
	}
	if !required {
		return true
	}
	a := obj.GetAnnotations()
	if a == nil {
		return false
	}
	return a[AnnotationArmed] == "true"
}

// NewCondition builds a metav1.Condition with the observed generation stamped
// in, so status updates don't race with spec changes.
func NewCondition(
	obj client.Object,
	condType, status, reason, msg string,
) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionStatus(status),
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: obj.GetGeneration(),
		LastTransitionTime: metav1.Now(),
	}
}
