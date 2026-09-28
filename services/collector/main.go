// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Command collector is the KubeHero node agent, run as a DaemonSet: it
// prices the pods on its node, samples container usage, detects health
// events, tails container logs, scrapes pprof endpoints and (on Linux)
// runs the eBPF flow + CPU profilers, shipping everything to the control
// plane over Connect.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"github.com/kubehero-io/platform/services/collector/internal/app"
)

// version is stamped at build time: -ldflags "-X main.version=v0.3.0".
var version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "collector",
		Short:         "KubeHero node agent — cost, usage, events, logs, profiles and eBPF telemetry",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(serveCmd(), versionCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the collector version",
		Run:   func(*cobra.Command, []string) { fmt.Println(version) },
	}
}

const serveLong = `Run the collector (normally as a DaemonSet, one pod per node).

Environment:
  NODE_NAME            Node this collector runs on (downward API: spec.nodeName).
                       Scopes every watch, kubelet call and report to this node.
                       Unset = ALL-NODES mode for local development: one process
                       prices the whole cluster (double-counts if run as a DaemonSet).
  POD_NAME             This pod's name (downward API: metadata.name); the identity
                       for leader election.
  POD_NAMESPACE        This pod's namespace (downward API: metadata.namespace);
                       where the "kubehero-collector" Lease lives.
  CLUSTER_ID           Cluster slug or UUID stamped on every request.
  CONTROL_PLANE_URL    Control plane base URL, e.g. http://kubehero-control-plane:8080.
                       Unset = nothing is shipped (cost is still exposed on /metrics).
  CONTROL_PLANE_TOKEN  Bearer token for the control plane (member role or above).
                       Never logged.
  PRICING_ENGINE_URL   PricingService base URL. When set, node prices come from
                       PricingService.Quote (cached 1h) unless a node carries the
                       kubehero.io/node-hourly-usd annotation.
  KUBECONFIG           Kubeconfig for out-of-cluster runs (in-cluster config wins).

Endpoints: /healthz (liveness), /readyz (informer cache synced), /metrics.`

func serveCmd() *cobra.Command {
	cfg := app.Config{Version: version}
	var logLevel string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the collector",
		Long:  serveLong,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg.NodeName = os.Getenv("NODE_NAME")
			cfg.PodName = os.Getenv("POD_NAME")
			cfg.PodNamespace = os.Getenv("POD_NAMESPACE")
			cfg.ClusterID = os.Getenv("CLUSTER_ID")
			cfg.ControlPlaneURL = strings.TrimSpace(os.Getenv("CONTROL_PLANE_URL"))
			cfg.ControlPlaneToken = strings.TrimSpace(os.Getenv("CONTROL_PLANE_TOKEN"))
			cfg.PricingEngineURL = strings.TrimSpace(os.Getenv("PRICING_ENGINE_URL"))

			var level slog.Level
			if err := level.UnmarshalText([]byte(logLevel)); err != nil {
				return fmt.Errorf("--log-level: %w", err)
			}
			cfg.Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
			// client-go (leader election, reflectors) logs through klog;
			// send it to the same JSON stream.
			klog.SetSlogLogger(cfg.Logger)

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			return app.Run(ctx, cfg)
		},
	}
	f := c.Flags()
	f.StringVar(&cfg.Addr, "addr", ":8081", "HTTP listen address for /healthz, /readyz and /metrics")
	f.BoolVar(&cfg.Demo, "demo", false, "also expose synthetic chargeback series (labelled source=\"demo\") on /metrics")
	f.StringVar(&logLevel, "log-level", "info", "log level: debug | info | warn | error")
	f.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "kubeconfig path for out-of-cluster runs (default $KUBECONFIG or ~/.kube/config)")
	f.DurationVar(&cfg.ScanInterval, "scan-interval", 5*time.Second, "cost scan + container-status event interval")
	f.DurationVar(&cfg.UsageInterval, "usage-interval", 30*time.Second, "per-container usage sample interval (rightsizing history)")
	f.BoolVar(&cfg.Events, "events", true, "detect health events (OOM kills, crash loops, image pull failures, unschedulable pods, evictions, node pressure)")
	f.BoolVar(&cfg.LeaderElect, "leader-elect", true,
		"run cluster-scoped duties (pending pods, Warning events, node conditions) only on the holder of the kubehero-collector Lease in $POD_NAMESPACE")
	f.StringVar(&cfg.KubeletURL, "kubelet-url", "",
		"read stats straight from this kubelet (e.g. https://$(NODE_IP):10250; needs get nodes/stats) instead of via the API-server node proxy (needs get nodes/proxy)")
	f.BoolVar(&cfg.KubeletInsecureTLS, "kubelet-insecure-tls", false, "skip kubelet serving-certificate verification with --kubelet-url")
	f.DurationVar(&cfg.KubeletStatsTTL, "kubelet-stats-ttl", 10*time.Second, "reuse a kubelet summary for this long across the cost and usage loops")
	f.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", 10*time.Second, "how long to keep flushing queued data on SIGTERM")
	return c
}
