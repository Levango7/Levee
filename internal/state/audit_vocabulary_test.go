// audit_vocabulary_test.go — the audit table's two vocabulary columns, whose
// inline schema comments described values no writer produces.
//
// The same shape as the batch/steps/approval guards next door, on the columns
// that had gone the longest without one:
//
//   - `audit.action` was commented `plan|apply|verify|rollback|approval|lock|
//     credential|archive|login|config`. Ten of those ten had no writer at all,
//     while the sixteen values that WERE written were missing from it — so the
//     comment was a list of intentions, and a reader querying "all rollbacks"
//     against it would have used a spelling half the rows do not carry.
//   - `audit.result` was commented `success|failure|denied|error`: none of
//     `failure`/`error` is written anywhere, and the values that are written
//     (the run status a transition moved to, plus two prose outcomes) were not
//     named.
//
// `action` is a closed vocabulary and is guarded set-equal to the constants.
// `result` is deliberately NOT closed: it carries outcome words AND the run
// status the action moved the run to (see the constants block in store.go for
// why), so its guard requires the documented families to be named and the
// closed half to match, rather than pretending the column is an enum.
package state

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditColumnComment returns the single-line inline comment on `column` inside
// the audit CREATE TABLE block.
func auditColumnComment(t *testing.T, rel, column string) string {
	t.Helper()
	raw, err := os.ReadFile(schemaRepoFile(t, rel))
	require.NoError(t, err)

	blockRe := regexp.MustCompile(`(?ms)CREATE TABLE IF NOT EXISTS audit \(.*?\n\);`)
	block := blockRe.FindString(string(raw))
	require.NotEmpty(t, block, "could not find the audit CREATE TABLE block in %s", rel)

	lineRe := regexp.MustCompile(`(?m)^[ \t]*` + column + `[ \t]+\w+[ \t]+NOT NULL[^,]*,?[ \t]*--[ \t]*(.+?)\r?$`)
	line := lineRe.FindStringSubmatch(block)
	require.Len(t, line, 2, "audit.%s has no `TEXT NOT NULL, -- ...` line in %s", column, rel)
	return line[1]
}

// TestAuditActionCommentMatchesConstants requires the audit.action comment to
// list exactly the AuditAction* constants — two-way, so neither a new constant
// nor a stale comment entry can pass.
func TestAuditActionCommentMatchesConstants(t *testing.T) {
	// The Legacy constant is excluded from the list and asserted separately: it
	// is not written any more, but a reader querying historical rows still needs
	// to know the spelling exists.
	constants := declaredConstants(t, "AuditAction")
	require.NotEmpty(t, constants, "no AuditAction* constants parsed from store.go")

	live := map[string]bool{}
	for name, value := range constants {
		if strings.HasSuffix(name, "Legacy") {
			continue
		}
		live[value] = true
	}
	require.GreaterOrEqual(t, len(live), 10, "parsed suspiciously few live action constants: %d", len(live))

	for _, rel := range []string{"internal/state/schema.sql", "internal/state/pgschema.sql"} {
		comment := auditColumnComment(t, rel, "action")
		// The list is the head of the comment; prose (if any) follows a `;`.
		list := strings.SplitN(comment, ";", 2)[0]

		listed := map[string]bool{}
		for _, tok := range strings.Split(list, "|") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			assert.False(t, listed[tok], "%s: audit.action lists %q twice", rel, tok)
			listed[tok] = true
			assert.True(t, live[tok],
				"%s: audit.action comment lists %q, which no AuditAction* constant declares — no writer can produce it", rel, tok)
		}
		for value := range live {
			assert.True(t, listed[value],
				"%s: audit.action comment omits %q, which internal/state declares and writers use", rel, value)
		}

		// The superseded spelling must stay documented: rows written before the
		// convergence carry it, and the chain is append-only, so they are never
		// rewritten.
		assert.Contains(t, comment, constants["AuditActionRetryHostLegacy"],
			"%s: audit.action must keep documenting the historical retry-host spelling", rel)
	}
}

// TestAuditResultCommentNamesBothFamilies requires the audit.result comment to
// (a) list exactly the outcome constants, (b) list exactly the run statuses a
// transition can record — parsed from internal/runstatus, not restated — and
// (c) keep naming the two prose outcomes that are known defects rather than
// vocabulary.
func TestAuditResultCommentNamesBothFamilies(t *testing.T) {
	outcomes := declaredConstants(t, "AuditResult")
	live := map[string]bool{}
	var proseValues []string
	for name, value := range outcomes {
		if strings.ContainsAny(value, " ;") {
			// Prose outcomes cannot sit in a `|` list; they are asserted as
			// mentioned text instead (see below).
			proseValues = append(proseValues, value)
			continue
		}
		assert.Positive(t, len(value), "%s has an empty value", name)
		live[value] = true
	}
	require.NotEmpty(t, live, "no token-shaped AuditResult* constants parsed")

	statuses := runStatusVocabulary(t)
	require.GreaterOrEqual(t, len(statuses), 10, "parsed suspiciously few runstatus constants: %d", len(statuses))

	for _, rel := range []string{"internal/state/schema.sql", "internal/state/pgschema.sql"} {
		comment := auditColumnComment(t, rel, "result")

		// Family 1: the outcome tokens, before the first `,`/`;`.
		tokenList := strings.FieldsFunc(comment, func(r rune) bool { return r == ',' || r == ';' })[0]
		listed := map[string]bool{}
		for _, tok := range strings.Split(tokenList, "|") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			listed[tok] = true
			assert.True(t, live[tok],
				"%s: audit.result lists outcome %q, which no AuditResult* constant declares", rel, tok)
		}
		for value := range live {
			assert.True(t, listed[value],
				"%s: audit.result omits outcome %q, which internal/state declares and writers use", rel, value)
		}

		// Family 2: the run statuses, in the parenthesised list. Asserted
		// set-equal to internal/runstatus so a status added there (or removed)
		// makes this comment wrong in a way that fails here rather than in a
		// reader's query.
		parenRe := regexp.MustCompile(`\(([^)]*)\)`)
		paren := parenRe.FindStringSubmatch(comment)
		require.Len(t, paren, 2, "%s: audit.result no longer names the run-status family in parentheses", rel)
		statusSet := map[string]bool{}
		for _, tok := range strings.Split(paren[1], "|") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			statusSet[tok] = true
			assert.True(t, statuses[tok],
				"%s: audit.result lists run status %q, which internal/runstatus does not declare", rel, tok)
		}
		for value := range statuses {
			assert.True(t, statusSet[value],
				"%s: audit.result omits run status %q, which a transition can record", rel, value)
		}

		// The prose outcomes stay visible as defects.
		for _, value := range proseValues {
			assert.Contains(t, comment, value,
				"%s: audit.result must keep naming the prose outcome %q", rel, value)
		}
	}
}

// runStatusVocabulary parses the runstatus.Status* constants out of their owning
// package. Parsed rather than restated: a status added there must reach this
// comment too, and a hard-coded copy here would hide exactly that drift.
func runStatusVocabulary(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(schemaRepoFile(t, "internal/runstatus/runstatus.go"))
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^[ \t]*Status\w*[ \t]*=[ \t]*"([^"]+)"\r?$`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = true
	}
	return out
}
