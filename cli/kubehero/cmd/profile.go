// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

func profileCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "profile",
		Short: "Continuous profiling: hottest functions and flamegraphs, priced",
	}
	c.AddCommand(profileTopCmd(), profileFlameCmd(), profileTargetsCmd())
	return c
}

type profileFlags struct {
	service, namespace, typ, since, cluster string
}

func (p *profileFlags) bind(c *cobra.Command) {
	f := c.Flags()
	f.StringVar(&p.service, "service", "", "Service / workload name (required)")
	f.StringVar(&p.namespace, "namespace", "", "Namespace, when the name is ambiguous")
	f.StringVar(&p.typ, "type", "cpu", "Profile type: cpu | alloc_space | alloc_objects | inuse_space | inuse_objects | goroutine | mutex | block")
	f.StringVar(&p.since, "since", "30m", "Lookback, e.g. 30m, 6h")
	f.StringVar(&p.cluster, "cluster", "", "Cluster id")
}

func (p *profileFlags) selector() (*kuberov1.ProfileSelector, error) {
	if strings.TrimSpace(p.service) == "" {
		return nil, errors.New("--service is required (see: kubehero profile targets)")
	}
	return &kuberov1.ProfileSelector{Service: p.service, Namespace: p.namespace, Type: p.typ, ClusterId: p.cluster}, nil
}

func profileTopCmd() *cobra.Command {
	var pf profileFlags
	var limit int
	var orderBy string
	c := &cobra.Command{
		Use:     "top",
		Short:   "Functions ranked by self (or total) time, with $/mo",
		Example: `  kubehero profile top --service checkout-api --since 30m`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sel, err := pf.selector()
			if err != nil {
				return err
			}
			if orderBy != "self" && orderBy != "total" {
				return errors.New("--order must be self or total")
			}
			start, end, err := timeRange(pf.since, "", "", time.Now())
			if err != nil {
				return err
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Profiles.GetTopFunctions(ctx, connect.NewRequest(&kuberov1.GetTopFunctionsRequest{
				Selector: sel, StartUnixMs: start, EndUnixMs: end, Limit: int32(limit), OrderBy: orderBy,
			}))
			if err != nil {
				return err
			}
			rows := make([]*khfmt.Row, 0, len(res.Msg.GetFunctions()))
			for _, f := range res.Msg.GetFunctions() {
				rows = append(rows, khfmt.NewRow().
					Set("self", pctVal(f.GetSelfPct())).
					Set("total", pctVal(f.GetTotalPct())).
					Set("$/mo", usd(f.GetSelfCostUsdMonth())).
					Set("function", f.GetName()))
			}
			return render(cmd, cfg, rows, res.Msg)
		},
	}
	pf.bind(c)
	c.Flags().IntVar(&limit, "limit", 20, "Max functions")
	c.Flags().StringVar(&orderBy, "order", "self", "Rank by self | total")
	return c
}

func profileFlameCmd() *cobra.Command {
	var pf profileFlags
	var diffSince string
	var depth int
	var minPct float64
	c := &cobra.Command{
		Use:   "flame",
		Short: "Top-down tree of the hottest call paths (ASCII flamegraph)",
		Long: `Render the service's flamegraph as a top-down tree of its hottest
paths. With --diff-since D the same-length window D earlier is the
baseline, and every frame shows its change in share of the total
(regressions in red).`,
		Example: `  kubehero profile flame --service checkout-api
  kubehero profile flame --service checkout-api --since 1h --diff-since 24h`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sel, err := pf.selector()
			if err != nil {
				return err
			}
			start, end, err := timeRange(pf.since, "", "", time.Now())
			if err != nil {
				return err
			}
			req := &kuberov1.GetFlamegraphRequest{Selector: sel, StartUnixMs: start, EndUnixMs: end, MaxNodes: 2048}
			if diffSince != "" {
				d, err := parseSince(diffSince)
				if err != nil {
					return fmt.Errorf("--diff-since: %w", err)
				}
				req.BaselineStartUnixMs = start - d.Milliseconds()
				req.BaselineEndUnixMs = end - d.Milliseconds()
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Profiles.GetFlamegraph(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if structuredOutput(cfg) {
				return render(cmd, cfg, nil, res.Msg)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s · %s · %s samples", pf.service, nonEmptyStr(res.Msg.GetType(), pf.typ), count(res.Msg.GetSamples()))
			if c := res.Msg.GetCostUsdMonth(); c > 0 {
				fmt.Fprintf(out, " · %s/mo", usd(c))
			}
			fmt.Fprintln(out)
			renderFlame(out, newPainter(out), res.Msg, flameOptions{maxDepth: depth, minPct: minPct, diff: diffSince != ""})
			return nil
		},
	}
	pf.bind(c)
	c.Flags().StringVar(&diffSince, "diff-since", "", "Compare against the same window this long ago, e.g. 24h")
	c.Flags().IntVar(&depth, "depth", 14, "Max tree depth")
	c.Flags().Float64Var(&minPct, "min-pct", 1, "Hide frames below this share of the total")
	return c
}

func profileTargetsCmd() *cobra.Command {
	var since, namespace, cluster string
	c := &cobra.Command{
		Use:   "targets",
		Short: "Services with profile data, their profile types and what they cost",
		RunE: func(cmd *cobra.Command, _ []string) error {
			start, end, err := timeRange(since, "", "", time.Now())
			if err != nil {
				return err
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Profiles.ListProfileTargets(ctx, connect.NewRequest(&kuberov1.ListProfileTargetsRequest{
				StartUnixMs: start, EndUnixMs: end, ClusterId: cluster, Namespace: namespace,
			}))
			if err != nil {
				return err
			}
			rows := make([]*khfmt.Row, 0, len(res.Msg.GetTargets()))
			for _, t := range res.Msg.GetTargets() {
				rows = append(rows, khfmt.NewRow().
					Set("service", t.GetService()).
					Set("namespace", t.GetNamespace()).
					Set("cpu", cores(t.GetCpuCoresAvg())).
					Set("$/mo", usd(t.GetCostUsdMonth())).
					Set("origin", t.GetOrigin()).
					Set("types", strings.Join(t.GetTypes(), ",")))
			}
			return render(cmd, cfg, rows, res.Msg)
		},
	}
	c.Flags().StringVar(&since, "since", "1h", "Lookback")
	c.Flags().StringVar(&namespace, "namespace", "", "Namespace filter")
	c.Flags().StringVar(&cluster, "cluster", "", "Cluster id")
	return c
}

type flameOptions struct {
	maxDepth int
	minPct   float64
	diff     bool
	barWidth int
}

// renderFlame prints the flamegraph as a top-down tree: children sorted
// by total, frames under minPct hidden, depth capped. In diff mode each
// frame shows its change in share of the total vs the baseline window.
func renderFlame(w io.Writer, paint painter, res *kuberov1.GetFlamegraphResponse, o flameOptions) {
	nodes := res.GetNodes()
	if len(nodes) == 0 || res.GetTotal() <= 0 {
		fmt.Fprintln(w, "(no samples in the window)")
		return
	}
	if o.barWidth <= 0 {
		o.barWidth = 24
	}
	children := make([][]int, len(nodes))
	for i, n := range nodes {
		if p := int(n.GetParent()); i > 0 && p >= 0 && p < len(nodes) {
			children[p] = append(children[p], i)
		}
	}
	total := float64(res.GetTotal())
	baseTotal := float64(res.GetBaselineTotal())
	share := func(i int) float64 { return 100 * float64(nodes[i].GetTotal()) / total }
	for i := range children {
		sort.SliceStable(children[i], func(a, b int) bool {
			return nodes[children[i][a]].GetTotal() > nodes[children[i][b]].GetTotal()
		})
	}

	var walk func(i, depth int, prefix string, last bool)
	walk = func(i, depth int, prefix string, last bool) {
		n := nodes[i]
		branch, childPrefix := "", ""
		if depth > 0 {
			branch, childPrefix = "├─ ", prefix+"│  "
			if last {
				branch, childPrefix = "└─ ", prefix+"   "
			}
		}
		s := share(i)
		bar := strings.Repeat("█", max(1, int(s/100*float64(o.barWidth))))
		line := fmt.Sprintf("%s%s%s %s", prefix, branch, n.GetName(), pctVal(s))
		if self := n.GetSelf(); self > 0 && depth > 0 {
			line += paint.dim(" self " + pctVal(100*float64(self)/total))
		}
		if o.diff && baseTotal > 0 {
			delta := s - 100*float64(n.GetBaselineTotal())/baseTotal
			tag := fmt.Sprintf(" %+.1fpp", delta)
			switch {
			case delta >= 0.5:
				tag = paint.red(tag)
			case delta <= -0.5:
				tag = paint.green(tag)
			default:
				tag = paint.dim(tag)
			}
			line += tag
		}
		fmt.Fprintf(w, "%s  %s\n", line, paint.dim(bar))
		if depth >= o.maxDepth {
			return
		}
		var kids []int
		for _, c := range children[i] {
			if share(c) >= o.minPct {
				kids = append(kids, c)
			}
		}
		for k, c := range kids {
			walk(c, depth+1, childPrefix, k == len(kids)-1)
		}
	}
	walk(0, 0, "", true)
}
