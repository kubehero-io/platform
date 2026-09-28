// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// runCLI runs the root command against the fake with an isolated HOME
// (tests must never read or write the developer's ~/.kubehero).
func runCLI(t *testing.T, f *fakeCP, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"KUBEHERO_ENDPOINT", "KUBEHERO_TOKEN", "KUBEHERO_ORG", "KUBEHERO_ADVISOR_ENDPOINT"} {
		t.Setenv(k, "")
	}
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	r := Root()
	r.SetOut(out)
	r.SetErr(errb)
	full := append([]string{}, args...)
	if f != nil {
		full = append(full, "--endpoint", f.srv.URL)
		if f.token != "" {
			full = append(full, "--token", f.token)
		}
	}
	r.SetArgs(full)
	err := r.Execute()
	return out.String(), errb.String(), err
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
}

func TestLogsCommand(t *testing.T) {
	f := newFakeCP(t, "tok")
	out, _, err := runCLI(t, f, "logs", `{namespace="payments", level="error"}`, "--since", "2h", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "ERROR", "payments/checkout-api", "upstream timeout")
	req := f.got("QueryLogs").(*kuberov1.QueryLogsRequest)
	if req.GetLimit() != 5 || req.GetEndUnixMs()-req.GetStartUnixMs() != 2*3600*1000 || req.GetDirection() != "backward" {
		t.Errorf("request = %+v", req)
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("no colour when stdout is not a terminal")
	}
	if f.auths["QueryLogs"] != "Bearer tok" {
		t.Errorf("token not sent: %q", f.auths["QueryLogs"])
	}

	out, _, err = runCLI(t, f, "logs", `sum by (namespace) (count_over_time({level="error"}[5m]))`)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "namespace=payments", "42")

	out, _, err = runCLI(t, f, "logs", `{namespace="payments"}`, "--patterns")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "upstream timeout calling <_> after <_>ms", "18,240", "30,100 lines analysed")

	out, _, err = runCLI(t, f, "logs", `{level="error"}`, "--volume", "--group-by", "namespace")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "level=error", "910 lines", "/mo to store")
	if g := f.got("GetLogVolume").(*kuberov1.GetLogVolumeRequest).GetGroupBy(); g != "namespace" {
		t.Errorf("group_by = %q", g)
	}

	out, errOut, err := runCLI(t, f, "logs", `{namespace="payments"}`, "-f")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "tail line 0", "tail line 1")
	mustContain(t, errOut, "1 lines dropped")

	out, _, err = runCLI(t, f, "logs", `{namespace="payments"}`, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var js map[string]any
	if err := json.Unmarshal([]byte(out), &js); err != nil || js["resultType"] != "streams" {
		t.Errorf("json output: %v %s", err, out)
	}

	for _, bad := range [][]string{
		{"logs", `{a="b"}`, "--since", "forever"},
		{"logs", `{a="b"}`, "--patterns", "--volume"},
		{"logs", `{a="b"}`, "--limit", "0"},
	} {
		if _, _, err := runCLI(t, f, bad...); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
}

func TestProfileCommands(t *testing.T) {
	f := newFakeCP(t, "")
	out, _, err := runCLI(t, f, "profile", "top", "--service", "checkout-api", "--since", "30m")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "crypto/tls.(*Conn).Handshake", "35.1%", "$4,770")
	if s := f.got("GetTopFunctions").(*kuberov1.GetTopFunctionsRequest).GetSelector(); s.GetService() != "checkout-api" || s.GetType() != "cpu" {
		t.Errorf("selector = %+v", s)
	}

	out, _, err = runCLI(t, f, "profile", "flame", "--service", "checkout-api", "--diff-since", "24h")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "total 100.0%", "└─ ", "main.handleCharge 90.0%", "crypto/tls.(*Conn).Handshake 60.0%", "+40.0pp", "-30.0pp", "$13,650/mo")
	if strings.Contains(out, "tiny") {
		t.Error("frames below --min-pct should be hidden")
	}
	req := f.got("GetFlamegraph").(*kuberov1.GetFlamegraphRequest)
	if req.GetBaselineEndUnixMs()-req.GetEndUnixMs() != -24*3600*1000 {
		t.Errorf("baseline window not shifted by 24h: %+v", req)
	}

	if _, _, err := runCLI(t, f, "profile", "top"); err == nil || !strings.Contains(err.Error(), "--service") {
		t.Errorf("missing --service: %v", err)
	}
	out, _, err = runCLI(t, f, "profile", "targets")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "checkout-api", "ebpf", "$19,500")
}

func TestCostCommands(t *testing.T) {
	f := newFakeCP(t, "")
	out, _, err := runCLI(t, f, "cost", "--aggregate", "namespace,workload", "--window", "30d", "--idle",
		"--share-idle", "weighted", "--filter", "team=payments", "--shared-namespaces", "kube-system")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "payments", "$7,234", "26%", "total $10,150 over 30d")
	req := f.got("GetAllocation").(*kuberov1.GetAllocationRequest)
	if strings.Join(req.GetAggregate(), ",") != "namespace,workload" || !req.GetIncludeIdle() || req.GetShareIdle() != "weighted" ||
		req.GetFilters()["team"] != "payments" || req.GetSharedNamespaces()[0] != "kube-system" || req.GetWindow() != "30d" {
		t.Errorf("request = %+v", req)
	}
	for _, bad := range [][]string{
		{"cost", "--aggregate", "pods"},
		{"cost", "--filter", "nope"},
		{"cost", "--share-idle", "random"},
	} {
		if _, _, err := runCLI(t, f, bad...); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}

	out, _, err = runCLI(t, f, "cost", "timeseries", "--group-by", "namespace", "--step", "1d")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "namespace=payments", "month forecast $4,800", "DEMO DATA")

	out, _, err = runCLI(t, f, "cost", "efficiency")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "namespace/payments", "fleet score 41/100")

	dir := t.TempDir()
	dest := filepath.Join(dir, "focus.csv")
	_, errOut, err := runCLI(t, f, "cost", "export", "--format", "focus", "--window", "lastmonth", "-o", dest)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	mustContain(t, string(b), "BilledCost,EffectiveCost", "eks-1/payments/api")
	mustContain(t, errOut, "wrote "+dest)
	if f.focus != "aggregate=workload&window=lastmonth" {
		t.Errorf("export query = %q", f.focus)
	}
	if _, _, err := runCLI(t, f, "cost", "export", "-o", "json"); err == nil {
		t.Error("-o json makes no sense for a CSV export")
	}
	if _, _, err := runCLI(t, f, "cost", "export", "--format", "xlsx"); err == nil {
		t.Error("unknown format must fail")
	}
}

func TestRightsizeList(t *testing.T) {
	f := newFakeCP(t, "")
	out, _, err := runCLI(t, f, "rightsize", "list", "--namespace", "payments", "--min-savings", "100")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "payments/checkout-api (app)", "16.00 → 3.60", "32.0GiB → 18.0GiB", "$6,200", "4 OOM", "RightsizingPolicy")
	req := f.got("ListRightsizing").(*kuberov1.ListRightsizingRequest)
	if req.GetNamespace() != "payments" || req.GetMinSavingsUsdMonth() != 100 {
		t.Errorf("request = %+v", req)
	}
	out, _, err = runCLI(t, f, "rightsize", "checkout-api")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "embedder") {
		t.Error("workload argument should filter the list")
	}
	if _, _, err := runCLI(t, f, "rightsize", "--percentile", "p50"); err == nil {
		t.Error("bad percentile must fail")
	}
}

func TestNetworkCommands(t *testing.T) {
	f := newFakeCP(t, "")
	out, _, err := runCLI(t, f, "network", "map", "--namespace", "payments", "--max-edges", "1")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "workload:edge/api-gateway", "$2,100", "egress", "2 edges")
	if strings.Contains(out, "retx=") {
		t.Error("--max-edges 1 should keep only the most expensive edge")
	}
	out, _, err = runCLI(t, f, "network", "costs")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "edge/api-gateway", "internet", "$2,100/mo network spend")
}

func TestAlertsCommands(t *testing.T) {
	f := newFakeCP(t, "")
	out, _, err := runCLI(t, f, "alerts", "list", "--state", "firing")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "PaymentsErrorRate", "CRITICAL", "1 firing")

	out, _, err = runCLI(t, f, "alerts", "rules", "--kind", "logs")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "PaymentsErrorRate", "> 2")

	out, _, err = runCLI(t, f, "alerts", "silence", "--matcher", "alertname=PaymentsErrorRate", "--duration", "2h", "--comment", "rollback")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "silence sil-1", "alertname=PaymentsErrorRate")
	s := f.got("CreateSilence").(*kuberov1.CreateSilenceRequest).GetSilence()
	if s.GetComment() != "rollback" || s.GetStartsAt() == "" || s.GetEndsAt() <= s.GetStartsAt() {
		t.Errorf("silence = %+v", s)
	}
	out, _, err = runCLI(t, f, "alerts", "silence", "list")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "sil-1")
	if _, _, err := runCLI(t, f, "alerts", "silence", "delete", "sil-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, f, "alerts", "silence"); err == nil {
		t.Error("silence without matchers must fail")
	}

	rule := filepath.Join(t.TempDir(), "rule.yaml")
	_ = os.WriteFile(rule, []byte("name: errors\nkind: logs\nquery: 'sum by (namespace) (count_over_time({level=\"error\"}[5m]))'\nop: '>'\nthreshold: 100\npending_for: 5m\n"), 0o600)
	out, _, err = runCLI(t, f, "alerts", "test", "-f", rule, "--threshold", "50")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "namespace=payments", "YES", "1 of 2 series would fire")
	r := f.got("TestAlertRule").(*kuberov1.TestAlertRuleRequest).GetRule()
	if r.GetThreshold() != 50 || r.GetPendingFor() != "5m" || r.GetKind() != "logs" {
		t.Errorf("rule = %+v (flag should override file)", r)
	}
	if _, _, err := runCLI(t, f, "alerts", "test", "--kind", "logs"); err == nil {
		t.Error("rule without query must fail")
	}
}

func TestAskStreamsStepsThenAnswer(t *testing.T) {
	f := newFakeCP(t, "tok")
	out, errOut, err := runCLI(t, f, "ask", "why did checkout's spend jump?", "--window", "7d", "--speak")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, errOut, "Planning the investigation", "✓ get_cost_timeseries", "✗ query_logs")
	mustContain(t, out, "checkout-api spend rose 44%", "[logs] upstream timeout pattern", "query: {namespace=\"payments\"}",
		"Rightsize checkout-api", "kind: RightsizingPolicy", "Spoken summary", "Checkout's spend rose")
	req := f.got("InvestigateStream").(*kuberov1.InvestigateStreamRequest).GetRequest()
	if req.GetQuestion() != "why did checkout's spend jump?" || req.GetWindow() != "7d" {
		t.Errorf("request = %+v", req)
	}
	if f.auths["InvestigateStream"] != "Bearer tok" {
		t.Error("advisor call must carry the token")
	}

	out, _, err = runCLI(t, f, "ask", "anything?", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var js map[string]any
	if err := json.Unmarshal([]byte(out), &js); err != nil || js["answerMarkdown"] == nil {
		t.Errorf("json answer: %v %s", err, out)
	}
	if _, _, err := runCLI(t, f, "ask", "q", "--window", "2d"); err == nil {
		t.Error("bad window must fail")
	}
}

func TestAuthWhoamiAndLogin(t *testing.T) {
	f := newFakeCP(t, "tok")
	out, _, err := runCLI(t, f, "auth", "whoami")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "key:3f9a1c2e", "member")

	// A rejected token is not saved.
	home := t.TempDir()
	t.Setenv("HOME", home)
	r := Root()
	var buf bytes.Buffer
	r.SetOut(&buf)
	r.SetErr(&buf)
	r.SetArgs([]string{"auth", "login", "--endpoint", f.srv.URL, "--token", "wrong"})
	if err := r.Execute(); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("login with a bad token: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".kubehero", "config.yaml")); !os.IsNotExist(err) {
		t.Error("rejected credential was saved")
	}

	// A good token is verified, then saved with the identity shown.
	r = Root()
	buf.Reset()
	r.SetOut(&buf)
	r.SetErr(&buf)
	r.SetArgs([]string{"auth", "login", "--endpoint", f.srv.URL, "--token", "tok"})
	if err := r.Execute(); err != nil {
		t.Fatalf("login: %v\n%s", err, buf.String())
	}
	mustContain(t, buf.String(), "saved", "key:3f9a1c2e (member)")
	if b, err := os.ReadFile(filepath.Join(home, ".kubehero", "config.yaml")); err != nil || !strings.Contains(string(b), "tok") {
		t.Errorf("config not saved: %v", err)
	}

	// --no-verify saves without contacting anything (offline setup).
	r = Root()
	buf.Reset()
	r.SetOut(&buf)
	r.SetErr(&buf)
	r.SetArgs([]string{"auth", "login", "--endpoint", "http://127.0.0.1:1", "--token", "offline-token", "--no-verify"})
	if err := r.Execute(); err != nil {
		t.Fatalf("--no-verify login: %v", err)
	}
	mustContain(t, buf.String(), "saved · endpoint=http://127.0.0.1:1")
}

func TestSignalCommandsNeedAnEndpoint(t *testing.T) {
	if _, _, err := runCLI(t, nil, "logs", `{a="b"}`); err == nil || !strings.Contains(err.Error(), "no endpoint") {
		t.Errorf("err = %v", err)
	}
}
