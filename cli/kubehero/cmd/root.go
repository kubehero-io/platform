// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/config"
)

var Version = "0.0.0-dev"

// global flags shared by every subcommand
var (
	flagEndpoint        string
	flagAdvisorEndpoint string
	flagToken           string
	flagOrg             string
	flagOutput          string
	flagInsecure        bool
	flagNoColor         bool
)

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "kubehero",
		Short:         "KubeHero — Kubernetes efficiency across every cloud you run",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	pf := root.PersistentFlags()
	pf.StringVar(&flagEndpoint, "endpoint", "", "Control-plane URL (overrides ~/.kubehero/config.yaml)")
	pf.StringVar(&flagToken, "token", "", "Bearer token (overrides config + KUBEHERO_TOKEN)")
	pf.StringVar(&flagOrg, "org", "", "Organization slug")
	pf.StringVarP(&flagOutput, "output", "o", "", "Output format: table | json | yaml | wide")
	pf.BoolVar(&flagInsecure, "insecure", false, "Skip TLS verification (dev only)")
	pf.StringVar(&flagAdvisorEndpoint, "advisor-endpoint", "", "Advisor URL for ask / mcp (default: --endpoint)")
	pf.BoolVar(&flagNoColor, "no-color", false, "Disable colour (also honours NO_COLOR)")

	root.AddCommand(scanCmd(), rightsizeCmd(), applyCmd(), quoteCmd(),
		clusterCmd(), authCmd(), healthCmd(), capCmd(), undoCmd(),
		logsCmd(), profileCmd(), costCmd(), networkCmd(), alertsCmd(),
		askCmd(), mcpCmd())
	return root
}

// resolveConfig merges file + env + flags. Flags take precedence.
func resolveConfig() *config.Config {
	c, _ := config.Load()
	if flagEndpoint != "" {
		c.Endpoint = flagEndpoint
	}
	if flagToken != "" {
		c.Token = flagToken
	}
	if flagOrg != "" {
		c.Org = flagOrg
	}
	if flagAdvisorEndpoint != "" {
		c.AdvisorEndpoint = flagAdvisorEndpoint
	}
	if flagOutput != "" {
		c.Output = flagOutput
	}
	if flagInsecure {
		c.Insecure = true
	}
	return c
}
