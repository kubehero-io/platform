// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package budget reads BudgetPolicy / CeilingPolicy specs as mirrored
// into Postgres by the operator: the monthly ceiling and the scope the
// spend is measured over. ListPolicies (spent %) and kind=budget alert
// rules (burn-rate multiple) both need exactly this interpretation, so
// it lives in one place.
//
// Scope semantics — stated plainly because selectors can't all be
// evaluated server-side: spend is measured in the policy's own cluster.
// namespaceSelector narrows it when it names namespaces through the
// automatic kubernetes.io/metadata.name label (matchLabels or an In
// expression); any other namespace selector is reported but measured
// cluster-wide, because namespace labels are not stored centrally.
// clusterSelector is displayed but not evaluated for the same reason.
package budget

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var ceilingRE = regexp.MustCompile(`^\$?\s*([0-9][0-9,]*(?:\.[0-9]+)?)\s*(k|m)?\s*(?:/\s*(mo|month|monthly|hr|hour|h|day|d|wk|week))?$`)

// ParseCeiling reads "$100000/mo", "$300/hr", "$2k/day", "150000" and
// returns US dollars per 30-day month (the "month" all KubeHero $/mo
// figures use). ok is false for anything unparseable or ≤ 0.
func ParseCeiling(s string) (float64, bool) {
	m := ceilingRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	switch m[2] {
	case "k":
		v *= 1e3
	case "m":
		v *= 1e6
	}
	switch m[3] {
	case "hr", "hour", "h":
		v *= 24 * 30
	case "day", "d":
		v *= 30
	case "wk", "week":
		v *= 30.0 / 7
	}
	return v, true
}

// nameLabel is the label Kubernetes stamps on every namespace.
const nameLabel = "kubernetes.io/metadata.name"

type labelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"matchExpressions"`
}

// Spec is the part of a Budget/Ceiling policy spec the control plane
// interprets.
type Spec struct {
	Ceiling   string `json:"ceiling"`
	BudgetRef string `json:"budgetRef"`
	Scope     struct {
		ClusterSelector   *labelSelector `json:"clusterSelector"`
		NamespaceSelector *labelSelector `json:"namespaceSelector"`
	} `json:"scope"`
	Trigger struct {
		BurnRateMilli int32  `json:"burnRateMilli"`
		Window        string `json:"window"`
	} `json:"trigger"`
}

// ParseSpec decodes a mirrored spec.
func ParseSpec(raw []byte) (Spec, error) {
	var s Spec
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("policy spec: %w", err)
	}
	return s, nil
}

// Namespaces returns the namespaces the spend is measured over; nil
// means the whole cluster (see the package doc). exact is false when a
// selector was present but could not be evaluated.
func (s Spec) Namespaces() (ns []string, exact bool) {
	sel := s.Scope.NamespaceSelector
	if sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0) {
		return nil, true
	}
	exact = true
	var sets [][]string
	for k, v := range sel.MatchLabels {
		if k == nameLabel {
			sets = append(sets, []string{v})
		} else {
			exact = false
		}
	}
	for _, e := range sel.MatchExpressions {
		if e.Key == nameLabel && strings.EqualFold(e.Operator, "In") {
			sets = append(sets, append([]string(nil), e.Values...))
		} else {
			exact = false
		}
	}
	if len(sets) == 0 {
		return nil, false
	}
	// Terms AND together: intersect.
	cur := map[string]bool{}
	for _, v := range sets[0] {
		cur[v] = true
	}
	for _, set := range sets[1:] {
		next := map[string]bool{}
		for _, v := range set {
			if cur[v] {
				next[v] = true
			}
		}
		cur = next
	}
	for v := range cur {
		ns = append(ns, v)
	}
	sort.Strings(ns)
	if ns == nil {
		ns = []string{} // selector matches nothing
	}
	return ns, exact
}

// Describe renders the scope for display, e.g.
// "env=prod · ns=ml-inference · eks-use1-prod".
func (s Spec) Describe(cluster string) string {
	var parts []string
	if sel := s.Scope.ClusterSelector; sel != nil {
		parts = append(parts, selectorString(sel))
	}
	ns, exact := s.Namespaces()
	switch {
	case ns != nil && exact:
		parts = append(parts, "ns="+strings.Join(ns, ","))
	case s.Scope.NamespaceSelector != nil && (len(s.Scope.NamespaceSelector.MatchLabels) > 0 || len(s.Scope.NamespaceSelector.MatchExpressions) > 0):
		parts = append(parts, "ns selector "+selectorString(s.Scope.NamespaceSelector)+" (measured cluster-wide)")
	default:
		parts = append(parts, "all namespaces")
	}
	if cluster != "" {
		parts = append(parts, cluster)
	}
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func selectorString(sel *labelSelector) string {
	var terms []string
	keys := make([]string, 0, len(sel.MatchLabels))
	for k := range sel.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		terms = append(terms, k+"="+sel.MatchLabels[k])
	}
	for _, e := range sel.MatchExpressions {
		terms = append(terms, fmt.Sprintf("%s %s (%s)", e.Key, strings.ToLower(e.Operator), strings.Join(e.Values, ",")))
	}
	return strings.Join(terms, ",")
}
