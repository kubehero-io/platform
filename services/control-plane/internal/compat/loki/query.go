// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
	"github.com/kubehero-io/platform/services/control-plane/internal/logs"
	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
)

// BuildVersion is the Loki API level reported by buildinfo. Grafana
// feature-detects on it; 2.9 matches the surface implemented here.
const BuildVersion = "2.9.0"

// QueryAPI serves Loki's HTTP query API from the logs engine.
type QueryAPI struct {
	Engine *logs.Engine
	Auth   *httpauth.Authenticator
	Log    *slog.Logger
	Now    func() time.Time
}

// Mount registers the query endpoints on mux.
func (a *QueryAPI) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/loki/api/v1/query_range", a.guard(a.queryRange))
	mux.HandleFunc("/loki/api/v1/query", a.guard(a.query))
	mux.HandleFunc("/loki/api/v1/labels", a.guard(a.labels))
	mux.HandleFunc("/loki/api/v1/label", a.guard(a.labels))
	mux.HandleFunc("/loki/api/v1/label/{name}/values", a.guard(a.labelValues))
	mux.HandleFunc("/loki/api/v1/series", a.guard(a.series))
	mux.HandleFunc("/loki/api/v1/index/volume", a.guard(a.volume))
	mux.HandleFunc("/loki/api/v1/index/stats", a.guard(a.stats))
	mux.HandleFunc("/loki/api/v1/status/buildinfo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{
			"version": BuildVersion, "revision": "kubehero", "branch": "kubehero",
			"buildUser": "kubehero", "buildDate": "", "goVersion": "",
		})
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		if a.Engine.Source() == "" {
			http.Error(w, "logs store not configured", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
}

func (a *QueryAPI) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

type handler func(ctx context.Context, w http.ResponseWriter, r *http.Request) error

// guard authenticates (viewer), parses the form (GET query or POST
// form, as Loki accepts both) and turns errors into Loki-style
// plain-text responses.
func (a *QueryAPI) guard(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
			return
		}
		ctx, aerr := a.Auth.Require(r.Context(), r.Header, auth.RoleViewer)
		if aerr != nil {
			httpauth.WriteError(w, aerr)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := h(ctx, w, r); err != nil {
			a.writeErr(w, err)
		}
	}
}

type badParam struct{ msg string }

func (b badParam) Error() string { return b.msg }

func (a *QueryAPI) writeErr(w http.ResponseWriter, err error) {
	var pe *logql.ParseError
	var le *logql.Error
	var br *logs.BadRequest
	var bp badParam
	switch {
	case errors.As(err, &pe), errors.As(err, &br), errors.As(err, &bp):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &le):
		if le.Kind == logql.KindLimit {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, logs.ErrUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "query timed out", http.StatusGatewayTimeout)
	case errors.Is(err, context.Canceled):
		http.Error(w, "request cancelled", 499)
	default:
		if a.Log != nil {
			a.Log.Error("loki api query failed", "err", err)
		}
		http.Error(w, "query failed (see control-plane logs)", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// ParseTime reads Loki's timestamp formats: RFC3339(Nano), unix
// seconds (integer ≤ 10 digits or float), or unix nanoseconds.
func ParseTime(v string, def time.Time) (time.Time, error) {
	if v == "" {
		return def, nil
	}
	if strings.Contains(v, ".") {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s, frac := math.Modf(f)
			return time.Unix(int64(s), int64(math.Round(frac*1000)*float64(time.Millisecond))).UTC(), nil
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t, terr := time.Parse(time.RFC3339Nano, v)
		if terr != nil {
			return time.Time{}, badParam{fmt.Sprintf("cannot parse %q as a timestamp", v)}
		}
		return t.UTC(), nil
	}
	if len(strings.TrimPrefix(v, "-")) <= 10 {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Unix(0, n).UTC(), nil
}

// ParseStep reads a duration ("15s", "1m") or float seconds ("15").
func ParseStep(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		if f <= 0 || f > 365*86400 {
			return 0, badParam{fmt.Sprintf("step %q out of range", v)}
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := logql.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, badParam{fmt.Sprintf("cannot parse step %q", v)}
	}
	return d, nil
}

// timeRange reads start/end/since with Loki's defaults (end = now,
// start = end - since, since = 1h).
func (a *QueryAPI) timeRange(r *http.Request) (time.Time, time.Time, error) {
	end, err := ParseTime(r.Form.Get("end"), a.now())
	if err != nil {
		return end, end, err
	}
	since := time.Hour
	if s := r.Form.Get("since"); s != "" {
		if since, err = logql.ParseDuration(s); err != nil {
			return end, end, badParam{fmt.Sprintf("cannot parse since %q", s)}
		}
	}
	start, err := ParseTime(r.Form.Get("start"), end.Add(-since))
	return start, end, err
}

// cluster maps X-Scope-OrgID (Loki's tenant) onto a cluster scope.
func cluster(r *http.Request) (string, error) {
	org := strings.TrimSpace(r.Header.Get("X-Scope-OrgID"))
	if strings.Contains(org, "|") {
		return "", badParam{"multi-tenant queries (X-Scope-OrgID with '|') are not supported; query one cluster"}
	}
	return org, nil
}

// defaultStep is Loki's: range / 250, at least 1s.
func defaultStep(start, end time.Time) time.Duration {
	s := math.Max(math.Floor(end.Sub(start).Seconds()/250), 1)
	return time.Duration(s) * time.Second
}

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// unixSeconds renders a sample time the way Prometheus does: a JSON
// number of seconds with millisecond precision.
func unixSeconds(t time.Time) json.Number {
	return json.Number(strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64))
}

type sampleStream struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
}

type sample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

type logStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

func statsBlock(st logs.Stats) map[string]any {
	secs := st.Exec.Seconds()
	perSec := func(n int64) int64 {
		if secs <= 0 {
			return 0
		}
		return int64(float64(n) / secs)
	}
	return map[string]any{"summary": map[string]any{
		"bytesProcessedPerSecond": perSec(st.BytesScanned),
		"linesProcessedPerSecond": perSec(st.RowsScanned),
		"totalBytesProcessed":     st.BytesScanned,
		"totalLinesProcessed":     st.RowsScanned,
		"execTime":                secs,
	}}
}

// streams groups lines into Loki streams (by full label set), keeping
// the result order within and across streams.
func streams(lines []logs.Line) []logStream {
	idx := map[string]int{}
	var out []logStream
	for _, l := range lines {
		k := logql.LabelsKey(l.Labels)
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, logStream{Stream: l.Labels, Values: [][2]string{}})
		}
		out[i].Values = append(out[i].Values, [2]string{strconv.FormatInt(l.TS, 10), l.Body})
	}
	return out
}

func matrix(series []signals.Series) []sampleStream {
	out := make([]sampleStream, 0, len(series))
	for _, s := range series {
		ss := sampleStream{Metric: nonNil(s.Labels), Values: make([][2]any, 0, len(s.Points))}
		for _, p := range s.Points {
			ss.Values = append(ss.Values, [2]any{unixSeconds(p.TS), formatValue(p.Value)})
		}
		out = append(out, ss)
	}
	return out
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (a *QueryAPI) queryRange(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	start, end, err := a.timeRange(r)
	if err != nil {
		return err
	}
	c, err := cluster(r)
	if err != nil {
		return err
	}
	limit := logs.DefaultLimit
	if v := r.Form.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit <= 0 {
			return badParam{fmt.Sprintf("bad limit %q", v)}
		}
		limit = min(limit, logs.MaxLimit)
	}
	step, err := ParseStep(r.Form.Get("step"))
	if err != nil {
		return err
	}
	if step == 0 {
		step = defaultStep(start, end)
	}
	dir := r.Form.Get("direction")
	if dir != "" && dir != "backward" && dir != "forward" && dir != "BACKWARD" && dir != "FORWARD" {
		return badParam{fmt.Sprintf("bad direction %q", dir)}
	}
	res, err := a.Engine.Query(ctx, logs.QueryParams{
		Query: r.Form.Get("query"), Start: start, End: end, Limit: limit,
		Forward: strings.EqualFold(dir, "forward"), Step: step, ClusterID: c,
	})
	if err != nil {
		return err
	}
	data := map[string]any{"stats": statsBlock(res.Stats)}
	if res.IsMetric() {
		data["resultType"], data["result"] = "matrix", matrix(res.Metric.Series)
	} else {
		data["resultType"], data["result"] = "streams", streams(res.Lines)
	}
	writeJSON(w, map[string]any{"status": "success", "data": data})
	return nil
}

func (a *QueryAPI) query(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	at, err := ParseTime(r.Form.Get("time"), a.now())
	if err != nil {
		return err
	}
	c, err := cluster(r)
	if err != nil {
		return err
	}
	q := r.Form.Get("query")
	expr, err := logql.Parse(strings.TrimSpace(q))
	if err != nil {
		return err
	}
	if _, isLog := expr.(*logql.LogSelectorExpr); isLog {
		return badParam{"log queries are not supported as an instant query type; use query_range"}
	}
	res, err := a.Engine.Query(ctx, logs.QueryParams{Query: q, End: at, Start: at, Instant: true, ClusterID: c})
	if err != nil {
		return err
	}
	data := map[string]any{"stats": statsBlock(res.Stats)}
	if res.Metric.Scalar {
		v := 0.0
		if len(res.Metric.Series) > 0 && len(res.Metric.Series[0].Points) > 0 {
			v = res.Metric.Series[0].Points[0].Value
		}
		data["resultType"], data["result"] = "scalar", [2]any{unixSeconds(at), formatValue(v)}
	} else {
		vec := make([]sample, 0, len(res.Metric.Series))
		for _, s := range res.Metric.Series {
			if len(s.Points) == 0 {
				continue
			}
			p := s.Points[len(s.Points)-1]
			vec = append(vec, sample{Metric: nonNil(s.Labels), Value: [2]any{unixSeconds(p.TS), formatValue(p.Value)}})
		}
		data["resultType"], data["result"] = "vector", vec
	}
	writeJSON(w, map[string]any{"status": "success", "data": data})
	return nil
}

func (a *QueryAPI) labels(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	start, end, err := a.timeRange(r)
	if err != nil {
		return err
	}
	c, err := cluster(r)
	if err != nil {
		return err
	}
	names, err := a.Engine.LabelNames(ctx, r.Form.Get("query"), c, start, end)
	if err != nil {
		return err
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, map[string]any{"status": "success", "data": names})
	return nil
}

func (a *QueryAPI) labelValues(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	start, end, err := a.timeRange(r)
	if err != nil {
		return err
	}
	c, err := cluster(r)
	if err != nil {
		return err
	}
	vals, err := a.Engine.LabelValues(ctx, r.PathValue("name"), r.Form.Get("query"), c, start, end, 0)
	if err != nil {
		return err
	}
	if vals == nil {
		vals = []string{}
	}
	writeJSON(w, map[string]any{"status": "success", "data": vals})
	return nil
}

func (a *QueryAPI) series(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	start, end, err := a.timeRange(r)
	if err != nil {
		return err
	}
	c, err := cluster(r)
	if err != nil {
		return err
	}
	match := r.Form["match[]"]
	if len(match) == 0 {
		match = r.Form["match"]
	}
	set, err := a.Engine.Series(ctx, match, c, start, end)
	if err != nil {
		return err
	}
	if set == nil {
		set = []map[string]string{}
	}
	writeJSON(w, map[string]any{"status": "success", "data": set})
	return nil
}

// volume answers /index/volume: bytes per group over the range. One
// grouping label is supported (the first of targetLabels, else the
// first equality matcher of the selector).
func (a *QueryAPI) volume(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	start, end, err := a.timeRange(r)
	if err != nil {
		return err
	}
	c, err := cluster(r)
	if err != nil {
		return err
	}
	q := r.Form.Get("query")
	group := ""
	if t := strings.TrimSpace(r.Form.Get("targetLabels")); t != "" {
		group = strings.TrimSpace(strings.Split(t, ",")[0])
	} else if sel, err := logql.ParseLogSelector(strings.TrimSpace(q)); err == nil {
		for _, m := range sel.Matchers {
			if m.Type == logql.MatchEqual {
				group = m.Name
				break
			}
		}
	}
	totals, err := a.Engine.VolumeTotals(ctx, q, c, group, start, end)
	if err != nil {
		return err
	}
	limit := 100
	if v, err := strconv.Atoi(r.Form.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	vec := make([]sample, 0, min(len(totals), limit))
	for i, t := range totals {
		if i >= limit {
			break
		}
		vec = append(vec, sample{Metric: nonNil(t.Labels), Value: [2]any{unixSeconds(end), strconv.FormatInt(t.Bytes, 10)}})
	}
	writeJSON(w, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": vec}})
	return nil
}

// stats answers /index/stats (Grafana's query-size estimate). Loki has
// chunks; KubeHero reports one per stream.
func (a *QueryAPI) stats(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	start, end, err := a.timeRange(r)
	if err != nil {
		return err
	}
	c, err := cluster(r)
	if err != nil {
		return err
	}
	q := r.Form.Get("query")
	totals, err := a.Engine.VolumeTotals(ctx, q, c, "", start, end)
	if err != nil {
		return err
	}
	var lines, bytes int64
	for _, t := range totals {
		lines += t.Lines
		bytes += t.Bytes
	}
	set, err := a.Engine.Series(ctx, []string{q}, c, start, end)
	if err != nil {
		return err
	}
	writeJSON(w, map[string]int64{"streams": int64(len(set)), "chunks": int64(len(set)), "entries": lines, "bytes": bytes})
	return nil
}
