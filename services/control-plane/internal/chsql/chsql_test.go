// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package chsql

import (
	"reflect"
	"testing"
)

func TestWhereBuildsParameterisedSQL(t *testing.T) {
	var w Where
	if w.SQL() != "1" {
		t.Fatalf("empty where = %q", w.SQL())
	}
	w.Add("ts >= ?", int64(10))
	w.In("cluster_id", []string{"a", "b"})
	w.In("namespace", []string{"prod"})
	w.In("team", nil) // no-op
	if err := w.Matcher("workload", OpRe, "api-.*"); err != nil {
		t.Fatal(err)
	}
	if err := w.Matcher("pod", OpNre, "x"); err != nil {
		t.Fatal(err)
	}
	want := "ts >= ? AND cluster_id IN (?) AND namespace = ? AND match(workload, ?) AND NOT match(pod, ?)"
	if got := w.SQL(); got != want {
		t.Fatalf("SQL = %q\nwant  %q", got, want)
	}
	wantArgs := []any{int64(10), []string{"a", "b"}, "prod", "^(?:api-.*)$", "^(?:x)$"}
	if !reflect.DeepEqual(w.Args(), wantArgs) {
		t.Fatalf("args = %#v", w.Args())
	}
	c := w.Clone()
	c.Add("x = ?", 1)
	if len(w.Args()) != 5 || len(c.Args()) != 6 {
		t.Fatal("Clone must not alias")
	}
}

func TestWherePanicsOnPlaceholderMismatch(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	var w Where
	w.Add("a = ? AND b = ?", 1)
}

func TestMatcherRejectsBadRegex(t *testing.T) {
	var w Where
	if err := w.Matcher("x", OpRe, "(unclosed"); err == nil {
		t.Fatal("want error")
	}
	if err := w.Matcher("x", MatchOp("~~"), "a"); err == nil {
		t.Fatal("want error for unknown op")
	}
}

func TestIdent(t *testing.T) {
	if Ident("cluster_id") != "cluster_id" {
		t.Fatal()
	}
	for _, bad := range []string{"a;DROP", "A", "x y", "", "1a"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Ident(%q) should panic", bad)
				}
			}()
			Ident(bad)
		}()
	}
}

func TestValidLabelKey(t *testing.T) {
	for k, want := range map[string]bool{
		"app":                           true,
		"app.kubernetes.io/name":        true,
		"kubehero.io/team":              true,
		"a_b.c-d":                       true,
		"":                              false,
		"-app":                          false,
		"app'] OR 1=1 --":               false,
		"UPPER.example.com/x":           false,
		"example.com/":                  false,
		"x/" + string(make([]byte, 64)): false,
	} {
		if got := ValidLabelKey(k); got != want {
			t.Errorf("ValidLabelKey(%q) = %v, want %v", k, got, want)
		}
	}
}
