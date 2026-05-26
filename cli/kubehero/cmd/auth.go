// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/config"
)

func authCmd() *cobra.Command {
	c := &cobra.Command{Use: "auth", Short: "Authenticate the CLI to a control plane"}
	c.AddCommand(authLoginCmd(), authStatusCmd())
	return c
}

func authLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Save endpoint + token to ~/.kubehero/config.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _ := config.Load()
			if flagEndpoint != "" {
				cfg.Endpoint = flagEndpoint
			}
			if flagToken != "" {
				cfg.Token = flagToken
			}
			if flagOrg != "" {
				cfg.Org = flagOrg
			}
			if cfg.Endpoint == "" {
				return fmt.Errorf("--endpoint is required (e.g. https://api.kubehero.io)")
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved · endpoint=%s · token=%s\n",
				cfg.Endpoint, redact(cfg.Token))
			return nil
		},
	}
}

func authStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current configuration (token redacted)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := resolveConfig()
			fmt.Fprintf(cmd.OutOrStdout(),
				"endpoint  %s\norg       %s\ntoken     %s\noutput    %s\ninsecure  %v\n",
				cfg.Endpoint, cfg.Org, redact(cfg.Token), cfg.Output, cfg.Insecure)
			return nil
		},
	}
}

func redact(s string) string {
	if len(s) <= 8 {
		return "(unset)"
	}
	return s[:4] + "…" + s[len(s)-4:]
}
