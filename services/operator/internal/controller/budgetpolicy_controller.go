// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

// BudgetPolicyReconciler reconciles a BudgetPolicy object
type BudgetPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=budgetpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=budgetpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=budgetpolicies/finalizers,verbs=update

// Reconcile observes a BudgetPolicy and writes Ready + Armed conditions.
// No enforcement steps execute unless the resource carries the armed
// annotation (see AnnotationArmed) — or humanArm is explicitly false.
func (r *BudgetPolicyReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var bp kubeherov1.BudgetPolicy
	if err := r.Get(ctx, req.NamespacedName, &bp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	armed := IsArmed(&bp, bp.Spec.HumanArm)
	advisory := !bp.Spec.HardStop

	armedStatus := "False"
	armedReason := ReasonNotArmed
	armedMsg := "awaiting `kubehero cap --arm` or dashboard arming"
	if armed {
		armedStatus = "True"
		armedReason = ReasonArmedOK
		armedMsg = "enforcement steps will run when trigger conditions are met"
	} else if advisory {
		armedStatus = "True"
		armedReason = ReasonObserveOnly
		armedMsg = "policy is advisory (hardStop=false); alerting only"
	}

	readyStatus := "True"
	readyMsg := "spec observed; policy tracked"

	meta.SetStatusCondition(&bp.Status.Conditions,
		NewCondition(&bp, ConditionReady, readyStatus, ReasonReconciled, readyMsg))
	meta.SetStatusCondition(&bp.Status.Conditions,
		NewCondition(&bp, ConditionArmed, armedStatus, armedReason, armedMsg))

	if err := r.Status().Update(ctx, &bp); err != nil {
		log.Error(err, "status update failed")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *BudgetPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeherov1.BudgetPolicy{}).
		Named("budgetpolicy").
		Complete(r)
}
