// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/httpauth"
)

func str(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

var now = uint64(time.Now().UnixNano())

func sampleData() *logsv1.LogsData {
	return &logsv1.LogsData{ResourceLogs: []*logsv1.ResourceLogs{{
		Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{
			str("k8s.namespace.name", "shop"), str("k8s.pod.name", "api-1"), str("k8s.container.name", "app"),
			str("k8s.deployment.name", "api"), str("k8s.node.name", "n1"), str("k8s.cluster.name", "prod-eu"),
			str("service.name", "api-svc"), str("service.version", "1.2.3"),
		}},
		ScopeLogs: []*logsv1.ScopeLogs{{LogRecords: []*logsv1.LogRecord{
			{TimeUnixNano: now, SeverityNumber: logsv1.SeverityNumber_SEVERITY_NUMBER_ERROR2,
				Body:       &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "payment failed"}},
				Attributes: []*commonv1.KeyValue{str("http.route", "/pay"), str("log.iostream", "stderr")},
				TraceId:    []byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}},
			{ObservedTimeUnixNano: now + 1, SeverityText: "Warning",
				Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_KvlistValue{KvlistValue: &commonv1.KeyValueList{Values: []*commonv1.KeyValue{
					str("msg", "slow"), {Key: "ms", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 1500}}},
				}}}}},
		}}},
	}}}
}

func TestRecordsMapping(t *testing.T) {
	recs := toRecords(sampleData())
	if len(recs) != 2 {
		t.Fatalf("records = %d", len(recs))
	}
	r := recs[0]
	src := r.entry.GetSource()
	if r.cluster != "prod-eu" || src.GetNamespace() != "shop" || src.GetPod() != "api-1" || src.GetContainer() != "app" ||
		src.GetWorkload() != "api" || src.GetWorkloadKind() != "Deployment" || src.GetNode() != "n1" {
		t.Fatalf("record = %+v / %+v", r, src)
	}
	e := r.entry
	if e.GetLevel() != "error" || e.GetStream() != "stderr" || e.GetBody() != "payment failed" ||
		e.GetTraceId() != "4bf92f3577b34da6a3ce929d0e0e4736" || e.GetTsUnixNano() != int64(now) {
		t.Fatalf("entry = %+v", e)
	}
	l := e.GetLabels()
	if l["service_name"] != "api-svc" || l["service_version"] != "1.2.3" || l["http_route"] != "/pay" {
		t.Fatalf("labels = %v", l)
	}
	for _, gone := range []string{"k8s_namespace_name", "k8s_pod_name", "k8s_cluster_name", "log_iostream"} {
		if _, ok := l[gone]; ok {
			t.Errorf("mapped attribute %s leaked into labels", gone)
		}
	}
	w := recs[1].entry
	if w.GetLevel() != "warn" || w.GetTsUnixNano() != int64(now+1) || w.GetBody() != `{"ms":1500,"msg":"slow"}` {
		t.Fatalf("second entry = %+v", w)
	}
}

func TestSeverityNumbers(t *testing.T) {
	for n, want := range map[int32]string{1: "trace", 5: "debug", 9: "info", 12: "info", 13: "warn", 17: "error", 21: "fatal", 24: "fatal"} {
		if got := level(logsv1.SeverityNumber(n), ""); got != want {
			t.Errorf("severity %d = %q, want %q", n, got, want)
		}
	}
	if level(0, "ERROR") != "error" || level(0, "whatever") != "" {
		t.Fatal("severity text fallback")
	}
}

type fakeWriter struct {
	mu       sync.Mutex
	got      map[string][]*kuberov1.LogEntry
	dropNext int
	fail     error
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
	d := min(f.dropNext, len(entries))
	f.dropNext -= d
	return len(entries) - d, d, nil
}

func handler(w *fakeWriter) *Handler {
	a := httpauth.New(connect.WithInterceptors(auth.NewInterceptor(auth.Config{APIKeys: []string{"k:member"}})))
	return &Handler{Writer: w, Auth: a, DefaultCluster: "default"}
}

func post(h http.Handler, body []byte, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerProtobuf(t *testing.T) {
	w := &fakeWriter{}
	h := handler(w)
	body, err := proto.Marshal(sampleData())
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(body)
	_ = zw.Close()
	rec := post(h, gz.Bytes(), "Content-Type", "application/x-protobuf", "Content-Encoding", "gzip")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.Bytes())
	}
	if len(w.got["prod-eu"]) != 2 {
		t.Fatalf("written = %v", w.got)
	}

	// Rejected rows come back as partial_success in protobuf.
	w.dropNext = 1
	rec = post(h, body, "Content-Type", "application/x-protobuf")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var rejected uint64
	var msg string
	b := rec.Body.Bytes()
	num, typ, n := protowire.ConsumeTag(b)
	if num != 1 || typ != protowire.BytesType {
		t.Fatalf("response is not ExportLogsServiceResponse{partial_success}: %x", b)
	}
	inner, _ := protowire.ConsumeBytes(b[n:])
	for len(inner) > 0 {
		f, ft, m := protowire.ConsumeTag(inner)
		inner = inner[m:]
		switch {
		case f == 1 && ft == protowire.VarintType:
			v, k := protowire.ConsumeVarint(inner)
			rejected, inner = v, inner[k:]
		case f == 2 && ft == protowire.BytesType:
			v, k := protowire.ConsumeBytes(inner)
			msg, inner = string(v), inner[k:]
		}
	}
	if rejected != 1 || msg == "" {
		t.Fatalf("partial success = %d %q", rejected, msg)
	}
}

func TestHandlerJSON(t *testing.T) {
	w := &fakeWriter{}
	h := handler(w)
	body := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[
		{"key":"k8s.namespace.name","value":{"stringValue":"pay"}},
		{"key":"k8s.pod.name","value":{"stringValue":"ledger-0"}},
		{"key":"k8s.statefulset.name","value":{"stringValue":"ledger"}}]},
	  "scopeLogs":[{"logRecords":[
		{"timeUnixNano":"%d","severityNumber":"SEVERITY_NUMBER_INFO","body":{"stringValue":"ok"},
		 "traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"00f067aa0ba902b7",
		 "attributes":[{"key":"count","value":{"intValue":"7"}},{"key":"ratio","value":{"doubleValue":0.5}}]},
		{"timeUnixNano":%d,"severityNumber":17,"body":{"arrayValue":{"values":[{"stringValue":"a"},{"boolValue":true}]}}}
	  ]}]}]}`, now, now)
	rec := post(h, []byte(body), "Content-Type", "application/json", "X-Scope-OrgID", "tenant-x")
	if rec.Code != http.StatusOK || rec.Body.String() != "{}" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	got := w.got["tenant-x"]
	if len(got) != 2 {
		t.Fatalf("written = %v", w.got)
	}
	e := got[0]
	if e.GetLevel() != "info" || e.GetTraceId() != "4bf92f3577b34da6a3ce929d0e0e4736" || e.GetLabels()["count"] != "7" ||
		e.GetLabels()["ratio"] != "0.5" || e.GetSource().GetWorkloadKind() != "StatefulSet" {
		t.Fatalf("entry = %+v", e)
	}
	if got[1].GetLevel() != "error" || got[1].GetBody() != `["a",true]` {
		t.Fatalf("second = %+v", got[1])
	}

	w.dropNext = 2
	rec = post(h, []byte(body), "Content-Type", "application/json")
	var resp struct {
		PartialSuccess struct {
			RejectedLogRecords string `json:"rejectedLogRecords"`
		} `json:"partialSuccess"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.PartialSuccess.RejectedLogRecords != "2" {
		t.Fatalf("partial success json = %s", rec.Body)
	}

	for _, tc := range []struct {
		body string
		hdrs []string
		code int
	}{
		{`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"traceId":"zz"}]}]}]}`, []string{"Content-Type", "application/json"}, http.StatusBadRequest},
		{`not json`, []string{"Content-Type", "application/json"}, http.StatusBadRequest},
		{`\x00\x01`, []string{"Content-Type", "application/x-protobuf"}, http.StatusBadRequest},
		{`x`, []string{"Content-Type", "text/csv"}, http.StatusUnsupportedMediaType},
	} {
		if rec := post(h, []byte(tc.body), tc.hdrs...); rec.Code != tc.code {
			t.Errorf("%q: %d %s, want %d", tc.body, rec.Code, rec.Body, tc.code)
		}
	}
	w.fail = connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("full"))
	if rec := post(h, []byte(body), "Content-Type", "application/json"); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("backpressure: %d %v", rec.Code, rec.Header())
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader([]byte(body)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(context.Background()))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", rec.Code)
	}
}
