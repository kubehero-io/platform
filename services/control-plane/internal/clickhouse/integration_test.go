// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

// Integration tests against a real ClickHouse. Run with:
//
//	docker run -d -p 19000:9000 -e CLICKHOUSE_USER=kubehero \
//	  -e CLICKHOUSE_PASSWORD=kubehero -e CLICKHOUSE_DB=kubehero \
//	  clickhouse/clickhouse-server:26.8-alpine
//	KUBEHERO_TEST_CLICKHOUSE_URL=clickhouse://kubehero:kubehero@localhost:19000/kubehero \
//	  go test -tags integration ./internal/clickhouse/
//
// Each test migrates its own throwaway database; the one in the URL is
// only used to create and drop it.
package clickhouse

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"testing"
	"time"
)

// OpenTestDB creates a throwaway database (kh_chtest_<random>) on the
// server named by KUBEHERO_TEST_CLICKHOUSE_URL, migrates it with Open,
// and drops it when the test ends — it never touches the database the
// DSN names, which other suites share. Skips when the variable is unset.
func OpenTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("KUBEHERO_TEST_CLICKHOUSE_URL")
	if dsn == "" {
		t.Skip("KUBEHERO_TEST_CLICKHOUSE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse KUBEHERO_TEST_CLICKHOUSE_URL: %v", err)
	}
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatal(err)
	}
	name := "kh_chtest_" + hex.EncodeToString(rnd[:])

	ctx := context.Background()
	admin, err := sql.Open("clickhouse", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name)
		_ = admin.Close()
	})

	u.Path = "/" + name
	db, err := Open(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{DSN: u.String()})
	if err != nil {
		t.Fatalf("open+migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrationsApplyAndAreIdempotent(t *testing.T) {
	db := OpenTestDB(t)
	ctx := context.Background()

	n, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if n != 0 {
		t.Fatalf("second migrate applied %d migrations, want 0", n)
	}
	for _, table := range []string{
		"pod_cost_1s", "workload_cost_1h", "node_cost_1s", "node_cost_1h",
		"pod_metadata", "logs", "log_volume_1m", "profile_stacks",
		"profile_samples", "net_flows", "net_flows_1h", "container_usage",
		"container_usage_5m", "cluster_events",
	} {
		var c uint64
		if err := db.QueryRowContext(ctx,
			`SELECT count() FROM system.tables WHERE database = currentDatabase() AND name = ?`, table).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c != 1 {
			t.Errorf("table %s missing", table)
		}
	}
}

// Spend rollups must price rows as rate × interval: two 5s samples at
// $0.01/s are $0.10, not $0.02.
func TestWorkloadRollupPricesRateTimesInterval(t *testing.T) {
	db := OpenTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour).Add(10 * time.Minute).UnixMilli()
	for i := 0; i < 2; i++ {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO pod_cost_1s (ts, org_id, cluster_id, node, namespace, pod, team, cost_center,
				nodepool, cloud, region, sku, lifecycle, gpu_kind, cpu_millicores, mem_bytes, gpu_util_pct,
				cost_usd_sec, recoverable_usd_sec, interval_sec, workload, workload_kind, zone,
				cpu_usage_millicores, mem_usage_bytes, cpu_cost_usd_sec, ram_cost_usd_sec)
			VALUES (?, 'default', 'c1', 'n1', 'shop', 'api-1', 'payments', '', 'general', 'aws',
				'us-east-1', 'm7i.large', 'on-demand', '', 500, 1073741824, 0,
				0.01, 0.002, 5, 'api', 'Deployment', 'us-east-1a', 250, 536870912, 0.006, 0.004)`,
			now+int64(i)*5000); err != nil {
			t.Fatal(err)
		}
	}
	var cost, cpuReqCoreSec, podSec float64
	if err := db.QueryRowContext(ctx, `
		SELECT sum(cost_usd), sum(cpu_request_core_seconds), sum(pod_seconds)
		FROM workload_cost_1h WHERE cluster_id = 'c1' AND workload = 'api'`).
		Scan(&cost, &cpuReqCoreSec, &podSec); err != nil {
		t.Fatal(err)
	}
	if math.Abs(cost-0.10) > 1e-9 {
		t.Errorf("cost = %v, want 0.10", cost)
	}
	if math.Abs(cpuReqCoreSec-5.0) > 1e-9 { // 0.5 cores × 10s
		t.Errorf("cpu_request_core_seconds = %v, want 5", cpuReqCoreSec)
	}
	if math.Abs(podSec-10) > 1e-9 {
		t.Errorf("pod_seconds = %v, want 10", podSec)
	}
}
