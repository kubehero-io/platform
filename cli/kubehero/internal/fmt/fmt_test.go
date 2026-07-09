// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package fmt_test

import (
	"bytes"
	"strings"
	"testing"

	kfmt "github.com/kubehero-io/platform/cli/kubehero/internal/fmt"
)

// clusterRows builds a 6-column record set so `table` (first 5 cols) and
// `wide` (all cols) diverge.
func clusterRows() []*kfmt.Row {
	return []*kfmt.Row{
		kfmt.NewRow().
			Set("name", "prod").Set("cloud", "aws").Set("region", "eu-west-1").
			Set("nodes", "12").Set("status", "OK").Set("cost", "$1.20"),
		kfmt.NewRow().
			Set("name", "dev").Set("cloud", "gcp").Set("region", "us-east1").
			Set("nodes", "3").Set("status", "Degraded").Set("cost", "$0.10"),
	}
}

type clusterDoc struct {
	Name  string `json:"name" yaml:"name"`
	Cloud string `json:"cloud" yaml:"cloud"`
	Nodes int    `json:"nodes" yaml:"nodes"`
}

func TestRenderGolden(t *testing.T) {
	structured := []clusterDoc{
		{Name: "prod", Cloud: "aws", Nodes: 12},
		{Name: "dev", Cloud: "gcp", Nodes: 3},
	}
	tests := []struct {
		name   string
		format string
		want   string
	}{
		{
			name:   "table caps at five columns",
			format: "table",
			want: "" +
				"NAME  CLOUD  REGION     NODES  STATUS\n" +
				"prod  aws    eu-west-1  12     OK\n" +
				"dev   gcp    us-east1   3      Degraded\n",
		},
		{
			name:   "empty format defaults to table",
			format: "",
			want: "" +
				"NAME  CLOUD  REGION     NODES  STATUS\n" +
				"prod  aws    eu-west-1  12     OK\n" +
				"dev   gcp    us-east1   3      Degraded\n",
		},
		{
			name:   "wide shows all columns",
			format: "wide",
			want: "" +
				"NAME  CLOUD  REGION     NODES  STATUS    COST\n" +
				"prod  aws    eu-west-1  12     OK        $1.20\n" +
				"dev   gcp    us-east1   3      Degraded  $0.10\n",
		},
		{
			name:   "json pretty-prints the structured value",
			format: "json",
			want: `[
  {
    "name": "prod",
    "cloud": "aws",
    "nodes": 12
  },
  {
    "name": "dev",
    "cloud": "gcp",
    "nodes": 3
  }
]
`,
		},
		{
			name:   "yaml encodes the structured value",
			format: "yaml",
			want: `- name: prod
  cloud: aws
  nodes: 12
- name: dev
  cloud: gcp
  nodes: 3
`,
		},
		{
			name:   "format is case-insensitive",
			format: "JSON",
			want: `[
  {
    "name": "prod",
    "cloud": "aws",
    "nodes": 12
  },
  {
    "name": "dev",
    "cloud": "gcp",
    "nodes": 3
  }
]
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			if err := kfmt.Render(buf, tt.format, clusterRows(), structured); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("Render(%q) mismatch\ngot:\n%s\nwant:\n%s", tt.format, got, tt.want)
			}
		})
	}
}

func TestRenderUnsupportedFormat(t *testing.T) {
	buf := &bytes.Buffer{}
	err := kfmt.Render(buf, "csv", clusterRows(), nil)
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
	if !strings.Contains(err.Error(), `"csv"`) {
		t.Errorf("error %q should name the bad format", err.Error())
	}
}

func TestRenderEmptyRows(t *testing.T) {
	for _, format := range []string{"table", "wide"} {
		t.Run(format, func(t *testing.T) {
			buf := &bytes.Buffer{}
			if err := kfmt.Render(buf, format, nil, nil); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != "(no rows)\n" {
				t.Errorf("output = %q, want %q", got, "(no rows)\n")
			}
		})
	}
}

// Header columns come from the first row; later rows missing a column
// render it as empty rather than panicking or shifting cells.
func TestRenderMissingColumnRendersEmpty(t *testing.T) {
	rows := []*kfmt.Row{
		kfmt.NewRow().Set("name", "prod").Set("cloud", "aws"),
		kfmt.NewRow().Set("name", "dev"), // no cloud column
	}
	buf := &bytes.Buffer{}
	if err := kfmt.Render(buf, "table", rows, nil); err != nil {
		t.Fatal(err)
	}
	// tabwriter still pads the "name" cell because the empty trailing cell
	// terminates the column, hence the trailing spaces after "dev".
	want := "" +
		"NAME  CLOUD\n" +
		"prod  aws\n" +
		"dev   \n"
	if got := buf.String(); got != want {
		t.Errorf("output mismatch\ngot:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestRowSetPreservesOrder(t *testing.T) {
	r := kfmt.NewRow().Set("b", "2").Set("a", "1").Set("c", "3")
	wantCols := []string{"b", "a", "c"}
	if len(r.Cols) != len(wantCols) {
		t.Fatalf("cols = %v, want %v", r.Cols, wantCols)
	}
	for i, c := range wantCols {
		if r.Cols[i] != c {
			t.Errorf("cols[%d] = %q, want %q", i, r.Cols[i], c)
		}
	}
}
