// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package logql_test

import (
	"context"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
)

// Every statement builder produces SQL ClickHouse accepts, for a spread
// of matcher, filter and prefilter shapes.
func TestGeneratedSQLRuns(t *testing.T) {
	db := chtest.Open(t, "kh_ingest_logql")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	w := &clickhouse.SignalWriter{Conn: db.Native}
	if err := w.WriteLogs(ctx, []clickhouse.LogRow{
		{TS: now.Add(-time.Minute), OrgID: "default", ClusterID: "c1", Namespace: "shop", Pod: "api-1", Level: "error",
			Labels: map[string]string{"app": "api"}, Body: `{"msg":"50%_off","status":503,"req":{"method":"GET"}}`},
	}); err != nil {
		t.Fatal(err)
	}
	// query → rows the SQL part of the plan must return
	queries := map[string]int{
		`{namespace="shop", app=~"a|api", pod!~"x.*", cluster=~".*", node!~".+"}`:       1,
		`{namespace="shop", team!=""}`:                                                  0, // team is empty
		`{namespace="shop"} |= "50%_off" != "debug" |~ "5\\d\\d" |~ "(?i)MSG" !~ "x|y"`: 1,
		`{namespace="shop"} |= "50%%off"`:                                               0, // % is literal
		`{namespace="shop"} | level="error" | json | status >= 500 | msg="50%_off"`:     1,
		`{namespace="shop"} | json m="req.method" | m="GET"`:                            1,
		`{namespace="shop"} | json m="req.method" | m="POST"`:                           0,
		`{app="api"} | logfmt | method="GET"`:                                           0, // body is JSON, not logfmt
		`{namespace="shop", level="error"}`:                                             1,
	}
	from, to := now.Add(-time.Hour), now.Add(time.Minute)
	g, err := logql.NewGrid(from, to, time.Minute, 5*time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	for q, wantRows := range queries {
		sel, err := logql.ParseLogSelector(q)
		if err != nil {
			t.Fatal(err)
		}
		p := logql.PlanSelector(sel)
		stmts := map[string]logql.Query{
			"logs":        p.LogsQuery("default", from.UnixNano(), to.UnixNano(), false, 10),
			"count":       p.CountQuery("default", from.UnixNano(), to.UnixNano()),
			"bucket":      p.BucketQuery("default", g, []string{"namespace", "app"}, 100),
			"bucket-all":  p.BucketQuery("default", g, nil, 100),
			"volume":      p.VolumeQuery("default", from.UnixNano(), to.UnixNano(), int64(time.Minute), "level", false),
			"label-names": p.LabelNamesQuery("default", from.UnixNano(), to.UnixNano(), 1000, 100),
			"label-vals":  p.LabelValuesQuery("default", "app", from.UnixNano(), to.UnixNano(), 100, false),
		}
		if p.RollupOK {
			stmts["rollup"] = p.RollupBucketQuery("default", g, []string{"namespace"}, 100)
			stmts["rollup-none"] = p.RollupBucketQuery("default", g, []string{}, 100)
			stmts["volume-rollup"] = p.VolumeQuery("default", from.UnixNano(), to.UnixNano(), int64(time.Minute), "level", true)
			stmts["label-vals-rollup"] = p.LabelValuesQuery("default", "namespace", from.UnixNano(), to.UnixNano(), 100, true)
		}
		for name, st := range stmts {
			rows, err := db.Native.Query(ctx, st.SQL, st.Args...)
			if err != nil {
				t.Fatalf("%s / %s: %v\n%s\n%#v", q, name, err, st.SQL, st.Args)
			}
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("%s / %s: %v", q, name, err)
			}
			_ = rows.Close()
			if name == "logs" && n != wantRows {
				t.Errorf("%s: logs query returned %d rows, want %d", q, n, wantRows)
			}
		}
	}
}
