// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package logs

import (
	"testing"
	"time"
)

func TestParseCRI(t *testing.T) {
	cases := []struct {
		in      string
		stream  string
		partial bool
		content string
		ok      bool
	}{
		{"2026-09-28T10:00:00.123456789Z stdout F hello world", "stdout", false, "hello world", true},
		{"2026-09-28T10:00:00.1Z stderr P first half ", "stderr", true, "first half ", true},
		{"2026-09-28T10:00:00+02:00 stdout F:x=y tagged", "stdout", false, "tagged", true},
		{"2026-09-28T10:00:00Z stdout F", "stdout", false, "", true}, // empty line, no trailing space
		{"2026-09-28T10:00:00Z stdout F ", "stdout", false, "", true},
		{"2026-09-28T10:00:00Z stdout F  leading spaces kept", "stdout", false, " leading spaces kept", true},
		{"not-a-time stdout F x", "", false, "", false},
		{"2026-09-28T10:00:00Z stdin F x", "", false, "", false},
		{"2026-09-28T10:00:00Z stdout X x", "", false, "", false},
		{"", "", false, "", false},
	}
	for _, c := range cases {
		r, err := parseLine([]byte(c.in))
		if (err == nil) != c.ok {
			t.Errorf("parseLine(%q) err = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if !c.ok {
			continue
		}
		if r.stream != c.stream || r.partial != c.partial || string(r.content) != c.content {
			t.Errorf("parseLine(%q) = %q %v %q", c.in, r.stream, r.partial, r.content)
		}
	}
	r, _ := parseLine([]byte("2026-09-28T10:00:00.123456789Z stdout F x"))
	if want := time.Date(2026, 9, 28, 10, 0, 0, 123456789, time.UTC); !r.ts.Equal(want) {
		t.Fatalf("ts = %v", r.ts)
	}
}

func TestParseDocker(t *testing.T) {
	cases := []struct {
		in      string
		stream  string
		partial bool
		content string
		ok      bool
	}{
		{`{"log":"hello\n","stream":"stdout","time":"2026-09-28T10:00:00.5Z"}`, "stdout", false, "hello", true},
		{`{"log":"crlf\r\n","stream":"stderr","time":"2026-09-28T10:00:00Z"}`, "stderr", false, "crlf", true},
		{`{"log":"no newline = partial","stream":"stdout","time":"2026-09-28T10:00:00Z"}`, "stdout", true, "no newline = partial", true},
		{`{"log":"quote \" and \\t café\n","stream":"stdout","time":"2026-09-28T10:00:00Z"}`, "stdout", false, "quote \" and \\t café", true},
		{`{"log":"x\n","stream":"weird","time":"2026-09-28T10:00:00Z"}`, "", false, "", false},
		{`{"log":"x\n","stream":"stdout","time":"yesterday"}`, "", false, "", false},
		{`{not json`, "", false, "", false},
	}
	for _, c := range cases {
		r, err := parseLine([]byte(c.in))
		if (err == nil) != c.ok {
			t.Errorf("parseLine(%s) err = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && (r.stream != c.stream || r.partial != c.partial || string(r.content) != c.content) {
			t.Errorf("parseLine(%s) = %q %v %q", c.in, r.stream, r.partial, r.content)
		}
	}
}

func TestDetectLevel(t *testing.T) {
	cases := []struct{ in, want string }{
		// JSON
		{`{"level":"info","msg":"started"}`, LevelInfo},
		{`{"lvl":"WARNING","msg":"x"}`, LevelWarn},
		{`{"severity":"ERROR","message":"gcp style"}`, LevelError},
		{`{"@timestamp":"…","log.level":"debug","message":"ecs flat"}`, LevelDebug},
		{`{"log":{"level":"warn"},"message":"ecs nested"}`, LevelWarn},
		{`{"@l":"Warning","@mt":"serilog compact"}`, LevelWarn},
		{`{"levelname":"CRITICAL","msg":"python json"}`, LevelFatal},
		{`{"level":30,"msg":"pino info"}`, LevelInfo},
		{`{"level":50,"msg":"pino error"}`, LevelError},
		{`{"level" : "trace" , "msg":"spaces"}`, LevelTrace},
		{`{"msg":"the \"level\": high","level":"info"}`, LevelInfo},
		{`{"msg":"no level field"}`, ""},
		// logfmt
		{`ts=2026-09-28T10:00:00Z level=error msg="db down"`, LevelError},
		{`time="2026" level="warning" msg=x`, LevelWarn},
		{`t=1 lvl=dbug msg=x`, LevelDebug},
		{`xlevel=error msg=not-a-level-key`, ""},
		// klog
		{`I0928 10:00:00.000000       1 controller.go:12] synced`, LevelInfo},
		{`W0928 10:00:00.000000       1 x.go:1] slow`, LevelWarn},
		{`E0928 10:00:00.000000       1 x.go:1] failed`, LevelError},
		{`F0928 10:00:00.000000       1 x.go:1] dying`, LevelFatal},
		// tokens
		{`2026-09-28 10:00:00.123 ERROR 1 --- [main] o.s.Application : boom`, LevelError},
		{`2026/09/28 10:00:00 [error] 29#29: *1 connect() failed`, LevelError},
		{`[WARN] cache miss ratio high`, LevelWarn},
		{`WARNING: disk almost full`, LevelWarn},
		{`error: could not open file`, LevelError},
		{`10:00:00 INF request handled`, LevelInfo},
		{`panic: runtime error: index out of range`, LevelFatal},
		{`Traceback (most recent call last):`, LevelError},
		{`Exception in thread "main" java.lang.NullPointerException`, LevelError},
		{`the error rate is fine`, ""},       // lowercase prose is not a level
		{`ALERT: disk at 90%`, ""},           // ambiguous prose token
		{`GET /healthz 200 1.2ms`, ""},       // plain access log
		{`   {"level":"fatal"}`, LevelFatal}, // leading whitespace
		{``, ""},
	}
	for _, c := range cases {
		if got := DetectLevel(c.in); got != c.want {
			t.Errorf("DetectLevel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDetectTraceID(t *testing.T) {
	const id = "4bf92f3577b34da6a3ce929d0e0e4736"
	cases := []struct{ in, want string }{
		{"traceparent=00-" + id + "-00f067aa0ba902b7-01 GET /", id},
		{"forwarding 00-" + id + "-00f067aa0ba902b7-01", id},
		{`{"trace_id":"` + id + `","msg":"x"}`, id},
		{`{"traceId": "` + "4BF92F3577B34DA6A3CE929D0E0E4736" + `"}`, id},
		{`msg=x trace_id=` + id + ` span=1`, id},
		{`trace.id=` + id, id},
		{`{"traceID":"a3ce929d0e0e4736"}`, "a3ce929d0e0e4736"}, // 64-bit B3
		{`trace_id=00000000000000000000000000000000`, ""},      // invalid all-zero
		{`trace_id=xyz`, ""},
		{`trace_id=` + id + `ab`, ""}, // wrong length
		{`00-00000000000000000000000000000000-00f067aa0ba902b7-01`, ""},
		{`no trace here`, ""},
	}
	for _, c := range cases {
		if got := DetectTraceID(c.in); got != c.want {
			t.Errorf("DetectTraceID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func BenchmarkDetect(b *testing.B) {
	lines := []string{
		`{"level":"info","ts":"2026-09-28T10:00:00Z","msg":"request handled","path":"/api/v1/orders","status":200,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}`,
		`ts=2026-09-28T10:00:00Z level=warn msg="slow query" duration=1.2s`,
		`2026-09-28 10:00:00.123 INFO 1 --- [nio-8080-exec-1] c.e.OrderController : created order 1234`,
		`GET /healthz 200 1.2ms`,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l := lines[i%len(lines)]
		_ = DetectLevel(l)
		_ = DetectTraceID(l)
	}
}
