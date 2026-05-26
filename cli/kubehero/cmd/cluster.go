// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/client"
	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
)

func clusterCmd() *cobra.Command {
	c := &cobra.Command{Use: "cluster", Short: "Manage cluster registrations"}
	c.AddCommand(clusterListCmd(), clusterAddCmd())
	return c
}

func clusterListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List clusters registered to your org",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := resolveConfig()
			cl := client.New(cfg)
			res, err := cl.ListClusters(100)
			if err != nil {
				return err
			}
			rows := make([]*khfmt.Row, 0, len(res.Clusters))
			for _, c := range res.Clusters {
				rows = append(rows, khfmt.NewRow().
					Set("name", c.Name).
					Set("cloud", c.Cloud).
					Set("region", c.Region).
					Set("nodes", fmt.Sprintf("%d", c.Nodes)).
					Set("id", c.ID),
				)
			}
			return khfmt.Render(cmd.OutOrStdout(), cfg.Output, rows, res)
		},
	}
}

func clusterAddCmd() *cobra.Command {
	var name, cloud, region, slug string
	c := &cobra.Command{
		Use:   "add",
		Short: "Register a new cluster and emit its enrollment token",
		Long: `Register a new cluster with the control-plane.

The control-plane generates a 32-byte enrollment token, stores only the
SHA-256 hash, and returns the token to you exactly once. Pipe the token
into 'helm install' on the target cluster.

Treat the token as a credential — it is not retrievable later.`,
		Example: `  kubehero cluster add --name prod-eu-1 --cloud aws --region eu-west-1`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" || cloud == "" || region == "" {
				return fmt.Errorf("--name, --cloud and --region are required")
			}
			cfg := resolveConfig()
			cl := client.New(cfg)
			res, err := cl.RegisterCluster(&client.RegisterClusterRequest{
				Name: name, Cloud: cloud, Region: region, Slug: slug, Org: cfg.Org,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			switch cfg.Output {
			case "json":
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			default:
				fmt.Fprintf(out, "✓ cluster registered\n\n")
				fmt.Fprintf(out, "  id      %s\n", res.Cluster.ID)
				fmt.Fprintf(out, "  name    %s\n", res.Cluster.Name)
				fmt.Fprintf(out, "  cloud   %s · %s\n\n", res.Cluster.Cloud, res.Cluster.Region)
				fmt.Fprintf(out, "  ENROLLMENT TOKEN (shown once · keep secret):\n\n")
				fmt.Fprintf(out, "    %s\n\n", res.Token)
				fmt.Fprintf(out, "  Install on the target cluster:\n\n%s\n", res.HelmInstall)
				return nil
			}
		},
	}
	c.Flags().StringVar(&name, "name", "", "Human-readable cluster name (required)")
	c.Flags().StringVar(&cloud, "cloud", "", "aws | gcp | azure (required)")
	c.Flags().StringVar(&region, "region", "", "Cloud region, e.g. us-east-1 (required)")
	c.Flags().StringVar(&slug, "slug", "", "URL-safe slug; defaults to a normalised name")
	return c
}
