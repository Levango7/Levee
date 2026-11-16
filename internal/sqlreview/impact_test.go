package sqlreview

// impact_test.go pins the static impact surface: which tables a batch names,
// what it does to them, and what the engine refuses to claim.
//
// The refusals are the point. The surface says nothing about rows and locks
// (that needs a live database — docs/blocked-items.md D-7), and it says
// nothing about a statement it only read partway: a prefix's tables are not
// the statement's tables, and an approver who reads "touches nothing" for a
// statement nobody read has been misled in the most expensive direction.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func impactOf(t *testing.T, batch string) *ReviewResult {
	t.Helper()
	res, err := Review(batch)
	require.NoError(t, err, "batch %q", batch)
	return res
}

func TestImpact_StatementsAndTables(t *testing.T) {
	cases := []struct {
		name       string
		sql        string
		operation  string
		writes     []string
		reads      []string
		wholeTable bool
		tables     []string
	}{
		{
			name: "bounded update", sql: "UPDATE orders SET a=1 WHERE id=1",
			operation: "update", writes: []string{"orders"}, tables: []string{"orders"},
		},
		{
			name: "unbounded delete", sql: "DELETE FROM orders",
			operation: "delete", writes: []string{"orders"}, wholeTable: true, tables: []string{"orders"},
		},
		{
			name: "join reads both sides", sql: "SELECT o.id FROM orders o JOIN users u ON u.id = o.uid",
			operation: "select", reads: []string{"orders", "users"}, wholeTable: true,
			tables: []string{"orders", "users"},
		},
		{
			name: "insert of literal rows is bounded", sql: "INSERT INTO t (a) VALUES (1)",
			operation: "insert", writes: []string{"t"}, tables: []string{"t"},
		},
		{
			name: "insert from a select is not", sql: "INSERT INTO t (a) SELECT a FROM src",
			operation: "insert", writes: []string{"t"}, reads: []string{"src"}, wholeTable: true,
			tables: []string{"src", "t"},
		},
		{
			name: "subquery table is found by the walker", sql: "DELETE FROM t WHERE id IN (SELECT id FROM audit)",
			operation: "delete", writes: []string{"t"}, reads: []string{"audit"}, tables: []string{"audit", "t"},
		},
		{
			name: "alter acts on the object", sql: "ALTER TABLE t ADD COLUMN c INT",
			operation: "alter", writes: []string{"t"}, wholeTable: true, tables: []string{"t"},
		},
		{
			name: "drop acts on the object", sql: "DROP TABLE t",
			operation: "drop", writes: []string{"t"}, wholeTable: true, tables: []string{"t"},
		},
		{
			name: "truncate acts on the object", sql: "TRUNCATE TABLE t",
			operation: "truncate", writes: []string{"t"}, wholeTable: true, tables: []string{"t"},
		},
		{
			// A create has no rows to be unbounded over, so it is not
			// "whole table" — the rule is about existing row sets.
			name: "create is not whole-table", sql: "CREATE TABLE t (id INT PRIMARY KEY) ENGINE=InnoDB",
			operation: "create", writes: []string{"t"}, tables: []string{"t"},
		},
		{
			name: "rename names both sides", sql: "RENAME TABLE a TO b",
			operation: "rename", writes: []string{"a", "b"}, wholeTable: true, tables: []string{"a", "b"},
		},
		{
			name: "qualified name keeps its qualifier", sql: "DELETE FROM db1.orders WHERE id=1",
			operation: "delete", writes: []string{"db1.orders"}, tables: []string{"db1.orders"},
		},
		{
			name:      "multi-table delete separates targets from sources",
			sql:       "DELETE t1 FROM t1 JOIN t2 ON t1.id = t2.id WHERE t2.gone = 1",
			operation: "delete", writes: []string{"t1"}, reads: []string{"t2"}, tables: []string{"t1", "t2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := impactOf(t, c.sql)
			require.Len(t, res.Statements, 1)
			imp := res.Statements[0].Impact

			assert.Equal(t, c.operation, imp.Operation)
			assert.Equal(t, c.writes, imp.Writes)
			assert.Equal(t, c.reads, imp.Reads)
			assert.Equal(t, c.wholeTable, imp.WholeTable)
			assert.True(t, imp.Complete, "a statement read in full is complete")
			assert.False(t, res.Impact.Incomplete)

			var got []string
			for _, te := range res.Impact.Tables {
				got = append(got, te.Table)
				assert.Contains(t, te.Operations, c.operation,
					"the table's operation list must carry the verb that named it")
				assert.Equal(t, c.wholeTable, te.WholeTable)
			}
			assert.Equal(t, c.tables, got, "tables are reported sorted")
		})
	}
}

// A table's operation list is the union across statements, and one unbounded
// statement makes the table's flag true: the flag is "is anything about this
// table unbounded", not "is the average".
func TestImpact_BatchAggregatesPerTable(t *testing.T) {
	res := impactOf(t, "DELETE FROM b WHERE id=1; UPDATE b SET x=1; SELECT * FROM a")
	require.Len(t, res.Impact.Tables, 2)

	byName := map[string]TableImpact{}
	for _, te := range res.Impact.Tables {
		byName[te.Table] = te
	}
	assert.Equal(t, []string{"delete", "update"}, byName["b"].Operations)
	assert.True(t, byName["b"].WholeTable, "the unbounded UPDATE is enough")
	assert.Equal(t, []string{"select"}, byName["a"].Operations)
	assert.True(t, byName["a"].WholeTable, "a select with no WHERE is unbounded too")
	assert.Equal(t, []string{"a", "b"}, []string{res.Impact.Tables[0].Table, res.Impact.Tables[1].Table})
}

// The expensive direction: a statement the engine did not read contributes NO
// tables and marks the whole surface incomplete. Reading its prefix would
// have listed a table for a statement whose real targets are unknown — an
// approver would take "touches nothing else" at face value.
func TestImpact_UnreadStatementMarksTheSurfaceIncomplete(t *testing.T) {
	for _, sql := range []string{
		// Partially read DDL (the library returns a prefix).
		"CREATE TABLE t (a INT) PARTITION BY RANGE (id) (PARTITION p0 VALUES LESS THAN (10))",
		"CREATE TABLE t AS SELECT * FROM t2",
		// Fully unread (another dialect).
		"SELECT id::text FROM users",
		"SELECT TOP (10) * FROM users",
	} {
		res := impactOf(t, sql)
		assert.True(t, res.Impact.Incomplete, "an unread statement must mark the surface: %q", sql)
		assert.Empty(t, res.Impact.Tables,
			"an unread statement's tables must not be claimed: %q", sql)

		imp := res.Statements[0].Impact
		assert.False(t, imp.Complete)
		assert.Equal(t, string(KindUnknown), imp.Operation, "no finer verb than unknown may be claimed")
		assert.Empty(t, imp.Writes)
		assert.Empty(t, imp.Reads)
	}
}

// The invariant that ties the impact to the findings: "this surface is
// incomplete" and "SR-01 fired" are the same fact, from the same parse. If
// they ever disagree, one of the two is lying about what was read.
func TestImpact_IncompleteAgreesWithSR01(t *testing.T) {
	for _, sql := range []string{
		"SELECT id FROM t",
		"DELETE FROM t WHERE id=1",
		"CREATE TABLE t (a INT) PARTITION BY RANGE (id) (PARTITION p0 VALUES LESS THAN (10))",
		"CREATE TABLE t AS SELECT * FROM t2",
		"SELECT id::text FROM users",
		"GRANT SELECT ON *.* TO 'u'@'%'",
		"SET SESSION sql_mode = 'STRICT_ALL_TABLES'",
	} {
		res := impactOf(t, sql)
		sr01 := ruleIDs(res)["SR-01"]
		assert.Equal(t, sr01, res.Impact.Incomplete,
			"SR-01 and an incomplete surface describe the same unread statement: %q", sql)
	}
}

// Statements that name no table carry no impact entry — but they are still
// reported per statement, because "the engine looked and found no table" is
// not the same claim as "the engine did not look".
func TestImpact_StatementsWithoutTables(t *testing.T) {
	res := impactOf(t, "SET SESSION sql_mode = 'STRICT_ALL_TABLES'")
	assert.Empty(t, res.Impact.Tables)
	assert.False(t, res.Impact.Incomplete)

	imp := res.Statements[0].Impact
	assert.Equal(t, "set", imp.Operation)
	assert.True(t, imp.Complete)
	assert.Empty(t, imp.Writes)
	assert.Empty(t, imp.Reads)
	assert.False(t, imp.WholeTable)
}

// Names only: a self-referencing statement lists its table once, under
// Writes. The engine does not pretend to tell the write target and the read
// source apart when they are the same name.
func TestImpact_SelfReferenceIsListedOnce(t *testing.T) {
	res := impactOf(t, "INSERT INTO t (a) SELECT a FROM t")
	imp := res.Statements[0].Impact
	assert.Equal(t, []string{"t"}, imp.Writes)
	assert.Empty(t, imp.Reads)
	require.Len(t, res.Impact.Tables, 1)
	assert.Equal(t, []string{"insert"}, res.Impact.Tables[0].Operations)
	assert.True(t, res.Impact.Tables[0].WholeTable, "INSERT … SELECT is unbounded")
}

func TestImpact_IsDeterministic(t *testing.T) {
	const batch = "UPDATE b SET x=1; DELETE FROM a WHERE id=1; SELECT * FROM c"
	first, err := json.Marshal(impactOf(t, batch).Impact)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		again, err := json.Marshal(impactOf(t, batch).Impact)
		require.NoError(t, err)
		assert.Equal(t, string(first), string(again))
	}
}

// The surface has to survive the JSON envelope: that is the channel the plan
// artifact, the ticket and any pipeline read.
func TestImpact_IsInTheJSONEnvelope(t *testing.T) {
	res := impactOf(t, "DELETE FROM orders")
	blob, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(blob), `"impact"`)
	assert.Contains(t, string(blob), `"table":"orders"`)
	assert.Contains(t, string(blob), `"whole_table":true`)
	assert.Contains(t, string(blob), `"operations":["delete"]`)
}

func TestMergeImpact_UnionAndStickyIncomplete(t *testing.T) {
	a := impactOf(t, "DELETE FROM orders").Impact
	b := impactOf(t, "UPDATE orders SET a=1 WHERE id=1; SELECT * FROM users").Impact
	c := impactOf(t, "SELECT TOP (10) * FROM other").Impact // read nothing

	merged := MergeImpact(&a, &b)
	require.Len(t, merged.Tables, 2)
	assert.False(t, merged.Incomplete)
	byName := map[string]TableImpact{}
	for _, te := range merged.Tables {
		byName[te.Table] = te
	}
	assert.Equal(t, []string{"delete", "update"}, byName["orders"].Operations)
	assert.True(t, byName["orders"].WholeTable)
	assert.Equal(t, []string{"select"}, byName["users"].Operations)

	// One unread statement makes the whole union a lower bound, and the unread
	// batch contributes no tables of its own.
	merged = MergeImpact(&a, &c)
	assert.True(t, merged.Incomplete)
	require.Len(t, merged.Tables, 1)
	assert.Equal(t, "orders", merged.Tables[0].Table)

	// nil inputs are "nothing to merge", not a reason to claim emptiness.
	merged = MergeImpact(nil, &a, nil)
	assert.False(t, merged.Incomplete)
	require.Len(t, merged.Tables, 1)
	assert.Empty(t, MergeImpact().Tables)
}
