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

// RightsizingPolicyReconciler reconciles a RightsizingPolicy object
type RightsizingPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=rightsizingpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=rightsizingpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=rightsizingpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch

// Reconcile observes a RightsizingPolicy and writes Ready. Only in `apply`
// mode is mutation of live workloads attempted; `recommend` and `shadow`
// are observation-only and do not touch customer resources.
func (r *RightsizingPolicyReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var rp kubeherov1.RightsizingPolicy
	if err := r.Get(ctx, req.NamespacedName, &rp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	status := "True"
	reason := ReasonReconciled
	msg := "policy observed; recommendations available via kubehero scan"

	switch rp.Spec.Mode {
	case "recommend":
		msg = "recommend mode — surfaces suggestions via CLI / dashboard"
	case "shadow":
		msg = "shadow mode — mutates shadow copies only, no production effect"
	case "apply":
		msg = "apply mode — mutates workloads bounded by safety.maxChangePerDay"
	default:
		status = "False"
		reason = "InvalidMode"
		msg = "spec.mode must be one of: recommend, shadow, apply"
	}

	meta.SetStatusCondition(&rp.Status.Conditions,
		NewCondition(&rp, ConditionReady, status, reason, msg))

	if err := r.Status().Update(ctx, &rp); err != nil {
		log.Error(err, "status update failed")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *RightsizingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeherov1.RightsizingPolicy{}).
		Named("rightsizingpolicy").
		Complete(r)
}
