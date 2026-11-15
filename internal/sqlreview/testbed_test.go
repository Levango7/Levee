package sqlreview

// testbed_test.go is the negative half of the rule table's executable
// definition. review_test.go proves every rule CAN fire; this file proves each
// one does NOT fire on the near-miss that looks like its target.
//
// Why both halves matter: a rule that fires on everything passes
// TestEveryRuleFires. That is not a hypothetical — SR-08 (ALTER TABLE) and
// SR-17 (any administrative statement) match whole statement kinds, so their
// "positive" case and a false positive are the same input until somebody
// writes down what should stay quiet. A test bed without the clean side cannot
// measure precision at all, and precision is what a DBA accepts or discards:
// twenty rules that cry wolf on every ordinary change get switched off, and
// then none of them protect anything.
//
// The clean case for a rule is deliberately a NEAR MISS, not an unrelated
// statement: `DELETE FROM t WHERE id = 1` for SR-02, a primary key present for
// SR-10, an explicit column list for SR-13. An unrelated statement would pass
// even if the rule had been widened to its whole kind.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanCases maps every rule ID to a statement that must NOT produce that
// rule's finding.
//
// The table is exhaustive by construction: TestCleanCaseCoverage requires an
// entry for every id in stmtRules and batchRules, so a new rule cannot ship
// without somebody deciding what its quiet neighbour looks like.
var cleanCases = map[string]string{
	// SR-01 is about parsing itself: a well-formed statement is the clean case.
	"SR-01": "SELECT id FROM t",
	"SR-02": "DELETE FROM t WHERE id = 1 LIMIT 10",
	"SR-03": "UPDATE t SET a = 1 WHERE id = 1 LIMIT 10",
	"SR-04": "DELETE FROM t WHERE id = 1 LIMIT 10",               // predicate really selects rows
	"SR-05": "DELETE FROM t WHERE id = 1 LIMIT 10",               // the bounded delete is the alternative
	"SR-06": "DROP TABLE t",                                      // object, not database
	"SR-07": "ALTER TABLE t ADD COLUMN c INT",                    // neither DROP nor RENAME
	"SR-08": "CREATE TABLE t (id INT PRIMARY KEY) ENGINE=InnoDB", // no ALTER body to read
	"SR-09": "ALTER TABLE t ADD COLUMN c INT",                    // not a rename
	"SR-10": "CREATE TABLE t (id INT PRIMARY KEY) ENGINE=InnoDB",
	"SR-11": "CREATE TABLE t (id INT PRIMARY KEY) ENGINE=InnoDB",
	"SR-12": "SELECT id, name FROM t",
	"SR-13": "INSERT INTO t (id, name) VALUES (1, 'a')",
	"SR-14": "DELETE FROM t WHERE id > 10 LIMIT 100",
	"SR-15": "UPDATE t SET a = 1 WHERE id > 10 LIMIT 100",
	"SR-16": "SET SESSION sql_mode = 'STRICT_ALL_TABLES'",
	// SR-17 matches whole kinds, so the only clean case is a kind it does not
	// own; the statement below exercises an ordinary DDL path instead.
	"SR-17": "CREATE TABLE t (id INT PRIMARY KEY) ENGINE=InnoDB",
	"SR-18": "SELECT id FROM t WHERE id = 1",
}

// cleanBatches covers the two batch-level rules, which take the whole batch
// rather than one statement.
var cleanBatches = map[string]string{
	// Pure DML: SR-19 needs a DDL+DML mix to fire.
	"SR-19": "UPDATE t SET a = 1 WHERE id = 1 LIMIT 10",
	// Single statement: SR-20 needs more than one to fire.
	"SR-20": "UPDATE t SET a = 1 WHERE id = 1 LIMIT 10",
}

// strictlyClean lists batches that must produce NO findings at all. This is the
// sharpest form of the false-positive measurement: a near miss can still trip
// a different rule, but these are ordinary changes a real operator writes
// daily, and every finding here is noise.
var strictlyClean = []string{
	"SELECT id, name FROM t WHERE id = 1",
	"INSERT INTO t (id, name) VALUES (1, 'a')",
	"UPDATE t SET name = 'b' WHERE id = 1 LIMIT 10",
	"DELETE FROM t WHERE id = 1 LIMIT 10",
	"CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(20)) ENGINE=InnoDB",
}

func allRuleIDs(t *testing.T) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, r := range stmtRules {
		ids[string(r.id)] = true
	}
	// The two batch-level rules are not table rows (batchRules switches on the
	// DDL/DML mix and the statement count); their ids are pinned here, and
	// TestBatchRuleIDsAreComplete below checks the switch still produces
	// exactly this set.
	for _, id := range batchRuleIDs {
		ids[id] = true
	}
	require.NotEmpty(t, ids, "no rule IDs found; the tables this file guards are empty")
	return ids
}

// batchRuleIDs names the rules the whole-batch pass emits.
var batchRuleIDs = []string{"SR-19", "SR-20"}

func TestBatchRuleIDsAreComplete(t *testing.T) {
	// One DDL and one DML => both batch rules must fire, and nothing else may
	// appear from this pass (the per-statement findings come from elsewhere).
	st := []StatementReview{
		{Kind: KindDDL}, {Kind: KindUpdate},
	}
	got := map[string]bool{}
	for _, f := range batchRules(st, true, true) {
		got[string(f.Rule)] = true
	}
	assert.True(t, got["SR-19"], "mixed DDL+DML batch must carry SR-19")
	assert.True(t, got["SR-20"], "two-statement batch must carry SR-20")
	assert.Len(t, got, len(batchRuleIDs), "the batch pass emitted unexpected rule ids: %v", got)
}

func TestCleanCaseCoverage(t *testing.T) {
	ids := allRuleIDs(t)
	missing := []string{}
	for id := range ids {
		if _, ok := cleanCases[id]; !ok {
			if _, okBatch := cleanBatches[id]; !okBatch {
				missing = append(missing, id)
			}
		}
	}
	assert.Empty(t, missing,
		"rules without a clean case: every rule needs a near miss that stays silent, "+
			"or a false positive goes unmeasured")
}

func TestCleanCasesStaySilentAboutTheirOwnRule(t *testing.T) {
	for id, sql := range cleanCases {
		res, err := Review(sql)
		require.NoError(t, err, "clean case for %s must parse: %q", id, sql)
		assert.NotContains(t, ruleIDs(res), id,
			"%s fired on its own clean case %q — the rule is wider than its intent",
			id, sql)
	}
	for id, batch := range cleanBatches {
		res, err := Review(batch)
		require.NoError(t, err, "clean batch for %s must parse: %q", id, batch)
		assert.NotContains(t, ruleIDs(res), id,
			"%s fired on its own clean batch %q", id, batch)
	}
}

// A clean case that trips a DIFFERENT rule is fine and expected (the ALTER
// cases below necessarily sit inside SR-08's kind). What matters is that the
// test above names the rule, so widening one rule cannot hide behind another's
// warning.
func TestStrictlyCleanBatchesProduceNoFindings(t *testing.T) {
	for _, batch := range strictlyClean {
		res, err := Review(batch)
		require.NoError(t, err, "strictly-clean batch must parse: %q", batch)
		assert.Empty(t, res.Findings,
			"ordinary change %q produced %v; each one is noise a DBA learns to ignore",
			batch, res.Findings)
	}
}

// Both sides of the pair must exist for the measurement to mean anything:
// a rule with a fire case but no clean case can still be crying wolf, and a
// clean case with no fire case would be measuring nothing.
func TestFireAndCleanTablesCoverTheSameRules(t *testing.T) {
	fire := map[string]bool{}
	for id := range ruleCases {
		fire[id] = true
	}
	clean := map[string]bool{}
	for id := range cleanCases {
		clean[id] = true
	}
	for id := range cleanBatches {
		clean[id] = true
	}
	for id := range fire {
		assert.True(t, clean[id], "rule %s has a fire case but no clean case", id)
	}
	ids := allRuleIDs(t)
	for id := range ids {
		assert.True(t, fire[id] || clean[id], "rule %s appears in neither table", id)
	}
}
