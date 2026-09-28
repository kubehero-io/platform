// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
)

// Query is one parameterised ClickHouse statement.
type Query struct {
	SQL  string
	Args []any
}

// StreamColumns are the column-backed stream labels in the order the
// fetch and full-identity bucket queries select them (label names in
// logschema.ColumnLabels, same order).
var StreamColumns = func() []string {
	out := make([]string, len(logschema.ColumnLabels))
	for i, l := range logschema.ColumnLabels {
		out[i], _ = logschema.Column(l)
	}
	return out
}()

const minuteNS = int64(time.Minute)

// LogsQuery fetches lines with ts in [fromNS, toNS), newest first
// unless forward. Columns: ts (unix ns), StreamColumns…, trace_id,
// labels, body.
func (p *Plan) LogsQuery(org string, fromNS, toNS int64, forward bool, limit int) Query {
	order := "DESC"
	if forward {
		order = "ASC"
	}
	var b strings.Builder
	b.WriteString("SELECT toUnixTimestamp64Nano(ts), ")
	b.WriteString(strings.Join(StreamColumns, ", "))
	b.WriteString(", trace_id, labels, body FROM logs WHERE org_id = ?")
	b.WriteString(" AND ts >= fromUnixTimestamp64Nano(toInt64(?)) AND ts < fromUnixTimestamp64Nano(toInt64(?))")
	b.WriteString(" AND (" + p.Where.sql + ")")
	b.WriteString(" ORDER BY ts " + order + " LIMIT ?")
	args := append([]any{org, fromNS, toNS}, p.Where.args...)
	return Query{SQL: b.String(), Args: append(args, limit)}
}

// CountQuery counts matching stored lines in [fromNS, toNS) (SQL
// predicates only — an upper bound when the plan is not exact).
func (p *Plan) CountQuery(org string, fromNS, toNS int64) Query {
	sql := "SELECT count() FROM logs WHERE org_id = ?" +
		" AND ts >= fromUnixTimestamp64Nano(toInt64(?)) AND ts < fromUnixTimestamp64Nano(toInt64(?))" +
		" AND (" + p.Where.sql + ")"
	return Query{SQL: sql, Args: append([]any{org, fromNS, toNS}, p.Where.args...)}
}

// groupSelect renders the label columns of a bucket query: the kept
// labels as l0…ln, or (keep == nil) every stream column plus the map.
// It returns the SELECT items, the GROUP BY items and the SELECT
// arguments; both lists are empty when keep is empty.
func groupSelect(keep []string) (sel, group []string, args []any) {
	if keep == nil {
		cols := append(append([]string{}, StreamColumns...), "labels")
		return cols, cols, nil
	}
	for i, name := range keep {
		alias := "l" + itoa(i)
		e := labelExpr(name)
		sel = append(sel, e.sql+" AS "+alias)
		group = append(group, alias)
		args = append(args, e.args...)
	}
	return sel, group, args
}

func joinItems(items []string, more ...string) string {
	return strings.Join(append(append([]string{}, items...), more...), ", ")
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

// BucketQuery is the raw-table range aggregation: per series (full
// stream identity, or only keep) and grid bucket, the line count and
// byte sum. Only valid when the plan is exact. Columns: labels (see
// groupSelect), bucket, count, bytes.
func (p *Plan) BucketQuery(org string, g Grid, keep []string, limit int) Query {
	sel, group, selArgs := groupSelect(keep)
	sql := "SELECT " + joinItems(sel, "intDiv(toUnixTimestamp64Nano(ts) - toInt64(?), toInt64(?)) AS b",
		"count() AS c", "sum(length(body)) AS bs") + " FROM logs WHERE org_id = ?" +
		" AND ts > fromUnixTimestamp64Nano(toInt64(?)) AND ts <= fromUnixTimestamp64Nano(toInt64(?))" +
		" AND (" + p.Where.sql + ")" +
		" GROUP BY " + joinItems(group, "b") + " LIMIT ?"
	args := append(selArgs, g.Origin+1, g.Width, org, g.DataStart(), g.DataEnd())
	args = append(args, p.Where.args...)
	return Query{SQL: sql, Args: append(args, limit)}
}

// RollupEligible reports whether a range aggregation over this plan
// can be answered from log_volume_1m: a bare selector over rollup
// labels, grouped only by rollup labels, on a minute-aligned grid.
func (p *Plan) RollupEligible(g Grid, keep []string) bool {
	if !p.RollupOK || keep == nil {
		return false
	}
	for _, k := range keep {
		if !logschema.IsRollupLabel(k) {
			return false
		}
	}
	return g.Origin%minuteNS == 0 && g.Width%minuteNS == 0
}

// RollupBucketQuery answers a range aggregation from log_volume_1m.
// Bucket b holds minutes [Origin + b·Width, Origin + (b+1)·Width);
// ec / eb are the edge counters of the minute at the bucket's start,
// which windowSums uses to turn [t - range, t) into (t - range, t].
// Columns: kept labels, bucket, count, bytes, edge count, edge bytes.
func (p *Plan) RollupBucketQuery(org string, g Grid, keep []string, limit int) Query {
	sel, group, selArgs := groupSelect(keep)
	originS, widthS := g.Origin/int64(time.Second), g.Width/int64(time.Second)
	minute := "toInt64(toUnixTimestamp(ts_minute))"
	sql := "SELECT " + joinItems(sel,
		"intDiv("+minute+" - toInt64(?), toInt64(?)) AS b",
		"sum(lines) AS c", "sum(bytes) AS bs",
		"sumIf(edge_lines, ("+minute+" - toInt64(?)) % toInt64(?) = 0) AS ec",
		"sumIf(edge_bytes, ("+minute+" - toInt64(?)) % toInt64(?) = 0) AS eb") +
		" FROM log_volume_1m WHERE org_id = ?" +
		" AND ts_minute >= toDateTime(toInt64(?)) AND ts_minute <= toDateTime(toInt64(?))" +
		" AND (" + p.Where.sql + ")" +
		" GROUP BY " + joinItems(group, "b") + " LIMIT ?"
	args := append(selArgs, originS, widthS, originS, widthS, originS, widthS, org,
		originS, g.DataEnd()/int64(time.Second))
	args = append(args, p.Where.args...)
	return Query{SQL: sql, Args: append(args, limit)}
}

// VolumeQuery groups lines per step bucket [start + k·step, …) and one
// label (column or map key; "" = no grouping). With rollup it reads
// log_volume_1m (the caller guarantees eligibility: RollupOK, a rollup
// label, minute-aligned start and step). Columns: group value, bucket,
// lines, bytes.
func (p *Plan) VolumeQuery(org string, startNS, endNS, stepNS int64, groupBy string, rollup bool) Query {
	grp := sqlPart{sql: "''"}
	if groupBy != "" {
		grp = labelExpr(groupBy)
	}
	if rollup {
		s, st := startNS/int64(time.Second), stepNS/int64(time.Second)
		sql := "SELECT " + grp.sql + " AS g, intDiv(toInt64(toUnixTimestamp(ts_minute)) - toInt64(?), toInt64(?)) AS b," +
			" sum(lines), sum(bytes) FROM log_volume_1m WHERE org_id = ?" +
			" AND ts_minute >= toDateTime(toInt64(?)) AND ts_minute < toDateTime(toInt64(?))" +
			" AND (" + p.Where.sql + ") GROUP BY g, b"
		args := append(append([]any{}, grp.args...), s, st, org, s, endNS/int64(time.Second))
		return Query{SQL: sql, Args: append(args, p.Where.args...)}
	}
	sql := "SELECT " + grp.sql + " AS g, intDiv(toUnixTimestamp64Nano(ts) - toInt64(?), toInt64(?)) AS b," +
		" count(), sum(length(body)) FROM logs WHERE org_id = ?" +
		" AND ts >= fromUnixTimestamp64Nano(toInt64(?)) AND ts < fromUnixTimestamp64Nano(toInt64(?))" +
		" AND (" + p.Where.sql + ") GROUP BY g, b"
	args := append(append([]any{}, grp.args...), startNS, stepNS, org, startNS, endNS)
	return Query{SQL: sql, Args: append(args, p.Where.args...)}
}

// LabelNamesQuery reports which column labels occur and which map keys
// exist, over a bounded sample of matching lines (label names are for
// autocomplete; an exhaustive scan of a busy hour is not worth it).
// Columns: one UInt8 per StreamColumns entry, then Array(String) keys.
func (p *Plan) LabelNamesQuery(org string, fromNS, toNS int64, sample, maxKeys int) Query {
	var flags []string
	for _, c := range StreamColumns {
		flags = append(flags, "max("+c+" != '')")
	}
	sql := "SELECT " + strings.Join(flags, ", ") + ", groupUniqArrayArray(?)(mapKeys(labels))" +
		" FROM (SELECT " + strings.Join(StreamColumns, ", ") + ", labels FROM logs WHERE org_id = ?" +
		" AND ts >= fromUnixTimestamp64Nano(toInt64(?)) AND ts < fromUnixTimestamp64Nano(toInt64(?))" +
		" AND (" + p.Where.sql + ") LIMIT ?)"
	args := append([]any{maxKeys, org, fromNS, toNS}, p.Where.args...)
	return Query{SQL: sql, Args: append(args, sample)}
}

// LabelValuesQuery lists distinct non-empty values of one label. With
// rollup (a rollup label and a RollupOK plan) it reads log_volume_1m.
func (p *Plan) LabelValuesQuery(org, name string, fromNS, toNS int64, limit int, rollup bool) Query {
	e := labelExpr(name)
	if rollup {
		sql := "SELECT DISTINCT " + e.sql + " AS v FROM log_volume_1m WHERE org_id = ?" +
			" AND ts_minute >= toDateTime(toInt64(?)) AND ts_minute < toDateTime(toInt64(?))" +
			" AND (" + p.Where.sql + ") AND v != '' ORDER BY v LIMIT ?"
		args := append(append([]any{}, e.args...), org, fromNS/int64(time.Minute)*60, ceilDiv(toNS, int64(time.Minute))*60)
		args = append(args, p.Where.args...)
		return Query{SQL: sql, Args: append(args, limit)}
	}
	sql := "SELECT DISTINCT " + e.sql + " AS v FROM logs WHERE org_id = ?" +
		" AND ts >= fromUnixTimestamp64Nano(toInt64(?)) AND ts < fromUnixTimestamp64Nano(toInt64(?))" +
		" AND (" + p.Where.sql + ") AND v != '' ORDER BY v LIMIT ?"
	args := append(append([]any{}, e.args...), org, fromNS, toNS)
	args = append(args, p.Where.args...)
	return Query{SQL: sql, Args: append(args, limit)}
}

func ceilDiv(a, b int64) int64 {
	if a%b == 0 {
		return a / b
	}
	return a/b + 1
}
