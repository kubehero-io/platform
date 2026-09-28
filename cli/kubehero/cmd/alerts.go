// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"

	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

var (
	alertKinds  = map[string]bool{"logs": true, "cost": true, "budget": true, "anomaly": true, "network": true, "event": true}
	alertOps    = map[string]bool{">": true, ">=": true, "<": true, "<=": true, "==": true, "!=": true}
	alertLevels = map[string]bool{"info": true, "warn": true, "critical": true}
)

func alertsCmd() *cobra.Command {
	c := &cobra.Command{Use: "alerts", Short: "Alerts over logs, spend, budgets, anomalies, network and events"}
	c.AddCommand(alertsListCmd(), alertsRulesCmd(), alertsSilenceCmd(), alertsTestCmd())
	return c
}

func alertsListCmd() *cobra.Command {
	var state string
	var limit int
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Active (pending + firing) and recently resolved alerts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch state {
			case "", "pending", "firing", "resolved":
			default:
				return errors.New("--state must be pending, firing or resolved")
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Alerts.ListAlerts(ctx, connect.NewRequest(&kuberov1.ListAlertsRequest{State: state, Limit: int32(limit)}))
			if err != nil {
				return err
			}
			paint := newPainter(cmd.OutOrStdout())
			rows := make([]*khfmt.Row, 0, len(res.Msg.GetAlerts()))
			for _, a := range res.Msg.GetAlerts() {
				st := a.GetState()
				if a.GetSilenced() {
					st += " (silenced)"
				}
				rows = append(rows, khfmt.NewRow().
					Set("state", st).
					Set("severity", paint.level(a.GetSeverity())).
					Set("rule", a.GetRuleName()).
					Set("value", trimFloat(a.GetValue())).
					Set("summary", a.GetSummary()).
					Set("since", nonEmptyStr(a.GetFiredAt(), a.GetStartedAt())).
					Set("labels", labelString(a.GetLabels())))
			}
			if err := render(cmd, cfg, rows, res.Msg); err != nil {
				return err
			}
			if !structuredOutput(cfg) {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d firing · %d pending\n", res.Msg.GetFiring(), res.Msg.GetPending())
			}
			return nil
		},
	}
	c.Flags().StringVar(&state, "state", "", "Filter: pending | firing | resolved")
	c.Flags().IntVar(&limit, "limit", 100, "Max alerts")
	return c
}

func alertsRulesCmd() *cobra.Command {
	var kind string
	c := &cobra.Command{
		Use:   "rules",
		Short: "Alert rules",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if kind != "" && !alertKinds[kind] {
				return errors.New("--kind must be logs, cost, budget, anomaly, network or event")
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Alerts.ListAlertRules(ctx, connect.NewRequest(&kuberov1.ListAlertRulesRequest{Kind: kind}))
			if err != nil {
				return err
			}
			rows := make([]*khfmt.Row, 0, len(res.Msg.GetRules()))
			for _, r := range res.Msg.GetRules() {
				enabled := "yes"
				if !r.GetEnabled() {
					enabled = "no"
				}
				rows = append(rows, khfmt.NewRow().
					Set("name", r.GetName()).
					Set("kind", r.GetKind()).
					Set("condition", fmt.Sprintf("%s %s %s", r.GetQuery(), r.GetOp(), trimFloat(r.GetThreshold()))).
					Set("for", r.GetPendingFor()).
					Set("severity", r.GetSeverity()).
					Set("enabled", enabled).
					Set("channels", strings.Join(r.GetChannels(), ",")).
					Set("id", r.GetId()))
			}
			return render(cmd, cfg, rows, res.Msg)
		},
	}
	c.Flags().StringVar(&kind, "kind", "", "Filter by kind")
	return c
}

func alertsSilenceCmd() *cobra.Command {
	var matchers []string
	var duration, startsAt, comment string
	c := &cobra.Command{
		Use:   "silence",
		Short: "Silence alerts matching label matchers (list / delete with subcommands)",
		Example: `  kubehero alerts silence --matcher alertname=PaymentsErrorRate --duration 2h --comment "deploy rollback in progress"
  kubehero alerts silence list
  kubehero alerts silence delete sil-123`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(matchers) == 0 {
				return errors.New("at least one --matcher key=value is required (alertname matches the rule name)")
			}
			m := map[string]string{}
			for _, kv := range matchers {
				k, v, ok := strings.Cut(kv, "=")
				k, v = strings.TrimSpace(k), strings.TrimSpace(v)
				if !ok || k == "" || v == "" {
					return fmt.Errorf("--matcher %q must look like key=value", kv)
				}
				m[k] = v
			}
			d, err := parseSince(duration)
			if err != nil {
				return fmt.Errorf("--duration: %w", err)
			}
			start := time.Now().UTC()
			if startsAt != "" {
				if start, err = time.Parse(time.RFC3339, startsAt); err != nil {
					return fmt.Errorf("--starts-at: %w", err)
				}
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Alerts.CreateSilence(ctx, connect.NewRequest(&kuberov1.CreateSilenceRequest{Silence: &kuberov1.Silence{
				Matchers: m, StartsAt: start.Format(time.RFC3339), EndsAt: start.Add(d).Format(time.RFC3339), Comment: comment,
			}}))
			if err != nil {
				return err
			}
			if structuredOutput(cfg) {
				return render(cmd, cfg, nil, res.Msg)
			}
			s := res.Msg.GetSilence()
			fmt.Fprintf(cmd.OutOrStdout(), "✓ silence %s · %s until %s\n", s.GetId(), labelString(s.GetMatchers()), s.GetEndsAt())
			return nil
		},
	}
	c.Flags().StringSliceVar(&matchers, "matcher", nil, "Label matcher key=value (repeatable)")
	c.Flags().StringVar(&duration, "duration", "2h", "How long, e.g. 30m, 2h, 1d")
	c.Flags().StringVar(&startsAt, "starts-at", "", "Start (RFC3339; default now)")
	c.Flags().StringVar(&comment, "comment", "", "Why (recorded with the silence)")

	var expired bool
	list := &cobra.Command{
		Use:   "list",
		Short: "Active silences",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Alerts.ListSilences(ctx, connect.NewRequest(&kuberov1.ListSilencesRequest{IncludeExpired: expired}))
			if err != nil {
				return err
			}
			rows := make([]*khfmt.Row, 0, len(res.Msg.GetSilences()))
			for _, s := range res.Msg.GetSilences() {
				rows = append(rows, khfmt.NewRow().
					Set("id", s.GetId()).
					Set("matchers", labelString(s.GetMatchers())).
					Set("ends", s.GetEndsAt()).
					Set("by", s.GetCreatedBy()).
					Set("comment", s.GetComment()))
			}
			return render(cmd, cfg, rows, res.Msg)
		},
	}
	list.Flags().BoolVar(&expired, "include-expired", false, "Include expired silences")
	del := &cobra.Command{
		Use:   "delete <silence-id>",
		Short: "End a silence early",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			if _, err := cl.Alerts.DeleteSilence(ctx, connect.NewRequest(&kuberov1.DeleteSilenceRequest{Id: args[0]})); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ silence %s deleted\n", args[0])
			return nil
		},
	}
	c.AddCommand(list, del)
	return c
}

func alertsTestCmd() *cobra.Command {
	var file, name, kind, query, op, severity, pendingFor string
	var threshold float64
	c := &cobra.Command{
		Use:   "test",
		Short: "Evaluate a rule once, right now — no state, no notifications",
		Example: `  kubehero alerts test --kind logs --query 'sum by (namespace) (count_over_time({level="error"}[5m]))' --op '>' --threshold 100
  kubehero alerts test -f rule.yaml`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rule := &kuberov1.AlertRule{}
			if file != "" {
				raw, err := os.ReadFile(file)
				if err != nil {
					return err
				}
				if err := yamlToProto(raw, rule); err != nil {
					return fmt.Errorf("%s: %w", file, err)
				}
			}
			for flagName, set := range map[string]func(){
				"name": func() { rule.Name = name }, "kind": func() { rule.Kind = kind }, "query": func() { rule.Query = query },
				"op": func() { rule.Op = op }, "threshold": func() { rule.Threshold = threshold },
				"severity": func() { rule.Severity = severity }, "for": func() { rule.PendingFor = pendingFor },
			} {
				if cmd.Flags().Changed(flagName) {
					set()
				}
			}
			if rule.Op == "" {
				rule.Op = ">"
			}
			switch {
			case !alertKinds[rule.GetKind()]:
				return errors.New("kind must be logs, cost, budget, anomaly, network or event")
			case strings.TrimSpace(rule.GetQuery()) == "":
				return errors.New("a query is required")
			case !alertOps[rule.GetOp()]:
				return errors.New("op must be one of > >= < <= == !=")
			case rule.GetSeverity() != "" && !alertLevels[rule.GetSeverity()]:
				return errors.New("severity must be info, warn or critical")
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Alerts.TestAlertRule(ctx, connect.NewRequest(&kuberov1.TestAlertRuleRequest{Rule: rule}))
			if err != nil {
				return err
			}
			if e := res.Msg.GetError(); e != "" && !structuredOutput(cfg) {
				return fmt.Errorf("rule error: %s", e)
			}
			rows := make([]*khfmt.Row, 0, len(res.Msg.GetResults()))
			fired := 0
			for _, r := range res.Msg.GetResults() {
				trig := "no"
				if r.GetTriggered() {
					trig = "YES"
					fired++
				}
				rows = append(rows, khfmt.NewRow().
					Set("series", labelString(r.GetLabels())).
					Set("value", trimFloat(r.GetValue())).
					Set("fires", trig))
			}
			if err := render(cmd, cfg, rows, res.Msg); err != nil {
				return err
			}
			if !structuredOutput(cfg) {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d of %d series would fire (%s %s) · evaluated in %.0fms\n",
					fired, len(res.Msg.GetResults()), rule.GetOp(), trimFloat(rule.GetThreshold()), res.Msg.GetExecMs())
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVarP(&file, "file", "f", "", "Rule as YAML/JSON (AlertRule fields)")
	f.StringVar(&name, "name", "", "Rule name")
	f.StringVar(&kind, "kind", "", "logs | cost | budget | anomaly | network | event")
	f.StringVar(&query, "query", "", "Rule query in the kind's language")
	f.StringVar(&op, "op", "", "Comparison: > >= < <= == != (default >)")
	f.Float64Var(&threshold, "threshold", 0, "Threshold")
	f.StringVar(&severity, "severity", "", "info | warn | critical")
	f.StringVar(&pendingFor, "for", "", "How long the condition must hold, e.g. 5m")
	return c
}

// yamlToProto decodes YAML (or JSON) into a proto message via protojson,
// so files may use either camelCase or snake_case field names.
func yamlToProto(raw []byte, m *kuberov1.AlertRule) error {
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(b, m)
}
