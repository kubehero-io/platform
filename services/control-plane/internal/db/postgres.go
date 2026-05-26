// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package db owns KubeHero's control-plane persistence. Postgres is
// where metadata, policies, audit log, and sessions live. ClickHouse
// is a separate concern — see internal/clickhouse.

package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Options configures the Postgres handle.
type Options struct {
	URL             string // e.g. postgres://u:p@h:5432/d?sslmode=require
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Open returns a connected, migrated *sql.DB. Migrations run to the
// latest version before the handle is returned.
func Open(ctx context.Context, log *slog.Logger, opts Options) (*sql.DB, error) {
	if opts.URL == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	if opts.MaxOpenConns == 0 {
		opts.MaxOpenConns = 20
	}
	if opts.MaxIdleConns == 0 {
		opts.MaxIdleConns = 5
	}
	if opts.ConnMaxLifetime == 0 {
		opts.ConnMaxLifetime = 30 * time.Minute
	}

	conn, err := sql.Open("pgx", opts.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres open: %w", err)
	}
	conn.SetMaxOpenConns(opts.MaxOpenConns)
	conn.SetMaxIdleConns(opts.MaxIdleConns)
	conn.SetConnMaxLifetime(opts.ConnMaxLifetime)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.PingContext(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}

	if err := migrateUp(conn, log); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("postgres migrate: %w", err)
	}
	log.Info("postgres connected + migrated")
	return conn, nil
}

func migrateUp(conn *sql.DB, log *slog.Logger) error {
	driver, err := postgres.WithInstance(conn, &postgres.Config{})
	if err != nil {
		return err
	}
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	v, _, verr := m.Version()
	if verr == nil {
		log.Info("postgres migrated", "version", v)
	}
	return nil
}
