// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package leader elects one collector per cluster for cluster-scoped
// duties (pending-pod scans, Warning-event watch, node conditions) via a
// coordination.k8s.io Lease. Everything node-local runs on every
// collector regardless; only work that would otherwise be duplicated N
// times goes through here.
//
// Timings are deliberately relaxed compared with controller defaults
// (15s/10s/2s): every DaemonSet pod is a candidate, and each candidate
// polls the Lease every RetryPeriod, so a 1000-node cluster at 2s would
// add ~500 GETs/s to the API server for a role that tolerates a minute
// of failover.
package leader

import (
	"context"
	"log/slog"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

// Defaults for Config timings.
const (
	DefaultLeaseDuration = 60 * time.Second
	DefaultRenewDeadline = 40 * time.Second
	DefaultRetryPeriod   = 15 * time.Second
)

// Config configures the election.
type Config struct {
	Client    kubernetes.Interface
	Namespace string // Lease namespace (POD_NAMESPACE)
	Name      string // Lease name, "kubehero-collector"
	Identity  string // POD_NAME; generated when empty

	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	Logger                                    *slog.Logger
}

// Run takes part in the election until ctx ends. onLeading runs on each
// acquisition with a context that is cancelled when leadership is lost;
// after a loss the collector re-joins as a candidate.
//
// With no namespace (local development outside a cluster) there is no
// one to coordinate with: onLeading runs directly.
func Run(ctx context.Context, cfg Config, onLeading func(context.Context)) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	gauge := metrics.Leader.With()
	if cfg.Namespace == "" {
		log.Info("POD_NAMESPACE unset — no leader election; this process runs cluster-scoped duties itself")
		gauge.Set(1)
		onLeading(ctx)
		gauge.Set(0)
		return
	}
	if cfg.Identity == "" {
		host, _ := os.Hostname()
		cfg.Identity = host + "_" + rand.String(6)
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = DefaultLeaseDuration
	}
	if cfg.RenewDeadline <= 0 {
		cfg.RenewDeadline = DefaultRenewDeadline
	}
	if cfg.RetryPeriod <= 0 {
		cfg.RetryPeriod = DefaultRetryPeriod
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.Name},
		Client:     cfg.Client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: cfg.Identity},
	}
	for ctx.Err() == nil {
		le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			Name:            cfg.Name,
			LeaseDuration:   cfg.LeaseDuration,
			RenewDeadline:   cfg.RenewDeadline,
			RetryPeriod:     cfg.RetryPeriod,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(lctx context.Context) {
					gauge.Set(1)
					log.Info("acquired collector leadership — running cluster-scoped duties", "lease", cfg.Namespace+"/"+cfg.Name, "identity", cfg.Identity)
					onLeading(lctx)
				},
				OnStoppedLeading: func() {
					gauge.Set(0)
					if ctx.Err() == nil {
						log.Warn("lost collector leadership — rejoining as candidate", "identity", cfg.Identity)
					}
				},
				OnNewLeader: func(id string) {
					if id != cfg.Identity {
						log.Debug("collector leader", "identity", id)
					}
				},
			},
		})
		if err != nil {
			log.Error("leader election misconfigured — cluster-scoped duties disabled", "err", err)
			return
		}
		le.Run(ctx)
		// Brief pause so a flapping API server doesn't spin the loop.
		select {
		case <-ctx.Done():
		case <-time.After(cfg.RetryPeriod):
		}
	}
}
