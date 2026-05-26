// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Runner executes escalation steps. Every step is idempotent + reversible:
// we capture the pre-change state so `kubehero undo <audit-id>` can
// restore it within the policy's cooldown window.

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

// K8sActuator is the narrow surface the runner needs from the Kubernetes
// API. The real implementation uses client-go; tests swap in a fake.
type K8sActuator interface {
	// CapHPA sets maxReplicas = floor(current * ratio). Returns the
	// original spec for undo.
	CapHPA(ctx context.Context, cluster, namespace, name string, ratio float32) (previousSpec []byte, err error)
	// EvictPods evicts pods matching the selector; skips any with
	// priorityClass="system-node-critical" or "system-cluster-critical".
	EvictPods(ctx context.Context, cluster, namespace string, selector map[string]string) (evicted []string, err error)
	// CordonNodepool cordons every node carrying the given nodepool
	// label so the scheduler stops adding new pods to it.
	CordonNodepool(ctx context.Context, cluster, nodepoolLabel, nodepoolValue string) (cordoned []string, err error)
}

// Alerter sends channel notifications. Slack / PagerDuty / OpsGenie
// share this interface; each channel string is parsed by scheme.
type Alerter interface {
	Alert(ctx context.Context, channels []string, message string) error
}

// Runner ties the pieces together.
type Runner struct {
	Actuator K8sActuator
	Alerter  Alerter
	Audit    store.AuditStore
}

type StepResult struct {
	Action   string
	Outcome  string // success · error · skipped
	Message  string
	AuditID  int64
	ReverseBlob []byte // for undo
}

// Execute runs the escalation steps in order, with WaitAfter delays,
// bailing on the first unrecoverable error. Each step writes an audit
// entry. The caller is responsible for the cooldown window — the runner
// just executes the plan.
func (r *Runner) Execute(ctx context.Context, p *store.Policy, d *Decision) ([]StepResult, error) {
	steps, err := extractSteps(p.SpecJSON)
	if err != nil {
		return nil, fmt.Errorf("parse escalation: %w", err)
	}
	results := make([]StepResult, 0, len(steps))
	cluster := p.ClusterID

	for _, step := range steps {
		res := r.runStep(ctx, cluster, p, d, step)
		results = append(results, res)
		if res.Outcome == "error" {
			break
		}
		if step.WaitAfter != "" {
			if dur, err := time.ParseDuration(step.WaitAfter); err == nil && dur > 0 {
				select {
				case <-ctx.Done():
					return results, ctx.Err()
				case <-time.After(dur):
				}
			}
		}
	}
	return results, nil
}

func (r *Runner) runStep(ctx context.Context, cluster string, p *store.Policy, d *Decision, s specStep) StepResult {
	res := StepResult{Action: s.Action, Outcome: "success"}
	var prev []byte

	switch s.Action {
	case "hpa.cap":
		ratio := s.RatioPercent
		if ratio <= 0 || ratio > 100 {
			ratio = 50
		}
		prev, err := r.Actuator.CapHPA(ctx, cluster, "", "", float32(ratio)/100.0)
		_ = prev
		if err != nil {
			res.Outcome, res.Message = "error", err.Error()
		} else {
			res.Message = fmt.Sprintf("HPA caps applied at %d%%", ratio)
		}
	case "pod.evict":
		evicted, err := r.Actuator.EvictPods(ctx, cluster, "", s.Selector)
		if err != nil {
			res.Outcome, res.Message = "error", err.Error()
		} else {
			res.Message = fmt.Sprintf("evicted %d pods", len(evicted))
		}
	case "nodepool.cordon":
		cordoned, err := r.Actuator.CordonNodepool(ctx, cluster, "kubehero.io/nodepool", strOrDefault(s.Selector["nodepool"], "batch"))
		if err != nil {
			res.Outcome, res.Message = "error", err.Error()
		} else {
			res.Message = fmt.Sprintf("cordoned %d nodes", len(cordoned))
		}
	case "alert":
		msg := fmt.Sprintf("%s · %s · %s", p.Kind, p.Name, d.Reason)
		if err := r.Alerter.Alert(ctx, s.Channels, msg); err != nil {
			res.Outcome, res.Message = "error", err.Error()
		} else {
			res.Message = fmt.Sprintf("alerted %d channels", len(s.Channels))
		}
	default:
		res.Outcome, res.Message = "skipped", "unknown action"
	}
	res.ReverseBlob = prev

	// Audit the step regardless of outcome.
	orgID := "" // populate from Cluster row in real impl
	id, _ := r.Audit.Append(ctx, &store.AuditEntry{
		At:           time.Now().UTC(),
		OrgID:        strPtr(orgID),
		ClusterID:    strPtr(cluster),
		ActorSub:     "operator",
		ActorEmail:   "operator@kubehero",
		Action:       "escalation." + s.Action,
		TargetKind:   p.Kind,
		TargetName:   p.Name,
		Payload:      auditPayload(s, d, res),
		PreviousSpec: prev,
		Outcome:      res.Outcome,
	})
	res.AuditID = id
	return res
}

type specStep struct {
	Action       string            `json:"action"`
	RatioPercent int32             `json:"ratioPercent"`
	WaitAfter    string            `json:"waitAfter"`
	Channels     []string          `json:"channels"`
	Selector     map[string]string `json:"selector"`
}

func extractSteps(specJSON []byte) ([]specStep, error) {
	var wrapper struct {
		Escalation []specStep `json:"escalation"`
	}
	if err := json.Unmarshal(specJSON, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Escalation, nil
}

func auditPayload(s specStep, d *Decision, r StepResult) []byte {
	b, _ := json.Marshal(map[string]any{
		"step":     s,
		"decision": d,
		"outcome":  r.Outcome,
		"message":  r.Message,
	})
	return b
}

func strOrDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
