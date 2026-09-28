// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clickhouse

import (
	"strings"
	"testing"
)

func TestSplitStatementsStripsCommentsWithSemicolons(t *testing.T) {
	in := `-- header; with; semicolons
CREATE TABLE a (x Int8) ENGINE = Memory; -- trailing; comment
-- only a comment;
INSERT INTO a VALUES ('it''s; fine'), ('back\'slash; too');
`
	got := SplitStatements(in)
	if len(got) != 2 {
		t.Fatalf("want 2 statements, got %d: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "CREATE TABLE a") {
		t.Errorf("stmt 0 = %q", got[0])
	}
	if !strings.Contains(got[1], `'it''s; fine'`) || !strings.Contains(got[1], `'back\'slash; too'`) {
		t.Errorf("quoted semicolons must survive: %q", got[1])
	}
}

// Every statement in every embedded migration must start with a DDL /
// DML keyword — a stray comment fragment executed as SQL is exactly
// the failure mode the old splitter had.
func TestEmbeddedMigrationsParse(t *testing.T) {
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 2 {
		t.Fatalf("want at least 2 migrations, got %d", len(ms))
	}
	for i, m := range ms {
		if m.Version != i+1 {
			t.Errorf("migration %s: version %d, want contiguous %d", m.Name, m.Version, i+1)
		}
		stmts := SplitStatements(m.SQL)
		if len(stmts) == 0 {
			t.Errorf("%s: no statements", m.Name)
		}
		for _, s := range stmts {
			head := strings.ToUpper(strings.Fields(s)[0])
			switch head {
			case "CREATE", "ALTER", "DROP", "INSERT", "RENAME", "OPTIMIZE":
			default:
				t.Errorf("%s: statement starts with %q: %.80s", m.Name, head, s)
			}
		}
	}
}
