// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func scanCmd() *cobra.Command {
	var cluster, report string
	c := &cobra.Command{
		Use:   "scan",
		Short: "Scan a cluster for rightsizing opportunities",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := resolveConfig()
			if cluster == "" && cfg.Org == "" {
				return fmt.Errorf("--cluster is required (or set default org via auth login)")
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"scan: cluster=%q report=%q · endpoint=%s · output=%s\n"+
					"(scan rpc not wired yet — see /docs/cli)\n",
				cluster, report, cfg.Endpoint, cfg.Output)
			return nil
		},
	}
	c.Flags().StringVar(&cluster, "cluster", "", "cluster name")
	c.Flags().StringVar(&report, "report", "waste", "report type: waste | gpu | overcommit")
	return c
}
