// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package cost

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
)

// Dimension names accepted by aggregate and filters. controller is the
// owning workload qualified by its kind ("Deployment/api"); label:<key>
// is a pod label.
const (
	DimCluster    = "cluster"
	DimNamespace  = "namespace"
	DimWorkload   = "workload"
	DimController = "controller"
	DimPod        = "pod"
	DimContainer  = "container"
	DimNode       = "node"
	DimNodepool   = "nodepool"
	DimTeam       = "team"
	DimCostCenter = "cost_center"
	DimZone       = "zone"
	// Filter/aggregate extras that exist on every cost row.
	DimCloud        = "cloud"
	DimRegion       = "region"
	DimLifecycle    = "lifecycle"
	DimWorkloadKind = "workload_kind"

	labelPrefix = "label:"

	// Unallocated names an empty dimension value (OpenCost's spelling);
	// as a filter value it matches the empty value.
	Unallocated = "__unallocated__"
	// IdleName is the idle-capacity row (OpenCost's spelling).
	IdleName = "__idle__"
	// SharedName carries shared-namespace cost that had no allocation
	// to be redistributed to.
	SharedName = "__shared__"
)

// Limits on request shape — each bounds SQL size or result cardinality.
const (
	maxAggregateDims = 5
	maxLabelDims     = 3
	maxFilterKeys    = 10
	maxFilterValues  = 50
	maxValueLen      = 253
	maxShared        = 50
)

// Dim is one validated dimension.
type Dim struct {
	Name     string // canonical spelling ("namespace", "label:app")
	LabelKey string // set for label dims
}

func (d Dim) isLabel() bool { return d.LabelKey != "" }

// rawOnly dims exist only on pod_cost_1s (the hourly rollup is
// workload-grained), so they force the raw path.
func (d Dim) rawOnly() bool { return d.Name == DimPod || d.Name == DimNode }

// column returns the SQL expression for a plain column dim on alias t
// (both cost tables share column names). Label, container and the
// always-present identity dims are handled by the caller.
func (d Dim) column() string {
	switch d.Name {
	case DimController:
		return "concat(t.workload_kind, '/', t.workload)"
	case DimCostCenter:
		return "t.cost_center"
	case DimWorkloadKind:
		return "t.workload_kind"
	case DimCluster:
		return "t.cluster_id"
	case DimNamespace, DimWorkload, DimPod, DimNode, DimNodepool, DimTeam, DimZone, DimCloud, DimRegion, DimLifecycle:
		return "t." + chsql.Ident(d.Name)
	}
	panic("cost: no column for dim " + d.Name)
}

var knownDims = map[string]bool{
	DimCluster: true, DimNamespace: true, DimWorkload: true, DimController: true,
	DimPod: true, DimContainer: true, DimNode: true, DimNodepool: true, DimTeam: true,
	DimCostCenter: true, DimZone: true, DimCloud: true, DimRegion: true, DimLifecycle: true,
	DimWorkloadKind: true,
}

// ParseDim validates one dimension name.
func ParseDim(s string) (Dim, error) {
	s = strings.TrimSpace(s)
	if key, ok := strings.CutPrefix(s, labelPrefix); ok {
		if !chsql.ValidLabelKey(key) {
			return Dim{}, fmt.Errorf("invalid label key %q", key)
		}
		return Dim{Name: labelPrefix + key, LabelKey: key}, nil
	}
	s = strings.ToLower(s)
	if s == "costcenter" || s == "cost-center" {
		s = DimCostCenter
	}
	if !knownDims[s] {
		return Dim{}, fmt.Errorf("unknown dimension %q (want cluster, namespace, workload, controller, pod, container, node, nodepool, team, cost_center, zone or label:<key>)", s)
	}
	return Dim{Name: s}, nil
}

// ParseAggregate validates the aggregate list. Empty means namespace:
// OpenCost's own default (one row per container) is unbounded, and
// namespace is what cost views start from.
func ParseAggregate(in []string) ([]Dim, error) {
	var out []Dim
	seen := map[string]bool{}
	labels := 0
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			d, err := ParseDim(part)
			if err != nil {
				return nil, err
			}
			if seen[d.Name] {
				continue
			}
			seen[d.Name] = true
			if d.isLabel() {
				labels++
			}
			out = append(out, d)
		}
	}
	if len(out) > maxAggregateDims {
		return nil, fmt.Errorf("at most %d aggregate dimensions", maxAggregateDims)
	}
	if labels > maxLabelDims {
		return nil, fmt.Errorf("at most %d label dimensions", maxLabelDims)
	}
	if len(out) == 0 {
		out = []Dim{{Name: DimNamespace}}
	}
	return out, nil
}

// Filter is one dimension restricted to a value set (OR within a key,
// AND across keys).
type Filter struct {
	Dim    Dim
	Values []string // "" = the empty (unallocated) value
}

// ParseFilters validates dimension → values filters. Empty values are
// ignored (a blank form field is "no filter"); Unallocated matches "".
func ParseFilters(in map[string][]string) ([]Filter, error) {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic SQL
	var out []Filter
	for _, k := range keys {
		d, err := ParseDim(k)
		if err != nil {
			return nil, fmt.Errorf("filter: %w", err)
		}
		var vals []string
		for _, v := range in[k] {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if len(v) > maxValueLen {
				return nil, fmt.Errorf("filter %s: value longer than %d bytes", d.Name, maxValueLen)
			}
			if v == Unallocated {
				v = ""
			}
			vals = append(vals, v)
		}
		if len(vals) == 0 {
			continue
		}
		if len(vals) > maxFilterValues {
			return nil, fmt.Errorf("filter %s: at most %d values", d.Name, maxFilterValues)
		}
		out = append(out, Filter{Dim: d, Values: vals})
	}
	if len(out) > maxFilterKeys {
		return nil, fmt.Errorf("at most %d filters", maxFilterKeys)
	}
	return out, nil
}

// ParseSharedNamespaces validates the shared-namespace list.
func ParseSharedNamespaces(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		for _, ns := range strings.Split(raw, ",") {
			ns = strings.TrimSpace(ns)
			if ns == "" || seen[ns] {
				continue
			}
			if len(ns) > 63 {
				return nil, fmt.Errorf("shared namespace %q too long", ns)
			}
			seen[ns] = true
			out = append(out, ns)
		}
	}
	if len(out) > maxShared {
		return nil, fmt.Errorf("at most %d shared namespaces", maxShared)
	}
	return out, nil
}

// ShareIdle modes.
const (
	ShareIdleNone     = ""
	ShareIdleWeighted = "weighted"
	ShareIdleEven     = "even"
)

// ParseShareIdle validates share_idle.
func ParseShareIdle(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "false":
		return ShareIdleNone, nil
	case "weighted", "true":
		return ShareIdleWeighted, nil
	case "even":
		return ShareIdleEven, nil
	}
	return "", fmt.Errorf("share_idle must be \"\", \"weighted\" or \"even\", got %q", s)
}

// displayValue renders a dimension value for a row name.
func displayValue(v string) string {
	if v == "" {
		return Unallocated
	}
	return v
}
