// SPDX-License-Identifier: BUSL-1.1
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

type fakeActuator struct {
	capCalls    int
	evictCalls  int
	cordonCalls int
	failEvict   bool
}

func (f *fakeActuator) CapHPA(_ context.Context, _, _, _ string, _ float32) ([]byte, error) {
	f.capCalls++
	return []byte(`{"maxReplicas":100}`), nil
}
func (f *fakeActuator) EvictPods(_ context.Context, _, _ string, _ map[string]string) ([]string, error) {
	f.evictCalls++
	if f.failEvict {
		return nil, errors.New("forbidden")
	}
	return []string{"pod-a", "pod-b", "pod-c"}, nil
}
func (f *fakeActuator) CordonNodepool(_ context.Context, _, _, _ string) ([]string, error) {
	f.cordonCalls++
	return []string{"node-1", "node-2"}, nil
}

type fakeAlerter struct{ calls int }

func (f *fakeAlerter) Alert(_ context.Context, _ []string, _ string) error {
	f.calls++
	return nil
}

type fakeAudit struct{ appended []*store.AuditEntry }

func (f *fakeAudit) Append(_ context.Context, e *store.AuditEntry) (int64, error) {
	f.appended = append(f.appended, e)
	return int64(len(f.appended)), nil
}
func (f *fakeAudit) List(_ context.Context, _ string, _ int) ([]*store.AuditEntry, error) {
	return f.appended, nil
}

func TestRunnerExecutesInOrder(t *testing.T) {
	act := &fakeActuator{}
	alr := &fakeAlerter{}
	aud := &fakeAudit{}
	r := &Runner{Actuator: act, Alerter: alr, Audit: aud}

	spec, _ := json.Marshal(map[string]any{
		"escalation": []map[string]any{
			{"action": "alert", "channels": []string{"slack://ops"}},
			{"action": "hpa.cap", "ratioPercent": 50},
			{"action": "pod.evict"},
		},
	})
	p := &store.Policy{ID: "p1", ClusterID: "c1", Kind: "CeilingPolicy", Name: "test", SpecJSON: spec}
	d := &Decision{Reason: "test"}

	results, err := r.Execute(context.Background(), p, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if alr.calls != 1 || act.capCalls != 1 || act.evictCalls != 1 {
		t.Fatalf("expected 1 of each; got alert=%d cap=%d evict=%d", alr.calls, act.capCalls, act.evictCalls)
	}
	if len(aud.appended) != 3 {
		t.Fatalf("expected 3 audit entries; got %d", len(aud.appended))
	}
}

func TestRunnerStopsOnError(t *testing.T) {
	act := &fakeActuator{failEvict: true}
	alr := &fakeAlerter{}
	aud := &fakeAudit{}
	r := &Runner{Actuator: act, Alerter: alr, Audit: aud}

	spec, _ := json.Marshal(map[string]any{
		"escalation": []map[string]any{
			{"action": "pod.evict"},
			{"action": "alert", "channels": []string{"slack://ops"}},
		},
	})
	p := &store.Policy{ID: "p2", ClusterID: "c1", Kind: "CeilingPolicy", Name: "test", SpecJSON: spec}

	results, _ := r.Execute(context.Background(), p, &Decision{})
	if len(results) != 1 {
		t.Fatalf("expected bail after first failure; got %d results", len(results))
	}
	if results[0].Outcome != "error" {
		t.Fatalf("expected outcome=error; got %+v", results[0])
	}
	if alr.calls != 0 {
		t.Fatalf("expected alert NOT to run after evict failure; got %d calls", alr.calls)
	}
}
