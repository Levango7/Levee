// status_column_comment_guard_test.go — the other two status columns whose
// inline schema comments described a vocabulary no writer produces.
//
// The batches case (batch_status_schema_comment_test.go) recorded what a false
// comment costs: a reader coded against `skipped`, a state no batch row can
// carry, while the two states that DO occur were missing from the list. The same
// shape was found on two more columns and fixed in the same change:
//
//   - `steps.status` was commented with the steps vocabulary, which is correct,
//     but had no owning constants at all — the engine wrote bare literals.
//     state.StepStatus* now owns it, and the comment must list exactly them.
//   - `runs.approval_status` was commented `pending|approved|rejected|timeout|
//     skipped`. The last two belong to the APPROVALS table's status column one
//     schema line over; no writer sets them on a run. A reader would build a
//     "timed out approvals" query against a run column that never holds it.
//
// This guard parses the constants out of store.go (not a restatement, so a newly
// added constant cannot pass unnoticed) and requires each comment to list exactly
// that set.
package state

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredConstants parses one constant block out of store.go by variable-name
// prefix. Empty result is a hard failure: a renamed block must not silently
// downgrade the guard into a no-op.
func declaredConstants(t *testing.T, prefix string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(schemaRepoFile(t, "internal/state/store.go"))
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^[ \t]*(\w*` + prefix + `\w*)[ \t]*=[ \t]*"([^"]+)"\r?$`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out, "no %s* constants parsed from store.go — the const block shape changed", prefix)
	return out
}

// columnCommentList returns the inline comment list on `column` inside the
// named CREATE TABLE block. Scoping to the block matters: the same column name
// appears in several tables (status appears in runs, batches, steps, targets).
func columnCommentList(t *testing.T, rel, table, column string) []string {
	t.Helper()
	raw, err := os.ReadFile(schemaRepoFile(t, rel))
	require.NoError(t, err)

	blockRe := regexp.MustCompile(`(?ms)CREATE TABLE IF NOT EXISTS ` + table + ` \(.*?\n\);`)
	block := blockRe.FindString(string(raw))
	require.NotEmpty(t, block, "could not find the CREATE TABLE block for %s in %s", table, rel)

	lineRe := regexp.MustCompile(`(?m)^[ \t]*` + column + `[ \t]+\w+[ \t]+NOT NULL[^,]*,?[ \t]*--[ \t]*(.+?)\r?$`)
	line := lineRe.FindStringSubmatch(block)
	require.Len(t, line, 2, "%s.%s has no `TEXT NOT NULL, -- list` line in %s", table, column, rel)

	// Stop at the first prose sentence: these comments continue past the list.
	list := strings.Split(line[1], "|")
	var out []string
	for i, s := range list {
		trimmed := strings.TrimSpace(s)
		if i == len(list)-1 {
			// Terminal element carries any trailing prose; keep only the token.
			trimmed = strings.Fields(trimmed)[0]
		}
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	require.NotEmpty(t, out, "parsed an empty comment list out of %s.%s", table, column)
	return out
}

func TestStepsStatusCommentMatchesConstants(t *testing.T) {
	constants := declaredConstants(t, "StepStatus")
	declared := map[string]bool{}
	for _, v := range constants {
		declared[v] = true
	}
	require.Len(t, constants, 5, "expected the five declared step states, got %d", len(constants))

	for _, rel := range []string{"internal/state/schema.sql", "internal/state/pgschema.sql"} {
		listed := columnCommentList(t, rel, "steps", "status")
		seen := map[string]bool{}
		for _, name := range listed {
			assert.False(t, seen[name], "%s: %q listed twice in steps.status", rel, name)
			seen[name] = true
			assert.True(t, declared[name],
				"%s: steps.status comment lists %q, which no StepStatus constant declares — a reader would code against a state the engine cannot produce", rel, name)
		}
		for _, value := range constants {
			assert.True(t, seen[value], "%s: steps.status comment omits %q, which internal/state does declare", rel, value)
		}
	}
}

func TestRunsApprovalStatusCommentMatchesConstants(t *testing.T) {
	constants := declaredConstants(t, "ApprovalStatus")
	declared := map[string]bool{}
	for _, v := range constants {
		declared[v] = true
	}
	require.Len(t, constants, 3, "expected the three declared run approval states, got %d", len(constants))

	for _, rel := range []string{"internal/state/schema.sql", "internal/state/pgschema.sql"} {
		listed := columnCommentList(t, rel, "runs", "approval_status")
		seen := map[string]bool{}
		for _, name := range listed {
			assert.False(t, seen[name], "%s: %q listed twice in runs.approval_status", rel, name)
			seen[name] = true
			assert.True(t, declared[name],
				"%s: runs.approval_status comment lists %q, which no ApprovalStatus constant declares — timeout/skipped belong to the approvals table's status column", rel, name)
		}
		for _, value := range constants {
			assert.True(t, seen[value], "%s: runs.approval_status comment omits %q, which internal/state does declare", rel, value)
		}
	}
}
