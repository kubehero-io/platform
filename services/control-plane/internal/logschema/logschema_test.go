// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logschema

import "testing"

func TestSanitizeLabelName(t *testing.T) {
	tests := map[string]string{
		"app":                    "app",
		"app.kubernetes.io/name": "app_kubernetes_io_name",
		"k8s-app":                "k8s_app",
		"9lives":                 "_9lives",
		"":                       "",
		"ünï":                    "__n__", // each 2-byte rune → two underscores
		"_ok":                    "_ok",
	}
	for in, want := range tests {
		if got := SanitizeLabelName(in); got != want {
			t.Errorf("SanitizeLabelName(%q) = %q, want %q", in, got, want)
		}
		if in != "" && !ValidLabelName(SanitizeLabelName(in)) {
			t.Errorf("sanitised %q is not a valid label name", in)
		}
	}
}

func TestNormalizeLevel(t *testing.T) {
	tests := map[string]string{
		"ERROR": "error", "Warning": "warn", "wrn": "warn", "INFO": "info", "notice": "info",
		"dbg": "debug", "crit": "fatal", "panic": "fatal", "trace": "trace", "": "", "banana": "",
	}
	for in, want := range tests {
		if got := NormalizeLevel(in); got != want {
			t.Errorf("NormalizeLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s := "aé" // 'é' is 2 bytes: cutting at 2 must not split it
	got, cut := Truncate(s, 2)
	if got != "a" || !cut {
		t.Fatalf("Truncate = %q %v", got, cut)
	}
	if got, cut := Truncate("abc", 5); got != "abc" || cut {
		t.Fatalf("no-op truncate = %q %v", got, cut)
	}
}

func TestCleanString(t *testing.T) {
	if got := CleanString("a\x00b\xffc"); got != "ab�c" {
		t.Fatalf("CleanString = %q", got)
	}
}

func TestReservedAndColumns(t *testing.T) {
	for _, l := range ColumnLabels {
		if !Reserved(l) {
			t.Errorf("%s should be reserved", l)
		}
		if _, ok := Column(l); !ok {
			t.Errorf("%s should map to a column", l)
		}
	}
	if Reserved("app") || !Reserved("__error__") {
		t.Fatal("reserved set wrong")
	}
	if c, _ := Column("cluster"); c != "cluster_id" {
		t.Fatalf("cluster column = %q", c)
	}
	for _, l := range RollupLabels {
		if !IsRollupLabel(l) {
			t.Errorf("%s should be a rollup label", l)
		}
	}
	if IsRollupLabel("pod") {
		t.Fatal("pod is not in log_volume_1m")
	}
}
