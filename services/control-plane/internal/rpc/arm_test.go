// SPDX-License-Identifier: BUSL-1.1
package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

// fakePolicyStore keeps policies in memory and implements the full
// store.PolicyStore surface (only SetArmedByName matters for the
// ArmPolicy tests; the rest are inert).
type fakePolicyStore struct {
	mu       sync.Mutex
	policies []*store.Policy
}

func (f *fakePolicyStore) Upsert(_ context.Context, p *store.Policy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, p)
	return nil
}

func (f *fakePolicyStore) Get(_ context.Context, id string) (*store.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.policies {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("policy %q: %w", id, store.ErrNotFound)
}

func (f *fakePolicyStore) List(_ context.Context, clusterID string) ([]*store.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*store.Policy
	for _, p := range f.policies {
		if p.ClusterID == clusterID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakePolicyStore) Arm(_ context.Context, _, _ string) error        { return nil }
func (f *fakePolicyStore) Disarm(_ context.Context, _ string) error        { return nil }
func (f *fakePolicyStore) RecordEval(_ context.Context, _, _ string) error { return nil }

func (f *fakePolicyStore) SetArmedByName(_ context.Context, clusterID, name string, armed bool, _ string) (*store.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.policies {
		if p.ClusterID == clusterID && p.Name == name {
			p.Armed = armed
			if armed {
				now := time.Now().UTC()
				p.ArmedAt = &now
			} else {
				p.ArmedAt = nil
			}
			return p, nil
		}
	}
	return nil, fmt.Errorf("policy %q in cluster %s: %w", name, clusterID, store.ErrNotFound)
}

// fakeAlerter records the last page-out so tests can assert the arm
// path fired it.
type fakeAlerter struct {
	mu       sync.Mutex
	channels []string
	messages []string
}

func (f *fakeAlerter) Alert(_ context.Context, channels []string, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = append([]string(nil), channels...)
	f.messages = append(f.messages, message)
	return nil
}

func seedPolicy() *store.Policy {
	return &store.Policy{
		ID:        "5f0d8a3e-0000-4000-8000-000000000001",
		ClusterID: "eks-use1-prod",
		Kind:      "CeilingPolicy",
		Namespace: "ml-inference",
		Name:      "gpu-inference-cap",
		SpecJSON:  []byte(`{}`),
	}
}

func TestArmPolicyRoundTrip(t *testing.T) {
	policies := &fakePolicyStore{policies: []*store.Policy{seedPolicy()}}
	audit := &fakeAuditStore{}
	alerts := &fakeAlerter{}
	svc := New(Options{
		Policies:      policies,
		Audit:         audit,
		Alerts:        alerts,
		AlertChannels: []string{"slack://hooks.slack.com/services/T00/B00/XXX"},
	})

	res, err := svc.ArmPolicy(adminCtx(), connect.NewRequest(&kuberov1.ArmPolicyRequest{
		ClusterId:  "eks-use1-prod",
		PolicyName: "gpu-inference-cap",
		Armed:      true,
		Actor:      "sre@example.com",
		Reason:     "runaway GPU spend",
	}))
	if err != nil {
		t.Fatalf("ArmPolicy: %v", err)
	}
	if !res.Msg.GetArmed() || res.Msg.GetPolicyName() != "gpu-inference-cap" {
		t.Fatalf("response = %+v", res.Msg)
	}
	if res.Msg.GetAuditId() != "aud-1" {
		t.Errorf("audit_id = %q, want aud-1", res.Msg.GetAuditId())
	}
	if res.Msg.GetEffectiveAtUnix() <= 0 {
		t.Error("effective_at_unix not stamped")
	}

	// Store round-trip: the armed bit persisted.
	if p := policies.policies[0]; !p.Armed || p.ArmedAt == nil {
		t.Fatalf("store not armed: %+v", p)
	}

	// Audit row written with who/why.
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "policy.arm" || e.Outcome != "armed" {
		t.Errorf("audit action/outcome = %q/%q", e.Action, e.Outcome)
	}
	if e.ActorSub != "sre@example.com" {
		t.Errorf("actor = %q", e.ActorSub)
	}
	if e.TargetKind != "CeilingPolicy" || e.TargetName != "gpu-inference-cap" {
		t.Errorf("target = %q/%q", e.TargetKind, e.TargetName)
	}
	var payload struct {
		Armed  bool   `json:"armed"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if !payload.Armed || payload.Reason != "runaway GPU spend" {
		t.Errorf("payload = %+v", payload)
	}

	// Alerter paged.
	if len(alerts.messages) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts.messages))
	}
	if msg := alerts.messages[0]; !strings.Contains(msg, "gpu-inference-cap") ||
		!strings.Contains(msg, "ARMED") || !strings.Contains(msg, "runaway GPU spend") {
		t.Errorf("alert message = %q", msg)
	}
	if len(alerts.channels) != 1 {
		t.Errorf("alert channels = %v", alerts.channels)
	}

	// Disarm flips it back and audits as a revert.
	res2, err := svc.ArmPolicy(adminCtx(), connect.NewRequest(&kuberov1.ArmPolicyRequest{
		ClusterId:  "eks-use1-prod",
		PolicyName: "gpu-inference-cap",
		Armed:      false,
		Actor:      "sre@example.com",
	}))
	if err != nil {
		t.Fatalf("disarm: %v", err)
	}
	if res2.Msg.GetArmed() {
		t.Error("disarm response still armed")
	}
	if policies.policies[0].Armed {
		t.Error("store still armed after disarm")
	}
	if got := audit.entries[len(audit.entries)-1]; got.Action != "policy.disarm" || got.Outcome != "reverted" {
		t.Errorf("disarm audit = %q/%q", got.Action, got.Outcome)
	}
	if len(alerts.messages) != 2 || !strings.Contains(alerts.messages[1], "DISARMED") {
		t.Errorf("disarm should page too: %v", alerts.messages)
	}
}

func TestArmPolicyValidation(t *testing.T) {
	policies := &fakePolicyStore{policies: []*store.Policy{seedPolicy()}}
	svc := New(Options{Policies: policies, Audit: &fakeAuditStore{}})

	cases := []struct {
		name string
		req  *kuberov1.ArmPolicyRequest
		code connect.Code
	}{
		{"empty policy name", &kuberov1.ArmPolicyRequest{ClusterId: "eks-use1-prod", Armed: true}, connect.CodeInvalidArgument},
		{"empty cluster with store wired", &kuberov1.ArmPolicyRequest{PolicyName: "gpu-inference-cap", Armed: true}, connect.CodeInvalidArgument},
		{"unknown policy", &kuberov1.ArmPolicyRequest{ClusterId: "eks-use1-prod", PolicyName: "nope", Armed: true}, connect.CodeNotFound},
		{"unknown cluster", &kuberov1.ArmPolicyRequest{ClusterId: "gke-nowhere", PolicyName: "gpu-inference-cap", Armed: true}, connect.CodeNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ArmPolicy(adminCtx(), connect.NewRequest(tc.req))
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *connect.Error
			if !errorsAs(err, &ce) || ce.Code() != tc.code {
				t.Fatalf("expected %v, got %v", tc.code, err)
			}
		})
	}
}

func TestArmPolicyDemoModeFakeSuccess(t *testing.T) {
	svc := New() // no stores wired, fixtures enabled
	res, err := svc.ArmPolicy(adminCtx(), connect.NewRequest(&kuberov1.ArmPolicyRequest{
		PolicyName: "prod-burn-rate-2x",
		Armed:      true,
	}))
	if err != nil {
		t.Fatalf("ArmPolicy demo mode: %v", err)
	}
	if !res.Msg.GetArmed() || res.Msg.GetPolicyName() != "prod-burn-rate-2x" {
		t.Fatalf("demo response = %+v", res.Msg)
	}
	if !strings.HasPrefix(res.Msg.GetAuditId(), "aud-") {
		t.Errorf("audit_id = %q, want aud-*", res.Msg.GetAuditId())
	}
	if res.Msg.GetEffectiveAtUnix() <= 0 {
		t.Error("effective_at_unix not stamped in demo mode")
	}
}

func TestArmPolicyDemoDisabledFailsLoudly(t *testing.T) {
	svc := New(Options{DemoFixturesDisabled: true})
	_, err := svc.ArmPolicy(adminCtx(), connect.NewRequest(&kuberov1.ArmPolicyRequest{
		PolicyName: "prod-burn-rate-2x",
		Armed:      true,
	}))
	if err == nil {
		t.Fatal("expected FailedPrecondition")
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestArmPolicyRequiresAdmin(t *testing.T) {
	svc := New()
	// Anonymous (no principal stamped) — Require(admin) rejects.
	_, err := svc.ArmPolicy(context.Background(), connect.NewRequest(&kuberov1.ArmPolicyRequest{
		PolicyName: "prod-burn-rate-2x", Armed: true,
	}))
	if err == nil {
		t.Fatal("expected PermissionDenied")
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestArmPolicyActorDefaultsToPrincipal(t *testing.T) {
	policies := &fakePolicyStore{policies: []*store.Policy{seedPolicy()}}
	audit := &fakeAuditStore{}
	svc := New(Options{Policies: policies, Audit: audit})
	_, err := svc.ArmPolicy(adminCtx(), connect.NewRequest(&kuberov1.ArmPolicyRequest{
		ClusterId: "eks-use1-prod", PolicyName: "gpu-inference-cap", Armed: true,
	}))
	if err != nil {
		t.Fatalf("ArmPolicy: %v", err)
	}
	// adminCtx stamps Sub="test-admin"; no actor in the request.
	if got := audit.entries[0].ActorSub; got != "test-admin" {
		t.Errorf("actor = %q, want test-admin (principal sub)", got)
	}
}
