// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

var allocDims = map[string]bool{
	"cluster": true, "namespace": true, "workload": true, "controller": true, "pod": true, "container": true,
	"node": true, "nodepool": true, "team": true, "cost_center": true, "zone": true,
}

func validDim(d string) bool {
	return allocDims[d] || strings.HasPrefix(d, "label:") && len(d) > len("label:")
}

// parseFilters turns repeated --filter k=v into a map.
func parseFilters(fs []string) (map[string]string, error) {
	if len(fs) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, f := range fs {
		k, v, ok := strings.Cut(f, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("--filter %q must look like key=value", f)
		}
		if !validDim(k) {
			return nil, fmt.Errorf("--filter key %q is not a cost dimension", k)
		}
		out[k] = v
	}
	return out, nil
}

func costCmd() *cobra.Command {
	var af allocationFlags
	c := &cobra.Command{
		Use:   "cost",
		Short: "Cost allocation, spend over time, efficiency and FOCUS export",
		Long: `Cost allocation (OpenCost-compatible) by any dimension, with idle and
shared-cost handling. "kubehero cost" alone is "kubehero cost allocation".`,
		Example: `  kubehero cost --aggregate namespace --window 7d
  kubehero cost allocation --aggregate namespace,workload --filter namespace=payments --idle
  kubehero cost timeseries --window 30d --group-by team
  kubehero cost export --format focus --window 30d -o focus.csv`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runAllocation(cmd, af) },
	}
	af.bind(c)
	c.AddCommand(costAllocationCmd(), costTimeseriesCmd(), costExportCmd(), costEfficiencyCmd())
	return c
}

type allocationFlags struct {
	aggregate, window, shareIdle, cluster string
	idle                                  bool
	filters, shared                       []string
	limit                                 int
}

func (a *allocationFlags) bind(c *cobra.Command) {
	f := c.Flags()
	f.StringVar(&a.aggregate, "aggregate", "namespace", "Group by (comma-separated): cluster, namespace, workload, controller, pod, container, node, nodepool, team, cost_center, zone, label:<key>")
	f.StringVar(&a.window, "window", "7d", `Window: 24h | 7d | 30d | today | yesterday | week | month | lastmonth | "<RFC3339>,<RFC3339>"`)
	f.BoolVar(&a.idle, "idle", false, "Include an __idle__ row per cluster")
	f.StringVar(&a.shareIdle, "share-idle", "", "Share idle cost back: weighted | even")
	f.StringSliceVar(&a.filters, "filter", nil, "Exact-match filter key=value (repeatable)")
	f.StringSliceVar(&a.shared, "shared-namespaces", nil, "Namespaces whose cost is redistributed to the rest")
	f.StringVar(&a.cluster, "cluster", "", "Cluster id")
	f.IntVar(&a.limit, "limit", 0, "Show at most N rows (0 = all)")
}

func costAllocationCmd() *cobra.Command {
	var af allocationFlags
	c := &cobra.Command{
		Use:   "allocation",
		Short: "Cost by dimension with CPU/RAM/GPU/network/idle breakdown and efficiency",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runAllocation(cmd, af) },
	}
	af.bind(c)
	return c
}

func runAllocation(cmd *cobra.Command, af allocationFlags) error {
	var dims []string
	for _, d := range strings.Split(af.aggregate, ",") {
		if d = strings.TrimSpace(d); d != "" {
			if !validDim(d) {
				return fmt.Errorf("--aggregate %q is not a cost dimension", d)
			}
			dims = append(dims, d)
		}
	}
	switch af.shareIdle {
	case "", "weighted", "even":
	default:
		return errors.New("--share-idle must be weighted or even")
	}
	filters, err := parseFilters(af.filters)
	if err != nil {
		return err
	}
	cl, cfg, err := signalClients()
	if err != nil {
		return err
	}
	ctx, cancel := unaryCtx(cmd)
	defer cancel()
	res, err := cl.Cost.GetAllocation(ctx, connect.NewRequest(&kuberov1.GetAllocationRequest{
		Window: af.window, Aggregate: dims, Filters: filters, IncludeIdle: af.idle,
		ShareIdle: af.shareIdle, SharedNamespaces: af.shared, ClusterId: af.cluster,
	}))
	if err != nil {
		return err
	}
	allocs := res.Msg.GetAllocations()
	if af.limit > 0 && len(allocs) > af.limit {
		allocs = allocs[:af.limit]
	}
	rows := make([]*khfmt.Row, 0, len(allocs))
	for _, a := range allocs {
		rows = append(rows, khfmt.NewRow().
			Set("name", a.GetName()).
			Set("total", usd(a.GetTotalCost())).
			Set("cpu eff", pct(a.GetCpuEfficiency())).
			Set("ram eff", pct(a.GetRamEfficiency())).
			Set("recoverable", usd(a.GetRecoverableCost())).
			Set("cpu", usd(a.GetCpuCost())).
			Set("ram", usd(a.GetRamCost())).
			Set("gpu", usd(a.GetGpuCost())).
			Set("network", usd(a.GetNetworkCost())).
			Set("idle", usd(a.GetIdleCost())).
			Set("shared", usd(a.GetSharedCost())))
	}
	if err := render(cmd, cfg, rows, res.Msg); err != nil {
		return err
	}
	if !structuredOutput(cfg) {
		t := res.Msg.GetTotals()
		fmt.Fprintf(cmd.OutOrStdout(), "\ntotal %s over %s · efficiency %s · recoverable %s%s\n",
			usd(t.GetTotalCost()), af.window, pct(t.GetTotalEfficiency()), usd(t.GetRecoverableCost()), sourceNote(res.Msg.GetSource()))
	}
	return nil
}

func sourceNote(src string) string {
	if src == "demo" {
		return " · DEMO DATA"
	}
	return ""
}

func costTimeseriesCmd() *cobra.Command {
	var window, step, groupBy, cluster string
	var top int
	var filters []string
	c := &cobra.Command{
		Use:     "timeseries",
		Short:   "Spend per step, optionally grouped, with a month-end forecast",
		Example: `  kubehero cost timeseries --window 30d --step 1d --group-by namespace --top 5`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch groupBy {
			case "", "namespace", "team", "cluster", "nodepool", "workload":
			default:
				return errors.New("--group-by must be namespace, team, cluster, nodepool or workload")
			}
			switch step {
			case "", "1h", "1d":
			default:
				return errors.New("--step must be 1h or 1d")
			}
			fm, err := parseFilters(filters)
			if err != nil {
				return err
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Cost.GetCostTimeseries(ctx, connect.NewRequest(&kuberov1.GetCostTimeseriesRequest{
				Window: window, Step: step, GroupBy: groupBy, Filters: fm, Top: int32(top), ClusterId: cluster,
			}))
			if err != nil {
				return err
			}
			rows := make([]*khfmt.Row, 0, len(res.Msg.GetSeries()))
			for _, s := range res.Msg.GetSeries() {
				var total, last float64
				for _, p := range s.GetPoints() {
					total += p.GetValue()
					last = p.GetValue()
				}
				rows = append(rows, khfmt.NewRow().
					Set("series", nonEmptyStr(labelString(s.GetLabels()), "total")).
					Set("total", usd(total)).
					Set("last step", usd(last)).
					Set("trend", sparkline(s.GetPoints())))
			}
			if err := render(cmd, cfg, rows, res.Msg); err != nil {
				return err
			}
			if !structuredOutput(cfg) {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s over %s · month forecast %s%s\n",
					usd(res.Msg.GetTotalUsd()), nonEmptyStr(window, "30d"), usd(res.Msg.GetForecastMonthUsd()), sourceNote(res.Msg.GetSource()))
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&window, "window", "30d", "Window")
	f.StringVar(&step, "step", "", "Resolution: 1h | 1d (default auto)")
	f.StringVar(&groupBy, "group-by", "", "namespace | team | cluster | nodepool | workload (default: total)")
	f.IntVar(&top, "top", 5, "Keep the N largest groups")
	f.StringSliceVar(&filters, "filter", nil, "Exact-match filter key=value (repeatable)")
	f.StringVar(&cluster, "cluster", "", "Cluster id")
	return c
}

func costEfficiencyCmd() *cobra.Command {
	var window, cluster string
	c := &cobra.Command{
		Use:   "efficiency",
		Short: "Fleet efficiency score with per-cluster and per-namespace breakdown",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Cost.GetEfficiency(ctx, connect.NewRequest(&kuberov1.GetEfficiencyRequest{ClusterId: cluster, Window: window}))
			if err != nil {
				return err
			}
			var rows []*khfmt.Row
			add := func(kind string, bs []*kuberov1.EfficiencyBreakdown) {
				for _, b := range bs {
					rows = append(rows, khfmt.NewRow().
						Set("scope", kind+"/"+b.GetName()).
						Set("score", fmt.Sprintf("%.0f", b.GetScore())).
						Set("cpu eff", pct(b.GetCpuEfficiency())).
						Set("ram eff", pct(b.GetRamEfficiency())).
						Set("idle $/mo", usd(b.GetIdleCostUsdMonth())).
						Set("total $/mo", usd(b.GetTotalCostUsdMonth())))
				}
			}
			add("cluster", res.Msg.GetClusters())
			add("namespace", res.Msg.GetNamespaces())
			if err := render(cmd, cfg, rows, res.Msg); err != nil {
				return err
			}
			if !structuredOutput(cfg) {
				m := res.Msg
				fmt.Fprintf(cmd.OutOrStdout(), "\nfleet score %.0f/100 · cpu %s · ram %s · idle %s/mo · recoverable %s/mo%s\n",
					m.GetScore(), pct(m.GetCpuEfficiency()), pct(m.GetRamEfficiency()), usd(m.GetIdleCostUsdMonth()),
					usd(m.GetRecoverableUsdMonth()), sourceNote(m.GetSource()))
			}
			return nil
		},
	}
	c.Flags().StringVar(&window, "window", "7d", "Window")
	c.Flags().StringVar(&cluster, "cluster", "", "Cluster id")
	return c
}

func costExportCmd() *cobra.Command {
	var format, window, aggregate, file string
	c := &cobra.Command{
		Use:   "export",
		Short: "Export allocated cost as a FinOps FOCUS 1.2 CSV",
		Long: `Download the FinOps FOCUS export (GET /api/v1/export/focus) as CSV.
Write it to a file with -o <file.csv> (the global -o doubles as the
destination here) or --file; with neither, the CSV goes to stdout.`,
		Example: `  kubehero cost export --format focus --window 30d -o focus.csv
  kubehero cost export --window lastmonth --aggregate namespace > focus.csv`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "focus" {
				return fmt.Errorf("--format %q is not supported (only focus)", format)
			}
			cfg := resolveConfig()
			dest := file
			switch o := strings.TrimSpace(cfg.Output); {
			case dest != "":
			case o == "" || o == "-" || o == "table":
			case o == "json" || o == "yaml" || o == "wide":
				return fmt.Errorf("the export is CSV; use -o <file.csv> or --file")
			default:
				dest = o
			}
			if dest != "" && !strings.EqualFold(filepath.Ext(dest), ".csv") && dest != "-" {
				return fmt.Errorf("refusing to write CSV to %q: use a .csv file name", dest)
			}
			cl, _, err := signalClientsFrom(cfg)
			if err != nil {
				return err
			}
			q := url.Values{}
			q.Set("window", window)
			if aggregate != "" {
				q.Set("aggregate", aggregate)
			}
			req, err := http.NewRequestWithContext(cmdContext(cmd), http.MethodGet,
				cl.Endpoint+"/api/v1/export/focus?"+q.Encode(), nil)
			if err != nil {
				return err
			}
			req.Header.Set("Accept", "text/csv")
			resp, err := cl.HTTP.Do(req)
			if err != nil {
				return fmt.Errorf("focus export: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return fmt.Errorf("focus export: %s · %s", resp.Status, strings.TrimSpace(string(msg)))
			}
			var w io.Writer = cmd.OutOrStdout()
			if dest != "" && dest != "-" {
				f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			n, err := io.Copy(w, resp.Body)
			if err != nil {
				return fmt.Errorf("focus export: %w", err)
			}
			if dest != "" && dest != "-" {
				fmt.Fprintf(cmd.ErrOrStderr(), "✓ wrote %s (%s)\n", dest, bytesIEC(float64(n)))
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&format, "format", "focus", "Export format (focus)")
	f.StringVar(&window, "window", "30d", "Window")
	f.StringVar(&aggregate, "aggregate", "workload", "Row granularity: workload | namespace | cluster")
	f.StringVar(&file, "file", "", "Destination file (alternative to -o)")
	return c
}

func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
