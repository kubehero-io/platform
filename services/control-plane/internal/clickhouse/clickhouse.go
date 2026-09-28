// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package clickhouse owns the time-series plane: pod-second cost
// samples and their rollups, plus every signal added in 0.3 — logs,
// profiles, network flows, container usage, cluster events and node
// cost.
//
// Schema lives in migrations/NNNN_*.sql, applied in order on startup
// and recorded in kubehero_schema_migrations so each file runs once.

package clickhouse

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Options struct {
	DSN string // e.g. clickhouse://user:pass@host:9000/kubehero
}

// Open connects, applies pending schema migrations, and returns the
// handle.
func Open(ctx context.Context, log *slog.Logger, opts Options) (*sql.DB, error) {
	if opts.DSN == "" {
		return nil, errors.New("CLICKHOUSE_URL is not set")
	}
	conn, err := sql.Open("clickhouse", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("clickhouse open: %w", err)
	}
	conn.SetMaxOpenConns(16)
	conn.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.PingContext(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}

	applied, err := Migrate(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse schema: %w", err)
	}
	log.Info("clickhouse connected + schema migrated", "applied", applied)
	return conn, nil
}

// Migration is one versioned schema file.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded schema files ordered by version.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: want NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", e.Name(), err)
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: e.Name(), SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("duplicate migration version %d", out[i].Version)
		}
	}
	return out, nil
}

// Migrate applies every embedded migration not yet recorded in
// kubehero_schema_migrations, in version order, and returns how many
// ran. Statements are idempotent (IF [NOT] EXISTS), so a crash between
// executing a file and recording it is safe to re-run.
func Migrate(ctx context.Context, conn *sql.DB) (int, error) {
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS kubehero_schema_migrations (
			version    UInt32,
			name       String,
			applied_at DateTime DEFAULT now()
		) ENGINE = ReplacingMergeTree(applied_at)
		  ORDER BY version`); err != nil {
		return 0, fmt.Errorf("create migrations table: %w", err)
	}

	done := map[int]bool{}
	rows, err := conn.QueryContext(ctx, `SELECT DISTINCT version FROM kubehero_schema_migrations`)
	if err != nil {
		return 0, fmt.Errorf("read applied migrations: %w", err)
	}
	for rows.Next() {
		var v uint32
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return 0, err
		}
		done[int(v)] = true
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	all, err := Migrations()
	if err != nil {
		return 0, err
	}
	applied := 0
	for _, m := range all {
		if done[m.Version] {
			continue
		}
		for _, stmt := range SplitStatements(m.SQL) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return applied, fmt.Errorf("%s: exec %q: %w", m.Name, firstLine(stmt), err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO kubehero_schema_migrations (version, name) VALUES (?, ?)`,
			uint32(m.Version), m.Name); err != nil {
			return applied, fmt.Errorf("record %s: %w", m.Name, err)
		}
		applied++
	}
	return applied, nil
}

// SplitStatements splits a migration file into executable statements.
// It strips `--` line comments (which may themselves contain
// semicolons) and splits on semicolons that sit outside single-quoted
// string literals. Empty statements are dropped.
func SplitStatements(sqlText string) []string {
	var (
		out     []string
		cur     strings.Builder
		inQuote bool
	)
	flush := func() {
		if stmt := strings.TrimSpace(cur.String()); stmt != "" {
			out = append(out, stmt)
		}
		cur.Reset()
	}
	for i := 0; i < len(sqlText); i++ {
		c := sqlText[i]
		switch {
		case inQuote:
			cur.WriteByte(c)
			switch {
			case c == '\\' && i+1 < len(sqlText):
				i++
				cur.WriteByte(sqlText[i])
			case c == '\'' && i+1 < len(sqlText) && sqlText[i+1] == '\'':
				i++
				cur.WriteByte(sqlText[i])
			case c == '\'':
				inQuote = false
			}
		case c == '\'':
			inQuote = true
			cur.WriteByte(c)
		case c == '-' && i+1 < len(sqlText) && sqlText[i+1] == '-':
			for i < len(sqlText) && sqlText[i] != '\n' {
				i++
			}
			cur.WriteByte('\n')
		case c == ';':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
