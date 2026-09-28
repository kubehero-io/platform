// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestOpenWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	calls := 0
	conn, err := openWithRetry(context.Background(), log, "pg", 10*time.Second, func(context.Context) (*sql.DB, error) {
		calls++
		if calls < 2 {
			return nil, errors.New("connection refused")
		}
		return &sql.DB{}, nil
	})
	if err != nil || conn == nil || calls != 2 {
		t.Fatalf("conn=%v err=%v calls=%d", conn, err, calls)
	}
}

func TestOpenWithRetryGivesUpAfterMaxWait(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()
	_, err := openWithRetry(context.Background(), log, "pg", 1500*time.Millisecond, func(context.Context) (*sql.DB, error) {
		return nil, errors.New("down")
	})
	if err == nil {
		t.Fatal("want error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s, should give up near maxWait", time.Since(start))
	}
}
