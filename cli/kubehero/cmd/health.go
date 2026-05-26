// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/client"
)

func healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Ping the control-plane HealthCheck RPC",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := resolveConfig()
			res, err := client.New(cfg).HealthCheck()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s · v%s\n", res.Status, res.Version)
			return nil
		},
	}
}
