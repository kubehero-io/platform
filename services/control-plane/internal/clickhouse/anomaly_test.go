// SPDX-License-Identifier: BUSL-1.1
package clickhouse

import "testing"

func TestWorkloadFromPod(t *testing.T) {
	cases := []struct {
		name string
		pod  string
		want string
	}{
		{"deployment two-suffix", "model-server-a100-7d9f8b6c5d-x2v4z", "model-server-a100"},
		{"deployment short template hash", "api-ingress-59d8c-p7q2r", "api-ingress"},
		{"statefulset ordinal", "vectordb-ingress-0", "vectordb-ingress"},
		{"statefulset multi-digit ordinal", "kafka-broker-12", "kafka-broker"},
		{"daemonset single rand suffix", "node-exporter-b7f4k", "node-exporter"},
		{"cronjob job pod", "etl-nightly-29184760-fk2vx", "etl-nightly"},
		{"bare pod untouched", "debug", "debug"},
		{"suffix with vowel is not a hash", "frontend-gateway", "frontend-gateway"},
		{"numeric-ish name keeps stripping only the ordinal", "redis-6379-2", "redis-6379"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WorkloadFromPod(c.pod); got != c.want {
				t.Fatalf("WorkloadFromPod(%q)=%q want %q", c.pod, got, c.want)
			}
		})
	}
}

func TestSpendAnomalyProviderEffectiveThreshold(t *testing.T) {
	var nilP *SpendAnomalyProvider
	if got := nilP.EffectiveThreshold(); got != 3.0 {
		t.Fatalf("nil provider threshold=%v want 3.0", got)
	}
	if got := (&SpendAnomalyProvider{}).EffectiveThreshold(); got != 3.0 {
		t.Fatalf("zero threshold=%v want default 3.0", got)
	}
	if got := (&SpendAnomalyProvider{ZThreshold: 2.5}).EffectiveThreshold(); got != 2.5 {
		t.Fatalf("threshold=%v want 2.5", got)
	}
}
