// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func rightsizeCmd() *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "rightsize [workload]",
		Short: "Recommend or apply rightsizing for a workload",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "<all>"
			if len(args) == 1 {
				target = args[0]
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rightsize: target=%q dry-run=%v (not yet implemented)\n", target, dryRun)
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", true, "dry-run (recommend only, do not mutate)")
	return c
}
