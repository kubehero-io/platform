// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package chtest gives integration tests a private, freshly migrated
// ClickHouse database on the shared test server.
//
// The server named by KUBEHERO_TEST_CLICKHOUSE_URL is shared by every
// test package (and other engineers' suites), and `go test ./...` runs
// packages in parallel — so each caller names its own database, which
// is emptied and migrated here and dropped when the test ends. Tests
// skip when the variable is unset.
package chtest

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
)

var dbName = regexp.MustCompile(`^kh_[a-z0-9_]{1,40}$`)

// DB is a migrated private database.
type DB struct {
	SQL    *sql.DB
	Native driver.Conn
	DSN    string
}

// Open empties (drops every table and view in) database `name`,
// migrates it, and returns both connection flavours. name must look
// like kh_<suffix>.
func Open(t testing.TB, name string) *DB {
	t.Helper()
	base := os.Getenv("KUBEHERO_TEST_CLICKHOUSE_URL")
	if base == "" {
		t.Skip("KUBEHERO_TEST_CLICKHOUSE_URL not set")
	}
	if !dbName.MatchString(name) {
		t.Fatalf("chtest: database name %q must match %s", name, dbName)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	admin, err := sql.Open("clickhouse", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close() //nolint:errcheck
	// name is validated above; identifiers cannot be bound.
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+name); err != nil {
		t.Fatal(err)
	}
	// The test server is shared and disk is tight: drop the database
	// when the test ends (cleanups run last-in-first-out, so this runs
	// after the connections below are closed). KUBEHERO_TEST_KEEP_DB=1
	// keeps it for debugging.
	if os.Getenv("KUBEHERO_TEST_KEEP_DB") == "" {
		t.Cleanup(func() {
			db, err := sql.Open("clickhouse", base)
			if err != nil {
				return
			}
			defer db.Close() //nolint:errcheck
			_, _ = db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" SYNC")
		})
	}
	rows, err := admin.QueryContext(ctx,
		`SELECT name, engine FROM system.tables WHERE database = ? ORDER BY engine = 'MaterializedView' DESC`, name)
	if err != nil {
		t.Fatal(err)
	}
	var drops []string
	for rows.Next() {
		var tbl, engine string
		if err := rows.Scan(&tbl, &engine); err != nil {
			t.Fatal(err)
		}
		kind := "TABLE"
		if engine == "MaterializedView" {
			kind = "VIEW"
		}
		drops = append(drops, "DROP "+kind+" IF EXISTS "+name+".`"+tbl+"`")
	}
	_ = rows.Close()
	for _, d := range drops {
		if _, err := admin.ExecContext(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	u.Path = "/" + name
	dsn := u.String()
	db, err := clickhouse.Open(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), clickhouse.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("chtest: open+migrate %s: %v", name, err)
	}
	native, err := clickhouse.OpenNative(ctx, dsn)
	if err != nil {
		_ = db.Close()
		t.Fatalf("chtest: native %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = native.Close()
		_ = db.Close()
	})
	return &DB{SQL: db, Native: native, DSN: dsn}
}

// Count runs a `SELECT count() ...` style query returning one integer.
func (d *DB) Count(t testing.TB, query string, args ...any) uint64 {
	t.Helper()
	var n uint64
	if err := d.SQL.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
