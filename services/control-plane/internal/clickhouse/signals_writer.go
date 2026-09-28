// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// OpenNative opens a native-protocol connection pool on the same DSN
// as Open. The signal write path uses it for column-oriented batch
// inserts (one Append per column instead of one per value, which is
// what makes 100k-row log batches cheap) and the logs engine for
// streaming reads. Schema migrations are Open's job; call this after
// Open succeeded.
func OpenNative(ctx context.Context, dsn string) (driver.Conn, error) {
	if dsn == "" {
		return nil, errors.New("CLICKHOUSE_URL is not set")
	}
	opts, err := chgo.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("clickhouse dsn: %w", err)
	}
	if opts.MaxOpenConns <= 0 {
		opts.MaxOpenConns = 16 // batch writers + concurrent log queries
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.Compression == nil {
		// Log bodies compress 5–10×; LZ4 costs little CPU on either end.
		opts.Compression = &chgo.Compression{Method: chgo.CompressionLZ4}
	}
	conn, err := chgo.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse open (native): %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping (native): %w", err)
	}
	return conn, nil
}

// ─── Rows ────────────────────────────────────────────────────────────────
// Plain Go mirrors of the 0002 signal tables. The telemetry service
// builds them from validated protos; SignalWriter stores them.

// LogRow is one log line (table logs).
type LogRow struct {
	TS           time.Time // UTC, nanosecond precision
	OrgID        string
	ClusterID    string
	Namespace    string
	Workload     string
	WorkloadKind string
	Pod          string
	Container    string
	Node         string
	Team         string
	Stream       string
	Level        string
	TraceID      string
	Labels       map[string]string
	Body         string
}

// ProfileSampleRow is one aggregated stack of one profile
// (table profile_samples); frames live in profile_stacks.
type ProfileSampleRow struct {
	TS         time.Time
	OrgID      string
	ClusterID  string
	Service    string
	Namespace  string
	Workload   string
	Pod        string
	Container  string
	Node       string
	Type       string
	Unit       string
	Origin     string
	Labels     map[string]string
	StackHash  uint64
	Value      int64
	DurationNS int64
}

// ProfileStackRow is one content-addressed stack (table profile_stacks).
type ProfileStackRow struct {
	StackHash uint64
	Frames    []string // root → leaf
	LastSeen  time.Time
}

// FlowRow is one priced network flow (table net_flows).
type FlowRow struct {
	TS           time.Time
	OrgID        string
	ClusterID    string
	WindowSec    uint16
	SrcKind      string
	SrcNamespace string
	SrcWorkload  string
	SrcPod       string
	SrcNode      string
	SrcZone      string
	SrcIP        string
	SrcName      string
	DstKind      string
	DstNamespace string
	DstWorkload  string
	DstPod       string
	DstNode      string
	DstZone      string
	DstIP        string
	DstService   string
	DstName      string
	Port         uint16
	Protocol     string
	Direction    string
	Bytes        uint64
	Packets      uint64
	Retransmits  uint32
	CrossZone    bool
	Egress       bool
	CostUSD      float64
}

// UsageRow is one container utilisation sample (table container_usage).
type UsageRow struct {
	TS                    time.Time
	OrgID                 string
	ClusterID             string
	Namespace             string
	Workload              string
	WorkloadKind          string
	Pod                   string
	Container             string
	Node                  string
	Team                  string
	CPUUsageCores         float32
	MemWorkingSetBytes    uint64
	CPURequestCores       float32
	MemRequestBytes       uint64
	CPULimitCores         float32
	MemLimitBytes         uint64
	Restarts              uint32
	LastTerminationReason string
}

// EventRow is one Kubernetes health event (table cluster_events).
type EventRow struct {
	TS         time.Time // millisecond precision
	OrgID      string
	ClusterID  string
	Kind       string
	Severity   string
	Namespace  string
	Workload   string
	Pod        string
	Container  string
	Node       string
	Reason     string
	Message    string
	Attributes map[string]string
	Count      uint32
}

// SignalWriter stores signal rows with native column-oriented batch
// inserts. Each call is one INSERT; batching across requests is the
// caller's job (see internal/batcher). Table and column names are
// compile-time constants — nothing user-supplied reaches the SQL text.
type SignalWriter struct {
	Conn driver.Conn
}

// errNoConn is returned by every write when the writer has no
// connection (ClickHouse not configured).
var errNoConn = errors.New("clickhouse native connection not configured")

func (w *SignalWriter) send(ctx context.Context, table string, cols []string, values ...any) error {
	if w == nil || w.Conn == nil {
		return errNoConn
	}
	batch, err := w.Conn.PrepareBatch(ctx, "INSERT INTO "+table+" ("+strings.Join(cols, ", ")+")")
	if err != nil {
		return fmt.Errorf("prepare %s: %w", table, err)
	}
	for i, v := range values {
		if err := batch.Column(i).Append(v); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("%s.%s: %w", table, cols[i], err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("insert %s: %w", table, err)
	}
	return nil
}

var logCols = []string{"ts", "org_id", "cluster_id", "namespace", "workload", "workload_kind",
	"pod", "container", "node", "team", "stream", "level", "trace_id", "labels", "body"}

// WriteLogs inserts log lines.
func (w *SignalWriter) WriteLogs(ctx context.Context, rows []LogRow) error {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	ts := make([]time.Time, n)
	str := make([][]string, 12)
	for i := range str {
		str[i] = make([]string, n)
	}
	labels := make([]map[string]string, n)
	body := make([]string, n)
	for i := range rows {
		r := &rows[i]
		ts[i] = r.TS
		str[0][i], str[1][i], str[2][i] = r.OrgID, r.ClusterID, r.Namespace
		str[3][i], str[4][i], str[5][i] = r.Workload, r.WorkloadKind, r.Pod
		str[6][i], str[7][i], str[8][i] = r.Container, r.Node, r.Team
		str[9][i], str[10][i], str[11][i] = r.Stream, r.Level, r.TraceID
		labels[i] = nonNilMap(r.Labels)
		body[i] = r.Body
	}
	return w.send(ctx, "logs", logCols,
		ts, str[0], str[1], str[2], str[3], str[4], str[5], str[6], str[7], str[8], str[9], str[10], str[11],
		labels, body)
}

var profileSampleCols = []string{"ts", "org_id", "cluster_id", "service", "namespace", "workload",
	"pod", "container", "node", "type", "unit", "origin", "labels", "stack_hash", "value", "duration_ns"}

// WriteProfileSamples inserts per-stack profile values.
func (w *SignalWriter) WriteProfileSamples(ctx context.Context, rows []ProfileSampleRow) error {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	ts := make([]time.Time, n)
	str := make([][]string, 11)
	for i := range str {
		str[i] = make([]string, n)
	}
	labels := make([]map[string]string, n)
	hash := make([]uint64, n)
	value := make([]int64, n)
	dur := make([]int64, n)
	for i := range rows {
		r := &rows[i]
		ts[i] = r.TS
		str[0][i], str[1][i], str[2][i], str[3][i] = r.OrgID, r.ClusterID, r.Service, r.Namespace
		str[4][i], str[5][i], str[6][i], str[7][i] = r.Workload, r.Pod, r.Container, r.Node
		str[8][i], str[9][i], str[10][i] = r.Type, r.Unit, r.Origin
		labels[i] = nonNilMap(r.Labels)
		hash[i], value[i], dur[i] = r.StackHash, r.Value, r.DurationNS
	}
	return w.send(ctx, "profile_samples", profileSampleCols,
		ts, str[0], str[1], str[2], str[3], str[4], str[5], str[6], str[7], str[8], str[9], str[10],
		labels, hash, value, dur)
}

// WriteProfileStacks inserts stack definitions. profile_stacks is a
// ReplacingMergeTree on stack_hash, so rewriting a known stack only
// refreshes last_seen (which keeps it inside its TTL).
func (w *SignalWriter) WriteProfileStacks(ctx context.Context, rows []ProfileStackRow) error {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	hash := make([]uint64, n)
	frames := make([][]string, n)
	seen := make([]time.Time, n)
	for i := range rows {
		hash[i], frames[i], seen[i] = rows[i].StackHash, rows[i].Frames, rows[i].LastSeen
	}
	return w.send(ctx, "profile_stacks", []string{"stack_hash", "frames", "last_seen"}, hash, frames, seen)
}

var flowCols = []string{"ts", "org_id", "cluster_id", "window_sec",
	"src_kind", "src_namespace", "src_workload", "src_pod", "src_node", "src_zone", "src_ip", "src_name",
	"dst_kind", "dst_namespace", "dst_workload", "dst_pod", "dst_node", "dst_zone", "dst_ip", "dst_service", "dst_name",
	"port", "protocol", "direction", "bytes", "packets", "retransmits", "cross_zone", "egress", "cost_usd"}

// WriteFlows inserts priced flows.
func (w *SignalWriter) WriteFlows(ctx context.Context, rows []FlowRow) error {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	ts := make([]time.Time, n)
	win := make([]uint16, n)
	port := make([]uint16, n)
	str := make([][]string, 21)
	for i := range str {
		str[i] = make([]string, n)
	}
	bytes := make([]uint64, n)
	packets := make([]uint64, n)
	retrans := make([]uint32, n)
	cross := make([]uint8, n)
	egress := make([]uint8, n)
	cost := make([]float64, n)
	for i := range rows {
		r := &rows[i]
		ts[i], win[i], port[i] = r.TS, r.WindowSec, r.Port
		str[0][i], str[1][i] = r.OrgID, r.ClusterID
		str[2][i], str[3][i], str[4][i], str[5][i] = r.SrcKind, r.SrcNamespace, r.SrcWorkload, r.SrcPod
		str[6][i], str[7][i], str[8][i], str[9][i] = r.SrcNode, r.SrcZone, r.SrcIP, r.SrcName
		str[10][i], str[11][i], str[12][i], str[13][i] = r.DstKind, r.DstNamespace, r.DstWorkload, r.DstPod
		str[14][i], str[15][i], str[16][i], str[17][i], str[18][i] = r.DstNode, r.DstZone, r.DstIP, r.DstService, r.DstName
		str[19][i], str[20][i] = r.Protocol, r.Direction
		bytes[i], packets[i], retrans[i] = r.Bytes, r.Packets, r.Retransmits
		cross[i], egress[i], cost[i] = b2u(r.CrossZone), b2u(r.Egress), r.CostUSD
	}
	return w.send(ctx, "net_flows", flowCols,
		ts, str[0], str[1], win,
		str[2], str[3], str[4], str[5], str[6], str[7], str[8], str[9],
		str[10], str[11], str[12], str[13], str[14], str[15], str[16], str[17], str[18],
		port, str[19], str[20], bytes, packets, retrans, cross, egress, cost)
}

var usageCols = []string{"ts", "org_id", "cluster_id", "namespace", "workload", "workload_kind",
	"pod", "container", "node", "team", "cpu_usage_cores", "mem_working_set_bytes",
	"cpu_request_cores", "mem_request_bytes", "cpu_limit_cores", "mem_limit_bytes",
	"restarts", "last_termination_reason"}

// WriteUsage inserts container utilisation samples.
func (w *SignalWriter) WriteUsage(ctx context.Context, rows []UsageRow) error {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	ts := make([]time.Time, n)
	str := make([][]string, 10)
	for i := range str {
		str[i] = make([]string, n)
	}
	cpuUse, cpuReq, cpuLim := make([]float32, n), make([]float32, n), make([]float32, n)
	memWS, memReq, memLim := make([]uint64, n), make([]uint64, n), make([]uint64, n)
	restarts := make([]uint32, n)
	for i := range rows {
		r := &rows[i]
		ts[i] = r.TS
		str[0][i], str[1][i], str[2][i], str[3][i] = r.OrgID, r.ClusterID, r.Namespace, r.Workload
		str[4][i], str[5][i], str[6][i], str[7][i] = r.WorkloadKind, r.Pod, r.Container, r.Node
		str[8][i], str[9][i] = r.Team, r.LastTerminationReason
		cpuUse[i], cpuReq[i], cpuLim[i] = r.CPUUsageCores, r.CPURequestCores, r.CPULimitCores
		memWS[i], memReq[i], memLim[i] = r.MemWorkingSetBytes, r.MemRequestBytes, r.MemLimitBytes
		restarts[i] = r.Restarts
	}
	return w.send(ctx, "container_usage", usageCols,
		ts, str[0], str[1], str[2], str[3], str[4], str[5], str[6], str[7], str[8],
		cpuUse, memWS, cpuReq, memReq, cpuLim, memLim, restarts, str[9])
}

var eventCols = []string{"ts", "org_id", "cluster_id", "kind", "severity", "namespace", "workload",
	"pod", "container", "node", "reason", "message", "attributes", "count"}

// WriteEvents inserts cluster events.
func (w *SignalWriter) WriteEvents(ctx context.Context, rows []EventRow) error {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	ts := make([]time.Time, n)
	str := make([][]string, 11)
	for i := range str {
		str[i] = make([]string, n)
	}
	attrs := make([]map[string]string, n)
	count := make([]uint32, n)
	for i := range rows {
		r := &rows[i]
		ts[i] = r.TS
		str[0][i], str[1][i], str[2][i], str[3][i] = r.OrgID, r.ClusterID, r.Kind, r.Severity
		str[4][i], str[5][i], str[6][i], str[7][i] = r.Namespace, r.Workload, r.Pod, r.Container
		str[8][i], str[9][i], str[10][i] = r.Node, r.Reason, r.Message
		attrs[i] = nonNilMap(r.Attributes)
		count[i] = r.Count
	}
	return w.send(ctx, "cluster_events", eventCols,
		ts, str[0], str[1], str[2], str[3], str[4], str[5], str[6], str[7], str[8], str[9], str[10],
		attrs, count)
}

func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
