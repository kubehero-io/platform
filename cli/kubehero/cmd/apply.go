// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func applyCmd() *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "apply",
		Short: "Apply a BudgetPolicy or CeilingPolicy from a YAML file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "apply: file=%q (not yet implemented)\n", file)
			return nil
		},
	}
	c.Flags().StringVarP(&file, "filename", "f", "", "path to YAML policy")
	_ = c.MarkFlagRequired("filename")
	return c
}
