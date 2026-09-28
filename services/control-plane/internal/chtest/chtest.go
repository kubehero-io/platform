// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package chtest gives integration tests a private, freshly migrated
// ClickHouse database. The shared test server is used by several
// engineers' suites at once, so each package gets its own database
// (kh_query_<suffix>) that is dropped and recreated per run — never the
// DSN's default database.
package chtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	_ "github.com/ClickHouse/clickhouse-go/v2" // driver

	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
)

var suffixRE = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// Open returns a migrated database kh_query_<suffix>, skipping the test
// when KUBEHERO_TEST_CLICKHOUSE_URL is unset. The database is dropped
// again when the test ends (disk on shared test servers is scarce).
func Open(t testing.TB, suffix string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("KUBEHERO_TEST_CLICKHOUSE_URL")
	if dsn == "" {
		t.Skip("KUBEHERO_TEST_CLICKHOUSE_URL not set")
	}
	if !suffixRE.MatchString(suffix) {
		t.Fatalf("bad suffix %q", suffix)
	}
	name := "kh_query_" + suffix
	ctx := context.Background()

	admin, err := sql.Open("clickhouse", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close() //nolint:errcheck
	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS " + name + " SYNC",
		"CREATE DATABASE " + name,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := sql.Open("clickhouse", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clickhouse.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		admin, err := sql.Open("clickhouse", dsn)
		if err != nil {
			return
		}
		defer admin.Close() //nolint:errcheck
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" SYNC")
	})
	return db
}

// Insert writes rows into table in one batch (clickhouse-go turns a
// prepared statement inside a transaction into a single block insert).
func Insert(t testing.TB, db *sql.DB, table string, cols []string, rows [][]any) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf("INSERT INTO %s (%s)", table, strings.Join(cols, ", ")))
	if err != nil {
		t.Fatalf("prepare %s: %v", table, err)
	}
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r...); err != nil {
			t.Fatalf("insert %s: %v", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit %s: %v", table, err)
	}
}
