package sqlguard

// tables_test.go pins the argument contract two modules share.
//
// It is a shared contract, so the tests are about what must be TRUE on both
// surfaces rather than about either one's SQL: a name is either acceptable to
// splice into a quoted identifier or it is refused, and a refusal always
// happens before the target is touched. The mysql and pg snapshot actions
// assert the same cases again at their own level (the message they wrap it
// in differs), but the rule itself is checked here once.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTableList_KeepsTheAuthoredOrder(t *testing.T) {
	got, err := TableList(map[string]any{"tables": []any{"zeta", "alpha", " beta "}}, "tables")
	require.NoError(t, err)
	// The order is the author's: a snapshot's evidence reads in the order the
	// change was written, not in whatever order a map happened to iterate.
	assert.Equal(t, []string{"zeta", "alpha", "beta"}, got)
}

func TestTableList_AcceptsAQualifier(t *testing.T) {
	got, err := TableList(map[string]any{"tables": []any{"shop.orders", "audit_log", "t$1"}}, "tables")
	require.NoError(t, err)
	assert.Equal(t, []string{"shop.orders", "audit_log", "t$1"}, got)
}

func TestTableList_Refuses(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing", map[string]any{}, `missing argument "tables"`},
		{"empty", map[string]any{"tables": []any{}}, "must not report success"},
		{"scalar instead of list", map[string]any{"tables": "users"}, "must be a list"},
		{"element not a string", map[string]any{"tables": []any{1}}, "must be a string"},
		{"duplicate", map[string]any{"tables": []any{"t", "t"}}, "listed twice"},
		{"statement injection", map[string]any{"tables": []any{"t; DROP TABLE x"}}, "invalid table name"},
		{"backtick", map[string]any{"tables": []any{"t`; DROP TABLE x; --"}}, "invalid table name"},
		{"quote", map[string]any{"tables": []any{`t"`}}, "invalid table name"},
		{"two qualifiers", map[string]any{"tables": []any{"a.b.c"}}, "invalid table name"},
		{"leading digit", map[string]any{"tables": []any{"1t"}}, "invalid table name"},
		{"empty name", map[string]any{"tables": []any{"  "}}, "invalid table name"},
		{"trailing dot", map[string]any{"tables": []any{"a."}}, "invalid table name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := TableList(c.args, "tables")
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// A single string is refused rather than read as a one-element list:
// `tables: users, orders` would otherwise mean one table named "users, orders",
// and that mistake would only surface as "relation does not exist" at
// execution time.
func TestTableList_RefusesAYamlScalar(t *testing.T) {
	_, err := TableList(map[string]any{"tables": "users, orders"}, "tables")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a list")
}

func TestSplitQualifier(t *testing.T) {
	schema, name := SplitQualifier("shop.orders")
	assert.Equal(t, "shop", schema)
	assert.Equal(t, "orders", name)

	schema, name = SplitQualifier("orders")
	assert.Empty(t, schema, "an unqualified name leaves the schema to the engine")
	assert.Equal(t, "orders", name)
}

func TestSortedUnique(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, SortedUnique([]string{"b", "a", "b", ""}))
	assert.Empty(t, SortedUnique(nil))
}
