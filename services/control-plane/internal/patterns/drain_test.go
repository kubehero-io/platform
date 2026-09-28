// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package patterns

import (
	"fmt"
	"testing"
)

func TestMask(t *testing.T) {
	tests := map[string]string{
		"123":                                  "<N>",
		"-1.5e3":                               "<N>",
		"0xdeadbeef":                           "<N>",
		"(123),":                               "(<N>),",
		"550e8400-e29b-41d4-a716-446655440000": "<U>",
		"10.0.0.1:8080":                        "<I>",
		"fe80::1ff:fe23:4567:890a":             "<I>",
		"2026-09-28T12:00:00Z":                 "<T>",
		"12:34:56.789":                         "<T>",
		"150ms":                                "<D>",
		"1h2m3s":                               "<D>",
		"512MiB":                               "<D>",
		"87%":                                  "<D>",
		"4bf92f3577b34da6":                     "<H>",
		"api-7d9f8b6c5-x2x4z":                  "<H>",
		"status=503":                           "status=<N>",
		"took=12ms,":                           "took=<D>,",
		"GET":                                  "GET",
		"error:":                               "error:",
		"deadbeef":                             "deadbeef", // no digits: a word, not an id
		"db1":                                  "db1",
	}
	for in, want := range tests {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDrainClustersTemplates(t *testing.T) {
	d := New(Config{}, 3)
	for i := 0; i < 50; i++ {
		d.Train(fmt.Sprintf("user %d logged in from 10.0.%d.%d in %dms", i, i%3, i, 10+i), "info", i%3, 1)
	}
	for i := 0; i < 20; i++ {
		d.Train(fmt.Sprintf("connection to db%d failed: timeout after %ds", i%4, 5), "error", 2, 1)
	}
	for i := 0; i < 5; i++ {
		d.Train("cache warmed", "debug", 0, 1)
	}
	ps := d.Patterns()
	if len(ps) != 3 {
		for _, p := range ps {
			t.Logf("%d %q", p.Count, p.Pattern)
		}
		t.Fatalf("got %d patterns, want 3", len(ps))
	}
	want := []struct {
		pattern string
		count   int64
		level   string
	}{
		{"user <_> logged in from <_> in <_>", 50, "info"},
		{"connection to <_> failed: timeout after <_>", 20, "error"},
		{"cache warmed", 5, "debug"},
	}
	for i, w := range want {
		if ps[i].Pattern != w.pattern || ps[i].Count != w.count || ps[i].Level != w.level {
			t.Errorf("pattern %d = %q ×%d (%s), want %q ×%d (%s)", i, ps[i].Pattern, ps[i].Count, ps[i].Level, w.pattern, w.count, w.level)
		}
	}
	if ps[1].Trend[2] != 20 || ps[0].Trend[0]+ps[0].Trend[1]+ps[0].Trend[2] != 50 {
		t.Errorf("trends = %v / %v", ps[0].Trend, ps[1].Trend)
	}
	if ps[0].Sample != "user 0 logged in from 10.0.0.0 in 10ms" {
		t.Errorf("sample = %q", ps[0].Sample)
	}
	if d.Lines() != 75 {
		t.Errorf("lines = %d", d.Lines())
	}
}

func TestDrainSimilarityThreshold(t *testing.T) {
	d := New(Config{}, 0)
	// Same length and prefix, but only 1 of 4 tokens equal: below 0.4.
	d.Train("alpha beta gamma delta", "", -1, 1)
	d.Train("alpha one two three", "", -1, 1)
	if n := len(d.Patterns()); n != 2 {
		t.Fatalf("dissimilar lines merged: %d patterns", n)
	}
	// 3 of 4 equal: merged, differing position generalised.
	d.Train("alpha beta gamma epsilon", "", -1, 1)
	ps := d.Patterns()
	found := false
	for _, p := range ps {
		if p.Pattern == "alpha beta gamma <_>" && p.Count == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("patterns = %+v", ps)
	}
}

func TestDrainBoundedClusters(t *testing.T) {
	d := New(Config{MaxClusters: 10}, 0)
	for i := 0; i < 500; i++ {
		// every line a distinct shape
		d.Train(fmt.Sprintf("w%c x%c y%c", 'a'+rune(i%26), 'a'+rune(i/26%26), 'a'+rune(i/676%26)), "", -1, 1)
	}
	if n := len(d.clusters); n > 10 {
		t.Fatalf("%d clusters, bound is 10", n)
	}
}

func TestDrainWeightsAndLongLines(t *testing.T) {
	d := New(Config{MaxTokens: 4}, 1)
	d.Train("a b c d e f g", "warn", 0, 10)
	d.Train("a b c x y z", "warn", 0, 10)
	ps := d.Patterns()
	if len(ps) != 1 || ps[0].Count != 20 || ps[0].Trend[0] != 20 || ps[0].Levels["warn"] != 20 {
		t.Fatalf("patterns = %+v", ps)
	}
	if ps[0].Pattern != "a b c <_>" {
		t.Fatalf("long lines should keep MaxTokens-1 tokens + one wildcard: %q", ps[0].Pattern)
	}
}
