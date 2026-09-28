// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package logs

import (
	"strconv"
	"strings"
)

// Normalised levels (LogEntry.level).
const (
	LevelTrace = "trace"
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelFatal = "fatal"
)

// DetectLevel guesses a line's severity, cheapest reliable signal first:
// a structured field in JSON (level | lvl | severity | log.level | @l |
// levelname | loglevel), a logfmt level=/lvl=/severity= pair, a klog
// header (I0101/W0101/E0101/F0101), then common tokens ([ERROR],
// ERROR:, WARN, panic:, Traceback …). Returns "" when nothing matches —
// stderr alone is not evidence of an error; plenty of software logs
// everything there.
func DetectLevel(body string) string {
	b := strings.TrimLeft(body, " \t")
	if b == "" {
		return ""
	}
	if b[0] == '{' {
		if lvl := jsonLevel(b); lvl != "" {
			return lvl
		}
	}
	if strings.IndexByte(b, '=') > 0 {
		if lvl := logfmtLevel(b); lvl != "" {
			return lvl
		}
	}
	if lvl := klogLevel(b); lvl != "" {
		return lvl
	}
	return tokenLevel(b)
}

// jsonKeys are checked in order; the first present wins.
var jsonKeys = []string{`"level"`, `"lvl"`, `"severity"`, `"log.level"`, `"@l"`, `"levelname"`, `"loglevel"`, `"@level"`}

func jsonLevel(b string) string {
	for _, k := range jsonKeys {
		v, ok := jsonValue(b, k)
		if !ok {
			continue
		}
		if lvl := normalizeLevel(v); lvl != "" {
			return lvl
		}
	}
	return ""
}

// jsonValue finds `"key": <value>` and returns the raw scalar (string
// contents or number). It is a targeted scan, not a JSON parser: fast
// and good enough for the flat objects structured loggers emit.
func jsonValue(b, quotedKey string) (string, bool) {
	from := 0
	for {
		i := strings.Index(b[from:], quotedKey)
		if i < 0 {
			return "", false
		}
		i += from
		from = i + len(quotedKey)
		if i > 0 && b[i-1] == '\\' {
			continue // an escaped quote inside a string value
		}
		rest := strings.TrimLeft(b[from:], " \t")
		if rest == "" || rest[0] != ':' {
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t")
		if rest == "" {
			return "", false
		}
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				return "", false
			}
			return rest[1 : 1+end], true
		}
		end := strings.IndexAny(rest, ",} \t")
		if end < 0 {
			end = len(rest)
		}
		return rest[:end], true
	}
}

var logfmtKeys = []string{"level=", "lvl=", "severity=", "loglevel="}

func logfmtLevel(b string) string {
	for _, k := range logfmtKeys {
		from := 0
		for {
			i := strings.Index(b[from:], k)
			if i < 0 {
				break
			}
			i += from
			from = i + len(k)
			if i > 0 && b[i-1] != ' ' && b[i-1] != '\t' {
				continue // part of a longer key, e.g. "loglevel=" inside "xlevel="
			}
			v := b[from:]
			if v != "" && v[0] == '"' {
				if end := strings.IndexByte(v[1:], '"'); end >= 0 {
					v = v[1 : 1+end]
				}
			} else if end := strings.IndexAny(v, " \t"); end >= 0 {
				v = v[:end]
			}
			if lvl := normalizeLevel(v); lvl != "" {
				return lvl
			}
		}
	}
	return ""
}

// klogLevel reads the klog header: Lmmdd hh:mm:ss.uuuuuu threadid file:line] msg.
func klogLevel(b string) string {
	if len(b) < 6 || b[5] != ' ' {
		return ""
	}
	for i := 1; i < 5; i++ {
		if b[i] < '0' || b[i] > '9' {
			return ""
		}
	}
	switch b[0] {
	case 'I':
		return LevelInfo
	case 'W':
		return LevelWarn
	case 'E':
		return LevelError
	case 'F':
		return LevelFatal
	}
	return ""
}

// tokenScanWindow bounds the free-text scan: severity tokens live in the
// line prefix, and scanning whole 64 KiB lines would cost more than it
// finds.
const tokenScanWindow = 256

func tokenLevel(b string) string {
	if strings.HasPrefix(b, "panic:") || strings.HasPrefix(b, "fatal error:") {
		return LevelFatal // Go runtime
	}
	if strings.HasPrefix(b, "Traceback (most recent call last)") || strings.HasPrefix(b, "Exception in thread") {
		return LevelError // Python / JVM
	}
	w := b
	if len(w) > tokenScanWindow {
		w = w[:tokenScanWindow]
	}
	first := true
	for i := 0; i < len(w); {
		if !isLetter(w[i]) {
			i++
			continue
		}
		j := i
		for j < len(w) && isLetter(w[j]) {
			j++
		}
		word := w[i:j]
		bracketed := i > 0 && w[i-1] == '[' && j < len(w) && w[j] == ']'
		colonFirst := first && j < len(w) && w[j] == ':'
		if bracketed || colonFirst || isUpper(word) {
			if lvl := levelTokens[strings.ToUpper(word)]; lvl != "" {
				return lvl
			}
		}
		first = false
		i = j
	}
	return ""
}

// levelTokens is the free-text vocabulary: narrower than normalizeLevel
// because prose is ambiguous ("ALERT: disk at 90%" is not a fatal).
var levelTokens = map[string]string{
	"TRACE": LevelTrace, "TRC": LevelTrace,
	"DEBUG": LevelDebug, "DBG": LevelDebug,
	"INFO": LevelInfo, "INF": LevelInfo, "NOTICE": LevelInfo,
	"WARN": LevelWarn, "WARNING": LevelWarn, "WRN": LevelWarn,
	"ERROR": LevelError, "ERR": LevelError, "SEVERE": LevelError,
	"FATAL": LevelFatal, "FTL": LevelFatal, "CRITICAL": LevelFatal, "CRIT": LevelFatal, "PANIC": LevelFatal,
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isUpper(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// normalizeLevel maps the many spellings (and pino/bunyan and syslog
// numbers) onto the six levels.
func normalizeLevel(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if n, err := strconv.Atoi(v); err == nil {
		switch {
		case n >= 60:
			return LevelFatal
		case n >= 50:
			return LevelError
		case n >= 40:
			return LevelWarn
		case n >= 30:
			return LevelInfo
		case n >= 20:
			return LevelDebug
		case n >= 10:
			return LevelTrace
		case n >= 0 && n <= 2: // syslog emerg/alert/crit
			return LevelFatal
		case n == 3:
			return LevelError
		case n == 4:
			return LevelWarn
		case n == 5 || n == 6:
			return LevelInfo
		case n == 7:
			return LevelDebug
		}
		return ""
	}
	switch strings.ToLower(v) {
	case "trace", "trc", "verbose", "vrb", "finest", "finer":
		return LevelTrace
	case "debug", "dbg", "dbug", "fine":
		return LevelDebug
	case "info", "inf", "information", "informational", "notice":
		return LevelInfo
	case "warn", "warning", "wrn":
		return LevelWarn
	case "error", "err", "eror", "severe":
		return LevelError
	case "fatal", "ftl", "critical", "crit", "alert", "emerg", "emergency", "panic", "dpanic":
		return LevelFatal
	}
	return ""
}

// traceKeys are the field names trace ids are logged under (OTel, W3C,
// Zipkin/B3, ECS spellings).
var traceKeys = []string{"trace_id", "traceId", "traceID", "trace.id", "trace-id", "otelTraceID", "x-b3-traceid", "X-B3-TraceId"}

// DetectTraceID extracts a trace id: a W3C traceparent anywhere in the
// line, or a trace_id / traceId / … field (JSON or logfmt) holding 16 or
// 32 hex digits. Returned lowercase; all-zero ids are invalid.
func DetectTraceID(body string) string {
	if id := traceparent(body); id != "" {
		return id
	}
	if !strings.Contains(body, "race") && !strings.Contains(body, "RACE") {
		return "" // cheap gate: every key spelling contains "race"
	}
	for _, k := range traceKeys {
		from := 0
		for {
			i := strings.Index(body[from:], k)
			if i < 0 {
				break
			}
			i += from
			from = i + len(k)
			rest := body[from:]
			rest = strings.TrimPrefix(rest, `"`)
			rest = strings.TrimLeft(rest, " \t")
			if rest == "" || (rest[0] != ':' && rest[0] != '=') {
				continue
			}
			rest = strings.TrimLeft(rest[1:], " \t")
			rest = strings.TrimPrefix(rest, `"`)
			n := hexRun(rest)
			if (n == 32 || n == 16) && (len(rest) == n || !isAlnum(rest[n])) {
				if id := strings.ToLower(rest[:n]); !allZero(id) {
					return id
				}
			}
		}
	}
	return ""
}

// traceparent finds "00-<32 hex>-<16 hex>-<2 hex>".
func traceparent(b string) string {
	from := 0
	for {
		i := strings.Index(b[from:], "00-")
		if i < 0 || i+from+55 > len(b) {
			return ""
		}
		i += from
		from = i + 3
		if i > 0 && isAlnum(b[i-1]) {
			continue
		}
		s := b[i:]
		if hexRun(s[3:]) != 32 || s[35] != '-' || hexRun(s[36:]) != 16 || s[52] != '-' || hexRun(s[53:]) < 2 {
			continue
		}
		if id := strings.ToLower(s[3:35]); !allZero(id) {
			return id
		}
	}
}

func hexRun(s string) int {
	n := 0
	for n < len(s) {
		c := s[n]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			n++
			continue
		}
		break
	}
	return n
}

func isAlnum(c byte) bool { return isLetter(c) || (c >= '0' && c <= '9') }

func allZero(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
