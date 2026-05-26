// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

// EscalationRunner is what the reconciler calls when a policy trips.
// Implementations live in internal/escalator; reconciler depends on
// the structural shape only.
type EscalationRunner interface {
	Execute(
		ctx context.Context,
		policy *kubeherov1.CeilingPolicy,
		clusterID, namespaceForActions string,
	) []EscalationResult
}

// EscalationResult mirrors escalator.StepResult so the controller
// package doesn't import escalator (escalator already imports
// controller's audit-event types via the sink interface — keeping the
// dependency one-way).
type EscalationResult struct {
	Action  string
	Outcome string
	Message string
}

// CeilingPolicyReconciler reconciles a CeilingPolicy object.
//
// The reconciler keeps three conditions in sync with reality:
//
//	Ready    — spec resolves (BudgetRef exists)
//	Armed    — operator pressed `kubehero cap --arm`, or humanArm=false
//	Tripped  — current burn rate ≥ trigger.burnRateMilli (read from
//	           BurnRateProvider; missing/zero → "below trigger")
//
// Tripped is informational on its own — the actual escalation runner
// lives in the control-plane and consumes Tripped=True via the audit
// log + a server-side reconcile loop. Surfacing the condition here
// gives operators a clear "is this thing about to fire?" signal in
// `kubectl get ceilingpolicy`.
type CeilingPolicyReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	BurnRate  BurnRateProvider // defaults to StubBurnRate when nil
	Audit     AuditEmitter     // defaults to NoopAuditEmitter when nil
	Escalator EscalationRunner // optional; when nil, Tripped just emits an audit row

	// ClusterID stamps every audit row + escalation event with the
	// originating cluster, so /ceilings can filter cleanly. Sourced
	// from the CLUSTER_ID env in main.go.
	ClusterID string

	// RequeueAfter controls how often we re-check burn rate. Defaults to 30s.
	RequeueAfter time.Duration
}

// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=ceilingpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=ceilingpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=ceilingpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubehero.kubehero.io,resources=budgetpolicies,verbs=get;list;watch

func (r *CeilingPolicyReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cp kubeherov1.CeilingPolicy
	if err := r.Get(ctx, req.NamespacedName, &cp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	var bp kubeherov1.BudgetPolicy
	refKey := types.NamespacedName{Namespace: cp.Namespace, Name: cp.Spec.BudgetRef}
	refOk := r.Get(ctx, refKey, &bp) == nil
	armed := IsArmed(&cp, cp.Spec.HumanArm)

	// ─── Ready ────────────────────────────────────────────────────────
	readyStatus, readyReason, readyMsg := "True", ReasonReconciled,
		"observing burn rate; will trip when trigger window sustains"
	if !refOk {
		readyStatus, readyReason, readyMsg = "False", "DanglingBudgetRef",
			"spec.budgetRef does not resolve to an existing BudgetPolicy"
	}

	// ─── Armed ────────────────────────────────────────────────────────
	armedStatus, armedReason, armedMsg := "False", ReasonNotArmed,
		"escalation plan locked until armed"
	if armed {
		armedStatus, armedReason, armedMsg = "True", ReasonArmedOK,
			"escalation plan will run when the trigger window sustains"
	}

	// ─── Tripped ──────────────────────────────────────────────────────
	provider := r.BurnRate
	if provider == nil {
		provider = StubBurnRate{}
	}
	current, err := provider.BurnRateMilli(ctx, cp.Namespace, cp.Spec.BudgetRef, cp.Spec.Trigger.Window)
	tripStatus, tripReason, tripMsg := "False", ReasonBurnRateOK,
		fmt.Sprintf("burn rate %d (×1000) below trigger %d", current, cp.Spec.Trigger.BurnRateMilli)
	switch {
	case err != nil:
		tripStatus, tripReason, tripMsg = "Unknown", ReasonBurnRateUnknown,
			"burn-rate provider unavailable: "+err.Error()
	case !refOk:
		// Without a budget reference there is nothing meaningful to compare.
		tripStatus, tripReason, tripMsg = "Unknown", ReasonBurnRateUnknown,
			"cannot evaluate burn rate without a resolved budgetRef"
	case current >= cp.Spec.Trigger.BurnRateMilli && cp.Spec.Trigger.BurnRateMilli > 0:
		tripStatus, tripReason, tripMsg = "True", ReasonBurnRateExceeded,
			fmt.Sprintf("burn rate %d (×1000) at or above trigger %d over %s",
				current, cp.Spec.Trigger.BurnRateMilli, cp.Spec.Trigger.Window)
	}

	// Detect the False/Unknown → True edge so we only emit one audit
	// row per actual tripping event, not on every 30s requeue.
	prevTripped := meta.FindStatusCondition(cp.Status.Conditions, ConditionTripped)
	wasTripped := prevTripped != nil && string(prevTripped.Status) == "True"
	nowTripped := tripStatus == "True"

	meta.SetStatusCondition(&cp.Status.Conditions,
		NewCondition(&cp, ConditionReady, readyStatus, readyReason, readyMsg))
	meta.SetStatusCondition(&cp.Status.Conditions,
		NewCondition(&cp, ConditionArmed, armedStatus, armedReason, armedMsg))
	meta.SetStatusCondition(&cp.Status.Conditions,
		NewCondition(&cp, ConditionTripped, tripStatus, tripReason, tripMsg))

	if err := r.Status().Update(ctx, &cp); err != nil {
		log.Error(err, "status update failed")
		return ctrl.Result{}, err
	}

	if !wasTripped && nowTripped {
		emitter := r.Audit
		if emitter == nil {
			emitter = NoopAuditEmitter{}
		}
		// Best effort — never fail a reconcile because the cp is offline.
		if err := emitter.Emit(ctx, AuditEvent{
			Action:     "ceiling.tripped",
			TargetKind: "CeilingPolicy",
			TargetName: cp.Name,
			ActorSub:   "operator",
			ClusterID:  r.ClusterID,
			Outcome:    "armed", // visible on /ceilings as a "Tripped → about to fire" row
			PayloadJSON: map[string]any{
				"namespace":      cp.Namespace,
				"budgetRef":      cp.Spec.BudgetRef,
				"burnRateMilli":  current,
				"trigger":        cp.Spec.Trigger.BurnRateMilli,
				"window":         cp.Spec.Trigger.Window,
				"armed":          armed,
			},
		}); err != nil {
			log.Error(err, "failed to emit audit entry for tripped policy")
		}

		// Run the escalation plan, but only when:
		//   1) the policy is armed (humanArm gate satisfied), AND
		//   2) an Escalator is wired (in stub mode the operator still
		//      emits the "ceiling.tripped" audit row but doesn't act).
		// The runner posts its own per-step audit events via the same
		// emitter, so /ceilings shows the full timeline.
		if armed && r.Escalator != nil {
			results := r.Escalator.Execute(ctx, &cp, r.ClusterID, cp.Namespace)
			for _, res := range results {
				log.Info("escalation step", "action", res.Action, "outcome", res.Outcome, "message", res.Message)
			}
		}
	}

	requeue := r.RequeueAfter
	if requeue <= 0 {
		requeue = 30 * time.Second
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *CeilingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeherov1.CeilingPolicy{}).
		Named("ceilingpolicy").
		Complete(r)
}
