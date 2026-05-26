// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package clickhouse owns the time-series plane: pod-second samples,
// rollups, and Savings Plan replay.
//
// Schema lives in schema.sql — applied idempotently on startup.

package clickhouse

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
)

//go:embed schema.sql
var schema string

type Options struct {
	DSN string // e.g. clickhouse://user:pass@host:9000/kubehero
}

// Open connects, runs the idempotent schema init, and returns the handle.
func Open(ctx context.Context, log *slog.Logger, opts Options) (*sql.DB, error) {
	if opts.DSN == "" {
		return nil, errors.New("CLICKHOUSE_URL is not set")
	}
	conn, err := sql.Open("clickhouse", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("clickhouse open: %w", err)
	}
	conn.SetMaxOpenConns(10)
	conn.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.PingContext(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}

	if err := applySchema(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse schema: %w", err)
	}
	log.Info("clickhouse connected + schema applied")
	return conn, nil
}

// applySchema runs every CREATE TABLE / MATERIALIZED VIEW in schema.sql.
// All statements use IF NOT EXISTS so re-applying is a no-op.
func applySchema(ctx context.Context, conn *sql.DB) error {
	// Split on trailing semicolons; preserve the statements themselves.
	stmts := strings.Split(schema, ";")
	for _, s := range stmts {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, trimmed); err != nil {
			return fmt.Errorf("exec %q: %w", firstLine(trimmed), err)
		}
	}
	return nil
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
