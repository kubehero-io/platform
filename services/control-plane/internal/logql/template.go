// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode"
)

// Template functions available to line_format / label_format — the
// commonly used subset of Loki's (which are Sprig-derived). Templates
// only ever see the line's labels; there is no I/O.
var templateFuncs = template.FuncMap{
	// per-line values, rebound for every pipeline (see bindTemplate)
	"__line__":      func() string { return "" },
	"__timestamp__": func() time.Time { return time.Time{} },

	"ToLower": strings.ToLower, "lower": strings.ToLower,
	"ToUpper": strings.ToUpper, "upper": strings.ToUpper,
	"title": titleCase, "Title": titleCase,
	"Replace": strings.Replace,
	"replace": func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
	"Trim":    strings.Trim, "TrimLeft": strings.TrimLeft, "TrimRight": strings.TrimRight,
	"TrimPrefix": strings.TrimPrefix, "TrimSuffix": strings.TrimSuffix, "TrimSpace": strings.TrimSpace,
	"trim":       strings.TrimSpace,
	"trimAll":    func(cut, s string) string { return strings.Trim(s, cut) },
	"trimPrefix": func(p, s string) string { return strings.TrimPrefix(s, p) },
	"trimSuffix": func(p, s string) string { return strings.TrimSuffix(s, p) },
	"contains":   func(sub, s string) bool { return strings.Contains(s, sub) },
	"hasPrefix":  func(p, s string) bool { return strings.HasPrefix(s, p) },
	"hasSuffix":  func(p, s string) bool { return strings.HasSuffix(s, p) },
	"repeat": func(n int, s string) string {
		if n < 0 || n*len(s) > maxFormattedLine {
			n = 0
		}
		return strings.Repeat(s, n)
	},
	"substr": substr,
	"trunc": func(n int, s string) string {
		r := []rune(s)
		switch {
		case n >= 0 && n < len(r):
			return string(r[:n])
		case n < 0 && -n < len(r):
			return string(r[len(r)+n:])
		}
		return s
	},
	"default": func(def string, v string) string {
		if v == "" {
			return def
		}
		return v
	},
	"count": func(sub, s string) int { return strings.Count(s, sub) },
	"len":   func(s string) int { return len([]rune(s)) },
	"printf": func(format string, args ...any) string {
		return fmt.Sprintf(format, args...)
	},
	"alignLeft": func(n int, s string) string {
		r := []rune(s)
		if n <= 0 || n > maxFormattedLine {
			return s
		}
		if len(r) >= n {
			return string(r[:n])
		}
		return s + strings.Repeat(" ", n-len(r))
	},
	"alignRight": func(n int, s string) string {
		r := []rune(s)
		if n <= 0 || n > maxFormattedLine {
			return s
		}
		if len(r) >= n {
			return string(r[len(r)-n:])
		}
		return strings.Repeat(" ", n-len(r)) + s
	},
	"regexReplaceAll": func(re, s, repl string) (string, error) {
		r, err := cachedRegex(re)
		if err != nil {
			return "", err
		}
		return r.ReplaceAllString(s, repl), nil
	},
	"regexReplaceAllLiteral": func(re, s, repl string) (string, error) {
		r, err := cachedRegex(re)
		if err != nil {
			return "", err
		}
		return r.ReplaceAllLiteralString(s, repl), nil
	},
	"b64enc": func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
	"b64dec": func(s string) string {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return s
		}
		return string(b)
	},
	"urlencode": url.QueryEscape,
	"urldecode": func(s string) string {
		v, err := url.QueryUnescape(s)
		if err != nil {
			return s
		}
		return v
	},
	"unixEpoch":       func(t time.Time) string { return fmt.Sprint(t.Unix()) },
	"unixEpochMillis": func(t time.Time) string { return fmt.Sprint(t.UnixMilli()) },
	"unixEpochNanos":  func(t time.Time) string { return fmt.Sprint(t.UnixNano()) },
	"date":            func(layout string, t time.Time) string { return t.Format(layout) },
	"toDate": func(layout, s string) time.Time {
		t, _ := time.Parse(layout, s)
		return t
	},
	"now": func() time.Time { return time.Now().UTC() },
}

func titleCase(s string) string {
	prev := ' '
	return strings.Map(func(r rune) rune {
		out := r
		if unicode.IsSpace(prev) {
			out = unicode.ToTitle(r)
		}
		prev = r
		return out
	}, s)
}

func substr(start, end int, s string) string {
	r := []rune(s)
	if start < 0 {
		start = 0
	}
	if end < 0 || end > len(r) {
		end = len(r)
	}
	if start > end {
		return ""
	}
	return string(r[start:end])
}

var (
	regexCacheMu sync.Mutex
	regexCache   = map[string]*regexp.Regexp{}
)

// cachedRegex compiles template regexes once; the cache is bounded.
func cachedRegex(expr string) (*regexp.Regexp, error) {
	regexCacheMu.Lock()
	defer regexCacheMu.Unlock()
	if r, ok := regexCache[expr]; ok {
		return r, nil
	}
	r, err := compileRegex(expr, expr)
	if err != nil {
		return nil, err
	}
	if len(regexCache) >= 256 {
		regexCache = map[string]*regexp.Regexp{}
	}
	regexCache[expr] = r
	return r, nil
}

// newTemplate parses a line_format / label_format template. Missing
// labels render as "" (not "<no value>").
func newTemplate(name, text string) (*template.Template, error) {
	if len(text) > maxRegexLen {
		return nil, fmt.Errorf("template is longer than %d bytes", maxRegexLen)
	}
	return template.New(name).Option("missingkey=zero").Funcs(templateFuncs).Parse(text)
}
