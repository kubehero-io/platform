// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package timewin parses the query windows every read API speaks —
// CostService, the OpenCost-compatible /allocation routes, the FOCUS
// export, team spend and rightsizing — into concrete UTC intervals.
//
// The grammar deliberately follows OpenCost's window parser so the
// same string means the same interval whether a Kubecost/OpenCost
// consumer or the KubeHero dashboard sends it:
//
//	30m, 12h         rolling: [now-N, now)
//	1d, 7d, 2w       "the past N days": today (midnight UTC → next
//	                 midnight) plus the N-1 whole days before it
//	today, yesterday calendar days (UTC)
//	week, lastweek   week-to-date / the previous full week (Sunday start)
//	month, lastmonth month-to-date / the previous calendar month
//	<rfc3339>,<rfc3339> or <unix>,<unix>   an explicit pair
//
// Day-aligned windows end in the future (tomorrow's midnight) exactly
// like OpenCost's; Window.QueryEnd clamps that to "now" for queries,
// while Window.End keeps the nominal edge for echoing back to callers.
package timewin

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Day and Week are fixed-length (UTC has no DST).
	Day  = 24 * time.Hour
	Week = 7 * Day
	// MaxSpan bounds every window: the hourly rollups keep 400 days.
	MaxSpan = 400 * Day
	// maxInputLen rejects absurd inputs before any regex runs.
	maxInputLen = 64
)

// Window is a half-open UTC interval [Start, End).
type Window struct {
	Start time.Time
	End   time.Time
	// Now is the reference instant the window was resolved against —
	// kept so QueryEnd and Covered are consistent within one request.
	Now time.Time
}

// QueryEnd is the end to query up to: data after "now" doesn't exist.
func (w Window) QueryEnd() time.Time {
	if w.End.After(w.Now) {
		return w.Now
	}
	return w.End
}

// Covered is the elapsed part of the window — what averages and
// month-scaling divide by. Never negative.
func (w Window) Covered() time.Duration {
	d := w.QueryEnd().Sub(w.Start)
	if d < 0 {
		return 0
	}
	return d
}

// Duration is the nominal span (End - Start).
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// String renders the canonical RFC3339 pair.
func (w Window) String() string {
	return w.Start.Format(time.RFC3339) + "," + w.End.Format(time.RFC3339)
}

// ErrEmpty is returned for "" so callers can substitute their default.
var ErrEmpty = errors.New("window is empty")

var durRE = regexp.MustCompile(`^(\d{1,4})(m|h|d|w)$`)

// Parse resolves s against now (converted to UTC). An empty string
// returns ErrEmpty; use ParseDefault to substitute a default.
func Parse(s string, now time.Time) (Window, error) {
	now = now.UTC()
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return Window{}, ErrEmpty
	}
	if len(s) > maxInputLen {
		return Window{}, fmt.Errorf("window %q: too long", s[:maxInputLen])
	}
	today := now.Truncate(Day)
	var start, end time.Time
	switch s {
	case "today":
		start, end = today, today.Add(Day)
	case "yesterday":
		start, end = today.Add(-Day), today
	case "week":
		start, end = today.Add(-Day*time.Duration(today.Weekday())), now
	case "lastweek":
		start = today.Add(-Day * time.Duration(today.Weekday()+7))
		end = start.Add(Week)
	case "month":
		start, end = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), now
	case "lastmonth":
		end = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		start = end.AddDate(0, -1, 0)
	default:
		var err error
		if m := durRE.FindStringSubmatch(s); m != nil {
			start, end, err = parseDuration(m[1], m[2], now, today)
		} else if strings.Contains(s, ",") {
			start, end, err = parsePair(s)
		} else {
			err = fmt.Errorf("unrecognised window %q (want e.g. 24h, 7d, today, month, lastmonth or an RFC3339 pair)", s)
		}
		if err != nil {
			return Window{}, err
		}
	}
	w := Window{Start: start, End: end, Now: now}
	if !w.End.After(w.Start) {
		return Window{}, fmt.Errorf("window %q: end must be after start", s)
	}
	if w.Duration() > MaxSpan {
		return Window{}, fmt.Errorf("window %q: longer than %d days", s, int(MaxSpan/Day))
	}
	if !w.Start.Before(now) {
		return Window{}, fmt.Errorf("window %q starts in the future", s)
	}
	return w, nil
}

// ParseDefault is Parse with a fallback for empty input.
func ParseDefault(s, def string, now time.Time) (Window, error) {
	if strings.TrimSpace(s) == "" {
		s = def
	}
	return Parse(s, now)
}

func parseDuration(num, unit string, now, today time.Time) (time.Time, time.Time, error) {
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("window duration must be a positive integer, got %q", num)
	}
	switch unit {
	case "m":
		return now.Add(-time.Duration(n) * time.Minute), now, nil
	case "h":
		return now.Add(-time.Duration(n) * time.Hour), now, nil
	}
	days := n
	if unit == "w" {
		days = n * 7
	}
	// OpenCost: "the past N days" is today plus the N-1 whole days
	// before it, midnight to midnight UTC.
	end := today.Add(Day)
	return end.Add(-Day * time.Duration(days)), end, nil
}

func parsePair(s string) (time.Time, time.Time, error) {
	a, b, _ := strings.Cut(s, ",")
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	start, err := parseInstant(a)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseInstant(b)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start, end, nil
}

func parseInstant(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("window pair has an empty side")
	}
	if isDigits(s) {
		sec, err := strconv.ParseInt(s, 10, 64)
		if err != nil || sec < 0 || sec > 1<<40 {
			return time.Time{}, fmt.Errorf("bad unix timestamp %q", s)
		}
		return time.Unix(sec, 0).UTC(), nil
	}
	// Parse lower-cased input: RFC3339 accepts "t"/"z" only uppercase.
	t, err := time.Parse(time.RFC3339, strings.ToUpper(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("bad RFC3339 time %q", s)
	}
	return t.UTC(), nil
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// FromUnixMS builds a window from the millisecond pair the signal RPCs
// take (start/end = 0 → defaults), bounded by maxSpan.
func FromUnixMS(startMS, endMS int64, defSpan, maxSpan time.Duration, now time.Time) (Window, error) {
	now = now.UTC()
	end := now
	if endMS > 0 {
		end = time.UnixMilli(endMS).UTC()
	}
	start := end.Add(-defSpan)
	if startMS > 0 {
		start = time.UnixMilli(startMS).UTC()
	}
	w := Window{Start: start, End: end, Now: now}
	if !end.After(start) {
		return Window{}, errors.New("end must be after start")
	}
	if maxSpan > 0 && w.Duration() > maxSpan {
		return Window{}, fmt.Errorf("time range longer than %s", maxSpan)
	}
	if !start.Before(now) {
		return Window{}, errors.New("time range starts in the future")
	}
	return w, nil
}

// MonthHours is the "month" every $/mo figure in KubeHero scales to:
// 30 days, matching the burn-rate and anomaly code (× 24 × 30).
const MonthHours = 24 * 30

// PerMonth scales an amount observed over d to a 30-day month.
func PerMonth(amount float64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return amount * (MonthHours * float64(time.Hour)) / float64(d)
}

// FloorHour / CeilHour align to the hourly rollup grid.
func FloorHour(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }

// CeilHour rounds t up to the next hour boundary (identity when aligned).
func CeilHour(t time.Time) time.Time {
	f := FloorHour(t)
	if f.Equal(t) {
		return f
	}
	return f.Add(time.Hour)
}

// ParseStep parses a step like "1h", "6h", "1d", "1w". Steps must be a
// whole number of hours so buckets line up with the hourly rollups.
func ParseStep(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	m := durRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("bad step %q (want e.g. 1h, 6h, 1d, 1w)", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad step %q", s)
	}
	var d time.Duration
	switch m[2] {
	case "m":
		d = time.Duration(n) * time.Minute
	case "h":
		d = time.Duration(n) * time.Hour
	case "d":
		d = time.Duration(n) * Day
	case "w":
		d = time.Duration(n) * Week
	}
	if d%time.Hour != 0 {
		return 0, fmt.Errorf("step %q must be a whole number of hours", s)
	}
	return d, nil
}
