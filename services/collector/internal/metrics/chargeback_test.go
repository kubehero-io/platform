// SPDX-License-Identifier: Apache-2.0
package metrics

import (
	"strings"
	"testing"
)

// The chart's PrometheusRule groups by these labels. If the schema
// drifts, dashboards and recording rules silently break.
var required = []struct {
	metric string
	labels []string
}{
	{"kubehero_pod_cost_usd_per_second",         []string{"team", "namespace", "pod", "nodepool", "cloud", "region"}},
	{"kubehero_pod_recoverable_usd_per_second",  []string{"team", "namespace", "pod"}},
	{"kubehero_pod_cpu_millicores",              []string{"team", "namespace", "pod"}},
}

func TestDemoExportsRequiredLabels(t *testing.T) {
	series := Demo()
	byName := map[string][]Series{}
	for _, s := range series {
		byName[s.Name] = append(byName[s.Name], s)
	}
	for _, r := range required {
		got, ok := byName[r.metric]
		if !ok || len(got) == 0 {
			t.Fatalf("no series for %s", r.metric)
		}
		for _, need := range r.labels {
			if _, ok := got[0].Labels[need]; !ok {
				t.Errorf("%s missing label %q", r.metric, need)
			}
		}
	}
}

func TestDemoGpuSeriesOnlyForGpuPools(t *testing.T) {
	for _, s := range Demo() {
		if s.Name != "kubehero_pod_gpu_util_ratio" {
			continue
		}
		if s.Labels["gpu_kind"] == "" {
			t.Errorf("gpu series %v has empty gpu_kind", s)
		}
	}
}

func TestWriteSeriesFormat(t *testing.T) {
	var sb strings.Builder
	WriteSeries(&sb, Series{
		Name:   "kubehero_pod_cost_usd_per_second",
		Labels: map[string]string{"team": "ml"},
		Value:  0.00012,
	})
	out := sb.String()
	if !strings.Contains(out, `team="ml"`) {
		t.Fatalf("malformed exposition: %q", out)
	}
}
