// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/kubehero-io/platform/cli/kubehero/internal/config"
	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
	"github.com/kubehero-io/platform/cli/kubehero/internal/rpc"
)

// Helpers shared by the signal commands (logs, profile, cost, network,
// alerts, ask): client construction, time ranges, proto-aware
// json/yaml output, human units and TTY-only colour.

func signalClients() (*rpc.Clients, *config.Config, error) {
	return signalClientsFrom(resolveConfig())
}

func signalClientsFrom(cfg *config.Config) (*rpc.Clients, *config.Config, error) {
	c, err := rpc.New(cfg)
	return c, cfg, err
}

// unaryCtx bounds one request/response RPC and follows Ctrl-C.
func unaryCtx(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, rpc.UnaryTimeout)
}

// protoValue converts a proto message into plain JSON-shaped values so
// -o json / -o yaml print the wire field names (camelCase), not Go
// struct internals.
func protoValue(m proto.Message) any {
	b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

// render prints rows as a table, or msg as json/yaml.
func render(cmd *cobra.Command, cfg *config.Config, rows []*khfmt.Row, msg proto.Message) error {
	return khfmt.Render(cmd.OutOrStdout(), cfg.Output, rows, protoValue(msg))
}

// structuredOutput reports whether the user asked for json/yaml.
func structuredOutput(cfg *config.Config) bool {
	switch strings.ToLower(cfg.Output) {
	case "json", "yaml":
		return true
	}
	return false
}

// parseSince accepts Go durations plus a day suffix: "30m", "6h", "7d".
func parseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n <= 0 || n > 400 {
			return 0, fmt.Errorf("invalid duration %q (use e.g. 30m, 6h, 7d)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 30m, 6h, 7d)", s)
	}
	return d, nil
}

// timeRange resolves --since / --from / --to into unix milliseconds.
// --from/--to take RFC3339 and win over --since.
func timeRange(since, from, to string, now time.Time) (int64, int64, error) {
	end := now
	if to != "" {
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return 0, 0, fmt.Errorf("--to: %w", err)
		}
		end = t
	}
	var start time.Time
	if from != "" {
		t, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return 0, 0, fmt.Errorf("--from: %w", err)
		}
		start = t
	} else {
		d, err := parseSince(since)
		if err != nil {
			return 0, 0, err
		}
		start = end.Add(-d)
	}
	if !start.Before(end) {
		return 0, 0, fmt.Errorf("time range is empty (%s → %s)", start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	return start.UnixMilli(), end.UnixMilli(), nil
}

// ─── units ───────────────────────────────────────────────────────────────

func usd(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "$0"
	}
	neg := v < 0
	v = math.Abs(v)
	var s string
	if v < 100 {
		s = strconv.FormatFloat(v, 'f', 2, 64)
	} else {
		s = thousands(strconv.FormatFloat(v, 'f', 0, 64))
	}
	if neg {
		return "-$" + s
	}
	return "$" + s
}

func thousands(s string) string {
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func pct(frac float64) string { return strconv.FormatFloat(frac*100, 'f', 0, 64) + "%" }

func pctVal(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + "%" }

func cores(v float64) string {
	if v < 1 {
		return strconv.Itoa(int(math.Round(v*1000))) + "m"
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func bytesIEC(b float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatFloat(b, 'f', 0, 64) + units[i]
	}
	return strconv.FormatFloat(b, 'f', 1, 64) + units[i]
}

func count(n int64) string { return thousands(strconv.FormatInt(n, 10)) }

func labelString(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

// ─── colour (TTY only; NO_COLOR and --no-color win) ─────────────────────

type painter struct{ on bool }

func newPainter(w io.Writer) painter {
	if flagNoColor || os.Getenv("NO_COLOR") != "" {
		return painter{}
	}
	f, ok := w.(*os.File)
	if !ok {
		return painter{}
	}
	st, err := f.Stat()
	return painter{on: err == nil && st.Mode()&os.ModeCharDevice != 0}
}

func (p painter) wrap(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p painter) level(l string) string {
	up := strings.ToUpper(nonEmptyStr(l, "-"))
	switch strings.ToLower(l) {
	case "error", "fatal", "critical", "crit", "panic":
		return p.wrap("31", up)
	case "warn", "warning":
		return p.wrap("33", up)
	case "info":
		return p.wrap("36", up)
	case "debug", "trace":
		return p.wrap("2", up)
	}
	return up
}

func (p painter) dim(s string) string   { return p.wrap("2", s) }
func (p painter) bold(s string) string  { return p.wrap("1", s) }
func (p painter) red(s string) string   { return p.wrap("31", s) }
func (p painter) green(s string) string { return p.wrap("32", s) }

func nonEmptyStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
