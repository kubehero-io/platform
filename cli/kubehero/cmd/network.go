// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"
	"sort"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

func networkCmd() *cobra.Command {
	c := &cobra.Command{Use: "network", Short: "eBPF service map and network spend (egress, cross-zone)"}
	c.AddCommand(networkMapCmd(), networkCostsCmd())
	return c
}

func networkMapCmd() *cobra.Command {
	var namespace, since, cluster string
	var maxEdges int
	c := &cobra.Command{
		Use:     "map",
		Short:   "Who talks to whom, how much, and what it costs",
		Example: `  kubehero network map --namespace payments --since 1h`,
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
			res, err := cl.Network.GetServiceMap(ctx, connect.NewRequest(&kuberov1.GetServiceMapRequest{
				ClusterId: cluster, Namespace: namespace, StartUnixMs: start, EndUnixMs: end,
			}))
			if err != nil {
				return err
			}
			edges := append([]*kuberov1.ServiceMapEdge(nil), res.Msg.GetEdges()...)
			sort.SliceStable(edges, func(i, j int) bool {
				if edges[i].GetCostUsdMonth() != edges[j].GetCostUsdMonth() {
					return edges[i].GetCostUsdMonth() > edges[j].GetCostUsdMonth()
				}
				return edges[i].GetBytes() > edges[j].GetBytes()
			})
			if maxEdges > 0 && len(edges) > maxEdges {
				edges = edges[:maxEdges]
			}
			rows := make([]*khfmt.Row, 0, len(edges))
			for _, e := range edges {
				flags := ""
				if e.GetCrossZone() {
					flags += "cross-zone "
				}
				if e.GetEgress() {
					flags += "egress "
				}
				if e.GetRetransmits() > 0 {
					flags += fmt.Sprintf("retx=%.0f", e.GetRetransmits())
				}
				rows = append(rows, khfmt.NewRow().
					Set("source", e.GetSource()).
					Set("target", fmt.Sprintf("%s:%d", e.GetTarget(), e.GetPort())).
					Set("bytes", bytesIEC(e.GetBytes())).
					Set("$/mo", usd(e.GetCostUsdMonth())).
					Set("flags", flags))
			}
			if err := render(cmd, cfg, rows, res.Msg); err != nil {
				return err
			}
			if !structuredOutput(cfg) {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d nodes · %d edges · %s/mo · egress %.1f GB · cross-zone %.1f GB%s\n",
					len(res.Msg.GetNodes()), len(res.Msg.GetEdges()), usd(res.Msg.GetTotalCostUsdMonth()),
					res.Msg.GetEgressGb(), res.Msg.GetCrossZoneGb(), sourceNote(res.Msg.GetSource()))
			}
			return nil
		},
	}
	c.Flags().StringVar(&namespace, "namespace", "", "Focus namespace")
	c.Flags().StringVar(&since, "since", "1h", "Lookback")
	c.Flags().StringVar(&cluster, "cluster", "", "Cluster id")
	c.Flags().IntVar(&maxEdges, "max-edges", 25, "Show the N most expensive edges")
	return c
}

func networkCostsCmd() *cobra.Command {
	var since, cluster string
	var limit int
	c := &cobra.Command{
		Use:   "costs",
		Short: "Per-workload internet egress and cross-zone spend",
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
			res, err := cl.Network.ListNetworkCosts(ctx, connect.NewRequest(&kuberov1.ListNetworkCostsRequest{
				ClusterId: cluster, StartUnixMs: start, EndUnixMs: end, Limit: int32(limit),
			}))
			if err != nil {
				return err
			}
			rows := make([]*khfmt.Row, 0, len(res.Msg.GetCosts()))
			for _, c := range res.Msg.GetCosts() {
				rows = append(rows, khfmt.NewRow().
					Set("workload", c.GetNamespace()+"/"+c.GetWorkload()).
					Set("total $/mo", usd(c.GetTotalUsdMonth())).
					Set("egress", fmt.Sprintf("%s (%.1f GB)", usd(c.GetEgressUsdMonth()), c.GetEgressGb())).
					Set("cross-zone", fmt.Sprintf("%s (%.1f GB)", usd(c.GetCrossZoneUsdMonth()), c.GetCrossZoneGb())).
					Set("top destination", c.GetTopDestination()))
			}
			if err := render(cmd, cfg, rows, res.Msg); err != nil {
				return err
			}
			if !structuredOutput(cfg) {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s/mo network spend%s\n", usd(res.Msg.GetTotalUsdMonth()), sourceNote(res.Msg.GetSource()))
			}
			return nil
		},
	}
	c.Flags().StringVar(&since, "since", "24h", "Lookback")
	c.Flags().StringVar(&cluster, "cluster", "", "Cluster id")
	c.Flags().IntVar(&limit, "limit", 25, "Max rows")
	return c
}
