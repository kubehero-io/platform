// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package pgtest gives integration tests a private Postgres database
// (kh_query_<suffix>), dropped, recreated and migrated with the real
// golang-migrate migrations on every run. Point
// KUBEHERO_TEST_POSTGRES_URL at a throwaway server, e.g.
//
//	docker run --rm -d -p 15432:5432 -e POSTGRES_USER=kubehero \
//	  -e POSTGRES_PASSWORD=kubehero -e POSTGRES_DB=kubehero postgres:17-alpine
//	KUBEHERO_TEST_POSTGRES_URL=postgres://kubehero:kubehero@localhost:15432/kubehero?sslmode=disable
package pgtest

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // driver

	"github.com/kubehero-io/platform/services/control-plane/internal/db"
)

var suffixRE = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// Open returns a migrated database, skipping when the env var is unset.
// The database is dropped again when the test ends.
func Open(t testing.TB, suffix string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("KUBEHERO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("KUBEHERO_TEST_POSTGRES_URL not set")
	}
	if !suffixRE.MatchString(suffix) {
		t.Fatalf("bad suffix %q", suffix)
	}
	name := "kh_query_" + suffix
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close() //nolint:errcheck
	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS " + name + " WITH (FORCE)",
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
	conn, err := db.Open(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), db.Options{URL: u.String()})
	if err != nil {
		t.Fatalf("open+migrate %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer admin.Close() //nolint:errcheck
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return conn
}
