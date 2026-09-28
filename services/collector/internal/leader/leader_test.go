// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package leader

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

func TestSingleLeaderAndFailover(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var leading atomic.Int32
	var maxConcurrent atomic.Int32
	var mu sync.Mutex
	acquired := map[string]int{}

	candidate := func(ctx context.Context, id string) {
		Run(ctx, Config{
			Client: cs, Namespace: "kubehero", Name: "kubehero-collector", Identity: id,
			// Lease durations are stored in whole seconds.
			LeaseDuration: 2 * time.Second, RenewDeadline: 1500 * time.Millisecond, RetryPeriod: 200 * time.Millisecond,
			Logger: slog.New(slog.DiscardHandler),
		}, func(lctx context.Context) {
			n := leading.Add(1)
			for {
				m := maxConcurrent.Load()
				if n <= m || maxConcurrent.CompareAndSwap(m, n) {
					break
				}
			}
			mu.Lock()
			acquired[id]++
			mu.Unlock()
			<-lctx.Done()
			leading.Add(-1)
		})
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	doneA := make(chan struct{})
	go func() { candidate(ctxA, "node-a"); close(doneA) }()
	waitFor(t, "first leader", func() bool { return leading.Load() == 1 })
	go candidate(ctxB, "node-b")
	time.Sleep(2500 * time.Millisecond) // longer than the lease: B must not take over a renewed lease
	mu.Lock()
	if acquired["node-a"] != 1 || acquired["node-b"] != 0 {
		mu.Unlock()
		t.Fatalf("acquisitions = %v", acquired)
	}
	mu.Unlock()

	lease, err := cs.CoordinationV1().Leases("kubehero").Get(context.Background(), "kubehero-collector", metav1.GetOptions{})
	if err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "node-a" {
		t.Fatalf("lease = %+v, %v", lease, err)
	}

	// A steps down (ReleaseOnCancel): B takes over quickly.
	cancelA()
	<-doneA
	waitFor(t, "failover", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return acquired["node-b"] == 1
	})
	if maxConcurrent.Load() != 1 {
		t.Fatalf("two leaders at once (max concurrent %d)", maxConcurrent.Load())
	}
}

func TestNoNamespaceRunsDirectly(t *testing.T) {
	ran := false
	Run(context.Background(), Config{Logger: slog.New(slog.DiscardHandler)}, func(context.Context) {
		ran = metrics.Leader.With().Get() == 1
	})
	if !ran {
		t.Fatal("onLeading must run (as leader) without a namespace")
	}
	if metrics.Leader.With().Get() != 0 {
		t.Fatal("leader gauge must reset after duties end")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
