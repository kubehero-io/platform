// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package investigate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	rulesbrain "github.com/kubehero-io/platform/services/advisor/internal/brain/rules"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

// Rules is the deterministic investigator: it reads the question for a
// subject (a workload, namespace or cluster that appears in the cost
// allocation) and intents (cost, errors, performance, network, memory,
// rightsizing, alerts), runs the matching tools, and writes an answer
// from what came back. Same data in, same answer out.
type Rules struct {
	Backend backend.Backend
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
	// MaxCalls defaults to DefaultMaxCalls.
	MaxCalls int
}

type intent int

// Intents in the order findings are reported.
const (
	intentAlerts intent = iota
	intentCost
	intentErrors
	intentPerf
	intentMemory
	intentRightsize
	intentNetwork
)

var intentWords = map[intent][]string{
	intentCost:      {"cost", "costs", "spend", "spent", "spending", "bill", "billing", "expensive", "money", "budget", "price", "pricing", "dollar", "dollars", "burn", "jump", "jumped", "spike", "spiked"},
	intentErrors:    {"error", "errors", "log", "logs", "exception", "exceptions", "fail", "failing", "failed", "failure", "failures", "crash", "crashing", "5xx", "500", "timeout", "timeouts", "panic", "broken", "down", "outage", "retry", "retries"},
	intentPerf:      {"slow", "slowness", "latency", "cpu", "hot", "profile", "profiling", "performance", "perf", "throttle", "throttled", "throttling", "p99", "p95", "flamegraph"},
	intentNetwork:   {"network", "egress", "traffic", "bandwidth", "cross-zone", "crosszone", "transfer", "nat", "bytes"},
	intentMemory:    {"oom", "oomkilled", "oom-killed", "memory", "mem", "killed", "leak", "rss"},
	intentRightsize: {"rightsize", "right-size", "rightsizing", "overprovisioned", "over-provisioned", "waste", "wasted", "idle", "requests", "underutilized", "oversized"},
	intentAlerts:    {"alert", "alerts", "firing", "page", "paged", "paging", "incident", "pager"},
}

// detectIntents returns the question's intents in report order. A
// question naming none gets a general health check (cost, errors,
// alerts).
func detectIntents(question string) []intent {
	q := strings.ToLower(question)
	toks := map[string]bool{}
	for _, t := range tokens(q) {
		toks[t] = true
	}
	found := map[intent]bool{}
	for in, words := range intentWords {
		for _, w := range words {
			if toks[w] {
				found[in] = true
				break
			}
		}
	}
	if strings.Contains(q, "$") || strings.Contains(q, "cross zone") {
		if strings.Contains(q, "$") {
			found[intentCost] = true
		} else {
			found[intentNetwork] = true
		}
	}
	if len(found) == 0 {
		found[intentCost], found[intentErrors], found[intentAlerts] = true, true, true
	}
	// Errors are what alerts are usually about, and vice versa.
	if found[intentErrors] {
		found[intentAlerts] = true
	}
	out := make([]intent, 0, len(found))
	for in := range found {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func tokens(s string) []string {
	s = strings.ReplaceAll(s, "'s", "")
	s = strings.ReplaceAll(s, "’s", "")
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.')
	})
}

// subject is what the question is about.
type subject struct {
	Cluster, Namespace, Workload string
}

func (s subject) String() string {
	switch {
	case s.Workload != "":
		return s.Namespace + "/" + s.Workload
	case s.Namespace != "":
		return s.Namespace
	case s.Cluster != "":
		return "cluster " + s.Cluster
	}
	return "the fleet"
}

type workloadRef struct {
	cluster, namespace, name string
	cost                     float64
}

// resolveSubject matches the question (and the dashboard context it was
// asked from) against names that exist in the allocation data.
func resolveSubject(req Request, rows []*kuberov1.Allocation) subject {
	var s subject
	// Context like /workloads/<cluster>/<namespace>/<name> is exact.
	if parts := strings.Split(strings.Trim(strings.SplitN(req.Context, "?", 2)[0], "/"), "/"); len(parts) == 4 && parts[0] == "workloads" {
		return subject{Cluster: parts[1], Namespace: parts[2], Workload: parts[3]}
	}
	toks := tokens(strings.ToLower(req.Question + " " + req.Context))
	tokSet := map[string]bool{}
	for _, t := range toks {
		tokSet[strings.Trim(t, ".-_")] = true
	}

	var workloads []workloadRef
	namespaces := map[string]bool{}
	clusters := map[string]bool{}
	for _, a := range rows {
		p := a.GetProperties()
		ns, wl, cl := p["namespace"], p["workload"], p["cluster"]
		if ns == "" && wl == "" {
			// Fall back to the composite name "cluster/namespace/workload".
			parts := strings.Split(a.GetName(), "/")
			if len(parts) == 3 {
				cl, ns, wl = parts[0], parts[1], parts[2]
			}
		}
		if ns != "" {
			namespaces[ns] = true
		}
		if cl != "" {
			clusters[cl] = true
		}
		if wl != "" {
			workloads = append(workloads, workloadRef{cluster: cl, namespace: ns, name: wl, cost: a.GetTotalCost()})
		}
	}
	sort.SliceStable(workloads, func(i, j int) bool {
		if workloads[i].cost != workloads[j].cost {
			return workloads[i].cost > workloads[j].cost
		}
		return workloads[i].namespace+"/"+workloads[i].name < workloads[j].namespace+"/"+workloads[j].name
	})

	// Workloads: exact name, or the distinctive first segment ("checkout"
	// → checkout-api). Exact beats partial; cost breaks ties.
	best, bestScore := workloadRef{}, 0
	for _, w := range workloads {
		score := 0
		lname := strings.ToLower(w.name)
		switch {
		case tokSet[lname]:
			score = 3
		case tokSet[trimSuffixes(lname)] && len(trimSuffixes(lname)) >= 4:
			score = 2
		case len(strings.SplitN(lname, "-", 2)[0]) >= 5 && tokSet[strings.SplitN(lname, "-", 2)[0]]:
			score = 1
		}
		if score > bestScore {
			best, bestScore = w, score
		}
	}
	if bestScore > 0 {
		s = subject{Cluster: best.cluster, Namespace: best.namespace, Workload: best.name}
	}
	if s.Namespace == "" {
		for _, ns := range sortedKeys(namespaces) {
			if tokSet[strings.ToLower(ns)] {
				s.Namespace = ns
				break
			}
		}
	}
	if s.Cluster == "" {
		for _, cl := range sortedKeys(clusters) {
			if tokSet[strings.ToLower(cl)] {
				s.Cluster = cl
				break
			}
		}
	}
	if s.Cluster == "" {
		s.Cluster = req.ClusterID
	}
	return s
}

func trimSuffixes(name string) string {
	for _, suf := range []string{"-api", "-service", "-svc", "-server", "-worker", "-app", "-deployment"} {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf)
		}
	}
	return name
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// finding is one grounded paragraph of the answer.
type finding struct {
	order    intent
	bullets  []string
	spoken   string
	evidence []*kuberov1.EvidenceItem
	actions  []*kuberov1.ProposedAction
}

type rulesRun struct {
	tb       *Toolbox
	req      Request
	subj     subject
	rows     []*kuberov1.Allocation
	findings []finding
	gaps     []string // tools that failed — said out loud, never hidden
	// costJumped is set when a significant spend increase was found;
	// errors and alerts are then checked as likely causes.
	costJumped bool
}

func (r *Rules) Investigate(ctx context.Context, req Request, emit Emitter) (*kuberov1.InvestigateResponse, error) {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	maxCalls := r.MaxCalls
	if maxCalls <= 0 {
		maxCalls = DefaultMaxCalls
	}
	run := &rulesRun{tb: NewToolbox(r.Backend, req, now, maxCalls, emit), req: req}

	intents := detectIntents(req.Question)
	emit.progress("Reading cost allocation to find what the question is about")
	if out, err := run.call(ctx, "get_cost_allocation", &allocationIn{Aggregate: []string{"cluster", "namespace", "workload"}, Limit: 25}); err == nil {
		run.rows = out.raw.(*kuberov1.GetAllocationResponse).GetAllocations()
	}
	run.subj = resolveSubject(req, run.rows)
	emit.progress("Investigating " + run.subj.String())

	if run.subj.Workload != "" {
		run.workloadHistory(ctx)
	}
	for _, in := range intents {
		switch in {
		case intentAlerts:
			run.alerts(ctx)
		case intentCost:
			run.cost(ctx)
		case intentErrors:
			run.errors(ctx)
		case intentPerf:
			run.performance(ctx)
		case intentMemory, intentRightsize:
			if in == intentRightsize || !hasIntent(intents, intentRightsize) {
				run.rightsizing(ctx, in == intentMemory || hasIntent(intents, intentMemory))
			}
		case intentNetwork:
			run.network(ctx)
		}
	}
	// A spend jump is usually a symptom: retries, scale-outs and error
	// storms cost money. Look for the cause the question didn't name.
	if run.costJumped && !hasIntent(intents, intentErrors) {
		emit.progress("Spend jumped — checking errors and alerts for a cause")
		if !hasIntent(intents, intentAlerts) {
			run.alerts(ctx)
		}
		run.errors(ctx)
	}
	emit.progress("Writing the answer")
	return run.answer(), nil
}

func hasIntent(in []intent, want intent) bool {
	for _, i := range in {
		if i == want {
			return true
		}
	}
	return false
}

// call runs one tool with a typed input, noting failures as gaps.
func (r *rulesRun) call(ctx context.Context, name string, in toolInput) (*toolOutput, error) {
	if err := in.normalize(r.req); err != nil {
		return nil, err
	}
	out, err := r.tb.invoke(ctx, name, in)
	if err != nil && !errors.Is(err, ErrBudget) {
		r.gaps = append(r.gaps, fmt.Sprintf("%s failed: %s", name, truncateRunes(err.Error(), 160)))
	}
	return out, err
}

func (r *rulesRun) nsFilter() map[string]string {
	if r.subj.Namespace == "" {
		return nil
	}
	return map[string]string{"namespace": r.subj.Namespace}
}

// ─── analyses ────────────────────────────────────────────────────────────

func (r *rulesRun) workloadHistory(ctx context.Context) {
	out, err := r.call(ctx, "get_workload", &workloadIn{Cluster: r.subj.Cluster, Namespace: r.subj.Namespace, Name: r.subj.Workload})
	if err != nil {
		return
	}
	res := out.raw.(*kuberov1.GetWorkloadResponse)
	f := finding{order: intentAlerts}
	for _, h := range capSlice(res.GetHistory(), 3) {
		f.bullets = append(f.bullets, fmt.Sprintf("Recent change on %s: **%s** (%s, %s) at %s.",
			r.subj, h.GetAction(), h.GetPolicy(), h.GetOutcome(), h.GetAt()))
		f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
			Kind: "event", Title: h.GetAction(), Detail: fmt.Sprintf("%s · %s · %s", h.GetPolicy(), h.GetOutcome(), h.GetAt()),
			LinkPath: fmt.Sprintf("/workloads/%s/%s/%s", nonEmpty(r.subj.Cluster, "fleet"), r.subj.Namespace, r.subj.Workload),
		})
	}
	if len(f.bullets) > 0 {
		f.spoken = fmt.Sprintf("The most recent change to %s was %s.", r.subj.Workload, res.GetHistory()[0].GetAction())
		r.findings = append(r.findings, f)
	}
}

func (r *rulesRun) alerts(ctx context.Context) {
	out, err := r.call(ctx, "list_alerts", &alertsIn{State: "firing", Limit: 20})
	if err != nil {
		return
	}
	res := out.raw.(*kuberov1.ListAlertsResponse)
	f := finding{order: intentAlerts}
	var names []string
	for _, a := range res.GetAlerts() {
		if a.GetState() != "firing" {
			continue
		}
		if ns := a.GetLabels()["namespace"]; r.subj.Namespace != "" && ns != "" && ns != r.subj.Namespace {
			continue
		}
		names = append(names, a.GetRuleName())
		f.bullets = append(f.bullets, fmt.Sprintf("Alert **%s** (%s) is firing: %s (since %s).",
			a.GetRuleName(), a.GetSeverity(), a.GetSummary(), nonEmpty(a.GetFiredAt(), a.GetStartedAt())))
		f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
			Kind: "alert", Title: a.GetRuleName() + " firing", Detail: a.GetSummary(), LinkPath: nonEmpty(a.GetLinkPath(), "/alerts"),
		})
	}
	if len(names) == 0 {
		f.bullets = append(f.bullets, fmt.Sprintf("No alerts are firing for %s.", r.subj))
	} else {
		f.spoken = fmt.Sprintf("%s %s firing.", strings.Join(names, " and "), pluralVerb(len(names), "is", "are"))
	}
	r.findings = append(r.findings, f)
}

func (r *rulesRun) cost(ctx context.Context) {
	f := finding{order: intentCost}
	groupBy, filters := "namespace", map[string]string(nil)
	if r.subj.Workload != "" {
		groupBy, filters = "workload", r.nsFilter()
	} else if r.subj.Namespace != "" {
		filters = r.nsFilter()
	}
	if out, err := r.call(ctx, "get_cost_timeseries", &timeseriesIn{GroupBy: groupBy, Filters: filters, Top: 5}); err == nil {
		res := out.raw.(*kuberov1.GetCostTimeseriesResponse)
		in := out.compactInput()
		var target *SeriesChange
		var changes []SeriesChange
		for _, s := range res.GetSeries() {
			changes = append(changes, seriesChange(s))
		}
		for i := range changes {
			c := &changes[i]
			if name := labelSummary(c.Labels); (r.subj.Workload != "" && name == r.subj.Workload) ||
				(r.subj.Workload == "" && r.subj.Namespace != "" && name == r.subj.Namespace) {
				target = c
				break
			}
		}
		if target == nil {
			// No named subject: the biggest mover is the story.
			for i := range changes {
				if target == nil || math.Abs(changes[i].ChangePct) > math.Abs(target.ChangePct) {
					target = &changes[i]
				}
			}
		}
		if target != nil && target.Baseline > 0 {
			name := labelSummary(target.Labels)
			stepHours := 1.0
			if in == "1d" {
				stepHours = 24
			}
			link := "/allocation?aggregate=" + groupBy + "&window=" + url.QueryEscape(windowOf(out))
			switch {
			case target.ChangePct >= 15:
				r.costJumped = true
				delta := (target.Recent - target.Baseline) / stepHours * 730
				f.bullets = append(f.bullets, fmt.Sprintf("Spend for **%s** rose **%.0f%%** over the last %s (%s → %s per %s) — about %s/mo more if it holds.",
					name, target.ChangePct, spanName(target.RecentPoints, stepHours), usd(target.Baseline), usd(target.Recent), stepName(stepHours), usd(delta)))
				f.spoken = fmt.Sprintf("Spend for %s rose about %.0f percent over the last %s.", name, target.ChangePct, spanName(target.RecentPoints, stepHours))
				f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
					Kind: "cost", Title: fmt.Sprintf("%s spend +%.0f%%", name, target.ChangePct),
					Detail:   fmt.Sprintf("%s → %s per %s (window %s)", usd(target.Baseline), usd(target.Recent), stepName(stepHours), windowOf(out)),
					LinkPath: link,
				})
				f.actions = append(f.actions, &kuberov1.ProposedAction{
					Id: "act-inv-spend-" + rulesbrain.Slug(name), Title: fmt.Sprintf("Review the %s spend increase", name),
					ImpactMonthlyUsd: math.Max(0, delta), Risk: "low", Kind: brain.KindInvestigate,
					Target:    targetOf(r.subj, name),
					Rationale: fmt.Sprintf("Spend is up %.0f%% versus the earlier part of the window; confirm the cause before it becomes the new baseline.", target.ChangePct),
				})
			case target.ChangePct <= -15:
				f.bullets = append(f.bullets, fmt.Sprintf("Spend for **%s** fell %.0f%% over the last %s (%s → %s per %s).",
					name, -target.ChangePct, spanName(target.RecentPoints, stepHours), usd(target.Baseline), usd(target.Recent), stepName(stepHours)))
			default:
				f.bullets = append(f.bullets, fmt.Sprintf("Spend for **%s** is steady (%+.0f%% recently, %s per %s).",
					name, target.ChangePct, usd(target.Recent), stepName(stepHours)))
			}
		}
	}
	for _, a := range r.rows {
		p := a.GetProperties()
		if r.subj.Workload != "" && p["workload"] == r.subj.Workload && p["namespace"] == r.subj.Namespace {
			f.bullets = append(f.bullets, fmt.Sprintf("%s cost %s over the window; CPU efficiency %.0f%%, memory %.0f%% (usage ÷ request).",
				r.subj, usd(a.GetTotalCost()), 100*a.GetCpuEfficiency(), 100*a.GetRamEfficiency()))
			break
		}
	}
	if out, err := r.call(ctx, "list_anomalies", &anomaliesIn{Limit: 10}); err == nil {
		for _, a := range out.raw.(*kuberov1.ListAnomaliesResponse).GetAnomalies() {
			if r.subj.Namespace != "" && a.GetSubject() != r.subj.Namespace && a.GetSubject() != r.subj.Workload {
				continue
			}
			f.bullets = append(f.bullets, fmt.Sprintf("Anomaly: **%s** — %s (~%s/mo).", a.GetTitle(), a.GetDetail(), usd(a.GetImpactUsdMonth())))
			f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
				Kind: "anomaly", Title: a.GetTitle(), Detail: a.GetDetail(), LinkPath: a.GetLinkPath(),
			})
		}
	}
	if len(f.bullets) > 0 {
		r.findings = append(r.findings, f)
	}
}

func (r *rulesRun) errors(ctx context.Context) {
	f := finding{order: intentErrors}
	query := `{level="error"}`
	groupBy := "namespace"
	if r.subj.Namespace != "" {
		query = fmt.Sprintf(`{namespace=%q, level="error"}`, r.subj.Namespace)
		groupBy = "workload"
	}
	ns := r.subj.Namespace
	if out, err := r.call(ctx, "get_log_volume", &logVolumeIn{Query: query, GroupBy: groupBy}); err == nil {
		res := out.raw.(*kuberov1.GetLogVolumeResponse)
		var total int64
		var worst *kuberov1.Series
		worstRatio := 0.0
		for _, s := range res.GetSeries() {
			lines, ratio := seriesSpikeOf(s)
			total += lines
			if ratio > worstRatio {
				worst, worstRatio = s, ratio
			}
		}
		if ns == "" && worst != nil && worstRatio >= 3 {
			ns = worst.GetLabels()["namespace"]
		}
		switch {
		case total == 0:
			f.bullets = append(f.bullets, fmt.Sprintf("No error-level log lines for %s in the window.", r.subj))
		case worstRatio >= 3:
			who := labelSummary(worst.GetLabels())
			f.bullets = append(f.bullets, fmt.Sprintf("Error logs are spiking: **%s** is running **%.1fx** its earlier rate (%s error lines in the window).",
				who, worstRatio, count(total)))
			f.spoken = fmt.Sprintf("Error logs in %s are running about %.0f times their earlier rate.", who, worstRatio)
		default:
			f.bullets = append(f.bullets, fmt.Sprintf("Error volume is steady: %s error lines in the window, no spike.", count(total)))
		}
		if total > 0 {
			f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
				Kind: "logs", Title: "Error log volume", Detail: fmt.Sprintf("%s lines; worst spike %.1fx", count(total), worstRatio),
				LinkPath: "/logs?query=" + url.QueryEscape(query), Query: query,
			})
		}
	}
	if ns != "" {
		pq := fmt.Sprintf(`{namespace=%q}`, ns)
		if out, err := r.call(ctx, "get_log_patterns", &logPatternsIn{Query: pq, Limit: 10}); err == nil {
			n := 0
			for _, p := range out.raw.(*kuberov1.GetLogPatternsResponse).GetPatterns() {
				if (p.GetLevel() != "error" && p.GetLevel() != "warn") || n >= 3 {
					continue
				}
				n++
				f.bullets = append(f.bullets, fmt.Sprintf("Top %s pattern in %s: `%s` ×%s (e.g. %q).",
					p.GetLevel(), ns, p.GetPattern(), count(p.GetCount()), truncateRunes(p.GetSample(), 120)))
				f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
					Kind: "logs", Title: truncateRunes(p.GetPattern(), 120),
					Detail:   fmt.Sprintf("%s lines (%.0f%% of %s), level %s", count(p.GetCount()), p.GetSharePct(), ns, p.GetLevel()),
					LinkPath: "/logs?query=" + url.QueryEscape(pq), Query: pq,
				})
				if n == 1 && f.spoken != "" {
					f.spoken += fmt.Sprintf(" The top pattern is %s.", spokenPattern(p.GetPattern()))
				}
			}
		}
	}
	if len(f.bullets) > 0 {
		r.findings = append(r.findings, f)
	}
}

func seriesSpikeOf(s *kuberov1.Series) (int64, float64) {
	sh := source.SeriesShift(s.GetPoints())
	return int64(sh.Total), sh.Ratio()
}

func (r *rulesRun) performance(ctx context.Context) {
	f := finding{order: intentPerf}
	service, ns := r.subj.Workload, r.subj.Namespace
	if service == "" {
		// No named service: profile the most expensive workload in scope.
		for _, a := range r.rows {
			p := a.GetProperties()
			if p["workload"] != "" && (ns == "" || p["namespace"] == ns) {
				service, ns = p["workload"], p["namespace"]
				break
			}
		}
	}
	if service == "" {
		f.bullets = append(f.bullets, "Name a service to profile — no workload in scope was found in the allocation data.")
		r.findings = append(r.findings, f)
		return
	}
	link := "/profiles?service=" + url.QueryEscape(service)
	if out, err := r.call(ctx, "get_top_functions", &topFunctionsIn{profileIn: profileIn{Service: service, Namespace: ns}, Limit: 5}); err == nil {
		res := out.raw.(*kuberov1.GetTopFunctionsResponse)
		for i, fn := range capSlice(res.GetFunctions(), 3) {
			f.bullets = append(f.bullets, fmt.Sprintf("In %s, `%s` takes **%.0f%%** of CPU (self) — ~%s/mo.",
				service, fn.GetName(), fn.GetSelfPct(), usd(fn.GetSelfCostUsdMonth())))
			f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
				Kind: "profile", Title: fn.GetName(), Detail: fmt.Sprintf("%.1f%% self, %.1f%% total CPU in %s", fn.GetSelfPct(), fn.GetTotalPct(), service),
				LinkPath: link,
			})
			if i == 0 {
				f.spoken = fmt.Sprintf("The hottest function in %s is %s at about %.0f percent of CPU.", service, spokenFunc(fn.GetName()), fn.GetSelfPct())
			}
		}
	}
	if out, err := r.call(ctx, "get_flamegraph_summary", &flamegraphIn{profileIn: profileIn{Service: service, Namespace: ns}, ComparePrevious: true, MaxPaths: 3}); err == nil {
		res := out.raw.(*kuberov1.GetFlamegraphResponse)
		if res.GetBaselineTotal() > 0 {
			if hps := hotPaths(res, true, 1); len(hps) > 0 && hps[0].DeltaPct >= 5 {
				f.bullets = append(f.bullets, fmt.Sprintf("Versus the previous window, the path `%s` grew **%+.0f points** of CPU share — a regression.",
					hps[0].Path, hps[0].DeltaPct))
				f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
					Kind: "profile", Title: "CPU regression vs previous window", Detail: hps[0].Path, LinkPath: link,
				})
			}
		}
	}
	if len(f.bullets) > 0 {
		r.findings = append(r.findings, f)
	}
}

func (r *rulesRun) rightsizing(ctx context.Context, memoryFocus bool) {
	f := finding{order: intentRightsize}
	if memoryFocus {
		f.order = intentMemory
	}
	out, err := r.call(ctx, "list_rightsizing", &rightsizingIn{Namespace: r.subj.Namespace, Limit: 20})
	if err != nil {
		return
	}
	res := out.raw.(*kuberov1.ListRightsizingResponse)
	link := "/rightsizing"
	if r.subj.Namespace != "" {
		link += "?namespace=" + url.QueryEscape(r.subj.Namespace)
	}
	proposed := map[string]bool{}
	for _, rec := range res.GetRecommendations() {
		if r.subj.Workload != "" && rec.GetWorkload() != r.subj.Workload {
			continue
		}
		where := rec.GetNamespace() + "/" + rec.GetWorkload()
		switch {
		case rec.GetOomKills() > 0:
			f.bullets = append(f.bullets, fmt.Sprintf("**%s** (%s) was OOM-killed %d times in the window — memory should go up, not down.",
				where, rec.GetContainer(), rec.GetOomKills()))
			f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
				Kind: "rightsizing", Title: fmt.Sprintf("%s: %d OOM kills", where, rec.GetOomKills()), Detail: rec.GetReason(), LinkPath: link,
			})
			f.actions = append(f.actions, &kuberov1.ProposedAction{
				Id: "act-inv-oom-" + rulesbrain.Slug(where), Title: "Raise memory for " + where,
				Risk: "medium", Kind: brain.KindInvestigate, Target: targetOf(r.subj, where),
				Rationale: fmt.Sprintf("%d OOM kills in the window; size memory up before anything else. Recommender: %s", rec.GetOomKills(), rec.GetReason()),
			})
			if f.spoken == "" {
				f.spoken = fmt.Sprintf("%s has been OOM-killed %d times.", rec.GetWorkload(), rec.GetOomKills())
			}
		case !memoryFocus && rec.GetSavingsUsdMonth() > 0:
			f.bullets = append(f.bullets, fmt.Sprintf("**%s** (%s) is over-provisioned: %s — ~%s/mo recoverable (%s confidence).",
				where, rec.GetContainer(), rec.GetReason(), usd(rec.GetSavingsUsdMonth()), rec.GetConfidence()))
			f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
				Kind: "rightsizing", Title: fmt.Sprintf("%s over-provisioned", where),
				Detail: fmt.Sprintf("%s; %s/mo", rec.GetReason(), usd(rec.GetSavingsUsdMonth())), LinkPath: link,
			})
			if (rec.GetConfidence() == "high" || rec.GetConfidence() == "medium") && !proposed[where] {
				proposed[where] = true
				f.actions = append(f.actions, &kuberov1.ProposedAction{
					Id: "act-rs-" + rulesbrain.Slug(where), Title: fmt.Sprintf("Rightsize %s in %s", rec.GetWorkload(), rec.GetNamespace()),
					ImpactMonthlyUsd: rec.GetSavingsUsdMonth(), Risk: "low", Kind: brain.KindRightsize, Target: targetOf(r.subj, where),
					Rationale: fmt.Sprintf("%s. A recommend-mode RightsizingPolicy surfaces the change without touching live pods.", strings.TrimSuffix(rec.GetReason(), ".")),
					CrdYaml:   rulesbrain.RightsizingYAML(rec.GetNamespace(), rec.GetWorkload()),
				})
			}
			if f.spoken == "" {
				f.spoken = fmt.Sprintf("%s is over-provisioned by about %s a month.", rec.GetWorkload(), spokenUSD(rec.GetSavingsUsdMonth()))
			}
		}
	}
	if len(f.bullets) == 0 {
		if memoryFocus {
			f.bullets = append(f.bullets, fmt.Sprintf("No OOM kills or memory pressure recorded for %s in the observation window.", r.subj))
		} else {
			f.bullets = append(f.bullets, fmt.Sprintf("No rightsizing opportunities for %s right now.", r.subj))
		}
	}
	r.findings = append(r.findings, f)
}

func (r *rulesRun) network(ctx context.Context) {
	f := finding{order: intentNetwork}
	link := "/network"
	if r.subj.Namespace != "" {
		link += "?namespace=" + url.QueryEscape(r.subj.Namespace)
	}
	if out, err := r.call(ctx, "list_network_costs", &networkCostsIn{Limit: 10}); err == nil {
		res := out.raw.(*kuberov1.ListNetworkCostsResponse)
		n := 0
		for _, c := range res.GetCosts() {
			if (r.subj.Namespace != "" && c.GetNamespace() != r.subj.Namespace) || n >= 3 {
				continue
			}
			n++
			f.bullets = append(f.bullets, fmt.Sprintf("**%s/%s** spends %s/mo on network (egress %s, cross-zone %s), mostly to %s.",
				c.GetNamespace(), c.GetWorkload(), usd(c.GetTotalUsdMonth()), usd(c.GetEgressUsdMonth()), usd(c.GetCrossZoneUsdMonth()), c.GetTopDestination()))
			f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
				Kind: "network", Title: fmt.Sprintf("%s/%s network %s/mo", c.GetNamespace(), c.GetWorkload(), usd(c.GetTotalUsdMonth())),
				Detail: "top destination " + c.GetTopDestination(), LinkPath: link,
			})
			if n == 1 {
				f.spoken = fmt.Sprintf("The biggest network spender is %s at about %s a month.", c.GetWorkload(), spokenUSD(c.GetTotalUsdMonth()))
			}
		}
	}
	if out, err := r.call(ctx, "get_service_map", &serviceMapIn{Namespace: r.subj.Namespace, MaxEdges: 10}); err == nil {
		for _, e := range out.raw.(*kuberov1.GetServiceMapResponse).GetEdges() {
			if e.GetCrossZone() && e.GetRetransmits() >= 1000 {
				f.bullets = append(f.bullets, fmt.Sprintf("The cross-zone edge %s → %s shows %.0f TCP retransmits — connections are struggling, which drives retries.",
					e.GetSource(), e.GetTarget(), e.GetRetransmits()))
				f.evidence = append(f.evidence, &kuberov1.EvidenceItem{
					Kind: "network", Title: "Retransmits " + e.GetSource() + " → " + e.GetTarget(),
					Detail: fmt.Sprintf("%.0f retransmits, %s/mo, cross-zone", e.GetRetransmits(), usd(e.GetCostUsdMonth())), LinkPath: link,
				})
				break
			}
		}
	}
	if len(f.bullets) > 0 {
		r.findings = append(r.findings, f)
	}
}

// ─── answer ──────────────────────────────────────────────────────────────

func (r *rulesRun) answer() *kuberov1.InvestigateResponse {
	sort.SliceStable(r.findings, func(i, j int) bool { return r.findings[i].order < r.findings[j].order })
	var b strings.Builder
	fmt.Fprintf(&b, "### What I found about %s\n\n", r.subj)
	fmt.Fprintf(&b, "_Question: %s — window %s._\n\n", r.req.Question, r.req.Window)

	var evidence []*kuberov1.EvidenceItem
	var actions []*kuberov1.ProposedAction
	var spoken []string
	for _, f := range r.findings {
		for _, bl := range f.bullets {
			b.WriteString("- " + bl + "\n")
		}
		evidence = append(evidence, f.evidence...)
		actions = append(actions, f.actions...)
		if f.spoken != "" {
			spoken = append(spoken, f.spoken)
		}
	}
	if len(r.findings) == 0 {
		b.WriteString("- I couldn't find anything unusual in the signals I checked.\n")
	}
	if len(r.gaps) > 0 {
		b.WriteString("\n**Couldn't check:** " + strings.Join(r.gaps, "; ") + "\n")
	}
	for _, a := range actions {
		a.Status = brain.StatusProposed
	}
	actions = brain.ValidateActions(actions)
	if len(actions) > 0 {
		b.WriteString("\n### Proposed actions\n\n")
		b.WriteString("Proposals only — manifests apply through the operator's human-arm flow.\n\n")
		for _, a := range actions {
			fmt.Fprintf(&b, "1. **%s** (%s, risk %s) — %s\n", a.Title, a.Kind, a.Risk, a.Rationale)
		}
	}

	summary := fmt.Sprintf("I looked into %s.", spokenSubject(r.subj))
	if len(spoken) == 0 {
		summary += " Nothing stood out in the signals I checked."
	} else {
		summary += " " + strings.Join(capSlice(spoken, 4), " ")
	}
	if len(actions) > 0 {
		summary += fmt.Sprintf(" I've queued %d %s for your review; nothing runs without your sign-off.",
			len(actions), plural(len(actions), "proposal", "proposals"))
	}

	return &kuberov1.InvestigateResponse{
		AnswerMarkdown: strings.TrimSpace(b.String()),
		SpokenSummary:  summary,
		Evidence:       capSlice(evidence, 20),
		Actions:        actions,
		Steps:          r.tb.Steps(),
		Source:         "rules",
	}
}

// ─── small helpers ───────────────────────────────────────────────────────

// compactInput returns the step granularity a timeseries call used.
func (o *toolOutput) compactInput() string {
	if m, ok := o.compact.(map[string]any); ok {
		if s, ok := m["step"].(string); ok {
			return s
		}
	}
	return "1h"
}

func windowOf(o *toolOutput) string {
	if m, ok := o.compact.(map[string]any); ok {
		if s, ok := m["window"].(string); ok {
			return s
		}
	}
	return ""
}

func stepName(hours float64) string {
	if hours >= 24 {
		return "day"
	}
	return "hour"
}

func targetOf(s subject, name string) string {
	cl := nonEmpty(s.Cluster, "fleet")
	if strings.Contains(name, "/") {
		return cl + "/" + name
	}
	if s.Namespace != "" && name != s.Namespace {
		return cl + "/" + s.Namespace + "/" + name
	}
	return cl + "/" + name
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func pluralVerb(n int, one, many string) string { return plural(n, one, many) }

func count(n int64) string { return strings.TrimPrefix(usd(float64(n)), "$") }

func spokenUSD(v float64) string { return strings.TrimPrefix(usd(v), "$") + " dollars" }

func spokenSubject(s subject) string {
	switch {
	case s.Workload != "":
		return s.Workload + " in the " + s.Namespace + " namespace"
	case s.Namespace != "":
		return "the " + s.Namespace + " namespace"
	case s.Cluster != "":
		return "cluster " + s.Cluster
	}
	return "the fleet"
}

// spokenPattern turns a log template into something a voice can read.
func spokenPattern(p string) string {
	p = strings.ReplaceAll(p, "<_>ms", "<_> milliseconds")
	p = strings.ReplaceAll(p, "<_>", "…")
	return `"` + truncateRunes(p, 80) + `"`
}

// spokenFunc turns "crypto/tls.(*Conn).Handshake" into "tls Conn Handshake".
func spokenFunc(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.NewReplacer("(*", "", "(", "", ")", "", "*", "", ".", " ").Replace(name)
	return truncateRunes(strings.Join(strings.Fields(name), " "), 60)
}

// spanName renders a trailing window like "6 hours" or "3 days".
func spanName(points int, stepHours float64) string {
	h := float64(points) * stepHours
	if h >= 48 {
		return fmt.Sprintf("%.0f days", h/24)
	}
	return fmt.Sprintf("%.0f hours", h)
}
