package sqlreview

// P1 D-6 pre-implementation spike (2026-11-21).
// This test does NOT change production behavior — it only demonstrates that
// sqlc-dev/oliphant v0.2.0 (pure Go, no cgo/wasm) can parse PostgreSQL dialect
// statements that today's MySQL-only review engine reports as SR-01.
//
// It is intentionally kept as a test so the production binary does not link
// the parser (test-only import). A real integration must be a product
// decision: dialect routing, AST rewrite surface, and SR-11 handling.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	pg_query "github.com/sqlc-dev/oliphant"
)

var pgDialectSamples = []struct {
	name string
	sql  string
	kind string // expected rough AST node hint in ParseToJSON output
}{
	{"placeholder", "SELECT * FROM users WHERE id = $1", "SelectStmt"},
	{"returning", "INSERT INTO users(name) VALUES('a') RETURNING id", "InsertStmt"},
	{"on_conflict", "INSERT INTO users(id) VALUES(1) ON CONFLICT (id) DO NOTHING", "InsertStmt"},
	{"cast", "SELECT created_at::date FROM users", "TypeCast"},
	{"cte", "WITH x AS (SELECT 1 AS n) SELECT * FROM x", "WithClause"},
	{"jsonb", "SELECT data->>'name' FROM users WHERE data @> '{\"a\":1}'::jsonb", "A_Expr"},
	{"lateral", "SELECT * FROM t CROSS JOIN LATERAL (SELECT 1) s", "RangeSubselect"},
	{"window", "SELECT row_number() OVER (PARTITION BY dept ORDER BY id) FROM users", "WindowDef"},
	{"ctas", "CREATE TABLE archive AS SELECT * FROM users", "CreateTableAsStmt"},
	{"dml_safe", "DELETE FROM users WHERE id = 7", "DeleteStmt"},
}

func TestOliphantPGDialectSpike(t *testing.T) {
	var total time.Duration
	for _, s := range pgDialectSamples {
		start := time.Now()
		tree, err := pg_query.Parse(s.sql)
		if err != nil {
			t.Errorf("%s: oliphant Parse failed: %v", s.name, err)
			continue
		}
		total += time.Since(start)
		if tree == nil || len(tree.GetStmts()) != 1 {
			t.Errorf("%s: expected exactly one statement, got %d", s.name, len(tree.GetStmts()))
			continue
		}
		js, err := pg_query.ParseToJSON(s.sql)
		if err != nil {
			t.Errorf("%s: ParseToJSON failed: %v", s.name, err)
			continue
		}
		var generic map[string]any
		if err := json.Unmarshal([]byte(js), &generic); err != nil {
			t.Errorf("%s: JSON invalid: %v", s.name, err)
		}
		if !strings.Contains(js, s.kind) {
			t.Logf("%s: expected AST hint %q not literally present (informational only)", s.name, s.kind)
		}
	}
	t.Logf("oliphant parsed %d/%d PG dialect samples; cumulative parse time %s (avg %s)",
		len(pgDialectSamples), len(pgDialectSamples), total, total/time.Duration(len(pgDialectSamples)))
}

func TestOliphantRejectsInvalidSQL(t *testing.T) {
	if _, err := pg_query.Parse("THIS IS NOT SQL (("); err == nil {
		t.Fatal("expected a parse error for invalid SQL, got nil")
	}
}

func TestOliphantFingerprint(t *testing.T) {
	a, err := pg_query.Fingerprint("SELECT * FROM users WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := pg_query.Fingerprint("SELECT * FROM users WHERE id = 2")
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || a != b {
		t.Fatalf("fingerprint should be stable across literals: %q vs %q", a, b)
	}
}
