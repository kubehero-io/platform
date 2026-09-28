// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
)

// Service implements kuberov1connect.AlertsServiceHandler. Reads need
// viewer, TestAlertRule member (it runs queries), mutations admin.
type Service struct {
	Store    Store
	Sources  Sources
	Engine   *Engine          // Reload after mutations; may be nil
	Channels ChannelValidator // alerter.Router
	// Demo serves demo alerts and previews (no ClickHouse and demo
	// fixtures allowed); rules and silences stay real.
	Demo bool
	Now  func() time.Time
}

var _ kuberov1connect.AlertsServiceHandler = (*Service)(nil)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func guard(ctx context.Context, role auth.Role) error {
	if err := auth.Require(ctx, role); err != nil {
		return err
	}
	return clusters.RequireFleet(ctx)
}

func storeErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, ErrNameTaken):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, ErrTooMany):
		return connect.NewError(connect.CodeResourceExhausted, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func actor(ctx context.Context) string {
	p := auth.PrincipalFromContext(ctx)
	if p.Email != "" {
		return p.Email
	}
	if p.Sub != "" {
		return p.Sub
	}
	return string(p.Role)
}

// ruleToProto renders a rule; channel URLs (credentials) are shown in
// full to admins only.
func ruleToProto(ctx context.Context, r *Rule) *kuberov1.AlertRule {
	chans := append([]string(nil), r.Channels...)
	if auth.Require(ctx, auth.RoleAdmin) != nil {
		for i, c := range chans {
			chans[i] = alerter.RedactChannel(c)
		}
	}
	return &kuberov1.AlertRule{
		Id: r.ID, Name: r.Name, Description: r.Description, Kind: r.Kind, Query: r.Query, Op: r.Op,
		Threshold: r.Threshold, PendingFor: FormatDuration(r.PendingFor), Severity: r.Severity,
		Channels: chans, Labels: cloneMap(r.Labels), Annotations: cloneMap(r.Annotations), Enabled: r.Enabled,
		EvalInterval: FormatDuration(r.EvalInterval), CreatedAt: ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt),
		CreatedBy: r.CreatedBy,
	}
}

// ruleFromProto parses durations; validation happens in Normalize.
func ruleFromProto(m *kuberov1.AlertRule) (*Rule, error) {
	if m == nil {
		return nil, errors.New("rule is required")
	}
	pending, err := ParseDuration(m.GetPendingFor())
	if err != nil {
		return nil, fmt.Errorf("pending_for: %w", err)
	}
	interval, err := ParseDuration(m.GetEvalInterval())
	if err != nil {
		return nil, fmt.Errorf("eval_interval: %w", err)
	}
	return &Rule{
		ID: strings.TrimSpace(m.GetId()), Name: m.GetName(), Description: m.GetDescription(), Kind: m.GetKind(),
		Query: m.GetQuery(), Op: strings.TrimSpace(m.GetOp()), Threshold: m.GetThreshold(), PendingFor: pending,
		Severity: m.GetSeverity(), Channels: m.GetChannels(), Labels: cloneMap(m.GetLabels()),
		Annotations: cloneMap(m.GetAnnotations()), Enabled: m.GetEnabled(), EvalInterval: interval,
	}, nil
}

// ListAlertRules lists rules, optionally of one kind.
func (s *Service) ListAlertRules(ctx context.Context, req *connect.Request[kuberov1.ListAlertRulesRequest]) (*connect.Response[kuberov1.ListAlertRulesResponse], error) {
	if err := guard(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	rules, err := s.Store.ListRules(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	kind := strings.ToLower(strings.TrimSpace(req.Msg.GetKind()))
	resp := &kuberov1.ListAlertRulesResponse{}
	for _, r := range rules {
		if kind == "" || r.Kind == kind {
			resp.Rules = append(resp.Rules, ruleToProto(ctx, r))
		}
	}
	return connect.NewResponse(resp), nil
}

// UpsertAlertRule creates (empty id) or replaces a rule.
func (s *Service) UpsertAlertRule(ctx context.Context, req *connect.Request[kuberov1.UpsertAlertRuleRequest]) (*connect.Response[kuberov1.UpsertAlertRuleResponse], error) {
	if err := guard(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	r, err := ruleFromProto(req.Msg.GetRule())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if r.ID != "" {
		old, err := s.Store.GetRule(ctx, r.ID)
		if err != nil {
			return nil, storeErr(err)
		}
		// The UI round-trips redacted channels: keep the stored secret.
		for i, c := range r.Channels {
			if !alerter.IsRedacted(c) {
				continue
			}
			for _, o := range old.Channels {
				if alerter.RedactChannel(o) == strings.TrimSpace(c) {
					r.Channels[i] = o
				}
			}
			if alerter.IsRedacted(r.Channels[i]) {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("channel %q is redacted and matches no stored channel; re-enter it", c))
			}
		}
	}
	if err := r.Normalize(s.Channels); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := s.Sources.ValidateQuery(ctx, r.Kind, r.Query); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("query: %w", err))
	}
	r.CreatedBy = actor(ctx)
	saved, err := s.Store.UpsertRule(ctx, r)
	if err != nil {
		return nil, storeErr(err)
	}
	if s.Engine != nil {
		s.Engine.Reload()
	}
	return connect.NewResponse(&kuberov1.UpsertAlertRuleResponse{Rule: ruleToProto(ctx, saved)}), nil
}

// DeleteAlertRule deletes a rule and its alert state.
func (s *Service) DeleteAlertRule(ctx context.Context, req *connect.Request[kuberov1.DeleteAlertRuleRequest]) (*connect.Response[kuberov1.DeleteAlertRuleResponse], error) {
	if err := guard(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	if err := s.Store.DeleteRule(ctx, strings.TrimSpace(req.Msg.GetId())); err != nil {
		return nil, storeErr(err)
	}
	if s.Engine != nil {
		s.Engine.Reload()
	}
	return connect.NewResponse(&kuberov1.DeleteAlertRuleResponse{}), nil
}

// TestAlertRule evaluates a rule once, now: no state, no notifications.
func (s *Service) TestAlertRule(ctx context.Context, req *connect.Request[kuberov1.TestAlertRuleRequest]) (*connect.Response[kuberov1.TestAlertRuleResponse], error) {
	if err := guard(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	r, err := ruleFromProto(req.Msg.GetRule())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if strings.TrimSpace(r.Name) == "" {
		r.Name = "preview"
	}
	r.Channels = nil // a preview never notifies
	if err := r.Normalize(nil); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	start := time.Now()
	resp := &kuberov1.TestAlertRuleResponse{}
	var samples []Sample
	if s.Demo {
		samples = demoSamples(r)
	} else if err := s.Sources.ValidateQuery(ctx, r.Kind, r.Query); err != nil {
		resp.Error = err.Error()
	} else if samples, err = s.Sources.Evaluate(ctx, r, s.now()); err != nil {
		resp.Error = err.Error()
	}
	for i, sm := range samples {
		if i >= MaxSeries {
			break
		}
		resp.Results = append(resp.Results, &kuberov1.AlertEvaluation{Labels: sm.Labels, Value: sm.Value,
			Triggered: Compare(sm.Value, r.Op, r.Threshold)})
	}
	resp.ExecMs = float64(time.Since(start).Microseconds()) / 1000
	return connect.NewResponse(resp), nil
}

var stateRank = map[string]int{StateFiring: 0, StatePending: 1, StateResolved: 2}
var severityRank = map[string]int{"critical": 0, "warn": 1, "info": 2}

// ListAlerts returns pending + firing alerts and recently resolved ones.
func (s *Service) ListAlerts(ctx context.Context, req *connect.Request[kuberov1.ListAlertsRequest]) (*connect.Response[kuberov1.ListAlertsResponse], error) {
	if err := guard(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	state := strings.ToLower(strings.TrimSpace(req.Msg.GetState()))
	if _, ok := stateRank[state]; state != "" && !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("state must be pending, firing or resolved"))
	}
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	now := s.now()

	var alerts []*kuberov1.Alert
	if s.Demo {
		alerts = demoAlerts(now)
	} else {
		stored, err := s.Store.ListAlerts(ctx)
		if err != nil {
			return nil, storeErr(err)
		}
		rules, err := s.Store.ListRules(ctx)
		if err != nil {
			return nil, storeErr(err)
		}
		byID := map[string]*Rule{}
		for _, r := range rules {
			byID[r.ID] = r
		}
		silences, err := s.Store.ListSilences(ctx, false, now)
		if err != nil {
			return nil, storeErr(err)
		}
		for _, a := range stored {
			r := byID[a.RuleID]
			if r == nil {
				continue
			}
			alerts = append(alerts, alertToProto(a, r, silenced(a, silences, now)))
		}
	}
	resp := &kuberov1.ListAlertsResponse{}
	var out []*kuberov1.Alert
	for _, a := range alerts {
		switch a.GetState() {
		case StateFiring:
			resp.Firing++
		case StatePending:
			resp.Pending++
		}
		if state == "" || a.GetState() == state {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if stateRank[a.GetState()] != stateRank[b.GetState()] {
			return stateRank[a.GetState()] < stateRank[b.GetState()]
		}
		if severityRank[a.GetSeverity()] != severityRank[b.GetSeverity()] {
			return severityRank[a.GetSeverity()] < severityRank[b.GetSeverity()]
		}
		return a.GetStartedAt() > b.GetStartedAt()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	resp.Alerts = out
	return connect.NewResponse(resp), nil
}

func alertToProto(a *Alert, r *Rule, isSilenced bool) *kuberov1.Alert {
	sev := a.Labels["severity"]
	if sev == "" {
		sev = r.Severity
	}
	v := a.Value
	if math.IsNaN(v) || math.IsInf(v, 0) {
		v = 0
	}
	return &kuberov1.Alert{
		Id: a.ID, RuleId: r.ID, RuleName: r.Name, Kind: r.Kind, State: a.State, Severity: sev,
		Labels: cloneMap(a.Labels), Value: v, Summary: a.Summary, Description: a.Description,
		StartedAt: ts(a.StartedAt), FiredAt: ts(a.FiredAt), ResolvedAt: ts(a.ResolvedAt), LastEvalAt: ts(a.LastEvalAt),
		Silenced: isSilenced, LinkPath: a.LinkPath,
	}
}

func silenceToProto(sl *Silence) *kuberov1.Silence {
	return &kuberov1.Silence{Id: sl.ID, Matchers: cloneMap(sl.Matchers), StartsAt: ts(sl.StartsAt), EndsAt: ts(sl.EndsAt),
		CreatedBy: sl.CreatedBy, Comment: sl.Comment}
}

func parseTS(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad RFC3339 time %q", s)
	}
	return t.UTC(), nil
}

// CreateSilence suppresses notifications for matching alerts.
func (s *Service) CreateSilence(ctx context.Context, req *connect.Request[kuberov1.CreateSilenceRequest]) (*connect.Response[kuberov1.CreateSilenceResponse], error) {
	if err := guard(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	m := req.Msg.GetSilence()
	if m == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("silence is required"))
	}
	starts, err := parseTS(m.GetStartsAt())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ends, err := parseTS(m.GetEndsAt())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	sl := &Silence{Matchers: cloneMap(m.GetMatchers()), StartsAt: starts, EndsAt: ends, Comment: m.GetComment(), CreatedBy: actor(ctx)}
	if err := sl.Validate(s.now()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	saved, err := s.Store.CreateSilence(ctx, sl)
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&kuberov1.CreateSilenceResponse{Silence: silenceToProto(saved)}), nil
}

// ListSilences lists active (and optionally expired) silences.
func (s *Service) ListSilences(ctx context.Context, req *connect.Request[kuberov1.ListSilencesRequest]) (*connect.Response[kuberov1.ListSilencesResponse], error) {
	if err := guard(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	sls, err := s.Store.ListSilences(ctx, req.Msg.GetIncludeExpired(), s.now())
	if err != nil {
		return nil, storeErr(err)
	}
	resp := &kuberov1.ListSilencesResponse{}
	for _, sl := range sls {
		resp.Silences = append(resp.Silences, silenceToProto(sl))
	}
	return connect.NewResponse(resp), nil
}

// DeleteSilence expires a silence now (it stays listed as expired).
func (s *Service) DeleteSilence(ctx context.Context, req *connect.Request[kuberov1.DeleteSilenceRequest]) (*connect.Response[kuberov1.DeleteSilenceResponse], error) {
	if err := guard(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	if err := s.Store.ExpireSilence(ctx, strings.TrimSpace(req.Msg.GetId()), s.now()); err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&kuberov1.DeleteSilenceResponse{}), nil
}
