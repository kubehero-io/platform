// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package telemetry

import (
	"math"
	"net/netip"
	"regexp"
	"strings"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
)

// orgID is the tenant every row is written under until orgs are wired
// end to end (IngestPodCost uses the same constant).
const orgID = "default"

// Retention per table (the TTLs in migrations/0002). Rows older than
// this would be deleted by the next merge, so they are dropped at the
// door instead of costing an insert.
const (
	logRetention     = 14 * 24 * time.Hour
	profileRetention = 14 * 24 * time.Hour
	flowRetention    = 30 * 24 * time.Hour
	usageRetention   = 35 * 24 * time.Hour
	eventRetention   = 90 * 24 * time.Hour
)

// Per-row caps beyond logschema's.
const (
	maxStackDepth     = 1024
	maxFrameLen       = 1024
	maxEventMessage   = 4096
	maxEventReason    = 256
	maxEventAttrs     = 32
	maxTraceIDLen     = 128
	maxStreamLen      = 32
	truncatedRootName = "[truncated]"
)

// clock stamps and bounds row timestamps for one request.
type clock struct {
	now time.Time
}

// stamp resolves a unix-nanosecond timestamp: 0 means "now", and
// anything more than MaxFutureSkew ahead or older than retention is
// rejected.
func (c clock) stamp(ns int64, retention time.Duration) (time.Time, bool) {
	if ns == 0 {
		return c.now, true
	}
	t := time.Unix(0, ns).UTC()
	if t.After(c.now.Add(clickhouse.MaxFutureSkew)) || t.Before(c.now.Add(-retention)) {
		return time.Time{}, false
	}
	return t, true
}

func (c clock) stampMS(ms int64, retention time.Duration) (time.Time, bool) {
	if ms == 0 {
		return c.now, true
	}
	if ms > math.MaxInt64/int64(time.Millisecond) || ms < math.MinInt64/int64(time.Millisecond) {
		return time.Time{}, false
	}
	return c.stamp(ms*int64(time.Millisecond), retention)
}

// extraLabels sanitises a map of extra labels: names become legal
// LogQL label names, reserved / column-backed names are dropped (the
// column wins), values are cleaned and truncated, and at most
// logschema.MaxLabels survive. Keys are visited in sorted order so the
// survivors of an over-full map are deterministic.
func extraLabels(in map[string]string, reserve int) map[string]string {
	if len(in) == 0 {
		return nil
	}
	budget := logschema.MaxLabels - reserve
	out := make(map[string]string, min(len(in), budget))
	for _, k := range sortedKeys(in) {
		if len(out) >= budget {
			break
		}
		name, _ := logschema.Truncate(logschema.SanitizeLabelName(logschema.CleanString(k)), logschema.MaxLabelNameLen)
		if name == "" || logschema.Reserved(name) {
			continue
		}
		v, _ := logschema.Truncate(logschema.CleanString(in[k]), logschema.MaxLabelValueLen)
		if v == "" {
			continue // an empty label is an absent label in LogQL
		}
		out[name] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Insertion sort is plenty for ≤ a few dozen labels and avoids an
	// allocation-heavy sort.Strings on the hot path.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

var traceIDRE = regexp.MustCompile(`^[0-9A-Za-z_.:-]+$`)

func traceID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxTraceIDLen || !traceIDRE.MatchString(s) {
		return ""
	}
	return s
}

// LogRow converts one entry; ok=false means it failed validation.
func (c clock) LogRow(cluster string, e *kuberov1.LogEntry) (clickhouse.LogRow, bool) {
	ts, ok := c.stamp(e.GetTsUnixNano(), logRetention)
	if !ok {
		return clickhouse.LogRow{}, false
	}
	src := e.GetSource()
	body, cut := logschema.Truncate(logschema.CleanString(e.GetBody()), logschema.MaxLineBytes)
	reserve := 0
	if cut {
		reserve = 1
	}
	labels := extraLabels(e.GetLabels(), reserve)
	if cut {
		if labels == nil {
			labels = make(map[string]string, 1)
		}
		labels[logschema.TruncatedLabel] = "true"
	}
	stream, _ := logschema.Truncate(strings.ToLower(logschema.Field(e.GetStream())), maxStreamLen)
	return clickhouse.LogRow{
		TS:           ts,
		OrgID:        orgID,
		ClusterID:    cluster,
		Namespace:    logschema.Field(src.GetNamespace()),
		Workload:     logschema.Field(src.GetWorkload()),
		WorkloadKind: logschema.Field(src.GetWorkloadKind()),
		Pod:          logschema.Field(src.GetPod()),
		Container:    logschema.Field(src.GetContainer()),
		Node:         logschema.Field(src.GetNode()),
		Team:         logschema.Field(src.GetTeam()),
		Stream:       stream,
		Level:        logschema.NormalizeLevel(e.GetLevel()),
		TraceID:      traceID(e.GetTraceId()),
		Labels:       labels,
		Body:         body,
	}, true
}

var (
	profileTypeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	unitRE        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	originRE      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// defaultUnit is the natural unit of the well-known profile types.
func defaultUnit(typ string) string {
	switch {
	case typ == "cpu", typ == "wall", typ == "mutex", typ == "block", strings.HasSuffix(typ, "_duration"):
		return "nanoseconds"
	case strings.HasSuffix(typ, "_space"):
		return "bytes"
	}
	return "count"
}

// ProfileResult is one profile's rows.
type ProfileResult struct {
	Samples []clickhouse.ProfileSampleRow
	Stacks  []clickhouse.ProfileStackRow // candidates; the service de-dups via StackCache
	Dropped int                          // stack samples discarded
}

// ProfileRows converts one profile. ok=false rejects the whole profile
// (bad timestamp or type); individual bad samples are only counted.
// Identical stacks within the profile are merged.
func (c clock) ProfileRows(cluster string, p *kuberov1.Profile) (ProfileResult, bool) {
	var res ProfileResult
	ts, ok := c.stamp(p.GetTsUnixNano(), profileRetention)
	typ := strings.ToLower(strings.TrimSpace(p.GetType()))
	if typ == "" {
		typ = "cpu"
	}
	if !ok || !profileTypeRE.MatchString(typ) {
		res.Dropped = max(1, len(p.GetSamples()))
		return res, false
	}
	unit := strings.ToLower(strings.TrimSpace(p.GetUnit()))
	if !unitRE.MatchString(unit) {
		unit = defaultUnit(typ)
	}
	origin := strings.ToLower(strings.TrimSpace(p.GetOrigin()))
	if !originRE.MatchString(origin) {
		origin = "unknown"
	}
	src := p.GetSource()
	service := logschema.Field(p.GetService())
	if service == "" {
		service = logschema.Field(src.GetWorkload())
	}
	if service == "" {
		service = logschema.Field(src.GetPod())
	}
	if service == "" {
		service = "unknown"
	}
	labels := extraLabels(p.GetLabels(), 0)
	dur := p.GetDurationNano()
	if dur < 0 {
		dur = 0
	}
	base := clickhouse.ProfileSampleRow{
		TS:         ts.Truncate(time.Second),
		OrgID:      orgID,
		ClusterID:  cluster,
		Service:    service,
		Namespace:  logschema.Field(src.GetNamespace()),
		Workload:   logschema.Field(src.GetWorkload()),
		Pod:        logschema.Field(src.GetPod()),
		Container:  logschema.Field(src.GetContainer()),
		Node:       logschema.Field(src.GetNode()),
		Type:       typ,
		Unit:       unit,
		Origin:     origin,
		Labels:     labels,
		DurationNS: dur,
	}

	index := make(map[uint64]int, len(p.GetSamples()))
	for _, s := range p.GetSamples() {
		frames := cleanFrames(s.GetFrames())
		if len(frames) == 0 || s.GetValue() <= 0 {
			res.Dropped++
			continue
		}
		h := StackHash(frames)
		if i, seen := index[h]; seen {
			res.Samples[i].Value = satAdd(res.Samples[i].Value, s.GetValue())
			continue
		}
		row := base
		row.StackHash = h
		row.Value = s.GetValue()
		index[h] = len(res.Samples)
		res.Samples = append(res.Samples, row)
		res.Stacks = append(res.Stacks, clickhouse.ProfileStackRow{StackHash: h, Frames: frames, LastSeen: c.now.Truncate(time.Second)})
	}
	return res, true
}

// cleanFrames trims, cleans and bounds one stack. Over-deep stacks keep
// their leaf-most frames (where the time is spent) under a synthetic
// "[truncated]" root.
func cleanFrames(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	start := 0
	if len(in) > maxStackDepth {
		start = len(in) - (maxStackDepth - 1)
	}
	out := make([]string, 0, len(in)-start+1)
	if start > 0 {
		out = append(out, truncatedRootName)
	}
	for _, f := range in[start:] {
		f, _ = logschema.Truncate(logschema.CleanString(strings.TrimSpace(f)), maxFrameLen)
		if f == "" {
			f = "[unknown]"
		}
		out = append(out, f)
	}
	return out
}

func satAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

var (
	flowKinds = map[string]bool{"pod": true, "service": true, "node": true, "external": true}
	protocols = map[string]bool{"tcp": true, "udp": true, "icmp": true, "other": true}
)

// endpoint is a normalised flow endpoint.
type endpoint struct {
	kind, namespace, workload, pod, node, zone, ip, service, name string
}

func normEndpoint(e *kuberov1.FlowEndpoint) (endpoint, bool) {
	ref := e.GetPod()
	kind := strings.ToLower(strings.TrimSpace(e.GetKind()))
	if kind == "" {
		switch {
		case ref.GetPod() != "":
			kind = "pod"
		case e.GetService() != "":
			kind = "service"
		default:
			kind = "external"
		}
	}
	if !flowKinds[kind] {
		return endpoint{}, false
	}
	ip := strings.TrimSpace(e.GetIp())
	if addr, err := netip.ParseAddr(ip); err == nil {
		ip = addr.String()
	} else {
		ip = ""
	}
	ep := endpoint{
		kind:      kind,
		namespace: logschema.Field(ref.GetNamespace()),
		workload:  logschema.Field(ref.GetWorkload()),
		pod:       logschema.Field(ref.GetPod()),
		node:      logschema.Field(ref.GetNode()),
		zone:      logschema.Field(e.GetZone()),
		ip:        ip,
		service:   logschema.Field(e.GetService()),
		name:      logschema.Field(e.GetName()),
	}
	if ep.zone == "" {
		ep.zone = logschema.Field(ref.GetZone())
	}
	if ep.name == "" {
		switch {
		case ep.workload != "":
			ep.name = ep.workload
		case ep.service != "":
			ep.name = ep.service
		case ep.node != "":
			ep.name = ep.node
		default:
			ep.name = ep.ip
		}
	}
	return ep, true
}

// FlowRow converts one flow. Pricing needs the cluster's cloud, which
// the caller resolves once per request.
//
//   - cross_zone: both endpoint zones are known and differ;
//   - egress: the destination is outside the cluster and the flow was
//     observed leaving (direction egress);
//   - cost_usd: bytes/1e9 × the cloud's egress or cross-zone $/GB.
//
// Pod ↔ pod traffic is observed at both ends; both rows are kept with
// their direction and the query side prefers the ingress copy.
func (c clock) FlowRow(cluster, cloud string, pricing NetPricing, f *kuberov1.Flow) (clickhouse.FlowRow, bool) {
	ts, ok := c.stampMS(f.GetTsUnixMs(), flowRetention)
	if !ok {
		return clickhouse.FlowRow{}, false
	}
	src, ok1 := normEndpoint(f.GetSrc())
	dst, ok2 := normEndpoint(f.GetDst())
	if !ok1 || !ok2 || f.GetPort() < 0 || f.GetPort() > math.MaxUint16 {
		return clickhouse.FlowRow{}, false
	}
	if f.GetBytes() == 0 && f.GetPackets() == 0 {
		return clickhouse.FlowRow{}, false
	}
	proto := strings.ToLower(strings.TrimSpace(f.GetProtocol()))
	if !protocols[proto] {
		proto = "other"
	}
	dir := strings.ToLower(strings.TrimSpace(f.GetDirection()))
	switch dir {
	case "ingress", "egress":
	case "":
		// Only one end of an internet flow is observable, so the
		// direction follows from which end is external.
		if dst.kind == "external" && src.kind != "external" {
			dir = "egress"
		} else if src.kind == "external" && dst.kind != "external" {
			dir = "ingress"
		}
	default:
		dir = ""
	}
	window := f.GetWindowSec()
	if window < 0 {
		window = 0
	} else if window > math.MaxUint16 {
		window = math.MaxUint16
	}
	crossZone := src.zone != "" && dst.zone != "" && src.zone != dst.zone
	egress := dst.kind == "external" && dir == "egress"
	return clickhouse.FlowRow{
		TS: ts.Truncate(time.Second), OrgID: orgID, ClusterID: cluster, WindowSec: uint16(window),
		SrcKind: src.kind, SrcNamespace: src.namespace, SrcWorkload: src.workload, SrcPod: src.pod,
		SrcNode: src.node, SrcZone: src.zone, SrcIP: src.ip, SrcName: src.name,
		DstKind: dst.kind, DstNamespace: dst.namespace, DstWorkload: dst.workload, DstPod: dst.pod,
		DstNode: dst.node, DstZone: dst.zone, DstIP: dst.ip, DstService: dst.service, DstName: dst.name,
		Port: uint16(f.GetPort()), Protocol: proto, Direction: dir,
		Bytes: f.GetBytes(), Packets: f.GetPackets(), Retransmits: f.GetRetransmits(),
		CrossZone: crossZone, Egress: egress,
		CostUSD: pricing.FlowCost(cloud, f.GetBytes(), egress, crossZone),
	}, true
}

func finite32(v float64) (float32, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > math.MaxFloat32 {
		return 0, false
	}
	return float32(v), true
}

// UsageRow converts one container usage sample. Namespace, pod and
// container are required; usage must be a finite non-negative number;
// broken requests / limits read as 0 (unset).
func (c clock) UsageRow(cluster string, u *kuberov1.ContainerUsage) (clickhouse.UsageRow, bool) {
	ts, ok := c.stampMS(u.GetTsUnixMs(), usageRetention)
	src := u.GetSource()
	ns, pod, ctr := logschema.Field(src.GetNamespace()), logschema.Field(src.GetPod()), logschema.Field(src.GetContainer())
	cpu, cpuOK := finite32(u.GetCpuUsageCores())
	if !ok || ns == "" || pod == "" || ctr == "" || !cpuOK {
		return clickhouse.UsageRow{}, false
	}
	req, _ := finite32(u.GetCpuRequestCores())
	lim, _ := finite32(u.GetCpuLimitCores())
	reason, _ := logschema.Truncate(logschema.Field(u.GetLastTerminationReason()), maxEventReason)
	return clickhouse.UsageRow{
		TS: ts.Truncate(time.Second), OrgID: orgID, ClusterID: cluster,
		Namespace: ns, Workload: logschema.Field(src.GetWorkload()), WorkloadKind: logschema.Field(src.GetWorkloadKind()),
		Pod: pod, Container: ctr, Node: logschema.Field(src.GetNode()), Team: logschema.Field(src.GetTeam()),
		CPUUsageCores: cpu, MemWorkingSetBytes: u.GetMemWorkingSetBytes(),
		CPURequestCores: req, MemRequestBytes: u.GetMemRequestBytes(),
		CPULimitCores: lim, MemLimitBytes: u.GetMemLimitBytes(),
		Restarts: u.GetRestarts(), LastTerminationReason: reason,
	}, true
}

var eventKinds = map[string]string{
	// kind → default severity
	"oom_killed":         "warn",
	"crash_loop":         "warn",
	"image_pull_backoff": "warn",
	"unschedulable":      "warn",
	"evicted":            "warn",
	"node_not_ready":     "critical",
	"node_pressure":      "warn",
	"restarted":          "info",
	"warning":            "warn",
}

func normSeverity(s, kind string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info", "normal", "notice":
		return "info"
	case "warn", "warning":
		return "warn"
	case "critical", "crit", "error", "fatal":
		return "critical"
	}
	return eventKinds[kind]
}

// EventRow converts one cluster event. Unknown kinds are kept as
// "warning" (with the original kind in attributes) rather than lost.
func (c clock) EventRow(cluster string, e *kuberov1.ClusterEvent) (clickhouse.EventRow, bool) {
	ts, ok := c.stampMS(e.GetTsUnixMs(), eventRetention)
	if !ok {
		return clickhouse.EventRow{}, false
	}
	attrs := make(map[string]string, min(len(e.GetAttributes()), maxEventAttrs)+1)
	for _, k := range sortedKeys(e.GetAttributes()) {
		if len(attrs) >= maxEventAttrs {
			break
		}
		key, _ := logschema.Truncate(logschema.CleanString(k), logschema.MaxLabelNameLen)
		if key == "" {
			continue
		}
		v, _ := logschema.Truncate(logschema.CleanString(e.GetAttributes()[k]), logschema.MaxLabelValueLen)
		attrs[key] = v
	}
	kind := strings.ToLower(strings.TrimSpace(e.GetKind()))
	if _, known := eventKinds[kind]; !known {
		if kind != "" {
			attrs["original_kind"], _ = logschema.Truncate(logschema.CleanString(kind), 64)
		}
		kind = "warning"
	}
	count := e.GetCount()
	if count <= 0 {
		count = 1
	}
	src := e.GetSource()
	msg, _ := logschema.Truncate(logschema.CleanString(e.GetMessage()), maxEventMessage)
	reason, _ := logschema.Truncate(logschema.Field(e.GetReason()), maxEventReason)
	return clickhouse.EventRow{
		TS: ts.Truncate(time.Millisecond), OrgID: orgID, ClusterID: cluster,
		Kind: kind, Severity: normSeverity(e.GetSeverity(), kind),
		Namespace: logschema.Field(src.GetNamespace()), Workload: logschema.Field(src.GetWorkload()),
		Pod: logschema.Field(src.GetPod()), Container: logschema.Field(src.GetContainer()),
		Node: logschema.Field(src.GetNode()), Reason: reason, Message: msg,
		Attributes: attrs, Count: uint32(count),
	}, true
}
