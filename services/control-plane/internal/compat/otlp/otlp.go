// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package otlp accepts OpenTelemetry logs over OTLP/HTTP (POST
// /v1/logs, protobuf or JSON), so the OpenTelemetry Collector, Grafana
// Alloy, Fluent Bit and the OTel SDKs can ship straight to KubeHero.
//
// Kubernetes resource attributes (k8s.namespace.name, k8s.pod.name,
// k8s.container.name, k8s.<kind>.name, k8s.node.name,
// k8s.cluster.name, service.name) map onto the log columns; severity
// maps onto the level; trace_id becomes hex. Everything else becomes
// extra labels with dots turned into underscores.
package otlp

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
	"github.com/kubehero-io/platform/services/control-plane/internal/telemetry"
)

// Body limits: compressed payload and decoded size.
const (
	MaxBodyBytes    = 16 << 20
	MaxDecodedBytes = 64 << 20
	chunkEntries    = 10_000
	maxBodyValue    = 64 << 10 // rendered structured bodies (the line cap applies later too)
)

// LogWriter is the telemetry entry point OTLP logs land in.
type LogWriter interface {
	WriteLogs(cluster string, entries []*kuberov1.LogEntry) (accepted, dropped int, err error)
}

var _ LogWriter = (*telemetry.Service)(nil)

// Handler serves POST /v1/logs.
type Handler struct {
	Writer         LogWriter
	Auth           *httpauth.Authenticator
	DefaultCluster string
	Log            *slog.Logger
}

// record is one log record with its resource attributes resolved.
type record struct {
	cluster string
	entry   *kuberov1.LogEntry
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	isJSON := false
	var data *logsv1.LogsData
	switch ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); ct {
	case "application/x-protobuf", "application/protobuf", "":
		data = &logsv1.LogsData{}
		// ExportLogsServiceRequest and LogsData are wire-identical:
		// field 1 is repeated ResourceLogs in both.
		if err := proto.Unmarshal(body, data); err != nil {
			http.Error(w, "invalid OTLP protobuf: "+err.Error(), http.StatusBadRequest)
			return
		}
	case "application/json":
		isJSON = true
		if data, err = decodeJSON(body); err != nil {
			http.Error(w, "invalid OTLP JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, fmt.Sprintf("unsupported Content-Type %q (want application/x-protobuf or application/json)", ct), http.StatusUnsupportedMediaType)
		return
	}
	records := toRecords(data)
	rejected, status, err := h.write(ctx, r.Header.Get("X-Scope-OrgID"), records)
	if err != nil {
		if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeResponse(w, isJSON, rejected)
}

func readBody(r *http.Request) ([]byte, int, error) {
	var src io.Reader = http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	switch enc := strings.ToLower(r.Header.Get("Content-Encoding")); enc {
	case "", "identity":
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

// writeResponse answers with ExportLogsServiceResponse in the request's
// encoding: empty on full success, partial_success when rows were
// rejected (OTLP/HTTP spec).
func writeResponse(w http.ResponseWriter, isJSON bool, rejected int64) {
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		if rejected == 0 {
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"partialSuccess": map[string]any{
			"rejectedLogRecords": strconv.FormatInt(rejected, 10),
			"errorMessage":       "log records failed validation (timestamp outside the accepted window or no cluster)",
		}})
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	if rejected == 0 {
		return
	}
	var ps []byte
	ps = protowire.AppendTag(ps, 1, protowire.VarintType)
	ps = protowire.AppendVarint(ps, uint64(rejected))
	ps = protowire.AppendTag(ps, 2, protowire.BytesType)
	ps = protowire.AppendString(ps, "log records failed validation (timestamp outside the accepted window or no cluster)")
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	out = protowire.AppendBytes(out, ps)
	_, _ = w.Write(out)
}

func (h *Handler) write(ctx context.Context, orgID string, records []record) (int64, int, error) {
	byCluster := map[string][]*kuberov1.LogEntry{}
	var order []string
	for _, rec := range records {
		requested := rec.cluster
		if requested == "" {
			requested = strings.TrimSpace(orgID)
		}
		cluster, err := telemetry.ResolveCluster(ctx, requested)
		if err != nil {
			return 0, connectStatus(err), err
		}
		if cluster == "" {
			cluster = h.DefaultCluster
		}
		if _, seen := byCluster[cluster]; !seen {
			order = append(order, cluster)
		}
		byCluster[cluster] = append(byCluster[cluster], rec.entry)
	}
	var rejected int64
	for _, cluster := range order {
		entries := byCluster[cluster]
		for i := 0; i < len(entries); i += chunkEntries {
			_, dropped, err := h.Writer.WriteLogs(cluster, entries[i:min(i+chunkEntries, len(entries))])
			if err != nil {
				return 0, connectStatus(err), err
			}
			rejected += int64(dropped)
		}
	}
	return rejected, 0, nil
}

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

// Workload owner attributes, most specific first.
var workloadAttrs = []struct{ key, kind string }{
	{"k8s.deployment.name", "Deployment"}, {"k8s.statefulset.name", "StatefulSet"},
	{"k8s.daemonset.name", "DaemonSet"}, {"k8s.cronjob.name", "CronJob"}, {"k8s.job.name", "Job"},
	{"k8s.replicaset.name", "ReplicaSet"},
}

// mapped attributes never also appear as extra labels.
var mappedAttrs = map[string]bool{
	"k8s.namespace.name": true, "k8s.pod.name": true, "k8s.container.name": true, "k8s.node.name": true,
	"k8s.cluster.name": true, "k8s.pod.uid": true, "k8s.deployment.name": true, "k8s.statefulset.name": true,
	"k8s.daemonset.name": true, "k8s.cronjob.name": true, "k8s.job.name": true, "k8s.replicaset.name": true,
	"log.iostream": true,
}

// toRecords flattens OTLP logs into LogEntry protos with their cluster.
func toRecords(data *logsv1.LogsData) []record {
	var out []record
	for _, rl := range data.GetResourceLogs() {
		res := attrMap(rl.GetResource().GetAttributes())
		src := &kuberov1.PodRef{
			Namespace: res["k8s.namespace.name"],
			Pod:       res["k8s.pod.name"],
			Container: res["k8s.container.name"],
			Node:      res["k8s.node.name"],
			PodUid:    res["k8s.pod.uid"],
		}
		for _, wa := range workloadAttrs {
			if v := res[wa.key]; v != "" {
				src.Workload, src.WorkloadKind = v, wa.kind
				break
			}
		}
		if src.Workload == "" {
			src.Workload = res["service.name"] // the app, not a controller: no kind
		}
		cluster := res["k8s.cluster.name"]
		stream := res["log.iostream"]
		resLabels := map[string]string{}
		for k, v := range res {
			if !mappedAttrs[k] {
				resLabels[logschema.SanitizeLabelName(k)] = v
			}
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				labels := make(map[string]string, len(resLabels)+len(lr.GetAttributes()))
				for k, v := range resLabels {
					labels[k] = v
				}
				recStream := stream
				for k, v := range attrMap(lr.GetAttributes()) {
					if k == "log.iostream" {
						recStream = v
						continue
					}
					labels[logschema.SanitizeLabelName(k)] = v
				}
				ts := int64(lr.GetTimeUnixNano())
				if ts == 0 {
					ts = int64(lr.GetObservedTimeUnixNano())
				}
				entry := &kuberov1.LogEntry{
					TsUnixNano: ts,
					Source:     src,
					Stream:     recStream,
					Level:      level(lr.GetSeverityNumber(), lr.GetSeverityText()),
					Body:       render(lr.GetBody(), 0),
					Labels:     labels,
				}
				if id := lr.GetTraceId(); len(id) == 16 && !allZero(id) {
					entry.TraceId = hex.EncodeToString(id)
				}
				out = append(out, record{cluster: cluster, entry: entry})
			}
		}
	}
	return out
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// level maps OTLP severity: numbers 1–4 trace, 5–8 debug, 9–12 info,
// 13–16 warn, 17–20 error, 21–24 fatal; else the text.
func level(num logsv1.SeverityNumber, text string) string {
	switch n := int(num); {
	case n >= 21:
		return "fatal"
	case n >= 17:
		return "error"
	case n >= 13:
		return "warn"
	case n >= 9:
		return "info"
	case n >= 5:
		return "debug"
	case n >= 1:
		return "trace"
	}
	return logschema.NormalizeLevel(text)
}

func attrMap(kvs []*commonv1.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if kv.GetKey() == "" {
			continue
		}
		out[kv.GetKey()] = render(kv.GetValue(), 0)
	}
	return out
}

// render turns an AnyValue into text: strings as-is, scalars in their
// natural form, bytes as base64, arrays and maps as JSON.
func render(v *commonv1.AnyValue, depth int) string {
	if v == nil {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonv1.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonv1.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	}
	b, err := json.Marshal(toJSON(v, depth))
	if err != nil || len(b) > maxBodyValue {
		return string(b[:min(len(b), maxBodyValue)])
	}
	return string(b)
}

func toJSON(v *commonv1.AnyValue, depth int) any {
	if v == nil || depth > 16 {
		return nil
	}
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_BoolValue:
		return x.BoolValue
	case *commonv1.AnyValue_IntValue:
		return x.IntValue
	case *commonv1.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonv1.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonv1.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, toJSON(e, depth+1))
		}
		return out
	case *commonv1.AnyValue_KvlistValue:
		out := map[string]any{}
		for _, kv := range x.KvlistValue.GetValues() {
			out[kv.GetKey()] = toJSON(kv.GetValue(), depth+1)
		}
		return out
	}
	return nil
}

// ─── OTLP/JSON ───────────────────────────────────────────────────────────
// The OTLP JSON encoding is protojson with two twists: trace and span
// ids are hex (not base64), and int64s may be strings or numbers. A
// small typed decoder handles both instead of protojson.

type jsonAny struct {
	StringValue *string  `json:"stringValue"`
	BoolValue   *bool    `json:"boolValue"`
	IntValue    *flexInt `json:"intValue"`
	DoubleValue *float64 `json:"doubleValue"`
	BytesValue  *string  `json:"bytesValue"`
	ArrayValue  *struct {
		Values []jsonAny `json:"values"`
	} `json:"arrayValue"`
	KvlistValue *struct {
		Values []jsonKV `json:"values"`
	} `json:"kvlistValue"`
}

type jsonKV struct {
	Key   string  `json:"key"`
	Value jsonAny `json:"value"`
}

// flexInt accepts 123 or "123".
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		u, uerr := strconv.ParseUint(s, 10, 64)
		if uerr != nil {
			return fmt.Errorf("bad integer %s", b)
		}
		v = int64(u)
	}
	*f = flexInt(v)
	return nil
}

// flexSeverity accepts the number or the enum name.
type flexSeverity int32

func (f *flexSeverity) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if v, err := strconv.Atoi(s); err == nil {
		*f = flexSeverity(v)
		return nil
	}
	if v, ok := logsv1.SeverityNumber_value[s]; ok {
		*f = flexSeverity(v)
		return nil
	}
	return fmt.Errorf("bad severityNumber %s", b)
}

type jsonLogs struct {
	ResourceLogs []struct {
		Resource struct {
			Attributes []jsonKV `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				TimeUnixNano         flexInt      `json:"timeUnixNano"`
				ObservedTimeUnixNano flexInt      `json:"observedTimeUnixNano"`
				SeverityNumber       flexSeverity `json:"severityNumber"`
				SeverityText         string       `json:"severityText"`
				Body                 jsonAny      `json:"body"`
				Attributes           []jsonKV     `json:"attributes"`
				TraceID              string       `json:"traceId"`
				SpanID               string       `json:"spanId"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

func (a jsonAny) proto() *commonv1.AnyValue {
	switch {
	case a.StringValue != nil:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: *a.StringValue}}
	case a.BoolValue != nil:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: *a.BoolValue}}
	case a.IntValue != nil:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: int64(*a.IntValue)}}
	case a.DoubleValue != nil:
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: *a.DoubleValue}}
	case a.BytesValue != nil:
		b, _ := base64.StdEncoding.DecodeString(*a.BytesValue)
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_BytesValue{BytesValue: b}}
	case a.ArrayValue != nil:
		arr := &commonv1.ArrayValue{}
		for _, v := range a.ArrayValue.Values {
			arr.Values = append(arr.Values, v.proto())
		}
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{ArrayValue: arr}}
	case a.KvlistValue != nil:
		kv := &commonv1.KeyValueList{}
		for _, e := range a.KvlistValue.Values {
			kv.Values = append(kv.Values, &commonv1.KeyValue{Key: e.Key, Value: e.Value.proto()})
		}
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_KvlistValue{KvlistValue: kv}}
	}
	return nil
}

func kvs(in []jsonKV) []*commonv1.KeyValue {
	out := make([]*commonv1.KeyValue, 0, len(in))
	for _, kv := range in {
		out = append(out, &commonv1.KeyValue{Key: kv.Key, Value: kv.Value.proto()})
	}
	return out
}

func decodeJSON(b []byte) (*logsv1.LogsData, error) {
	var in jsonLogs
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	out := &logsv1.LogsData{}
	for _, rl := range in.ResourceLogs {
		prl := &logsv1.ResourceLogs{}
		prl.Resource = &resourcev1.Resource{Attributes: kvs(rl.Resource.Attributes)}
		for _, sl := range rl.ScopeLogs {
			psl := &logsv1.ScopeLogs{}
			for _, lr := range sl.LogRecords {
				rec := &logsv1.LogRecord{
					TimeUnixNano:         uint64(lr.TimeUnixNano),
					ObservedTimeUnixNano: uint64(lr.ObservedTimeUnixNano),
					SeverityNumber:       logsv1.SeverityNumber(lr.SeverityNumber),
					SeverityText:         lr.SeverityText,
					Body:                 lr.Body.proto(),
					Attributes:           kvs(lr.Attributes),
				}
				if lr.TraceID != "" {
					id, err := hex.DecodeString(lr.TraceID)
					if err != nil || len(id) != 16 {
						return nil, fmt.Errorf("traceId %q is not 32 hex characters", lr.TraceID)
					}
					rec.TraceId = id
				}
				psl.LogRecords = append(psl.LogRecords, rec)
			}
			prl.ScopeLogs = append(prl.ScopeLogs, psl)
		}
		out.ResourceLogs = append(out.ResourceLogs, prl)
	}
	return out, nil
}
