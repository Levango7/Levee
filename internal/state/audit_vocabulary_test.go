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
// Both columns are now closed vocabularies and guarded set-equal to their
// owners: `action` to the AuditAction* constants, `result` to the outcome
// constants PLUS the runstatus vocabulary (see the constants block in store.go
// for why the run status legitimately lands there). The prose outcomes the
// column used to carry were removed, not accommodated — so the guard also
// refuses a sentence-shaped AuditResult* constant outright.
package state

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/runstatus"
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
// (a) list exactly the outcome constants, and (b) list exactly the run statuses
// a transition can record — parsed from internal/runstatus, not restated. It
// also refuses a prose-shaped AuditResult* constant: such a value could not be
// listed in the `|` vocabulary at all, which is how the column stopped being
// queryable before.
func TestAuditResultCommentNamesBothFamilies(t *testing.T) {
	outcomes := declaredConstants(t, "AuditResult")
	live := map[string]bool{}
	for name, value := range outcomes {
		assert.Positive(t, len(value), "%s has an empty value", name)
		// A token, not a sentence: the column is what audit queries filter on,
		// and there is no detail column to hold prose (the chain hashes a fixed
		// field set). Values like `tier` and "recorded; quorum pending" were
		// removed rather than given one; the tier is recoverable from the
		// approvals row the audit row's Target names.
		assert.Regexp(t, `^[a-z][a-z_]*$`, value,
			"%s = %q is not a token; audit.result is a vocabulary column", name, value)
		live[value] = true
	}
	require.NotEmpty(t, live, "no AuditResult* constants parsed")

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

		// The comment must state the contract, not only the vocabulary: a
		// reader who sees the two lists needs to know the column is tokens
		// only. (This is also what stops the next writer from folding a
		// message in and calling the comment wrong.)
		assert.Contains(t, comment, "tokens only",
			"%s: audit.result comment must state that the column holds tokens only", rel)
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

// TestAuditResultVocabularyIsTheTwoDocumentedFamilies pins the predicate the
// audit writer calls on every row: it must accept exactly the AuditResult*
// constants plus the runstatus vocabulary. No third family, and no prose — the
// prose the column used to carry is what the predicate exists to make loud.
func TestAuditResultVocabularyIsTheTwoDocumentedFamilies(t *testing.T) {
	declared := map[string]bool{}
	for _, value := range declaredConstants(t, "AuditResult") {
		declared[value] = true
	}
	require.NotEmpty(t, declared, "no AuditResult* constants parsed from store.go")

	// Two-way against production: AuditResultKnown reads auditOutcomeTokens, so
	// a constant missing from that list would be a value writers use and the
	// write-boundary check rejects.
	listed := map[string]bool{}
	for _, tok := range auditOutcomeTokens {
		assert.False(t, listed[tok], "auditOutcomeTokens lists %q twice", tok)
		listed[tok] = true
		assert.True(t, declared[tok],
			"auditOutcomeTokens lists %q, which no AuditResult* constant declares", tok)
	}
	for value := range declared {
		assert.True(t, listed[value],
			"auditOutcomeTokens omits %q, which internal/state declares and writers use", value)
		assert.True(t, AuditResultKnown(value),
			"AuditResultKnown must accept the declared outcome token %q", value)
	}

	// The runstatus half comes from its own owner (runstatus.All), so it cannot
	// drift here the way a copied list would.
	require.NotEmpty(t, runstatus.All)
	for _, s := range runstatus.All {
		assert.True(t, AuditResultKnown(s),
			"AuditResultKnown must accept run status %q: a transition records it in this column", s)
	}

	// The prose this column used to carry, plus the concatenated shape the
	// gate verifier wrote: the whole point is that these are warned about now.
	for _, prose := range []string{"L2", "recorded; quorum pending", "passed: disk ok"} {
		assert.False(t, AuditResultKnown(prose),
			"AuditResultKnown must reject %q: audit.result is tokens only", prose)
	}
}
