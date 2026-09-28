// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// askTimeout bounds one investigation from the CLI's side (the advisor
// itself budgets 120s for the LLM loop, plus a rules fallback).
const askTimeout = 4 * time.Minute

func askCmd() *cobra.Command {
	var cluster, window, askContext string
	var speak, quiet bool
	c := &cobra.Command{
		Use:   `ask "<question>"`,
		Short: "Ask KubeHero a question; it investigates with read-only tools",
		Long: `Ask a free-form question. The advisor investigates with read-only tools
over cost, logs, profiles, the network map, alerts and rightsizing,
streaming each step as it runs, then answers with cited evidence and
guarded proposals (CRD manifests you apply through the arming flow —
nothing runs automatically).

The advisor endpoint defaults to --endpoint; point --advisor-endpoint
(or KUBEHERO_ADVISOR_ENDPOINT) at the advisor service when it is
exposed separately.`,
		Example: `  kubehero ask "why did checkout's spend jump last night?"
  kubehero ask "is anything slow in payments?" --window 1h --speak
  kubehero ask "what should we rightsize first?" -o json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			question := strings.TrimSpace(strings.Join(args, " "))
			if question == "" {
				return errors.New("ask needs a question")
			}
			switch window {
			case "1h", "24h", "7d":
			default:
				return errors.New("--window must be 1h, 24h or 7d")
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmdContext(cmd), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, askTimeout)
			defer cancel()

			stream, err := cl.Advisor.InvestigateStream(ctx, connect.NewRequest(&kuberov1.InvestigateStreamRequest{
				Request: &kuberov1.InvestigateRequest{Question: question, ClusterId: cluster, Window: window, Context: askContext},
			}))
			if err != nil {
				return err
			}
			defer stream.Close()
			// Live progress goes to stderr so stdout stays the answer
			// (pipe-friendly: kubehero ask "…" > answer.md).
			progress := cmd.ErrOrStderr()
			paint := newPainter(progress)
			var result *kuberov1.InvestigateResponse
			for stream.Receive() {
				switch ev := stream.Msg().Event.(type) {
				case *kuberov1.InvestigateStreamResponse_Progress:
					if !quiet && !structuredOutput(cfg) {
						fmt.Fprintf(progress, "%s %s\n", paint.dim("…"), paint.dim(ev.Progress))
					}
				case *kuberov1.InvestigateStreamResponse_Step:
					if !quiet && !structuredOutput(cfg) {
						printStep(progress, paint, ev.Step)
					}
				case *kuberov1.InvestigateStreamResponse_Result:
					result = ev.Result
				}
			}
			if err := stream.Err(); err != nil {
				return err
			}
			if result == nil {
				return errors.New("the advisor ended the stream without an answer")
			}
			if structuredOutput(cfg) {
				return render(cmd, cfg, nil, result)
			}
			printAnswer(cmd.OutOrStdout(), newPainter(cmd.OutOrStdout()), result, speak)
			return nil
		},
	}
	c.Flags().StringVar(&cluster, "cluster", "", "Cluster id (default: fleet-wide)")
	c.Flags().StringVar(&window, "window", "24h", "1h | 24h | 7d")
	c.Flags().StringVar(&askContext, "context", "", "Dashboard path the question is about, e.g. /workloads/eks-1/payments/checkout-api")
	c.Flags().BoolVar(&speak, "speak", false, "Also print the TTS-ready spoken summary")
	c.Flags().BoolVarP(&quiet, "quiet", "q", false, "Hide live progress")
	return c
}

func printStep(w io.Writer, paint painter, s *kuberov1.InvestigateStep) {
	mark := paint.green("✓")
	if s.GetError() {
		mark = paint.red("✗")
	}
	fmt.Fprintf(w, "%s %s %s %s\n", mark, paint.bold(s.GetTool()),
		paint.dim(fmt.Sprintf("(%.0fms)", s.GetDurationMs())), s.GetSummary())
}

func printAnswer(w io.Writer, paint painter, r *kuberov1.InvestigateResponse, speak bool) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, strings.TrimSpace(r.GetAnswerMarkdown()))
	if ev := r.GetEvidence(); len(ev) > 0 {
		fmt.Fprintf(w, "\n%s\n", paint.bold("Evidence"))
		for _, e := range ev {
			line := fmt.Sprintf("  [%s] %s", e.GetKind(), e.GetTitle())
			if d := e.GetDetail(); d != "" {
				line += " — " + d
			}
			fmt.Fprintln(w, line)
			if q := e.GetQuery(); q != "" {
				fmt.Fprintf(w, "      %s %s\n", paint.dim("query:"), q)
			}
			if l := e.GetLinkPath(); l != "" {
				fmt.Fprintf(w, "      %s %s\n", paint.dim("open:"), l)
			}
		}
	}
	if acts := r.GetActions(); len(acts) > 0 {
		fmt.Fprintf(w, "\n%s %s\n", paint.bold("Proposed actions"), paint.dim("(proposals only — apply through the arming flow)"))
		for i, a := range acts {
			fmt.Fprintf(w, "  %d. %s [%s, risk %s", i+1, a.GetTitle(), a.GetKind(), a.GetRisk())
			if v := a.GetImpactMonthlyUsd(); v > 0 {
				fmt.Fprintf(w, ", ~%s/mo", usd(v))
			}
			fmt.Fprintln(w, "]")
			if a.GetRationale() != "" {
				fmt.Fprintf(w, "     %s\n", a.GetRationale())
			}
			if y := strings.TrimSpace(a.GetCrdYaml()); y != "" {
				for _, l := range strings.Split(y, "\n") {
					fmt.Fprintf(w, "     %s\n", paint.dim(l))
				}
			}
		}
	}
	if speak && r.GetSpokenSummary() != "" {
		fmt.Fprintf(w, "\n%s\n%s\n", paint.bold("Spoken summary"), r.GetSpokenSummary())
	}
	src := r.GetSource()
	if src == "demo" {
		src = "DEMO DATA"
	}
	fmt.Fprintf(w, "\n%s\n", paint.dim(fmt.Sprintf("source: %s · %d tool calls · %s", src, len(r.GetSteps()), r.GetId())))
}
