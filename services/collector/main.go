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
	"runtime"
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

// saNamespaceFile is mounted into every pod with a service account token.
var saNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// podNamespace is POD_NAMESPACE, else — in a cluster whose manifest
// doesn't set it — the service account's namespace, so leader election
// still has somewhere to put its Lease instead of every collector
// running cluster-scoped duties. "" only outside a cluster.
func podNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	if raw, err := os.ReadFile(saNamespaceFile); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

// podName is POD_NAME, else the hostname (a pod's hostname defaults to
// its name).
func podName() string {
	if n := strings.TrimSpace(os.Getenv("POD_NAME")); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

func defaultExcludes() []string {
	if ns := podNamespace(); ns != "" {
		return []string{ns}
	}
	return nil
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the collector version",
		Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), version) },
	}
}

const serveLong = `Run the collector (normally as a DaemonSet, one pod per node).

Environment:
  NODE_NAME            Node this collector runs on (downward API: spec.nodeName).
                       Scopes every watch, kubelet call and report to this node.
                       Unset = ALL-NODES mode for local development: one process
                       prices the whole cluster (double-counts if run as a DaemonSet).
  POD_NAME             This pod's name (downward API: metadata.name); the identity
                       for leader election. Default: the hostname.
  POD_NAMESPACE        This pod's namespace (downward API: metadata.namespace);
                       where the "kubehero-collector" Lease lives. Default: the
                       service account's namespace. Outside a cluster (no
                       namespace at all) this process runs cluster-scoped duties
                       itself.
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
			cfg.PodName = podName()
			cfg.PodNamespace = podNamespace()
			cfg.ClusterID = os.Getenv("CLUSTER_ID")
			cfg.ControlPlaneURL = strings.TrimSpace(os.Getenv("CONTROL_PLANE_URL"))
			cfg.ControlPlaneToken = strings.TrimSpace(os.Getenv("CONTROL_PLANE_TOKEN"))
			cfg.PricingEngineURL = strings.TrimSpace(os.Getenv("PRICING_ENGINE_URL"))

			if cfg.Logs.From != "end" && cfg.Logs.From != "start" {
				return fmt.Errorf("--logs-from must be end or start, got %q", cfg.Logs.From)
			}
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

	f.BoolVar(&cfg.Logs.Enabled, "logs", true, "tail container logs from --logs-root and ship them (needs CONTROL_PLANE_URL)")
	f.StringVar(&cfg.Logs.Root, "logs-root", "/var/log/pods", "kubelet pod log directory (mount read-only)")
	f.StringVar(&cfg.Logs.PositionsFile, "logs-positions", "/var/lib/kubehero/log-positions.json",
		"checkpoint of {path, inode, offset} per file, written atomically (mount a writable hostPath so restarts resume)")
	f.StringVar(&cfg.Logs.From, "logs-from", "end", "where files found on the very first start begin: end | start (files that appear later always begin at the start)")
	f.Float64Var(&cfg.Logs.RateLimit, "logs-rate-limit", 2000, "per-container line rate limit (lines/s); excess lines are dropped and counted")
	f.IntVar(&cfg.Logs.Burst, "logs-burst", 4000, "per-container rate-limit burst (lines)")
	f.StringSliceVar(&cfg.Logs.ExcludeNamespaces, "logs-exclude-namespaces", defaultExcludes(),
		"namespaces whose logs are never tailed (default: the collector's own $POD_NAMESPACE, to avoid feedback loops)")
	f.StringSliceVar(&cfg.Logs.PodLabels, "logs-pod-labels", []string{"app", "app.kubernetes.io/name", "version", "app.kubernetes.io/version"},
		"pod labels copied onto every log entry (keys sanitised to LogQL names, e.g. app_kubernetes_io_name)")
	f.IntVar(&cfg.Logs.MaxBufferBytes, "logs-max-buffer-bytes", 64<<20, "max log bytes queued in memory while the control plane is slow or down (oldest dropped beyond)")

	f.BoolVar(&cfg.Profiles.Enabled, "profiles", true,
		"scrape pprof endpoints of pods annotated profiles.grafana.com/<type>.scrape=true or kubehero.io/profile=true (needs CONTROL_PLANE_URL)")
	f.DurationVar(&cfg.Profiles.Interval, "profile-interval", 60*time.Second, "pprof scrape interval")
	f.IntVar(&cfg.Profiles.CPUSeconds, "profile-cpu-seconds", 15, "CPU profile duration per scrape (/debug/pprof/profile?seconds=N)")

	f.BoolVar(&cfg.EBPF.Enabled, "ebpf", runtime.GOOS == "linux", "load eBPF programs (Linux, cgroup v2, CAP_BPF/CAP_PERFMON or privileged); degrades to off when unsupported")
	f.BoolVar(&cfg.EBPF.Netflow, "ebpf-netflow", true, "eBPF L3/L4 flow accounting + TCP retransmits, attributed to pods/services/nodes")
	f.BoolVar(&cfg.EBPF.Profiler, "ebpf-profiler", true, "eBPF whole-node CPU profiler (perf_event sampling)")
	f.IntVar(&cfg.EBPF.ProfileHz, "ebpf-profile-hz", 49, "eBPF CPU sampling frequency per CPU")
	f.StringVar(&cfg.EBPF.CgroupRoot, "cgroup-root", "/sys/fs/cgroup", "cgroup v2 mount the flow programs attach to")
	f.DurationVar(&cfg.EBPF.FlushInterval, "ebpf-flush-interval", 15*time.Second, "how often eBPF maps are drained and shipped")
	return c
}
