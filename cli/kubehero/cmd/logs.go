// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	khfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

func logsCmd() *cobra.Command {
	var since, from, to, direction, cluster string
	var limit int
	var tail, patterns, volume bool
	var groupBy string
	c := &cobra.Command{
		Use:   "logs '<logql>'",
		Short: "Query, tail, cluster and size logs with LogQL",
		Long: `Run a LogQL query against KubeHero's log store.

Log queries print lines (newest first); metric queries
(count_over_time, rate, sum by (...) ...) print series.

  --patterns  cluster matching lines into templates with counts
  --volume    line volume split by a label (default level), with $/mo
  -f/--tail   stream new lines as they arrive (Ctrl-C to stop)`,
		Example: `  kubehero logs '{namespace="payments", level="error"}' --since 1h
  kubehero logs '{namespace="payments"} |= "timeout"' -f
  kubehero logs '{namespace="payments"}' --patterns --since 24h
  kubehero logs '{level="error"}' --volume --group-by namespace
  kubehero logs 'sum by (namespace) (count_over_time({level="error"}[5m]))'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.TrimSpace(args[0])
			if query == "" {
				return errors.New("a LogQL query is required, e.g. '{namespace=\"payments\"}'")
			}
			if n := boolCount(tail, patterns, volume); n > 1 {
				return errors.New("--tail, --patterns and --volume are mutually exclusive")
			}
			if limit < 1 || limit > 5000 {
				return errors.New("--limit must be within 1..5000")
			}
			cl, cfg, err := signalClients()
			if err != nil {
				return err
			}
			start, end, err := timeRange(since, from, to, time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			paint := newPainter(out)

			switch {
			case tail:
				return tailLogs(cmd, cl.Logs.TailLogs, query, cluster, paint, structuredOutput(cfg))
			case patterns:
				ctx, cancel := unaryCtx(cmd)
				defer cancel()
				res, err := cl.Logs.GetLogPatterns(ctx, connect.NewRequest(&kuberov1.GetLogPatternsRequest{
					Query: query, StartUnixMs: start, EndUnixMs: end, Limit: int32(min(limit, 200)), ClusterId: cluster,
				}))
				if err != nil {
					return err
				}
				rows := make([]*khfmt.Row, 0, len(res.Msg.GetPatterns()))
				for _, p := range res.Msg.GetPatterns() {
					rows = append(rows, khfmt.NewRow().
						Set("count", count(p.GetCount())).
						Set("share", pctVal(p.GetSharePct())).
						Set("level", paint.level(p.GetLevel())).
						Set("pattern", p.GetPattern()).
						Set("sample", p.GetSample()))
				}
				if err := render(cmd, cfg, rows, res.Msg); err != nil {
					return err
				}
				if !structuredOutput(cfg) {
					fmt.Fprintf(out, "\n%s lines analysed\n", count(res.Msg.GetLinesAnalyzed()))
				}
				return nil
			case volume:
				ctx, cancel := unaryCtx(cmd)
				defer cancel()
				res, err := cl.Logs.GetLogVolume(ctx, connect.NewRequest(&kuberov1.GetLogVolumeRequest{
					Query: query, StartUnixMs: start, EndUnixMs: end, GroupBy: groupBy, ClusterId: cluster,
				}))
				if err != nil {
					return err
				}
				rows := make([]*khfmt.Row, 0, len(res.Msg.GetSeries()))
				for _, s := range res.Msg.GetSeries() {
					var total float64
					for _, p := range s.GetPoints() {
						total += p.GetValue()
					}
					rows = append(rows, khfmt.NewRow().
						Set("series", labelString(s.GetLabels())).
						Set("lines", count(int64(total))).
						Set("trend", sparkline(s.GetPoints())))
				}
				if err := render(cmd, cfg, rows, res.Msg); err != nil {
					return err
				}
				if !structuredOutput(cfg) {
					fmt.Fprintf(out, "\n%s lines · %s · ~%s/mo to store\n",
						count(res.Msg.GetTotalLines()), bytesIEC(float64(res.Msg.GetTotalBytes())), usd(res.Msg.GetEstCostUsdMonth()))
				}
				return nil
			}

			ctx, cancel := unaryCtx(cmd)
			defer cancel()
			res, err := cl.Logs.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{
				Query: query, StartUnixMs: start, EndUnixMs: end, Limit: int32(limit),
				Direction: direction, ClusterId: cluster,
			}))
			if err != nil {
				return err
			}
			if structuredOutput(cfg) {
				return render(cmd, cfg, nil, res.Msg)
			}
			if res.Msg.GetResultType() == "matrix" {
				rows := make([]*khfmt.Row, 0, len(res.Msg.GetSeries()))
				for _, s := range res.Msg.GetSeries() {
					var last, peak float64
					for _, p := range s.GetPoints() {
						last = p.GetValue()
						peak = max(peak, p.GetValue())
					}
					rows = append(rows, khfmt.NewRow().
						Set("series", labelString(s.GetLabels())).
						Set("last", trimFloat(last)).
						Set("max", trimFloat(peak)).
						Set("trend", sparkline(s.GetPoints())))
				}
				return render(cmd, cfg, rows, res.Msg)
			}
			for _, l := range res.Msg.GetLines() {
				printLogLine(out, paint, l)
			}
			if len(res.Msg.GetLines()) == 0 {
				fmt.Fprintln(out, "(no lines)")
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&since, "since", "1h", "Lookback, e.g. 15m, 6h, 7d")
	f.StringVar(&from, "from", "", "Start time (RFC3339); overrides --since")
	f.StringVar(&to, "to", "", "End time (RFC3339); default now")
	f.IntVar(&limit, "limit", 100, "Max lines (log queries) or patterns")
	f.StringVar(&direction, "direction", "backward", "backward (newest first) | forward")
	f.StringVar(&cluster, "cluster", "", "Cluster id (also expressible as {cluster=\"…\"})")
	f.BoolVarP(&tail, "tail", "f", false, "Stream new lines as they arrive")
	f.BoolVar(&patterns, "patterns", false, "Cluster lines into patterns")
	f.BoolVar(&volume, "volume", false, "Show line volume by label")
	f.StringVar(&groupBy, "group-by", "", "Label to split --volume by (default level)")
	return c
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func printLogLine(w io.Writer, paint painter, l *kuberov1.LogLine) {
	ts := time.Unix(0, l.GetTsUnixNano()).UTC().Format("2006-01-02T15:04:05.000Z")
	lab := l.GetLabels()
	src := lab["namespace"]
	if wl := nonEmptyStr(lab["workload"], lab["pod"]); wl != "" {
		src += "/" + wl
	}
	fmt.Fprintf(w, "%s %-5s %s %s\n", paint.dim(ts), paint.level(l.GetLevel()), paint.dim(src), l.GetBody())
}

// tailLogs streams TailLogs until the server ends the stream or the
// user hits Ctrl-C.
func tailLogs(
	cmd *cobra.Command,
	tail func(context.Context, *connect.Request[kuberov1.TailLogsRequest]) (*connect.ServerStreamForClient[kuberov1.TailLogsResponse], error),
	query, cluster string,
	paint painter,
	asJSON bool,
) error {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	stream, err := tail(ctx, connect.NewRequest(&kuberov1.TailLogsRequest{Query: query, ClusterId: cluster}))
	if err != nil {
		return err
	}
	defer stream.Close()
	out := cmd.OutOrStdout()
	var dropped int64
	for stream.Receive() {
		msg := stream.Msg()
		for _, l := range msg.GetLines() {
			if asJSON {
				b, _ := jsonLine(l)
				fmt.Fprintln(out, b)
				continue
			}
			printLogLine(out, paint, l)
		}
		if d := msg.GetDropped(); d > 0 {
			dropped += d
			fmt.Fprintf(cmd.ErrOrStderr(), "… %d lines dropped to keep up\n", d)
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	if dropped > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "%d lines dropped in total\n", dropped)
	}
	return nil
}

func jsonLine(l *kuberov1.LogLine) (string, error) {
	b, err := json.Marshal(protoValue(l))
	return string(b), err
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline renders up to 40 points as a unicode bar strip.
func sparkline(pts []*kuberov1.Point) string {
	if len(pts) == 0 {
		return ""
	}
	vals := make([]float64, 0, 40)
	stride := (len(pts) + 39) / 40
	for i := 0; i < len(pts); i += stride {
		var sum float64
		for _, p := range pts[i:min(i+stride, len(pts))] {
			sum += p.GetValue()
		}
		vals = append(vals, sum)
	}
	peak := 0.0
	for _, v := range vals {
		peak = max(peak, v)
	}
	var b strings.Builder
	for _, v := range vals {
		idx := 0
		if peak > 0 {
			idx = int(v / peak * float64(len(sparkRunes)-1))
		}
		b.WriteRune(sparkRunes[idx])
	}
	return b.String()
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.3f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
