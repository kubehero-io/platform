// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"database/sql"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
)

// wireDeps is everything the signal engines need from serve(). Both
// stores are optional: nil means that store is not configured and the
// engine must degrade (demo fixtures unless DemoFixturesDisabled, else
// FailedPrecondition) exactly like the ControlPlaneService RPCs do.
type wireDeps struct {
	Log                  *slog.Logger
	PG                   *sql.DB // nil = no Postgres
	CH                   *sql.DB // nil = no ClickHouse
	Handler              []connect.HandlerOption
	DemoFixturesDisabled bool
	// Alerts routes notifications by channel URL scheme; nil when no
	// channel is configured.
	Alerts *alerter.Router
}
