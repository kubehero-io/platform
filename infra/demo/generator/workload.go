// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	_ "net/http/pprof" // /debug/pprof for the collector's pull-mode profiler
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// runWorkload turns the generator into a small, real microservice for
// end-to-end tests on a live cluster: it serves HTTP, burns a little
// CPU per request (hashing + JSON), writes structured logs to stdout,
// calls its downstream at a steady rate, and exposes /debug/pprof. A
// frontend → checkout → payments chain of these gives the collector
// real logs to tail, real traffic for the eBPF flow map, real CPU for
// the profilers, and real cost to attribute.
//
//	generator workload --name checkout --listen :8080 \
//	  --call http://payments.shop.svc:8080/pay --rps 20 --error-rate 0.02
func runWorkload(args []string) error {
	fs := flag.NewFlagSet("workload", flag.ExitOnError)
	name := fs.String("name", envOr("WORKLOAD_NAME", "service"), "service name used in logs")
	listen := fs.String("listen", ":8080", "listen address")
	call := fs.String("call", "", "downstream URL called --rps times per second (empty = leaf service)")
	rps := fs.Float64("rps", 10, "downstream calls per second")
	work := fs.Int("work", 2000, "hash rounds per request (CPU per request)")
	errRate := fs.Float64("error-rate", 0.01, "fraction of requests answered with 500")
	_ = fs.Parse(args)

	out := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 11))

	mux := http.DefaultServeMux // carries the pprof handlers
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		sum := burn(*work, req.URL.Path)
		status := http.StatusOK
		if r.Float64() < *errRate {
			status = http.StatusInternalServerError
		}
		body, _ := json.Marshal(map[string]any{"service": *name, "digest": fmt.Sprintf("%x", sum[:6]), "ok": status == 200})
		w.WriteHeader(status)
		_, _ = w.Write(body)
		attrs := []any{"method", req.Method, "path", req.URL.Path, "status", status,
			"duration_ms", time.Since(start).Milliseconds(), "trace_id", fmt.Sprintf("%032x", r.Uint64())}
		switch {
		case status >= 500:
			out.Error("request failed", attrs...)
		case r.Float64() < 0.03:
			out.Warn("slow request", attrs...)
		default:
			out.Info("request", attrs...)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *call != "" && *rps > 0 {
		go callLoop(ctx, out, *name, *call, *rps)
	}
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	out.Info("workload listening", "service", *name, "addr", *listen, "downstream", *call)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// burn does deterministic CPU work so profiles have a recognisable hot
// path (crypto/sha256 under main.burn).
func burn(rounds int, seed string) [32]byte {
	sum := sha256.Sum256([]byte(seed))
	for i := 0; i < rounds; i++ {
		sum = sha256.Sum256(sum[:])
	}
	return sum
}

func callLoop(ctx context.Context, out *slog.Logger, name, url string, rps float64) {
	client := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(time.Duration(float64(time.Second) / rps))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		resp, err := client.Get(url)
		if err != nil {
			out.Error("downstream call failed", "service", name, "url", url, "err", err.Error())
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			out.Warn("downstream returned error", "service", name, "url", url, "status", resp.StatusCode,
				"downstream", strings.TrimPrefix(url, "http://"))
		}
	}
}
