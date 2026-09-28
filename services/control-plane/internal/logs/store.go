// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logs

import (
	"context"

	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
)

// Row is one stored line with its stream labels (non-empty values).
type Row struct {
	TS      int64 // unix ns
	Labels  map[string]string
	TraceID string
	Body    string
}

// ScanRequest selects stored lines.
type ScanRequest struct {
	Selector *logql.LogSelectorExpr
	Plan     *logql.Plan // Plan.Where must hold for every returned row
	From, To int64       // [From, To) unix ns
	Forward  bool        // oldest first; newest first otherwise
	Max      int         // stop after this many rows
	Sample   float64     // keep about this share of rows (0 or ≥1 = all)
}

// ScanStats counts what a scan read.
type ScanStats struct {
	Rows  int64
	Bytes int64
}

// VolumeRequest groups line counts per step.
type VolumeRequest struct {
	Selector   *logql.LogSelectorExpr
	Plan       *logql.Plan
	From, To   int64 // [From, To)
	Step       int64
	GroupBy    string // "" = one series
	MaxScanned int    // Go-path cap (pipelines SQL cannot express)
}

// VolumeCell is one (group value, bucket) total.
type VolumeCell struct {
	Group  string
	Bucket int
	Lines  float64
	Bytes  float64
}

// Store is the log storage behind the engine: ClickHouse, or the
// in-memory demo data. Scan and the label listings honour only the
// plan's SQL part (callers run Plan.Stages on scanned rows); Range and
// Volume return final results.
type Store interface {
	logql.RangeSource
	Scan(ctx context.Context, req ScanRequest, fn func(Row) bool) (ScanStats, error)
	Count(ctx context.Context, plan *logql.Plan, sel *logql.LogSelectorExpr, from, to int64) (int64, error)
	Volume(ctx context.Context, req VolumeRequest) ([]VolumeCell, ScanStats, error)
	LabelNames(ctx context.Context, sel *logql.LogSelectorExpr, plan *logql.Plan, from, to int64) ([]string, error)
	LabelValues(ctx context.Context, sel *logql.LogSelectorExpr, plan *logql.Plan, name string, from, to int64, limit int) ([]string, error)
	// Series lists distinct stream label sets (the plan's SQL part).
	Series(ctx context.Context, sel *logql.LogSelectorExpr, plan *logql.Plan, from, to int64, limit int) ([]map[string]string, error)
	// Source labels results ("clickhouse" or "demo").
	Source() string
}

// streamLabels builds a line's stream labels from the column values
// (in logschema.ColumnLabels order) and the labels map.
func streamLabels(cols []string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(cols)+len(extra))
	for k, v := range extra {
		if v != "" {
			out[k] = v
		}
	}
	for i, v := range cols {
		if v != "" {
			out[logschema.ColumnLabels[i]] = v
		}
	}
	return out
}
