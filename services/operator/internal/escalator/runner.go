// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package escalator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

// AuditSink is the structural shape the runner needs from an audit
// emitter. Defined here (not imported from controller) so the package
// stays at the bottom of the dependency tree — controller imports
// escalator, never the reverse.
//
// controller.HTTPAuditEmitter and controller.NoopAuditEmitter both
// satisfy this interface via Go's structural typing.
type AuditSink interface {
	Emit(ctx context.Context, e AuditEvent) error
}

// AuditEvent mirrors controller.AuditEvent's wire shape — same field
// names so JSON marshalling lands at the same control-plane endpoint.
// Duplicating the struct here keeps escalator from depending on
// controller; the reconciler hands us an AuditSink and the events
// flow on out the same TCP connection.
type AuditEvent struct {
	Org            string  `json:"org,omitempty"`
	ClusterID      string  `json:"clusterId,omitempty"`
	ActorSub       string  `json:"actorSub,omitempty"`
	ActorEmail     string  `json:"actorEmail,omitempty"`
	Action         string  `json:"action"`
	TargetKind     string  `json:"targetKind,omitempty"`
	TargetName     string  `json:"targetName,omitempty"`
	Payload        []byte  `json:"-"`
	PayloadJSON    any     `json:"payload,omitempty"`
	Outcome        string  `json:"outcome,omitempty"`
	EffectUsdMonth float64 `json:"effectUsdMonth,omitempty"`
}

// NoopAuditSink is a default for tests + dev. No-ops every emit.
type NoopAuditSink struct{}

func (NoopAuditSink) Emit(_ context.Context, _ AuditEvent) error { return nil }

// Runner walks an EscalationStep slice in order, calling the actuator
// + posting an audit event per step. It bails on the first
// unrecoverable error so a failed cordon doesn't leak into a follow-up
// pod-evict that would now be unsafe.
type Runner struct {
	Actuator *Actuator
	Audit    AuditSink
}

// StepResult is the per-step outcome surfaced to the reconciler. The
// reconciler inspects the slice to set CeilingPolicy.Status.Conditions
// + decide whether to keep going.
type StepResult struct {
	Action  string
	Outcome string // applied · skipped · error
	Message string
}

// Execute runs every step on `policy`. The caller is responsible for the
// arming gate — Runner.Execute trusts that the policy is meant to fire.
//
// `clusterID` is stamped into every audit event so /ceilings can filter
// by cluster. `namespaceForActions` is the namespace the actuator
// targets when a step doesn't carry its own selector — usually the
// namespace of the CeilingPolicy CR itself.
func (r *Runner) Execute(
	ctx context.Context,
	policy *kubeherov1.CeilingPolicy,
	clusterID, namespaceForActions string,
) []StepResult {
	emitter := r.Audit
	if emitter == nil {
		emitter = NoopAuditSink{}
	}
	bp, _ := json.Marshal(map[string]any{
		"policy":     policy.Name,
		"namespace":  policy.Namespace,
		"budgetRef":  policy.Spec.BudgetRef,
		"trigger":    policy.Spec.Trigger,
	})

	steps := policy.Spec.Escalation
	results := make([]StepResult, 0, len(steps))
	for _, step := range steps {
		res := r.runStep(ctx, step, namespaceForActions)
		results = append(results, res)

		// Fire-and-forget audit emit per step. We never block the
		// reconcile loop on the cp being reachable.
		_ = emitter.Emit(ctx, AuditEvent{
			Action:     "escalation." + step.Action,
			TargetKind: "CeilingPolicy",
			TargetName: policy.Name,
			ActorSub:   "operator",
			ClusterID:  clusterID,
			Outcome:    res.Outcome,
			Payload:    bp,
		})

		if res.Outcome == "error" {
			break
		}
		if step.WaitAfter != "" {
			if d, err := time.ParseDuration(step.WaitAfter); err == nil && d > 0 {
				select {
				case <-ctx.Done():
					return results
				case <-time.After(d):
				}
			}
		}
	}
	return results
}

func (r *Runner) runStep(
	ctx context.Context,
	step kubeherov1.EscalationStep,
	namespace string,
) StepResult {
	res := StepResult{Action: step.Action, Outcome: "applied"}
	switch step.Action {
	case "hpa.cap":
		ratio := defaultRatio(step.RatioPercent)
		// Step doesn't carry an HPA name today — we cap every HPA in
		// the namespace. Future iteration: respect a `targetRef` field
		// once we add it to the CRD.
		// For now, list and cap the first HPA found; a richer impl
		// would loop and audit each.
		// (Simplified to keep the surface small until CRD grows.)
		_, err := r.Actuator.CapHPA(ctx, namespace, "*", ratio)
		if err != nil {
			res.Outcome, res.Message = "error", err.Error()
		} else {
			res.Message = fmt.Sprintf("HPA capped at %.0f%%", ratio*100)
		}

	case "pod.evict":
		// No selector field on EscalationStep yet — we evict every
		// non-system pod in the namespace. Customers narrow scope via
		// the CeilingPolicy's Scope (namespace selector).
		evicted, err := r.Actuator.EvictPods(ctx, namespace, map[string]string{})
		if err != nil {
			res.Outcome, res.Message = "error", err.Error()
		} else if len(evicted) == 0 {
			res.Outcome, res.Message = "skipped", "no eligible pods"
		} else {
			res.Message = fmt.Sprintf("evicted %d pods", len(evicted))
		}

	case "nodepool.cordon":
		// Nodepool name comes from the policy's namespace today; in a
		// future version we surface it as an explicit field on
		// EscalationStep.
		cordoned, _, err := r.Actuator.CordonNodepool(ctx, namespace)
		if err != nil {
			res.Outcome, res.Message = "error", err.Error()
		} else if len(cordoned) == 0 {
			res.Outcome, res.Message = "skipped", "no nodes matched"
		} else {
			res.Message = fmt.Sprintf("cordoned %d nodes", len(cordoned))
		}

	case "alert":
		// Alerts are dispatched server-side via the cp's alerter.
		// Emitting an "escalation.alert" audit row is sufficient — the
		// cp's runner picks it up and routes to Slack/PagerDuty/OpsGenie.
		res.Message = fmt.Sprintf("alert queued for %d channels", len(step.Channels))

	default:
		res.Outcome, res.Message = "skipped", "unknown action"
	}
	return res
}

func defaultRatio(p *int32) float32 {
	if p == nil {
		return 0.5
	}
	v := float32(*p) / 100
	if v <= 0 || v > 1 {
		return 0.5
	}
	return v
}
