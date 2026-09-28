// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package loki makes KubeHero a drop-in Loki for existing agents and
// for Grafana: the push API (Promtail, Grafana Alloy, Fluent Bit,
// Vector — JSON or snappy protobuf) and the query API Grafana's Loki
// datasource uses (query_range, query, labels, label values, series,
// index volume/stats, buildinfo, ready), answered by the KubeHero logs
// engine in Loki's exact JSON shapes.
package loki

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/klauspost/compress/snappy"
	"google.golang.org/protobuf/encoding/protowire"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
	"github.com/kubehero-io/platform/services/control-plane/internal/telemetry"
)

// Body limits: compressed payload and decoded size.
const (
	MaxBodyBytes    = 16 << 20
	MaxDecodedBytes = 64 << 20
	chunkEntries    = 10_000
)

// LogWriter is the telemetry entry point pushes land in.
type LogWriter interface {
	WriteLogs(cluster string, entries []*kuberov1.LogEntry) (accepted, dropped int, err error)
}

var _ LogWriter = (*telemetry.Service)(nil)

// PushHandler serves POST /loki/api/v1/push.
type PushHandler struct {
	Writer LogWriter
	Auth   *httpauth.Authenticator
	// DefaultCluster attributes pushes that name no cluster (no cluster
	// label, no X-Scope-OrgID, no cluster-scoped token).
	DefaultCluster string
	Log            *slog.Logger
}

type pushEntry struct {
	ts       int64
	line     string
	metadata map[string]string
}

type pushStream struct {
	labels  map[string]string
	entries []pushEntry
}

func (h *PushHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	ctx, aerr := h.Auth.Require(r.Context(), r.Header, auth.RoleMember)
	if aerr != nil {
		httpauth.WriteError(w, aerr)
		return
	}
	body, status, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	var streams []pushStream
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		streams, err = decodeJSONPush(body)
	case ct == "" || strings.HasPrefix(ct, "application/x-protobuf") || strings.HasPrefix(ct, "application/protobuf"):
		var raw []byte
		raw, err = decodeSnappy(body)
		if err == nil {
			streams, err = decodeProtoPush(raw)
		}
	default:
		http.Error(w, fmt.Sprintf("unsupported Content-Type %q (want application/x-protobuf or application/json)", ct), http.StatusUnsupportedMediaType)
		return
	}
	if err != nil {
		http.Error(w, "invalid push payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if status, err := h.write(ctx, r.Header.Get("X-Scope-OrgID"), streams); err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readBody reads a size-limited body, undoing Content-Encoding gzip.
func readBody(r *http.Request) ([]byte, int, error) {
	var src io.Reader = http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	switch enc := strings.ToLower(r.Header.Get("Content-Encoding")); enc {
	case "", "identity", "snappy":
	case "gzip":
		gz, err := gzip.NewReader(src)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close() //nolint:errcheck
		src = gz
	default:
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
	b, err := io.ReadAll(io.LimitReader(src, MaxDecodedBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("body larger than %d bytes", MaxBodyBytes)
		}
		return nil, http.StatusBadRequest, err
	}
	if len(b) > MaxDecodedBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("decoded body larger than %d bytes", MaxDecodedBytes)
	}
	return b, 0, nil
}

// decodeSnappy undoes Loki's snappy block compression, refusing
// payloads that would inflate past MaxDecodedBytes.
func decodeSnappy(b []byte) ([]byte, error) {
	n, err := snappy.DecodedLen(b)
	if err != nil {
		return nil, fmt.Errorf("snappy: %w", err)
	}
	if n > MaxDecodedBytes {
		return nil, fmt.Errorf("snappy payload decodes to %d bytes (limit %d)", n, MaxDecodedBytes)
	}
	out, err := snappy.Decode(nil, b)
	if err != nil {
		return nil, fmt.Errorf("snappy: %w", err)
	}
	return out, nil
}

// decodeJSONPush parses {"streams":[{"stream":{…},"values":[["ns","line",{meta}]]}]}.
func decodeJSONPush(b []byte) ([]pushStream, error) {
	var req struct {
		Streams []struct {
			Stream map[string]string   `json:"stream"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	out := make([]pushStream, 0, len(req.Streams))
	for i, s := range req.Streams {
		ps := pushStream{labels: s.Stream, entries: make([]pushEntry, 0, len(s.Values))}
		for j, v := range s.Values {
			if len(v) < 2 || len(v) > 3 {
				return nil, fmt.Errorf("streams[%d].values[%d]: want [timestamp, line] or [timestamp, line, metadata]", i, j)
			}
			var tsText, line string
			if err := json.Unmarshal(v[0], &tsText); err != nil {
				return nil, fmt.Errorf("streams[%d].values[%d]: timestamp must be a string of unix nanoseconds", i, j)
			}
			ts, err := strconv.ParseInt(tsText, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("streams[%d].values[%d]: bad timestamp %q", i, j, tsText)
			}
			if err := json.Unmarshal(v[1], &line); err != nil {
				return nil, fmt.Errorf("streams[%d].values[%d]: line must be a string", i, j)
			}
			e := pushEntry{ts: ts, line: line}
			if len(v) == 3 {
				if err := json.Unmarshal(v[2], &e.metadata); err != nil {
					return nil, fmt.Errorf("streams[%d].values[%d]: structured metadata must be an object of strings", i, j)
				}
			}
			ps.entries = append(ps.entries, e)
		}
		out = append(out, ps)
	}
	return out, nil
}

// decodeProtoPush parses logproto.PushRequest by field number:
//
//	PushRequest   { repeated StreamAdapter streams = 1; }
//	StreamAdapter { string labels = 1; repeated EntryAdapter entries = 2; uint64 hash = 3; }
//	EntryAdapter  { Timestamp timestamp = 1; string line = 2; repeated LabelPair structuredMetadata = 3; }
//	Timestamp     { int64 seconds = 1; int32 nanos = 2; }
//	LabelPair     { string name = 1; string value = 2; }
//
// Unknown fields are skipped, so newer clients keep working.
func decodeProtoPush(b []byte) ([]pushStream, error) {
	var out []pushStream
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		s, err := decodeStream(v)
		if err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

func decodeStream(b []byte) (pushStream, error) {
	var s pushStream
	var labelText string
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			labelText = string(v)
		case num == 2 && typ == protowire.BytesType:
			e, err := decodeEntry(v)
			if err != nil {
				return err
			}
			s.entries = append(s.entries, e)
		}
		return nil
	})
	if err != nil {
		return s, err
	}
	labels, err := logql.ParseLabels(labelText)
	if err != nil {
		return s, fmt.Errorf("stream labels %q: %w", labelText, err)
	}
	s.labels = labels
	return s, nil
}

func decodeEntry(b []byte) (pushEntry, error) {
	var e pushEntry
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			var secs, nanos int64
			if err := eachField(v, func(n protowire.Number, t protowire.Type, _ []byte, x uint64) error {
				switch {
				case n == 1 && t == protowire.VarintType:
					secs = int64(x)
				case n == 2 && t == protowire.VarintType:
					nanos = int64(int32(x))
				}
				return nil
			}); err != nil {
				return err
			}
			e.ts = secs*int64(time.Second) + nanos
		case num == 2 && typ == protowire.BytesType:
			e.line = string(v)
		case num == 3 && typ == protowire.BytesType:
			var name, value string
			if err := eachField(v, func(n protowire.Number, t protowire.Type, vv []byte, _ uint64) error {
				if t == protowire.BytesType {
					switch n {
					case 1:
						name = string(vv)
					case 2:
						value = string(vv)
					}
				}
				return nil
			}); err != nil {
				return err
			}
			if e.metadata == nil {
				e.metadata = map[string]string{}
			}
			e.metadata[name] = value
		}
		return nil
	})
	return e, err
}

// eachField walks one protobuf message. For varint fields the value is
// passed as x; for length-delimited fields as v.
func eachField(b []byte, fn func(num protowire.Number, typ protowire.Type, v []byte, x uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			x, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			if err := fn(num, typ, nil, x); err != nil {
				return err
			}
			b = b[m:]
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			if err := fn(num, typ, v, 0); err != nil {
				return err
			}
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			b = b[m:]
		}
	}
	return nil
}

// Loki / Promtail / OTel label names that map onto KubeHero columns.
var (
	nsKeys        = []string{"namespace", "k8s_namespace_name", "namespace_name", "kubernetes_namespace_name"}
	podKeys       = []string{"pod", "pod_name", "k8s_pod_name", "kubernetes_pod_name"}
	containerKeys = []string{"container", "container_name", "k8s_container_name", "kubernetes_container_name"}
	nodeKeys      = []string{"node", "node_name", "k8s_node_name", "nodename", "host"}
	levelKeys     = []string{"level", "detected_level", "severity", "severity_text", "lvl", "loglevel", "log_level"}
	clusterKeys   = []string{"cluster", "k8s_cluster_name", "cluster_name"}
	traceKeys     = []string{"trace_id", "traceid", "traceID", "traceId"}
	// Owner labels, most specific first. The last group only names an
	// app, so it fills workload without claiming a controller kind.
	workloadKeys = []struct{ key, kind string }{
		{"workload", ""}, {"k8s_deployment_name", "Deployment"}, {"k8s_statefulset_name", "StatefulSet"},
		{"k8s_daemonset_name", "DaemonSet"}, {"k8s_cronjob_name", "CronJob"}, {"k8s_job_name", "Job"},
		{"app_kubernetes_io_name", ""}, {"app", ""}, {"k8s_app", ""}, {"service_name", ""},
	}
)

// take returns the first present key's value and removes the mapped
// keys from rest (except keepInRest ones, which also stay as labels).
func take(rest map[string]string, keys []string) string {
	for _, k := range keys {
		if v, ok := rest[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

func drop(rest map[string]string, keys []string) {
	for _, k := range keys {
		delete(rest, k)
	}
}

// ToEntries maps a Loki stream onto LogEntry protos (shared with the
// OTLP shim's tests) and reports the stream's own cluster label.
func toEntries(s pushStream) (cluster string, out []*kuberov1.LogEntry) {
	rest := make(map[string]string, len(s.labels))
	for k, v := range s.labels {
		rest[logschema.SanitizeLabelName(k)] = v
	}
	src := &kuberov1.PodRef{
		Namespace: take(rest, nsKeys),
		Pod:       take(rest, podKeys),
		Container: take(rest, containerKeys),
		Node:      take(rest, nodeKeys),
		Team:      rest["team"],
	}
	for _, wk := range workloadKeys {
		if v := rest[wk.key]; v != "" {
			src.Workload, src.WorkloadKind = v, wk.kind
			if wk.key == "workload" {
				src.WorkloadKind = rest["workload_kind"]
			}
			break
		}
	}
	level := take(rest, levelKeys)
	stream := rest["stream"]
	cluster = take(rest, clusterKeys)
	drop(rest, nsKeys)
	drop(rest, podKeys)
	drop(rest, containerKeys)
	drop(rest, []string{"node", "node_name", "k8s_node_name", "nodename"})
	drop(rest, levelKeys)
	drop(rest, clusterKeys)
	drop(rest, []string{"stream", "team", "workload", "workload_kind"})
	out = make([]*kuberov1.LogEntry, 0, len(s.entries))
	for _, e := range s.entries {
		entry := &kuberov1.LogEntry{TsUnixNano: e.ts, Source: src, Stream: stream, Level: level, Body: e.line, Labels: rest}
		if len(e.metadata) > 0 {
			// Structured metadata: trace ids and levels get their
			// columns, the rest joins the line's extra labels.
			labels := make(map[string]string, len(rest)+len(e.metadata))
			for k, v := range rest {
				labels[k] = v
			}
			for k, v := range e.metadata {
				name := logschema.SanitizeLabelName(k)
				switch {
				case contains(traceKeys, k) || contains(traceKeys, name):
					entry.TraceId = v
				case contains(levelKeys, name):
					entry.Level = v
				default:
					labels[name] = v
				}
			}
			entry.Labels = labels
		}
		out = append(out, entry)
	}
	return cluster, out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// write groups entries by cluster and hands them to telemetry. It
// returns the HTTP status for an error.
func (h *PushHandler) write(ctx context.Context, orgID string, streams []pushStream) (int, error) {
	byCluster := map[string][]*kuberov1.LogEntry{}
	var order []string
	for _, s := range streams {
		own, entries := toEntries(s)
		requested := own
		if requested == "" {
			requested = strings.TrimSpace(orgID)
		}
		cluster, err := telemetry.ResolveCluster(ctx, requested)
		if err != nil {
			return connectStatus(err), err
		}
		if cluster == "" {
			cluster = h.DefaultCluster
		}
		if _, seen := byCluster[cluster]; !seen {
			order = append(order, cluster)
		}
		byCluster[cluster] = append(byCluster[cluster], entries...)
	}
	for _, cluster := range order {
		entries := byCluster[cluster]
		for i := 0; i < len(entries); i += chunkEntries {
			if _, _, err := h.Writer.WriteLogs(cluster, entries[i:min(i+chunkEntries, len(entries))]); err != nil {
				return connectStatus(err), err
			}
		}
	}
	return 0, nil
}

// connectStatus maps telemetry's Connect errors onto HTTP statuses
// agents understand (429 and 503 are retried by Promtail and Alloy).
func connectStatus(err error) int {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return http.StatusInternalServerError
	}
	switch ce.Code() {
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
