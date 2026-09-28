// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// rightsize lists measured recommendations. Applying them is the
// operator's job (RightsizingPolicy, human-armed); the CLI never patches
// workloads — `kubehero undo` is the only path back.
func rightsizeCmd() *cobra.Command {
	var rf rightsizeFlags
	c := &cobra.Command{
		Use:   "rightsize [workload]",
		Short: "Per-container rightsizing recommendations from measured usage",
		Long: `List rightsizing recommendations (CostService.ListRightsizing): current vs
recommended CPU / memory requests from usage percentiles, $/mo saving,
confidence and OOM kills. To act on them, create a RightsizingPolicy
(mode: recommend → shadow → apply); the operator applies changes only
when the policy is armed and every safety guard passes.`,
		Example: `  kubehero rightsize --namespace payments
  kubehero rightsize list --min-savings 100 --window 14d
  kubehero rightsize checkout-api --namespace payments -o yaml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runRightsize(cmd, rf, args) },
	}
	rf.bind(c)
	list := &cobra.Command{
		Use:   "list [workload]",
		Short: "List recommendations (same as `kubehero rightsize`)",
		Args:  cobra.MaximumNArgs(1),
	}
	var lf rightsizeFlags
	lf.bind(list)
	list.RunE = func(cmd *cobra.Command, args []string) error { return runRightsize(cmd, lf, args) }
	c.AddCommand(list)
	return c
}

type rightsizeFlags struct {
	namespace, window, percentile, cluster string
	headroom, minSavings                   float64
	limit                                  int
}

func (r *rightsizeFlags) bind(c *cobra.Command) {
	f := c.Flags()
	f.StringVar(&r.namespace, "namespace", "", "Namespace filter")
	f.StringVar(&r.window, "window", "7d", "Observation window")
	f.Float64Var(&r.headroom, "headroom", 0, "Headroom % over the sized percentile (default server-side: 15)")
	f.StringVar(&r.percentile, "percentile", "", "CPU percentile: p90 | p95 | p99 | max (default p95)")
	f.Float64Var(&r.minSavings, "min-savings", 0, "Only rows saving at least this many $/mo")
	f.IntVar(&r.limit, "limit", 50, "Max rows")
	f.StringVar(&r.cluster, "cluster", "", "Cluster id")
}

func runRightsize(cmd *cobra.Command, rf rightsizeFlags, args []string) error {
	switch rf.percentile {
	case "", "p90", "p95", "p99", "max":
	default:
		return errors.New("--percentile must be p90, p95, p99 or max")
	}
	if rf.headroom < 0 || rf.headroom > 500 {
		return errors.New("--headroom must be within 0..500")
	}
	cl, cfg, err := signalClients()
	if err != nil {
		return err
	}
	ctx, cancel := unaryCtx(cmd)
	defer cancel()
	res, err := cl.Cost.ListRightsizing(ctx, connect.NewRequest(&kuberov1.ListRightsizingRequest{
		ClusterId: rf.cluster, Namespace: rf.namespace, Window: rf.window, HeadroomPct: rf.headroom,
		CpuPercentile: rf.percentile, MinSavingsUsdMonth: rf.minSavings, Limit: int32(rf.limit),
	}))
	if err != nil {
		return err
	}
	msg := res.Msg
	if len(args) == 1 {
		keep := msg.GetRecommendations()[:0]
		for _, r := range msg.GetRecommendations() {
			if r.GetWorkload() == args[0] {
				keep = append(keep, r)
			}
		}
		msg.Recommendations = keep
	}
	rows := make([]*khfmt.Row, 0, len(msg.GetRecommendations()))
	for _, r := range msg.GetRecommendations() {
		// OOM kills ride in the 5th column so the default (5-column)
		// table never hides them: they veto any memory cut.
		conf := r.GetConfidence()
		if r.GetOomKills() > 0 {
			conf += fmt.Sprintf(", %d OOM", r.GetOomKills())
		}
		rows = append(rows, khfmt.NewRow().
			Set("workload", r.GetNamespace()+"/"+r.GetWorkload()+" ("+r.GetContainer()+")").
			Set("cpu", cores(r.GetCpuRequestCores())+" → "+cores(r.GetCpuRecommendedCores())).
			Set("memory", bytesIEC(float64(r.GetMemRequestBytes()))+" → "+bytesIEC(float64(r.GetMemRecommendedBytes()))).
			Set("$/mo", usd(r.GetSavingsUsdMonth())).
			Set("confidence", conf).
			Set("direction", r.GetDirection()).
			Set("reason", r.GetReason()))
	}
	if err := render(cmd, cfg, rows, msg); err != nil {
		return err
	}
	if !structuredOutput(cfg) {
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d recommendations · %s/mo total%s\n",
			len(msg.GetRecommendations()), usd(msg.GetTotalSavingsUsdMonth()), sourceNote(msg.GetSource()))
		if strings.EqualFold(cfg.Output, "table") || cfg.Output == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "Apply safely with a RightsizingPolicy (recommend → shadow → apply, human-armed).")
		}
	}
	return nil
}
