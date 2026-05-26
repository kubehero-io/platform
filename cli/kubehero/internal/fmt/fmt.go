// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package fmt renders any record set in table / json / yaml / wide.

package fmt

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"
)

// Row is a generic ordered string map. Use NewRow to preserve column order.
type Row struct {
	Cols   []string
	Values []string
}

func NewRow() *Row { return &Row{} }
func (r *Row) Set(col, val string) *Row {
	r.Cols = append(r.Cols, col)
	r.Values = append(r.Values, val)
	return r
}

// Render writes records using the requested format. `wide` shows every
// column; `table` (default) shows only the first 5 to stay readable.
func Render(w io.Writer, format string, rows []*Row, structured any) error {
	switch strings.ToLower(format) {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(structured)
	case "yaml":
		return yaml.NewEncoder(w).Encode(structured)
	case "wide":
		return tableOut(w, rows, 0)
	case "", "table":
		return tableOut(w, rows, 5)
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}

func tableOut(w io.Writer, rows []*Row, maxCols int) error {
	if len(rows) == 0 {
		fmt.Fprintln(w, "(no rows)")
		return nil
	}
	cols := rows[0].Cols
	if maxCols > 0 && len(cols) > maxCols {
		cols = cols[:maxCols]
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, r := range rows {
		vals := make([]string, len(cols))
		for i, c := range cols {
			vals[i] = lookup(r, c)
		}
		fmt.Fprintln(tw, strings.Join(vals, "\t"))
	}
	return tw.Flush()
}

func lookup(r *Row, col string) string {
	for i, c := range r.Cols {
		if c == col {
			return r.Values[i]
		}
	}
	return ""
}
