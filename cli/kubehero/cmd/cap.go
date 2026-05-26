// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// cap is the canonical "arm a CeilingPolicy" verb. Today's Connect schema
// doesn't expose ArmPolicy yet — when it does, swap stub for real RPC.
func capCmd() *cobra.Command {
	var arm bool
	var policy string
	c := &cobra.Command{
		Use:   "cap",
		Short: "Arm or disarm a CeilingPolicy / BudgetPolicy",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !arm {
				return fmt.Errorf("nothing to do · pass --arm to arm the policy")
			}
			if policy == "" {
				return fmt.Errorf("--policy is required")
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"arming policy %q (rpc not wired yet — annotate via kubectl in the meantime)\n", policy)
			return nil
		},
	}
	c.Flags().BoolVar(&arm, "arm", false, "Arm the policy")
	c.Flags().StringVar(&policy, "policy", "", "Policy name")
	return c
}

func undoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "undo <audit-id>",
		Short: "Reverse an applied action within its cooldown window",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(),
				"undoing %s (rpc not wired yet — see /docs/cli for manual recovery)\n", args[0])
			return nil
		},
	}
}
