// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package timewin

import (
	"errors"
	"math"
	"testing"
	"time"
)

// 2026-09-30 is a Wednesday.
var now = time.Date(2026, 9, 30, 14, 25, 0, 0, time.UTC)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParse(t *testing.T) {
	cases := []struct {
		in         string
		start, end string
	}{
		{"24h", "2026-09-29T14:25:00Z", "2026-09-30T14:25:00Z"},
		{"30m", "2026-09-30T13:55:00Z", "2026-09-30T14:25:00Z"},
		// OpenCost: today + the N-1 whole days before it.
		{"1d", "2026-09-30T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"7d", "2026-09-24T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"30d", "2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"2w", "2026-09-17T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"today", "2026-09-30T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"yesterday", "2026-09-29T00:00:00Z", "2026-09-30T00:00:00Z"},
		{"week", "2026-09-27T00:00:00Z", "2026-09-30T14:25:00Z"}, // Sunday start
		{"lastweek", "2026-09-20T00:00:00Z", "2026-09-27T00:00:00Z"},
		{"month", "2026-09-01T00:00:00Z", "2026-09-30T14:25:00Z"},
		{"lastmonth", "2026-08-01T00:00:00Z", "2026-09-01T00:00:00Z"},
		{" Today ", "2026-09-30T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"2026-09-01T00:00:00Z,2026-09-08T00:00:00Z", "2026-09-01T00:00:00Z", "2026-09-08T00:00:00Z"},
		{"2026-09-01t00:00:00z, 2026-09-08t00:00:00z", "2026-09-01T00:00:00Z", "2026-09-08T00:00:00Z"},
		{"2026-09-01T02:00:00+02:00,2026-09-02T00:00:00Z", "2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z"},
		{"1788220800,1788825600", "2026-09-01T00:00:00Z", "2026-09-08T00:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			w, err := Parse(c.in, now)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.in, err)
			}
			if !w.Start.Equal(ts(c.start)) || !w.End.Equal(ts(c.end)) {
				t.Fatalf("Parse(%q) = %s, want %s,%s", c.in, w, c.start, c.end)
			}
			if w.Start.Location() != time.UTC {
				t.Fatalf("start not UTC")
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{
		"", "0d", "-1d", "7x", "tomorrow", "2026-09-08T00:00:00Z,2026-09-01T00:00:00Z",
		"2026-09-01T00:00:00Z,", ",2026-09-01T00:00:00Z", "garbage,garbage",
		"401d", "99999d", "2027-01-01T00:00:00Z,2027-01-02T00:00:00Z", // future
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		if _, err := Parse(in, now); err == nil {
			t.Errorf("Parse(%q): want error", in)
		}
	}
	if _, err := Parse("", now); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty input must return ErrEmpty, got %v", err)
	}
}

func TestQueryEndAndCovered(t *testing.T) {
	w, _ := Parse("today", now)
	if !w.QueryEnd().Equal(now) {
		t.Fatalf("QueryEnd = %s, want now", w.QueryEnd())
	}
	if got := w.Covered(); got != 14*time.Hour+25*time.Minute {
		t.Fatalf("Covered = %s", got)
	}
	y, _ := Parse("yesterday", now)
	if y.Covered() != Day {
		t.Fatalf("yesterday covered %s", y.Covered())
	}
}

func TestParseDefault(t *testing.T) {
	w, err := ParseDefault("  ", "24h", now)
	if err != nil || w.Duration() != Day {
		t.Fatalf("default not applied: %v %s", err, w)
	}
}

func TestPerMonth(t *testing.T) {
	// $10 over one day → $300 per 30-day month.
	if got := PerMonth(10, Day); math.Abs(got-300) > 1e-9 {
		t.Fatalf("PerMonth = %v", got)
	}
	if PerMonth(10, 0) != 0 {
		t.Fatal("zero duration must scale to 0")
	}
}

func TestHourAlignment(t *testing.T) {
	if got := FloorHour(now); !got.Equal(ts("2026-09-30T14:00:00Z")) {
		t.Fatal(got)
	}
	if got := CeilHour(now); !got.Equal(ts("2026-09-30T15:00:00Z")) {
		t.Fatal(got)
	}
	aligned := ts("2026-09-30T14:00:00Z")
	if !CeilHour(aligned).Equal(aligned) {
		t.Fatal("CeilHour must be identity on boundaries")
	}
}

func TestParseStep(t *testing.T) {
	for in, want := range map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "1d": Day, "1w": Week, "60m": time.Hour} {
		got, err := ParseStep(in)
		if err != nil || got != want {
			t.Errorf("ParseStep(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "30m", "1.5h", "0h", "h"} {
		if _, err := ParseStep(in); err == nil {
			t.Errorf("ParseStep(%q): want error", in)
		}
	}
}

func TestFromUnixMS(t *testing.T) {
	w, err := FromUnixMS(0, 0, time.Hour, 14*Day, now)
	if err != nil || !w.End.Equal(now) || w.Duration() != time.Hour {
		t.Fatalf("defaults: %v %s", err, w)
	}
	if _, err := FromUnixMS(now.Add(-20*Day).UnixMilli(), now.UnixMilli(), time.Hour, 14*Day, now); err == nil {
		t.Fatal("want max-span error")
	}
	if _, err := FromUnixMS(now.UnixMilli(), now.Add(-time.Hour).UnixMilli(), time.Hour, 0, now); err == nil {
		t.Fatal("want ordering error")
	}
}
