// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/config"
	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
	"github.com/kubehero-io/platform/cli/kubehero/internal/rpc"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

func authCmd() *cobra.Command {
	c := &cobra.Command{Use: "auth", Short: "Authenticate the CLI to a control plane"}
	c.AddCommand(authLoginCmd(), authStatusCmd(), authWhoamiCmd())
	return c
}

// whoami asks the control plane who the configured credential is.
func whoami(cmd *cobra.Command, cfg *config.Config) (*kuberov1.WhoAmIResponse, error) {
	cl, err := rpc.New(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := unaryCtx(cmd)
	defer cancel()
	res, err := cl.Control.WhoAmI(ctx, connect.NewRequest(&kuberov1.WhoAmIRequest{}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func authLoginCmd() *cobra.Command {
	var skipVerify bool
	c := &cobra.Command{
		Use:   "login",
		Short: "Verify and save endpoint + token to ~/.kubehero/config.yaml",
		Long: `Save the control-plane endpoint and bearer token for later commands.

The credential is checked first with the WhoAmI RPC: a rejected token,
or an anonymous identity on a control plane that requires auth, is not
saved. --no-verify saves without checking (offline setup).`,
		Example: `  kubehero auth login --endpoint https://kubehero.example.com --token "$KUBEHERO_TOKEN"`,
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
			if flagAdvisorEndpoint != "" {
				cfg.AdvisorEndpoint = flagAdvisorEndpoint
			}
			if cfg.Endpoint == "" {
				return fmt.Errorf("--endpoint is required (e.g. https://api.kubehero.io)")
			}
			out := cmd.OutOrStdout()
			identity := ""
			if !skipVerify {
				me, err := whoami(cmd, cfg)
				switch {
				case connect.CodeOf(err) == connect.CodeUnauthenticated || connect.CodeOf(err) == connect.CodePermissionDenied:
					return fmt.Errorf("the control plane rejected this token (%v); nothing saved", err)
				case connect.CodeOf(err) == connect.CodeUnimplemented:
					fmt.Fprintln(cmd.ErrOrStderr(), "note: this control plane predates WhoAmI; saving without verification")
				case err != nil:
					return fmt.Errorf("could not verify the credential against %s: %w (use --no-verify to save anyway)", cfg.Endpoint, err)
				case me.GetAuthRequired() && strings.EqualFold(me.GetRole(), "anonymous"):
					return errors.New("the control plane requires auth but this login is anonymous; pass --token")
				default:
					identity = fmt.Sprintf(" · %s (%s)", me.GetSubject(), me.GetRole())
				}
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Fprintf(out, "saved · endpoint=%s · token=%s%s\n", cfg.Endpoint, redact(cfg.Token), identity)
			return nil
		},
	}
	c.Flags().BoolVar(&skipVerify, "no-verify", false, "Save without checking the credential")
	return c
}

func authWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who the configured token authenticates as",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := resolveConfig()
			me, err := whoami(cmd, cfg)
			if err != nil {
				return err
			}
			row := khfmt.NewRow().
				Set("subject", me.GetSubject()).
				Set("role", me.GetRole()).
				Set("email", me.GetEmail()).
				Set("cluster", me.GetClusterId()).
				Set("auth required", fmt.Sprintf("%v", me.GetAuthRequired())).
				Set("groups", strings.Join(me.GetGroups(), ","))
			if strings.EqualFold(cfg.Output, "table") || cfg.Output == "" {
				cfg.Output = "wide"
			}
			return render(cmd, cfg, []*khfmt.Row{row}, me)
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
				"endpoint  %s\nadvisor   %s\norg       %s\ntoken     %s\noutput    %s\ninsecure  %v\n",
				cfg.Endpoint, nonEmptyStr(cfg.AdvisorEndpoint, "(same as endpoint)"), cfg.Org, redact(cfg.Token), cfg.Output, cfg.Insecure)
			return nil
		},
	}
}

func redact(s string) string {
	switch {
	case s == "":
		return "(unset)"
	case len(s) <= 8:
		return "(set)"
	}
	return s[:4] + "…" + s[len(s)-4:]
}
