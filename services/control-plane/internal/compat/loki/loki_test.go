// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package loki

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/klauspost/compress/snappy"
	"google.golang.org/protobuf/encoding/protowire"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/logs"
)

// ─── protobuf fixture builders (logproto by field number) ───────────────

func pbBytes(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func pbVarint(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

type pbEntry struct {
	secs  int64
	nanos int32
	line  string
	meta  [][2]string
}

func pbStream(labels string, entries ...pbEntry) []byte {
	var s []byte
	s = pbBytes(s, 1, []byte(labels))
	for _, e := range entries {
		var ts []byte
		ts = pbVarint(ts, 1, uint64(e.secs))
		ts = pbVarint(ts, 2, uint64(e.nanos))
		var en []byte
		en = pbBytes(en, 1, ts)
		en = pbBytes(en, 2, []byte(e.line))
		for _, m := range e.meta {
			var lp []byte
			lp = pbBytes(lp, 1, []byte(m[0]))
			lp = pbBytes(lp, 2, []byte(m[1]))
			en = pbBytes(en, 3, lp)
		}
		en = pbVarint(en, 15, 99) // unknown field: must be skipped
		s = pbBytes(s, 2, en)
	}
	s = pbVarint(s, 3, 12345) // hash, ignored
	return s
}

func pbPush(streams ...[]byte) []byte {
	var p []byte
	for _, s := range streams {
		p = pbBytes(p, 1, s)
	}
	return snappy.Encode(nil, p)
}

func TestDecodeProtoPush(t *testing.T) {
	body := pbPush(
		pbStream(`{namespace="shop", pod="api-1", app="api", level="error"}`,
			pbEntry{secs: 1790596800, nanos: 123, line: "boom", meta: [][2]string{{"trace_id", "abc123"}, {"user", "u1"}}},
			pbEntry{secs: 1790596801, line: "bang"}),
		pbStream(`{job="varlogs", filename="/var/log/x.log"}`, pbEntry{secs: 1790596802, nanos: 5, line: "plain"}),
	)
	raw, err := decodeSnappy(body)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := decodeProtoPush(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 2 || len(streams[0].entries) != 2 || len(streams[1].entries) != 1 {
		t.Fatalf("streams = %+v", streams)
	}
	e := streams[0].entries[0]
	if e.ts != 1790596800*int64(time.Second)+123 || e.line != "boom" || e.metadata["trace_id"] != "abc123" || e.metadata["user"] != "u1" {
		t.Fatalf("entry = %+v", e)
	}
	if streams[0].labels["pod"] != "api-1" || streams[1].labels["filename"] != "/var/log/x.log" {
		t.Fatalf("labels = %v / %v", streams[0].labels, streams[1].labels)
	}
	if _, err := decodeProtoPush([]byte{0x0a, 0xff}); err == nil {
		t.Fatal("truncated protobuf must fail")
	}
	if _, err := decodeProtoPush(pbBytes(nil, 1, pbBytes(nil, 1, []byte(`{a=`)))); err == nil {
		t.Fatal("bad label text must fail")
	}
}

func TestDecodeJSONPush(t *testing.T) {
	streams, err := decodeJSONPush([]byte(`{"streams":[{"stream":{"app":"x"},"values":[["1790596800000000001","hello"],["1790596800000000002","hi",{"trace_id":"t1"}]]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0].entries[0].ts != 1790596800000000001 || streams[0].entries[1].metadata["trace_id"] != "t1" {
		t.Fatalf("streams = %+v", streams)
	}
	for _, bad := range []string{`{"streams":[{"stream":{},"values":[[1,"x"]]}]}`, `{"streams":[{"values":[["x","y"]]}]}`, `{"streams":[{"values":[["1"]]}]}`, `nope`} {
		if _, err := decodeJSONPush([]byte(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestToEntriesMapsLabels(t *testing.T) {
	cluster, entries := toEntries(pushStream{
		labels: map[string]string{
			"k8s_namespace_name": "shop", "k8s_pod_name": "api-1", "container": "app", "node_name": "n1",
			"k8s_deployment_name": "api", "app": "api", "detected_level": "WARNING", "cluster": "c9",
			"stream": "stderr", "filename": "/var/log/pods/x.log", "app.kubernetes.io/part-of": "shop",
		},
		entries: []pushEntry{{ts: 1, line: "x", metadata: map[string]string{"traceID": "t1", "level": "error", "req-id": "r1"}}},
	})
	if cluster != "c9" || len(entries) != 1 {
		t.Fatalf("cluster %q entries %d", cluster, len(entries))
	}
	e := entries[0]
	src := e.GetSource()
	if src.GetNamespace() != "shop" || src.GetPod() != "api-1" || src.GetContainer() != "app" || src.GetNode() != "n1" ||
		src.GetWorkload() != "api" || src.GetWorkloadKind() != "Deployment" {
		t.Fatalf("source = %+v", src)
	}
	if e.GetStream() != "stderr" || e.GetLevel() != "error" || e.GetTraceId() != "t1" {
		t.Fatalf("entry = %+v", e)
	}
	l := e.GetLabels()
	if l["app"] != "api" || l["filename"] != "/var/log/pods/x.log" || l["app_kubernetes_io_part_of"] != "shop" || l["req_id"] != "r1" {
		t.Fatalf("labels = %v", l)
	}
	for _, gone := range []string{"k8s_namespace_name", "k8s_pod_name", "container", "cluster", "detected_level", "stream"} {
		if _, ok := l[gone]; ok {
			t.Errorf("mapped label %s must not also stay in labels", gone)
		}
	}
}

// fakeWriter records pushes per cluster.
type fakeWriter struct {
	mu   sync.Mutex
	got  map[string][]*kuberov1.LogEntry
	fail error
}

func (f *fakeWriter) WriteLogs(cluster string, entries []*kuberov1.LogEntry) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, 0, f.fail
	}
	if f.got == nil {
		f.got = map[string][]*kuberov1.LogEntry{}
	}
	f.got[cluster] = append(f.got[cluster], entries...)
	return len(entries), 0, nil
}

type tokens map[string]string

func (t tokens) VerifyClusterToken(_ context.Context, tok string) (string, bool, error) {
	id, ok := t[tok]
	return id, ok, nil
}

func testAuth() *httpauth.Authenticator {
	return httpauth.New(connect.WithInterceptors(auth.NewInterceptor(auth.Config{
		APIKeys:       []string{"push-key:member", "read-key:viewer"},
		ClusterTokens: tokens{"c1-token": "c1"},
	})))
}

func TestPushHandler(t *testing.T) {
	w := &fakeWriter{}
	h := &PushHandler{Writer: w, Auth: testAuth(), DefaultCluster: "default"}
	do := func(body []byte, headers ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/push", bytes.NewReader(body))
		for i := 0; i < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	pb := pbPush(pbStream(`{namespace="shop", app="api"}`, pbEntry{secs: time.Now().Unix(), line: "one"}))
	if rec := do(pb); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", rec.Code)
	}
	if rec := do(pb, "Authorization", "Bearer read-key"); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer push: %d", rec.Code)
	}
	if rec := do(pb, "Authorization", "Bearer push-key", "Content-Type", "application/x-protobuf", "X-Scope-OrgID", "tenant-a"); rec.Code != http.StatusNoContent {
		t.Fatalf("protobuf push: %d %s", rec.Code, rec.Body)
	}
	jsonBody := []byte(`{"streams":[{"stream":{"app":"web","cluster":"c2"},"values":[["` + fmt.Sprint(time.Now().UnixNano()) + `","two"]]},{"stream":{"app":"web"},"values":[["1","three"]]}]}`)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(jsonBody)
	_ = zw.Close()
	if rec := do(gz.Bytes(), "Authorization", "Bearer push-key", "Content-Type", "application/json", "Content-Encoding", "gzip"); rec.Code != http.StatusNoContent {
		t.Fatalf("gzip json push: %d %s", rec.Code, rec.Body)
	}
	// An enrollment token attributes to its own cluster …
	if rec := do(pb, "Authorization", "Bearer c1-token"); rec.Code != http.StatusNoContent {
		t.Fatalf("cluster token push: %d %s", rec.Code, rec.Body)
	}
	// … and may not push for another one.
	if rec := do(pb, "Authorization", "Bearer c1-token", "X-Scope-OrgID", "c2"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-cluster push: %d %s", rec.Code, rec.Body)
	}
	w.mu.Lock()
	clusters := make([]string, 0, len(w.got))
	for c := range w.got {
		clusters = append(clusters, c)
	}
	sort.Strings(clusters)
	w.mu.Unlock()
	if fmt.Sprint(clusters) != "[c1 c2 default tenant-a]" {
		t.Fatalf("clusters = %v", clusters)
	}
	if len(w.got["tenant-a"]) != 1 || w.got["tenant-a"][0].GetSource().GetNamespace() != "shop" {
		t.Fatalf("tenant-a entries = %v", w.got["tenant-a"])
	}

	for _, tc := range []struct {
		name   string
		body   []byte
		hdrs   []string
		status int
	}{
		{"garbage protobuf", snappy.Encode(nil, []byte{0x0a, 0xff}), nil, http.StatusBadRequest},
		{"not snappy", []byte("plain"), nil, http.StatusBadRequest},
		{"bad json", []byte("{"), []string{"Content-Type", "application/json"}, http.StatusBadRequest},
		{"unknown type", []byte("x"), []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType},
		{"unknown encoding", []byte("x"), []string{"Content-Encoding", "br"}, http.StatusUnsupportedMediaType},
		{"too large", bytes.Repeat([]byte("x"), MaxBodyBytes+1), nil, http.StatusRequestEntityTooLarge},
	} {
		hdrs := append([]string{"Authorization", "Bearer push-key"}, tc.hdrs...)
		if rec := do(tc.body, hdrs...); rec.Code != tc.status {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body, tc.status)
		}
	}
	w.fail = connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("queue full"))
	if rec := do(pb, "Authorization", "Bearer push-key"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("backpressure: %d", rec.Code)
	}
}

func TestParseTimeAndStep(t *testing.T) {
	def := time.Unix(42, 0)
	tests := map[string]time.Time{
		"":                               def,
		"1790596800":                     time.Unix(1790596800, 0),
		"1790596800.25":                  time.Unix(1790596800, 250e6),
		"1790596800123456789":            time.Unix(0, 1790596800123456789),
		"2026-09-28T12:00:00.5Z":         time.Unix(1790596800, 500e6),
		"2026-09-28T12:00:00.000000001Z": time.Unix(1790596800, 1),
	}
	for in, want := range tests {
		got, err := ParseTime(in, def)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseTime("yesterday", def); err == nil {
		t.Error("bad time must fail")
	}
	steps := map[string]time.Duration{"15s": 15 * time.Second, "15": 15 * time.Second, "0.5": 500 * time.Millisecond, "1m": time.Minute, "": 0}
	for in, want := range steps {
		if got, err := ParseStep(in); err != nil || got != want {
			t.Errorf("ParseStep(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"-1", "abc", "0"} {
		if _, err := ParseStep(bad); err == nil {
			t.Errorf("ParseStep(%q) should fail", bad)
		}
	}
	if d := defaultStep(time.Unix(0, 0), time.Unix(3600, 0)); d != 14*time.Second {
		t.Fatalf("default step for 1h = %v, want 14s (Loki: floor(3600/250))", d)
	}
}

// ─── query API against Loki's response shapes ───────────────────────────

var base = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func queryAPI(t *testing.T) *httptest.Server {
	t.Helper()
	var rows []logs.Row
	for i := 0; i < 120; i++ {
		level := "info"
		if i%4 == 0 {
			level = "error"
		}
		rows = append(rows, logs.Row{TS: base.Add(-time.Hour + time.Duration(i)*30*time.Second).UnixNano(),
			Labels: map[string]string{"app": "api", "namespace": "shop", "level": level, "cluster": "c1"},
			Body:   fmt.Sprintf(`{"n":%d,"status":%d}`, i, 200+300*(i%4/3))})
	}
	store := &logs.MemStore{Rows: func(from, to int64) []logs.Row {
		var out []logs.Row
		for _, r := range rows {
			if r.TS >= from && r.TS < to {
				out = append(out, r)
			}
		}
		return out
	}}
	eng := logs.New(logs.Options{Store: store, Now: func() time.Time { return base }})
	mux := http.NewServeMux()
	(&QueryAPI{Engine: eng, Auth: testAuth(), Now: func() time.Time { return base }}).Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string, params url.Values, headers ...string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path+"?"+params.Encode(), nil)
	req.Header.Set("Authorization", "Bearer read-key")
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func TestQueryRangeStreamsShape(t *testing.T) {
	srv := queryAPI(t)
	code, out, raw := get(t, srv, "/loki/api/v1/query_range", url.Values{
		"query": {`{app="api"} | json | status >= 500`}, "limit": {"5"},
		"start": {fmt.Sprint(base.Add(-time.Hour).UnixNano())}, "end": {fmt.Sprint(base.UnixNano())},
	})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	// Loki: {"status":"success","data":{"resultType":"streams","result":[{"stream":{...},"values":[["<ns>","<line>"]]}],"stats":{...}}}
	if out["status"] != "success" {
		t.Fatalf("status = %v", out["status"])
	}
	data := out["data"].(map[string]any)
	if data["resultType"] != "streams" {
		t.Fatalf("resultType = %v", data["resultType"])
	}
	result := data["result"].([]any)
	total := 0
	for _, s := range result {
		st := s.(map[string]any)
		labels := st["stream"].(map[string]any)
		if labels["status"] != "500" || labels["app"] != "api" {
			t.Fatalf("stream labels = %v", labels)
		}
		for _, v := range st["values"].([]any) {
			pair := v.([]any)
			ts, ok := pair[0].(string)
			if !ok || len(pair) != 2 || len(ts) != 19 {
				t.Fatalf("value %v must be [\"<unix ns>\", \"<line>\"]", pair)
			}
			total++
		}
	}
	if total != 5 {
		t.Fatalf("lines = %d, want limit 5", total)
	}
	if _, ok := data["stats"].(map[string]any)["summary"]; !ok {
		t.Fatal("stats.summary missing")
	}
}

func TestQueryRangeMatrixAndInstantShapes(t *testing.T) {
	srv := queryAPI(t)
	code, out, raw := get(t, srv, "/loki/api/v1/query_range", url.Values{
		"query": {`sum by (level) (count_over_time({app="api"}[5m]))`},
		"start": {fmt.Sprint(base.Add(-30 * time.Minute).Unix())}, "end": {fmt.Sprint(base.Unix())}, "step": {"300"},
	})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	// Loki: {"resultType":"matrix","result":[{"metric":{...},"values":[[<unix seconds>,"<value>"]]}]}
	data := out["data"].(map[string]any)
	if data["resultType"] != "matrix" {
		t.Fatalf("resultType = %v", data["resultType"])
	}
	result := data["result"].([]any)
	if len(result) != 2 {
		t.Fatalf("series = %d", len(result))
	}
	for _, s := range result {
		m := s.(map[string]any)
		for _, v := range m["values"].([]any) {
			pair := v.([]any)
			if _, ok := pair[0].(float64); !ok {
				t.Fatalf("matrix timestamp %v must be a JSON number", pair[0])
			}
			if _, ok := pair[1].(string); !ok {
				t.Fatalf("matrix value %v must be a string", pair[1])
			}
		}
	}
	// Lines every 30s, errors every 4th: the (11:55, 12:00] window holds
	// 11:55:30 … 11:59:30 = 9 lines, 2 of them errors.
	if !strings.Contains(raw, `{"metric":{"level":"error"},"values":[[1790595000,"3"]`) || !strings.Contains(raw, `[1790596800,"2"]]}`) || !strings.Contains(raw, `[1790596800,"7"]]}`) {
		t.Fatalf("matrix body = %s", raw)
	}

	// Grafana's datasource health check.
	code, out, raw = get(t, srv, "/loki/api/v1/query", url.Values{"query": {"vector(1)+vector(1)"}, "time": {fmt.Sprint(base.Unix())}})
	if code != http.StatusOK || !strings.Contains(raw, `"result":[{"metric":{},"value":[1790596800,"2"]}],"resultType":"vector"`) {
		t.Fatalf("health check query: %d %s", code, raw)
	}
	code, _, raw = get(t, srv, "/loki/api/v1/query", url.Values{"query": {"1+1"}})
	if code != http.StatusOK || !strings.Contains(raw, `"result":[1790596800,"2"],"resultType":"scalar"`) {
		t.Fatalf("scalar: %d %s", code, raw)
	}
	code, _, _ = get(t, srv, "/loki/api/v1/query", url.Values{"query": {`{app="api"}`}})
	if code != http.StatusBadRequest {
		t.Fatalf("instant log query: %d", code)
	}
	code, _, raw = get(t, srv, "/loki/api/v1/query_range", url.Values{"query": {`{app=}`}})
	if code != http.StatusBadRequest || !strings.Contains(raw, "parse error") {
		t.Fatalf("parse error: %d %s", code, raw)
	}
}

func TestLabelsSeriesVolumeShapes(t *testing.T) {
	srv := queryAPI(t)
	start := fmt.Sprint(base.Add(-time.Hour).Unix())
	code, _, raw := get(t, srv, "/loki/api/v1/labels", url.Values{"start": {start}})
	if code != http.StatusOK || raw != `{"data":["app","cluster","level","namespace"],"status":"success"}`+"\n" {
		t.Fatalf("labels: %d %s", code, raw)
	}
	code, _, raw = get(t, srv, "/loki/api/v1/label/level/values", url.Values{"start": {start}})
	if code != http.StatusOK || raw != `{"data":["error","info"],"status":"success"}`+"\n" {
		t.Fatalf("label values: %d %s", code, raw)
	}
	code, out, raw := get(t, srv, "/loki/api/v1/series", url.Values{"match[]": {`{level="error"}`}, "start": {start}})
	if code != http.StatusOK {
		t.Fatalf("series: %d %s", code, raw)
	}
	set := out["data"].([]any)
	if len(set) != 1 || set[0].(map[string]any)["level"] != "error" {
		t.Fatalf("series = %s", raw)
	}
	code, out, raw = get(t, srv, "/loki/api/v1/index/volume", url.Values{"query": {`{app="api"}`}, "start": {start}, "targetLabels": {"level"}})
	if code != http.StatusOK {
		t.Fatalf("volume: %d %s", code, raw)
	}
	vec := out["data"].(map[string]any)["result"].([]any)
	if len(vec) != 2 || vec[0].(map[string]any)["metric"].(map[string]any)["level"] != "info" {
		t.Fatalf("volume = %s", raw)
	}
	code, out, raw = get(t, srv, "/loki/api/v1/index/stats", url.Values{"query": {`{app="api"}`}, "start": {start}})
	if code != http.StatusOK || out["entries"].(float64) != 120 || out["streams"].(float64) != 2 {
		t.Fatalf("stats: %d %s", code, raw)
	}
	code, _, raw = get(t, srv, "/loki/api/v1/status/buildinfo", nil)
	if code != http.StatusOK || !strings.Contains(raw, `"version":"`+BuildVersion+`"`) {
		t.Fatalf("buildinfo: %d %s", code, raw)
	}
	code, _, _ = get(t, srv, "/ready", nil)
	if code != http.StatusOK {
		t.Fatalf("ready: %d", code)
	}
	code, _, _ = get(t, srv, "/loki/api/v1/labels", url.Values{}, "X-Scope-OrgID", "a|b")
	if code != http.StatusBadRequest {
		t.Fatalf("multi-tenant query: %d", code)
	}
	code, _, raw = get(t, srv, "/loki/api/v1/labels", url.Values{}, "X-Scope-OrgID", "other")
	if code != http.StatusOK || raw != `{"data":[],"status":"success"}`+"\n" {
		t.Fatalf("scoped to an empty cluster: %d %s", code, raw)
	}
}
