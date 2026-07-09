// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
)

// llmBriefing is the JSON shape the system prompt asks the model for.
type llmBriefing struct {
	Headline     string      `json:"headline"`
	Markdown     string      `json:"markdown"`
	SpokenScript string      `json:"spoken_script"`
	Actions      []llmAction `json:"actions"`
}

type llmAction struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	ImpactMonthlyUSD float64 `json:"impact_monthly_usd"`
	Risk             string  `json:"risk"`
	Kind             string  `json:"kind"`
	Target           string  `json:"target"`
	Rationale        string  `json:"rationale"`
	CrdYAML          string  `json:"crd_yaml"`
}

// parseBriefing leniently parses model output into a validated
// Briefing. It tolerates markdown fences and stray prose around the
// JSON object; anything worse is an error and the caller falls back to
// the rules brain.
func parseBriefing(raw string) (*kuberov1.Briefing, error) {
	jsonText, err := extractJSON(raw)
	if err != nil {
		return nil, err
	}
	var lb llmBriefing
	if err := json.Unmarshal([]byte(jsonText), &lb); err != nil {
		return nil, fmt.Errorf("unmarshal briefing: %w", err)
	}
	if strings.TrimSpace(lb.Headline) == "" || strings.TrimSpace(lb.Markdown) == "" {
		return nil, errors.New("briefing missing headline or markdown")
	}
	if strings.TrimSpace(lb.SpokenScript) == "" {
		lb.SpokenScript = lb.Headline
	}

	actions := make([]*kuberov1.ProposedAction, 0, len(lb.Actions))
	for i, a := range lb.Actions {
		id := strings.TrimSpace(a.ID)
		if id == "" {
			id = fmt.Sprintf("act-llm-%d", i+1)
		}
		actions = append(actions, &kuberov1.ProposedAction{
			Id:               id,
			Title:            a.Title,
			ImpactMonthlyUsd: a.ImpactMonthlyUSD,
			Risk:             a.Risk,
			Kind:             a.Kind,
			Target:           a.Target,
			Rationale:        a.Rationale,
			CrdYaml:          a.CrdYAML,
			Status:           brain.StatusProposed,
		})
	}

	return &kuberov1.Briefing{
		Headline:     strings.TrimSpace(lb.Headline),
		Markdown:     strings.TrimSpace(lb.Markdown),
		SpokenScript: strings.TrimSpace(lb.SpokenScript),
		Source:       "llm",
		// The guardrail: whitelist kinds, clamp impacts, and downgrade
		// any action whose crd_yaml isn't a parseable guarded policy CRD.
		Actions: brain.ValidateActions(actions),
	}, nil
}

// extractJSON strips ``` fences and surrounding prose, returning the
// outermost {...} object.
func extractJSON(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	// Strip a leading fence line (``` or ```json) and a trailing fence.
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return "", errors.New("no JSON object found in model output")
	}
	return s[start : end+1], nil
}
