package sqlguard

// sqlguard_test.go pins the R2 boundary both database modules share. The cases
// are chosen so that each one would fail if the classifier were replaced by a
// naive substring test OR by an AST-only test: the regex catches text the
// grammar cannot read, and the AST catches what an end-anchored regex
// structurally misses.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRejectDestructive_RefusesTheDestructiveVerbs(t *testing.T) {
	for _, sql := range []string{
		"DROP TABLE t",
		"DROP DATABASE db1",
		"TRUNCATE TABLE t",
		"TRUNCATE t",
		"DELETE FROM t", // no WHERE: deletes every row
	} {
		err := RejectDestructive(sql, "pg.query")
		require.Error(t, err, "must refuse %q", sql)
		assert.Contains(t, err.Error(), "pg.query", "the refusal must name the surface")
	}
}

func TestRejectDestructive_AllowsOrdinaryChange(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE t ADD COLUMN c INT",
		"UPDATE t SET a = 1 WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
		"INSERT INTO t (id) VALUES (1)",
		"CREATE TABLE t (id INT PRIMARY KEY)",
	} {
		assert.NoError(t, RejectDestructive(sql, "pg.query"), "must allow %q", sql)
	}
}

// The grammar does not know DROP USER (it is not a MySQL statement it
// models), so an AST-only classifier would wave it through. The raw-text
// backstop is what refuses it — this is the case that justifies keeping both.
func TestRejectDestructive_RegexBackstopCoversUngrammaticalText(t *testing.T) {
	err := RejectDestructive("DROP USER 'app'@'%'", "mysql.query")
	require.Error(t, err, "an unparseable DROP USER must still be refused")
	assert.Contains(t, err.Error(), "mysql.query")
}

// Multi-statement payloads are classified per statement: one destructive arm
// is enough. A single trailing safe statement must not launder the first.
func TestRejectDestructive_ClassifiesPerStatement(t *testing.T) {
	err := RejectDestructive("UPDATE t SET a=1 WHERE id=1;\nDROP TABLE t", "pg.query")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DROP TABLE t")
}

// The action name is caller-supplied and must reach the message verbatim: an
// operator reading a refusal needs to know which step surface produced it.
func TestRejectDestructive_NamesTheAction(t *testing.T) {
	err := RejectDestructive("DROP TABLE t", "some.future.query")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "some.future.query"),
		"want the action name in %q", err.Error())
}

func TestRejectDestructive_EmptyInputIsNotDestructive(t *testing.T) {
	// Emptiness is the caller's problem (both modules refuse empty SQL before
	// reaching here); the classifier must not invent a verdict for it.
	assert.NoError(t, RejectDestructive("   ", "pg.query"))
}
