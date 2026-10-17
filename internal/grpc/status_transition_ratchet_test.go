package grpc

// status_transition_ratchet_test.go pins isValidTransition in two ways that
// matter separately.
//
// The first is a shape guard. internal/runstatus has long had a repo-wide
// ratchet against re-spelled run statuses, but it deliberately greps only the
// two underscore verdicts (rolled_back_partial / rollback_incomplete) because
// short names appear in unrelated contexts. That blind spot is exactly the
// transition table: it carried nine bare literals ("paused", "running",
// "pending", "approved", "draft", "planned", "cancelled", "completed",
// "archived") while the archived arm below them already used constants — a
// half-converted function, which is how a vocabulary ends up with a second home
// (the same drift #153 and the runs.status comment paid for). The guard is
// scoped to this one function rather than widened repo-wide on purpose: it
// buys zero noise, and its blind spot is the file it reads.
//
// The second is a behaviour table. Every (from, to) pair the function answers is
// listed here with the verdict written out by hand from the state machine, not
// computed from the code — otherwise a change to the code would simply move the
// bug into the test. It exists because the function had only four pairs pinned
// (the interrupted arms), so a literal edited into a different spelling would
// have shipped quietly.

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/runstatus"
)

// transitionFuncSource returns the body of isValidTransition as written in the
// product file. go test runs with the package directory as the working
// directory, so the source is read directly rather than through a repo-root
// guess (which is how a relocated script starts lying).
func transitionFuncSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("change_service.go")
	require.NoError(t, err, "reading change_service.go from the package directory")
	const marker = "func isValidTransition(from, to string) bool {"
	i := strings.Index(string(b), marker)
	require.Greater(t, i, -1, "isValidTransition not found in change_service.go — the guard anchor moved")
	rest := string(b)[i:]
	end := strings.Index(rest, "\n}\n")
	require.Greater(t, end, -1, "isValidTransition's closing brace not found; the function shape changed")
	body := rest[:end]
	// Non-vacuity: a truncated or empty extraction would satisfy "no literals".
	require.Greater(t, len(body), 500, "extracted function body is suspiciously short (%d bytes)", len(body))
	return body
}

func TestIsValidTransition_UsesRunstatusConstantsOnly(t *testing.T) {
	body := transitionFuncSource(t)

	var offenders []string
	for _, s := range runstatus.All {
		if strings.Contains(body, `"`+s+`"`) {
			offenders = append(offenders, s)
		}
	}
	assert.Empty(t, offenders,
		"isValidTransition re-spells run statuses as literals (%v); reference runstatus constants so the vocabulary has one home", offenders)

	// The counterpart assertion: the function must actually be doing these
	// comparisons through the constants. Without it, deleting the arms would
	// silence the check above and turn the guard green on an empty shell.
	assert.GreaterOrEqual(t, strings.Count(body, "runstatus.Status"), 12,
		"isValidTransition should compare against at least 12 runstatus constants; found fewer, so the transition table was rewritten rather than re-spelled")
}

// TestIsValidTransition_TruthTable pins the whole matrix. `to` covers the four
// arms the function knows about plus two it must refuse (an unhandled target and
// a status that is not a target at all).
func TestIsValidTransition_TruthTable(t *testing.T) {
	// allowed lists, per target, the `from` values that may reach it. Anything
	// not named is refused — including the empty string, which is what a
	// status-less row would carry.
	allowed := map[string][]string{
		runstatus.StatusPaused: {runstatus.StatusRunning, runstatus.StatusPending},
		runstatus.StatusRunning: {
			runstatus.StatusPaused, runstatus.StatusPending, runstatus.StatusApproved,
			runstatus.StatusDraft, runstatus.StatusPlanned,
		},
		// cancelled refuses only the terminals whose history is already settled:
		// its own, completed, archived, and the takeover verdict.
		runstatus.StatusCancelled: {
			runstatus.StatusDraft, runstatus.StatusPending, runstatus.StatusPlanned,
			runstatus.StatusApproved, runstatus.StatusRunning, runstatus.StatusPaused,
			runstatus.StatusFailed, runstatus.StatusRolledBack,
			runstatus.StatusRolledBackPartial, runstatus.StatusRollbackIncomplete,
			runstatus.StatusRejected,
		},
		runstatus.StatusArchived: {
			runstatus.StatusDraft, runstatus.StatusPlanned, runstatus.StatusCompleted,
			runstatus.StatusFailed, runstatus.StatusCancelled, runstatus.StatusRolledBack,
			runstatus.StatusRolledBackPartial, runstatus.StatusRollbackIncomplete,
			runstatus.StatusInterrupted,
		},
		runstatus.StatusCompleted:     nil,
		runstatus.StatusInterrupted:   nil,
		"definitely_not_a_run_status": nil,
		"":                            nil,
	}

	for _, to := range []string{
		runstatus.StatusPaused, runstatus.StatusRunning, runstatus.StatusCancelled,
		runstatus.StatusArchived, runstatus.StatusCompleted, runstatus.StatusInterrupted,
		"definitely_not_a_run_status", "",
	} {
		yes := map[string]bool{}
		for _, f := range allowed[to] {
			yes[f] = true
		}
		for _, from := range append(append([]string{}, runstatus.All...), "", "bogus") {
			want := yes[from]
			// `cancelled` is the one arm written as a refusal list, so the
			// table above spells it positively; cross-check the two readings
			// here rather than trusting either alone.
			if to == runstatus.StatusCancelled {
				want = from != runstatus.StatusCompleted && from != runstatus.StatusCancelled &&
					from != runstatus.StatusArchived && from != runstatus.StatusInterrupted
			}
			assert.Equal(t, want, isValidTransition(from, to),
				"transition %q -> %q: the table and the code disagree", from, to)
		}
	}
}
