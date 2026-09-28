// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/client"
)

// cap is the canonical "arm a CeilingPolicy" verb, backed by the
// control-plane's ArmPolicy RPC. The server persists the armed bit,
// writes an HMAC-signed audit row, and pages the configured alert
// channels.
func capCmd() *cobra.Command {
	var arm, disarm bool
	var policy, cluster, reason string
	c := &cobra.Command{
		Use:   "cap",
		Short: "Arm or disarm a CeilingPolicy / BudgetPolicy",
		Example: `  kubehero cap --arm --policy gpu-inference-cap --cluster eks-use1-prod --reason "runaway GPU spend"
  kubehero cap --disarm --policy gpu-inference-cap --cluster eks-use1-prod`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if arm && disarm {
				return fmt.Errorf("--arm and --disarm are mutually exclusive")
			}
			if !arm && !disarm {
				return fmt.Errorf("nothing to do · pass --arm to arm the policy (or --disarm)")
			}
			if policy == "" {
				return fmt.Errorf("--policy is required")
			}
			cfg := resolveConfig()
			cl := client.New(cfg)
			res, err := cl.ArmPolicy(&client.ArmPolicyRequest{
				ClusterID:  cluster,
				PolicyName: policy,
				Armed:      arm,
				Reason:     reason,
			})
			if err != nil {
				return err
			}
			verb := "disarmed"
			if res.Armed {
				verb = "armed"
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "✓ policy %q %s", res.PolicyName, verb)
			if res.EffectiveAtUnix > 0 {
				fmt.Fprintf(out, " · effective %s", time.Unix(res.EffectiveAtUnix, 0).UTC().Format(time.RFC3339))
			}
			if res.AuditID != "" {
				fmt.Fprintf(out, " · audit %s", res.AuditID)
			}
			fmt.Fprintln(out)
			return nil
		},
	}
	c.Flags().BoolVar(&arm, "arm", false, "Arm the policy")
	c.Flags().BoolVar(&disarm, "disarm", false, "Disarm the policy")
	c.Flags().StringVar(&policy, "policy", "", "Policy name")
	c.Flags().StringVar(&cluster, "cluster", "", "Cluster id or slug the policy lives in")
	c.Flags().StringVar(&reason, "reason", "", "Why you are arming/disarming (recorded in the audit log)")
	return c
}
