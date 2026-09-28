// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/klauspost/compress/snappy"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/encoding/protowire"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
)

type harness struct {
	db   *chtest.DB
	srv  *httptest.Server
	logs kuberov1connect.LogsServiceClient
	tel  kuberov1connect.TelemetryServiceClient
}

func newHarness(t *testing.T, name string) *harness {
	t.Helper()
	db := chtest.Open(t, name)
	t.Setenv("CLICKHOUSE_URL", db.DSN)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := http.NewServeMux()
	interceptor := connect.WithInterceptors(auth.NewInterceptor(auth.Config{APIKeys: []string{"k:member"}}))
	engine := wireTelemetry(ctx, mux, wireDeps{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), CH: db.SQL,
		Handler: []connect.HandlerOption{interceptor},
	})
	if engine == nil {
		t.Fatal("with ClickHouse the metric engine must be returned for alert rules")
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	t.Cleanup(srv.Close)
	opts := connect.WithInterceptors(bearer("k"))
	return &harness{
		db: db, srv: srv,
		logs: kuberov1connect.NewLogsServiceClient(srv.Client(), srv.URL, opts),
		tel:  kuberov1connect.NewTelemetryServiceClient(srv.Client(), srv.URL, opts, connect.WithSendGzip()),
	}
}

// eventually polls until cond holds (rows are flushed about once a
// second by the ingest batchers).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) do(t *testing.T, method, path string, body []byte, headers ...string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func lokiProtoPush(labels string, ts time.Time, lines ...string) []byte {
	var stream []byte
	stream = protowire.AppendTag(stream, 1, protowire.BytesType)
	stream = protowire.AppendString(stream, labels)
	for i, l := range lines {
		var tsb []byte
		tsb = protowire.AppendTag(tsb, 1, protowire.VarintType)
		tsb = protowire.AppendVarint(tsb, uint64(ts.Unix()))
		tsb = protowire.AppendTag(tsb, 2, protowire.VarintType)
		tsb = protowire.AppendVarint(tsb, uint64(ts.Nanosecond()+i))
		var e []byte
		e = protowire.AppendTag(e, 1, protowire.BytesType)
		e = protowire.AppendBytes(e, tsb)
		e = protowire.AppendTag(e, 2, protowire.BytesType)
		e = protowire.AppendString(e, l)
		stream = protowire.AppendTag(stream, 2, protowire.BytesType)
		stream = protowire.AppendBytes(stream, e)
	}
	var req []byte
	req = protowire.AppendTag(req, 1, protowire.BytesType)
	req = protowire.AppendBytes(req, stream)
	return snappy.Encode(nil, req)
}

func TestCompatRoundTrips(t *testing.T) {
	h := newHarness(t, "kh_ingest_wire")
	ctx := context.Background()
	now := time.Now().UTC()

	// Loki protobuf push → Loki query_range.
	code, body := h.do(t, http.MethodPost, "/loki/api/v1/push",
		lokiProtoPush(`{namespace="shop", pod="api-1", app="api", level="error"}`, now, "loki line one", "loki line two"),
		"Content-Type", "application/x-protobuf", "X-Scope-OrgID", "c1")
	if code != http.StatusNoContent {
		t.Fatalf("push: %d %s", code, body)
	}
	q := url.Values{"query": {`{app="api"} |= "loki line"`}, "since": {"10m"}, "limit": {"10"}}
	eventually(t, "pushed lines to be queryable", func() bool {
		code, body = h.do(t, http.MethodGet, "/loki/api/v1/query_range?"+q.Encode(), nil, "X-Scope-OrgID", "c1")
		return code == http.StatusOK && strings.Count(string(body), "loki line") == 2
	})
	var resp struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.ResultType != "streams" || len(resp.Data.Result) != 1 || resp.Data.Result[0].Stream["cluster"] != "c1" ||
		resp.Data.Result[0].Stream["namespace"] != "shop" || resp.Data.Result[0].Values[0][1] != "loki line two" {
		t.Fatalf("query_range = %s", body)
	}
	// metric query over the pushed data (Grafana's volume histogram)
	mq := url.Values{"query": {`sum by (level) (count_over_time({app="api"}[10m]))`}, "since": {"10m"}, "step": {"60"}}
	code, body = h.do(t, http.MethodGet, "/loki/api/v1/query_range?"+mq.Encode(), nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"metric":{"level":"error"}`) {
		t.Fatalf("metric query_range: %d %s", code, body)
	}

	// OTLP JSON → LogsService.QueryLogs.
	otlpBody := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[
		{"key":"k8s.namespace.name","value":{"stringValue":"pay"}},
		{"key":"k8s.pod.name","value":{"stringValue":"ledger-0"}},
		{"key":"k8s.cluster.name","value":{"stringValue":"c2"}},
		{"key":"service.name","value":{"stringValue":"ledger"}}]},
	  "scopeLogs":[{"logRecords":[{"timeUnixNano":"%d","severityNumber":17,"body":{"stringValue":"otlp charge failed"},
		"traceId":"4bf92f3577b34da6a3ce929d0e0e4736"}]}]}]}`, now.UnixNano())
	code, body = h.do(t, http.MethodPost, "/v1/logs", []byte(otlpBody), "Content-Type", "application/json")
	if code != http.StatusOK {
		t.Fatalf("otlp: %d %s", code, body)
	}
	var lines []*kuberov1.LogLine
	eventually(t, "OTLP lines to be queryable", func() bool {
		res, err := h.logs.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{namespace="pay"} |= "otlp"`, ClusterId: "c2"}))
		if err != nil {
			t.Fatal(err)
		}
		lines = res.Msg.GetLines()
		return len(lines) == 1
	})
	if l := lines[0]; l.GetLevel() != "error" || l.GetTraceId() != "4bf92f3577b34da6a3ce929d0e0e4736" || l.GetLabels()["workload"] != "ledger" {
		t.Fatalf("otlp line = %+v", l)
	}

	// Pyroscope folded → profile_samples / profile_stacks.
	code, body = h.do(t, http.MethodPost,
		fmt.Sprintf("/ingest?name=checkout.cpu%%7Bnamespace%%3Dshop%%2Ccluster%%3Dc1%%7D&from=%d&sampleRate=100&format=folded", now.Unix()),
		[]byte("main;handler;json.Marshal 30\nmain;handler;db.Query 12\n"))
	if code != http.StatusOK {
		t.Fatalf("pyroscope: %d %s", code, body)
	}
	eventually(t, "profile rows", func() bool {
		return h.db.Count(t, `SELECT count() FROM profile_samples WHERE service = 'checkout' AND origin = 'pyroscope' AND cluster_id = 'c1'`) == 2
	})
	if v := h.db.Count(t, `SELECT sum(value) FROM profile_samples WHERE service = 'checkout'`); v != 42*10_000_000 {
		t.Fatalf("cpu ns = %d, want 42 samples × 10ms", v)
	}
	// Stacks and samples flush through separate batchers, so the stacks
	// can land a flush interval after the samples.
	eventually(t, "profile stack rows", func() bool {
		return h.db.Count(t, `SELECT count() FROM profile_stacks FINAL WHERE has(frames, 'json.Marshal')`) == 1
	})
}

// Load sanity: 200k lines through the IngestLogs RPC (Connect,
// protobuf + gzip, 10k per request, 4 concurrent senders), then a
// filtered 1h query and a metric query.
func TestIngestLoad200k(t *testing.T) {
	h := newHarness(t, "kh_ingest_load")
	ctx := context.Background()
	const total, perReq, senders = 200_000, 10_000, 4
	now := time.Now().UTC()
	levels := []string{"info", "info", "info", "warn", "error"}
	mkReq := func(batch int) *kuberov1.IngestLogsRequest {
		req := &kuberov1.IngestLogsRequest{ClusterId: "load"}
		for i := 0; i < perReq; i++ {
			n := batch*perReq + i
			req.Entries = append(req.Entries, &kuberov1.LogEntry{
				TsUnixNano: now.Add(-time.Hour + time.Duration(n)*18*time.Millisecond).UnixNano(),
				Level:      levels[n%len(levels)],
				Body:       fmt.Sprintf("method=GET path=/api/items/%d status=%d dur=%dms user=u%d", n%977, 200+300*(n%5/4), n%350, n%40),
				Source: &kuberov1.PodRef{Namespace: []string{"shop", "pay", "web"}[n%3], Workload: "api", Pod: fmt.Sprintf("api-%d", n%8),
					Container: "app", Node: fmt.Sprintf("node-%d", n%4)},
				Labels: map[string]string{"app": "api"},
			})
		}
		return req
	}
	reqs := make(chan *kuberov1.IngestLogsRequest, total/perReq)
	for b := 0; b < total/perReq; b++ {
		reqs <- mkReq(b)
	}
	close(reqs)

	start := time.Now()
	errs := make(chan error, senders)
	for s := 0; s < senders; s++ {
		go func() {
			for r := range reqs {
				for {
					res, err := h.tel.IngestLogs(ctx, connect.NewRequest(r))
					if connect.CodeOf(err) == connect.CodeResourceExhausted {
						time.Sleep(100 * time.Millisecond) // backpressure: retry
						continue
					}
					if err == nil && int(res.Msg.GetAccepted()) != perReq {
						err = fmt.Errorf("accepted %d of %d", res.Msg.GetAccepted(), perReq)
					}
					if err != nil {
						errs <- err
						return
					}
					break
				}
			}
			errs <- nil
		}()
	}
	for s := 0; s < senders; s++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	accepted := time.Since(start)
	eventually(t, "all 200k lines stored", func() bool {
		return h.db.Count(t, `SELECT count() FROM logs WHERE cluster_id = 'load'`) == total
	})
	stored := time.Since(start)
	t.Logf("ingest: %d lines accepted by the RPC in %v (%.0f lines/s); stored in ClickHouse after %v (%.0f lines/s end to end)",
		total, accepted.Round(time.Millisecond), float64(total)/accepted.Seconds(), stored.Round(time.Millisecond), float64(total)/stored.Seconds())

	// Metric grids are minute-aligned, as the dashboard sends them, so
	// the minute rollup can answer (unaligned grids take the exact raw
	// path). The line filter keeps the second query on raw rows.
	end := now.Truncate(time.Minute)
	for _, q := range []struct {
		name       string
		query      string
		step       int64
		maxScanned int64 // 0: unbounded
	}{
		{"filtered 1h log query", `{namespace="shop", app="api"} |= "status=500" | logfmt | dur > 300ms`, 0, 0},
		{"1h metric query (SQL over raw rows)", `sum by (namespace) (count_over_time({cluster="load", level="error"} |= "status=500" [5m]))`, 60_000, 0},
		{"1h metric query (minute rollup)", `sum by (level) (rate({cluster="load"}[5m]))`, 60_000, total / 4},
		{"1h metric query (Go pipeline)", `sum by (status) (count_over_time({namespace="pay"} | logfmt | status >= 500 [5m]))`, 60_000, 0},
	} {
		began := time.Now()
		res, err := h.logs.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{
			Query: q.query, StartUnixMs: end.Add(-time.Hour).UnixMilli(), EndUnixMs: end.UnixMilli(), StepMs: q.step, Limit: 100,
		}))
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		if len(res.Msg.GetLines())+len(res.Msg.GetSeries()) == 0 {
			t.Fatalf("%s returned nothing", q.name)
		}
		if scanned := res.Msg.GetStats().GetRowsScanned(); q.maxScanned > 0 && scanned > q.maxScanned {
			t.Fatalf("%s scanned %d rows: the minute rollup should have answered it", q.name, scanned)
		}
		t.Logf("%s: %v (%d lines, %d series, %d rows scanned)", q.name, time.Since(began).Round(time.Millisecond),
			len(res.Msg.GetLines()), len(res.Msg.GetSeries()), res.Msg.GetStats().GetRowsScanned())
	}
}
