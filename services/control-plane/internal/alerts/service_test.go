// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

func as(role auth.Role) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Sub: "u-" + string(role), Email: string(role) + "@example.com", Role: role})
}

func code(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

// validatingSources parses queries like the real sources.
type validatingSources struct{ scripted }

func (v *validatingSources) ValidateQuery(_ context.Context, kind, q string) error {
	switch kind {
	case KindLogs:
		return nil
	case KindBudget:
		_, err := ParseBudgetQuery(q)
		return err
	}
	_, err := ParseQuery(kind, q)
	return err
}

func newService() (*Service, *MemoryStore, *validatingSources) {
	st := NewMemoryStore()
	src := &validatingSources{}
	return &Service{Store: st, Sources: src, Channels: alerter.NewRouter(), Now: func() time.Time { return t0 }}, st, src
}

func protoRule() *kuberov1.AlertRule {
	return &kuberov1.AlertRule{Name: "Edge egress", Kind: "network", Query: `network{namespace="edge"} by (workload)`,
		Op: ">", Threshold: 5, PendingFor: "5m", Severity: "warn", Enabled: true,
		Channels: []string{"slack://hooks.slack.com/services/T/B/SECRET"}}
}

func TestUpsertRolesValidationAndRedaction(t *testing.T) {
	s, _, _ := newService()
	if _, err := s.UpsertAlertRule(as(auth.RoleMember), connect.NewRequest(&kuberov1.UpsertAlertRuleRequest{Rule: protoRule()})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("member upsert: %v", err)
	}
	r, err := s.UpsertAlertRule(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.UpsertAlertRuleRequest{Rule: protoRule()}))
	if err != nil {
		t.Fatal(err)
	}
	saved := r.Msg.GetRule()
	if saved.GetId() == "" || saved.GetPendingFor() != "5m" || saved.GetEvalInterval() != "1m" || saved.GetCreatedBy() != "admin@example.com" ||
		saved.GetChannels()[0] != "slack://hooks.slack.com/services/T/B/SECRET" {
		t.Fatalf("admin sees the full rule: %+v", saved)
	}
	list, err := s.ListAlertRules(as(auth.RoleViewer), connect.NewRequest(&kuberov1.ListAlertRulesRequest{}))
	if err != nil || len(list.Msg.GetRules()) != 1 {
		t.Fatal(err)
	}
	if ch := list.Msg.GetRules()[0].GetChannels()[0]; ch != "slack://hooks.slack.com/…" {
		t.Fatalf("viewers must see redacted channels, got %q", ch)
	}

	// Round-tripping the redacted channel keeps the stored secret.
	upd := protoRule()
	upd.Id, upd.Threshold, upd.Channels = saved.GetId(), 7, []string{"slack://hooks.slack.com/…"}
	r2, err := s.UpsertAlertRule(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.UpsertAlertRuleRequest{Rule: upd}))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Msg.GetRule().GetChannels()[0] != "slack://hooks.slack.com/services/T/B/SECRET" || r2.Msg.GetRule().GetThreshold() != 7 {
		t.Fatalf("round trip: %+v", r2.Msg.GetRule())
	}
	upd.Channels = []string{"slack://other.example.com/…"}
	if _, err := s.UpsertAlertRule(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.UpsertAlertRuleRequest{Rule: upd})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown redacted channel: %v", err)
	}

	dup := protoRule()
	if _, err := s.UpsertAlertRule(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.UpsertAlertRuleRequest{Rule: dup})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate name: %v", err)
	}
	for _, mut := range []func(*kuberov1.AlertRule){
		func(r *kuberov1.AlertRule) { r.Query = `network{bogus="x"}` },
		func(r *kuberov1.AlertRule) { r.Channels = []string{"webex://room"} },
		func(r *kuberov1.AlertRule) { r.PendingFor = "soon" },
		func(r *kuberov1.AlertRule) { r.EvalInterval = "1s" },
		func(r *kuberov1.AlertRule) { r.Kind = "budget"; r.Query = "Not A Policy" },
	} {
		bad := protoRule()
		bad.Name = "other"
		mut(bad)
		if _, err := s.UpsertAlertRule(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.UpsertAlertRuleRequest{Rule: bad})); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%+v: want InvalidArgument, got %v", bad, err)
		}
	}
	if _, err := s.DeleteAlertRule(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.DeleteAlertRuleRequest{Id: "nope"})); code(err) != connect.CodeNotFound {
		t.Fatalf("delete unknown: %v", err)
	}
	if _, err := s.DeleteAlertRule(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.DeleteAlertRuleRequest{Id: saved.GetId()})); err != nil {
		t.Fatal(err)
	}
}

func TestTestAlertRuleHasNoSideEffects(t *testing.T) {
	s, st, src := newService()
	src.scripted.samples = []Sample{
		{Labels: map[string]string{"workload": "a"}, Value: 9},
		{Labels: map[string]string{"workload": "b"}, Value: 2},
	}
	rule := protoRule()
	rule.Name = ""
	if _, err := s.TestAlertRule(as(auth.RoleViewer), connect.NewRequest(&kuberov1.TestAlertRuleRequest{Rule: rule})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer test: %v", err)
	}
	r, err := s.TestAlertRule(as(auth.RoleMember), connect.NewRequest(&kuberov1.TestAlertRuleRequest{Rule: rule}))
	if err != nil {
		t.Fatal(err)
	}
	res := r.Msg.GetResults()
	if len(res) != 2 || !res[0].GetTriggered() || res[1].GetTriggered() || r.Msg.GetError() != "" {
		t.Fatalf("results: %+v", r.Msg)
	}
	rules, _ := st.ListRules(context.Background())
	alerts, _ := st.ListAlerts(context.Background())
	if len(rules) != 0 || len(alerts) != 0 {
		t.Fatal("TestAlertRule must not persist anything")
	}
	rule.Query = `network{nope="x"}`
	r, _ = s.TestAlertRule(as(auth.RoleMember), connect.NewRequest(&kuberov1.TestAlertRuleRequest{Rule: rule}))
	if !strings.Contains(r.Msg.GetError(), "not available") {
		t.Fatalf("query errors are reported in the response: %q", r.Msg.GetError())
	}
}

func TestListAlertsAndSilences(t *testing.T) {
	s, st, _ := newService()
	ctx := context.Background()
	rule, _ := st.UpsertRule(ctx, &Rule{Name: "High spend", Kind: KindCost, Query: "cost", Op: ">", Threshold: 1, Severity: "critical", Enabled: true, EvalInterval: time.Minute})
	res := Step(rule, nil, []Sample{{Labels: map[string]string{"namespace": "a"}, Value: 5}}, t0)
	pend := Step(&Rule{ID: rule.ID, Name: rule.Name, Op: ">", Threshold: 1, PendingFor: time.Hour, Severity: "critical"}, nil,
		[]Sample{{Labels: map[string]string{"namespace": "b"}, Value: 5}}, t0)
	_ = st.SaveAlerts(ctx, rule.ID, append(res.Alerts, pend.Alerts...), nil)

	sil, err := s.CreateSilence(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.CreateSilenceRequest{Silence: &kuberov1.Silence{
		Matchers: map[string]string{"namespace": "a"}, EndsAt: t0.Add(time.Hour).Format(time.RFC3339), Comment: "deploying"}}))
	if err != nil {
		t.Fatal(err)
	}
	if sil.Msg.GetSilence().GetCreatedBy() != "admin@example.com" || sil.Msg.GetSilence().GetStartsAt() != t0.Format(time.RFC3339) {
		t.Fatalf("silence: %+v", sil.Msg.GetSilence())
	}
	l, err := s.ListAlerts(as(auth.RoleViewer), connect.NewRequest(&kuberov1.ListAlertsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if l.Msg.GetFiring() != 1 || l.Msg.GetPending() != 1 || len(l.Msg.GetAlerts()) != 2 {
		t.Fatalf("counts: %+v", l.Msg)
	}
	first := l.Msg.GetAlerts()[0]
	if first.GetState() != StateFiring || !first.GetSilenced() || first.GetRuleName() != "High spend" || first.GetSeverity() != "critical" {
		t.Fatalf("firing first, silenced: %+v", first)
	}
	only, _ := s.ListAlerts(as(auth.RoleViewer), connect.NewRequest(&kuberov1.ListAlertsRequest{State: "pending"}))
	if len(only.Msg.GetAlerts()) != 1 || only.Msg.GetFiring() != 1 {
		t.Fatalf("state filter keeps global counts: %+v", only.Msg)
	}
	if _, err := s.ListAlerts(as(auth.RoleViewer), connect.NewRequest(&kuberov1.ListAlertsRequest{State: "exploded"})); code(err) != connect.CodeInvalidArgument {
		t.Fatal("bad state")
	}

	if _, err := s.DeleteSilence(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.DeleteSilenceRequest{Id: sil.Msg.GetSilence().GetId()})); err != nil {
		t.Fatal(err)
	}
	active, _ := s.ListSilences(as(auth.RoleViewer), connect.NewRequest(&kuberov1.ListSilencesRequest{}))
	all, _ := s.ListSilences(as(auth.RoleViewer), connect.NewRequest(&kuberov1.ListSilencesRequest{IncludeExpired: true}))
	if len(active.Msg.GetSilences()) != 0 || len(all.Msg.GetSilences()) != 1 {
		t.Fatalf("expired silence: active %d all %d", len(active.Msg.GetSilences()), len(all.Msg.GetSilences()))
	}
	if _, err := s.CreateSilence(as(auth.RoleAdmin), connect.NewRequest(&kuberov1.CreateSilenceRequest{Silence: &kuberov1.Silence{
		Matchers: map[string]string{"a": "b"}, EndsAt: "yesterday"}})); code(err) != connect.CodeInvalidArgument {
		t.Fatal("bad silence time")
	}
	if _, err := s.CreateSilence(as(auth.RoleViewer), connect.NewRequest(&kuberov1.CreateSilenceRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatal("viewer cannot silence")
	}
}

func TestClusterTokensAndDemo(t *testing.T) {
	s, _, _ := newService()
	tok := auth.WithPrincipal(context.Background(), auth.Principal{Role: auth.RoleMember, ClusterID: "c1"})
	if _, err := s.ListAlerts(tok, connect.NewRequest(&kuberov1.ListAlertsRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("cluster tokens must not read fleet alerts: %v", err)
	}
	s.Demo = true
	l, err := s.ListAlerts(as(auth.RoleViewer), connect.NewRequest(&kuberov1.ListAlertsRequest{}))
	if err != nil || len(l.Msg.GetAlerts()) != 4 || l.Msg.GetAlerts()[0].GetLabels()["source"] != "demo" || l.Msg.GetFiring() != 2 {
		t.Fatalf("demo alerts: %v %+v", err, l.Msg)
	}
	r, _ := s.TestAlertRule(as(auth.RoleMember), connect.NewRequest(&kuberov1.TestAlertRuleRequest{Rule: protoRule()}))
	if len(r.Msg.GetResults()) != 2 || !r.Msg.GetResults()[0].GetTriggered() {
		t.Fatalf("demo preview: %+v", r.Msg)
	}
}
