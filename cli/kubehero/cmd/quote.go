// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/client"
	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
)

func init() {
	// Register quote as a top-level command via Root() — but Root is a
	// function, so add it via the cmd factory.
}

// quoteCmd asks the pricing engine for a per-hour SKU quote.
// Wired into Root() in root.go via a separate call.
func quoteCmd() *cobra.Command {
	var cloud, sku, region, lifecycle string
	c := &cobra.Command{
		Use:   "quote",
		Short: "Quote a per-hour price for an instance SKU",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := resolveConfig()
			cl := client.New(cfg)
			res, err := cl.Quote(cloud, sku, region, lifecycle)
			if err != nil {
				return err
			}
			row := khfmt.NewRow().
				Set("cloud", cloud).
				Set("sku", sku).
				Set("region", region).
				Set("lifecycle", lifecycle).
				Set("price/hr", fmt.Sprintf("$%.4f", res.PricePerHour)).
				Set("currency", res.Currency)
			return khfmt.Render(cmd.OutOrStdout(), cfg.Output, []*khfmt.Row{row}, res)
		},
	}
	c.Flags().StringVar(&cloud, "cloud", "aws", "aws | gcp | azure")
	c.Flags().StringVar(&sku, "sku", "m5.large", "instance SKU")
	c.Flags().StringVar(&region, "region", "us-east-1", "region")
	c.Flags().StringVar(&lifecycle, "lifecycle", "on-demand", "on-demand | spot | savings-plan | committed")
	return c
}
