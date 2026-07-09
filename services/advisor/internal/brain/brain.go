// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package brain holds what every advisor brain shares: the Brain
// interface, the action-kind whitelist, and the validation guardrail
// that downgrades anything a brain proposes outside the allowed shape.
//
// GUARDRAILS (non-negotiable):
//   - The advisor is read-only. Brains only ever PROPOSE actions.
//   - Every action's crd_yaml must be a KubeHero policy CRD manifest
//     (BudgetPolicy / CeilingPolicy / RightsizingPolicy) that a human
//     applies through the operator's existing arming flow.
//   - Anything that fails validation is downgraded to a
//     "workload.investigate" action with no manifest — never dropped
//     silently into something applyable.
package brain

import (
	"context"
	"math"
	"strings"

	"gopkg.in/yaml.v3"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

// Brain turns a snapshot into a briefing. Implementations must be safe
// for concurrent use.
type Brain interface {
	Generate(ctx context.Context, snap *source.Snapshot) (*kuberov1.Briefing, error)
}

// Action kinds the wire contract allows.
const (
	KindRightsize   = "rightsize.requests"
	KindCeilingArm  = "ceiling.arm"
	KindConsolidate = "nodepool.consolidate"
	KindInvestigate = "workload.investigate"
)

// StatusProposed is the only status the advisor ever emits.
const StatusProposed = "proposed"

var allowedKinds = map[string]bool{
	KindRightsize:   true,
	KindCeilingArm:  true,
	KindConsolidate: true,
	KindInvestigate: true,
}

var allowedRisks = map[string]bool{"low": true, "medium": true, "high": true}

// crdKinds are the only manifests an action may carry — the operator's
// guarded policy CRDs.
var crdKinds = map[string]bool{
	"BudgetPolicy":      true,
	"CeilingPolicy":     true,
	"RightsizingPolicy": true,
}

// ValidateActions enforces the guardrails on a brain's output, in
// place. Rules:
//   - status is forced to "proposed"
//   - impact must be finite and >= 0, else clamped to 0
//   - risk must be low|medium|high, else "medium"
//   - kind must be whitelisted AND (unless investigate-only) crd_yaml
//     must YAML-parse to a manifest whose kind is one of BudgetPolicy /
//     CeilingPolicy / RightsizingPolicy — otherwise the action is
//     downgraded to kind="workload.investigate" with crd_yaml="".
func ValidateActions(actions []*kuberov1.ProposedAction) []*kuberov1.ProposedAction {
	out := actions[:0]
	for _, a := range actions {
		if a == nil {
			continue
		}
		a.Status = StatusProposed
		if math.IsNaN(a.ImpactMonthlyUsd) || math.IsInf(a.ImpactMonthlyUsd, 0) || a.ImpactMonthlyUsd < 0 {
			a.ImpactMonthlyUsd = 0
		}
		if !allowedRisks[a.Risk] {
			a.Risk = "medium"
		}
		if !allowedKinds[a.Kind] {
			downgrade(a)
		} else if a.Kind != KindInvestigate && !validCRD(a.CrdYaml) {
			downgrade(a)
		}
		if a.Kind == KindInvestigate {
			a.CrdYaml = ""
		}
		out = append(out, a)
	}
	return out
}

func downgrade(a *kuberov1.ProposedAction) {
	a.Kind = KindInvestigate
	a.CrdYaml = ""
}

// validCRD reports whether s parses as YAML and declares one of the
// operator's guarded policy kinds.
func validCRD(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	var m struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		return false
	}
	return crdKinds[m.Kind]
}
