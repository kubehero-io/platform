// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"reflect"
	"testing"
	"time"
)

func runPipeline(t *testing.T, query, line string, stream map[string]string) (string, map[string]string, bool) {
	t.Helper()
	sel, err := ParseLogSelector(query)
	if err != nil {
		t.Fatalf("parse %s: %v", query, err)
	}
	ts := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC).UnixNano()
	return NewPipeline(sel.Stages).Process(ts, line, stream)
}

func TestPipeline(t *testing.T) {
	stream := map[string]string{"namespace": "shop", "pod": "api-1", "level": "info"}
	tests := []struct {
		name     string
		query    string
		line     string
		keep     bool
		wantLine string
		want     map[string]string // labels that must be present (with values)
		absent   []string          // labels that must be absent
	}{
		{name: "contains", query: `{a="b"} |= "err"`, line: "an error", keep: true},
		{name: "contains miss", query: `{a="b"} |= "err"`, line: "all good", keep: false},
		{name: "or chain", query: `{a="b"} |= "timeout" or "refused"`, line: "conn refused", keep: true},
		{name: "not contains or", query: `{a="b"} != "debug" or "trace"`, line: "trace x", keep: false},
		{name: "regex", query: `{a="b"} |~ "status=5\\d\\d"`, line: "status=503", keep: true},
		{name: "not regex", query: `{a="b"} !~ "(?i)health"`, line: "GET /HEALTHZ", keep: false},
		{
			name: "json flatten", query: `{a="b"} | json`,
			line: `{"msg":"hi","status":503,"ok":true,"req":{"method":"GET","hdr":{"x-id":"7"}},"arr":[1,2],"nil":null,"level":"error","1st":"x"}`,
			keep: true,
			want: map[string]string{"msg": "hi", "status": "503", "ok": "true", "req_method": "GET", "req_hdr_x_id": "7",
				"nil": "", "level": "info", "level_extracted": "error", "_1st": "x"},
			absent: []string{"arr", "__error__"},
		},
		{name: "json invalid", query: `{a="b"} | json`, line: `not json`, keep: true, want: map[string]string{"__error__": "JSONParserErr"}},
		{name: "json invalid dropped by error filter", query: `{a="b"} | json | __error__=""`, line: `{broken`, keep: false},
		{
			name: "json params", query: `{a="b"} | json first="servers[0]", ua="request.headers[\"User-Agent\"]", obj="request", st="status"`,
			line: `{"servers":["s1","s2"],"request":{"headers":{"User-Agent":"curl"}},"status":200}`, keep: true,
			want:   map[string]string{"first": "s1", "ua": "curl", "obj": `{"headers":{"User-Agent":"curl"}}`, "st": "200"},
			absent: []string{"servers", "status"},
		},
		{
			name: "logfmt", query: `{a="b"} | logfmt`, line: `ts=1 msg="hello world" dur=1.5s bare esc="a\"b"`, keep: true,
			want: map[string]string{"msg": "hello world", "dur": "1.5s", "esc": `a"b`}, absent: []string{"bare"},
		},
		{name: "logfmt keep empty", query: `{a="b"} | logfmt --keep-empty`, line: `bare k=v`, keep: true, want: map[string]string{"bare": "", "k": "v"}},
		{name: "logfmt strict error", query: `{a="b"} | logfmt --strict`, line: `k="unterminated`, keep: true, want: map[string]string{"__error__": "LogfmtParserErr"}},
		{name: "logfmt params", query: `{a="b"} | logfmt host, ip="fwd"`, line: `host=h1 fwd=1.2.3.4 other=x`, keep: true, want: map[string]string{"host": "h1", "ip": "1.2.3.4"}, absent: []string{"other", "fwd"}},
		{name: "regexp", query: `{a="b"} | regexp "(?P<method>[A-Z]+) (?P<path>\\S+)"`, line: "GET /api 200", keep: true, want: map[string]string{"method": "GET", "path": "/api"}},
		{
			name: "pattern", query: `{a="b"} | pattern "<ip> - - [<_>] \"<method> <uri> <_>\" <status> <size>"`,
			line: `10.0.0.1 - - [28/Sep/2026:12:00:00 +0000] "GET /index.html HTTP/1.1" 200 512`, keep: true,
			want: map[string]string{"ip": "10.0.0.1", "method": "GET", "uri": "/index.html", "status": "200", "size": "512"},
		},
		{name: "pattern prefix mismatch", query: `{a="b"} | pattern "GET <path>"`, line: "POST /x", keep: true, absent: []string{"path"}},
		{name: "numeric gte", query: `{a="b"} | json | status >= 500`, line: `{"status":503}`, keep: true},
		{name: "numeric miss", query: `{a="b"} | json | status >= 500`, line: `{"status":200}`, keep: false},
		{name: "numeric missing label", query: `{a="b"} | json | status >= 500`, line: `{"x":1}`, keep: false},
		{name: "numeric unparsable kept with error", query: `{a="b"} | json | status >= 500`, line: `{"status":"oops"}`, keep: true, want: map[string]string{"__error__": "LabelFilterErr"}},
		{name: "numeric skipped after parse error", query: `{a="b"} | json | status >= 500`, line: `garbage`, keep: true, want: map[string]string{"__error__": "JSONParserErr"}},
		{name: "duration filter", query: `{a="b"} | logfmt | dur > 1s`, line: `dur=1500ms`, keep: true},
		{name: "duration filter miss", query: `{a="b"} | logfmt | dur > 1s`, line: `dur=900ms`, keep: false},
		{name: "bytes filter", query: `{a="b"} | logfmt | size >= 1KB`, line: `size=1.5KiB`, keep: true},
		{name: "and/or", query: `{a="b"} | json | (status >= 500 or slow="true") and method="GET"`, line: `{"status":200,"slow":"true","method":"GET"}`, keep: true},
		{name: "and/or miss", query: `{a="b"} | json | (status >= 500 or slow="true") and method="GET"`, line: `{"status":200,"slow":"false","method":"GET"}`, keep: false},
		{name: "stream label filter", query: `{a="b"} | namespace="shop" | pod=~"api-.*"`, line: "x", keep: true},
		{name: "absent label equals empty", query: `{a="b"} | missing=""`, line: "x", keep: true},
		{
			name: "line_format", query: `{a="b"} | logfmt | line_format "{{.level | upper}} {{.msg}} [{{__line__ | len}}] {{__timestamp__ | unixEpoch}} {{ .nope }}|"`,
			line: `msg=hi`, keep: true, wantLine: "INFO hi [6] 1790596800 |",
		},
		{name: "line filter after line_format", query: `{a="b"} | logfmt | line_format "{{.msg}}" |= "only"`, line: `msg="only this" other=x`, keep: true, wantLine: "only this"},
		{name: "line filter after line_format miss", query: `{a="b"} | logfmt | line_format "{{.msg}}" |= "other"`, line: `msg="only this" other=x`, keep: false},
		{
			name: "label_format", query: `{a="b"} | logfmt | label_format lvl=level, summary="{{.method}} {{.path}}", pod="{{.pod | trimPrefix \"api-\"}}"`,
			line: `method=GET path=/x`, keep: true,
			want: map[string]string{"lvl": "info", "summary": "GET /x", "pod": "1"}, absent: []string{"level"},
		},
		{name: "drop", query: `{a="b"} | logfmt | drop pod, x="1", y="no"`, line: `x=1 y=2`, keep: true, want: map[string]string{"y": "2"}, absent: []string{"pod", "x"}},
		{name: "keep", query: `{a="b"} | logfmt | keep namespace, x`, line: `x=1 y=2`, keep: true, want: map[string]string{"namespace": "shop", "x": "1"}, absent: []string{"pod", "y", "level"}},
		{name: "decolorize", query: `{a="b"} | decolorize`, line: "\x1b[31mred\x1b[0m text", keep: true, wantLine: "red text"},
		{name: "unpack", query: `{a="b"} | unpack`, line: `{"_entry":"original line","container":"app","n":5}`, keep: true, wantLine: "original line", want: map[string]string{"container": "app"}, absent: []string{"n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, labels, keep := runPipeline(t, tc.query, tc.line, stream)
			if keep != tc.keep {
				t.Fatalf("keep = %v, want %v (labels %v)", keep, tc.keep, labels)
			}
			if !keep {
				return
			}
			if tc.wantLine != "" && line != tc.wantLine {
				t.Fatalf("line = %q, want %q", line, tc.wantLine)
			}
			for k, v := range tc.want {
				if got, ok := labels[k]; !ok || got != v {
					t.Errorf("label %s = %q (present %v), want %q; all: %v", k, got, ok, v, labels)
				}
			}
			for _, k := range tc.absent {
				if _, ok := labels[k]; ok {
					t.Errorf("label %s should be absent; all: %v", k, labels)
				}
			}
		})
	}
	if !reflect.DeepEqual(stream, map[string]string{"namespace": "shop", "pod": "api-1", "level": "info"}) {
		t.Fatalf("pipeline mutated the shared stream labels: %v", stream)
	}
}

func TestParseBytes(t *testing.T) {
	tests := map[string]float64{"100": 100, "10KB": 10000, "1.5 MiB": 1.5 * (1 << 20), "2gb": 2e9, "1e3": 1000}
	for in, want := range tests {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseBytes("10 potatoes"); err == nil {
		t.Error("bad unit must fail")
	}
}

func TestLineFormatOutputIsBounded(t *testing.T) {
	line, _, keep := runPipeline(t, `{a="b"} | line_format "{{ repeat 100000 \"x\" }}{{ repeat 60000 \"y\" }}"`, "l", map[string]string{})
	if !keep || len(line) > maxFormattedLine {
		t.Fatalf("formatted line length %d exceeds %d", len(line), maxFormattedLine)
	}
}
