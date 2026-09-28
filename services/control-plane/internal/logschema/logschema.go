// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package logschema is the single definition of what a log stream
// label is in KubeHero, shared by the ingest path (which decides what
// lands in a column and what in the labels map), the LogQL compiler
// (which maps selector labels back to columns) and the compatibility
// shims (which map Loki / OTLP label names onto ours).
//
// Stream labels are the fixed columns of the logs table plus any key
// of its labels map. Label names follow Prometheus / Loki rules
// ([a-zA-Z_][a-zA-Z0-9_]*), so ingest sanitises map keys — the pod
// label app.kubernetes.io/name is stored and queried as
// app_kubernetes_io_name, exactly what Promtail's relabelling produces.
package logschema

import (
	"strings"
	"unicode/utf8"
)

// Column-backed stream labels: LogQL label name → logs column.
var columns = map[string]string{
	"cluster":       "cluster_id",
	"namespace":     "namespace",
	"workload":      "workload",
	"workload_kind": "workload_kind",
	"pod":           "pod",
	"container":     "container",
	"node":          "node",
	"team":          "team",
	"stream":        "stream",
	"level":         "level",
}

// ColumnLabels lists the column-backed label names in a stable order
// (the order responses and label listings use).
var ColumnLabels = []string{"cluster", "namespace", "workload", "workload_kind", "pod", "container", "node", "team", "stream", "level"}

// RollupLabels are the labels log_volume_1m keeps — the dimensions the
// volume fast path can filter and group by.
var RollupLabels = []string{"cluster", "namespace", "workload", "container", "team", "level"}

// Column returns the logs column behind a label, or false when the
// label lives in the labels map.
func Column(label string) (string, bool) {
	c, ok := columns[label]
	return c, ok
}

// IsRollupLabel reports whether log_volume_1m has the label.
func IsRollupLabel(label string) bool {
	for _, l := range RollupLabels {
		if l == label {
			return true
		}
	}
	return false
}

// Ingest limits shared by every log write path.
const (
	// MaxLineBytes truncates longer lines; the entry is kept and
	// labelled TruncatedLabel="true".
	MaxLineBytes = 64 << 10
	// MaxLabels caps extra (map) labels per line; extras are dropped.
	MaxLabels = 32
	// MaxLabelNameLen / MaxLabelValueLen truncate oversized labels.
	MaxLabelNameLen  = 128
	MaxLabelValueLen = 2048
	// MaxFieldLen caps column values (Kubernetes names are ≤ 253).
	MaxFieldLen = 253
	// TruncatedLabel marks a line cut at MaxLineBytes.
	TruncatedLabel = "truncated"
)

// ValidLabelName reports whether s is a legal LogQL label name.
func ValidLabelName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// SanitizeLabelName maps any string onto a legal label name: invalid
// bytes become '_' and a leading digit gets a '_' prefix. The empty
// string stays empty (callers drop it).
func SanitizeLabelName(s string) string {
	if ValidLabelName(s) || s == "" {
		return s
	}
	b := make([]byte, 0, len(s)+1)
	if s[0] >= '0' && s[0] <= '9' {
		b = append(b, '_')
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b = append(b, c)
		} else {
			b = append(b, '_')
		}
	}
	return string(b)
}

// Reserved reports whether an extra label name must not be stored in
// the labels map: it is column-backed (the column wins) or reserved
// for the query engine (__error__, __name__ …).
func Reserved(name string) bool {
	_, col := columns[name]
	return col || strings.HasPrefix(name, "__")
}

// NormalizeLevel maps the many spellings of a severity onto
// trace | debug | info | warn | error | fatal, or "" when unknown.
func NormalizeLevel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "trace", "trc", "finest", "verbose", "vrb":
		return "trace"
	case "debug", "dbg", "debu", "fine", "finer", "d":
		return "debug"
	case "info", "inf", "information", "informational", "notice", "i":
		return "info"
	case "warn", "warning", "wrn", "w":
		return "warn"
	case "error", "err", "eror", "e", "severe":
		return "error"
	case "fatal", "ftl", "critical", "crit", "crt", "emerg", "emergency", "alert", "panic", "f":
		return "fatal"
	}
	return ""
}

// Truncate cuts s to at most n bytes without splitting a UTF-8
// sequence, and reports whether it cut anything.
func Truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// CleanString makes s safe to store and to return through protobuf
// string fields: invalid UTF-8 becomes U+FFFD and NUL bytes are
// removed.
func CleanString(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}

// Field cleans and truncates a column value.
func Field(s string) string {
	s, _ = Truncate(CleanString(strings.TrimSpace(s)), MaxFieldLen)
	return s
}
