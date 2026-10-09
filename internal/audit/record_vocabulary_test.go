package audit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogger points the process-wide logger at a buffer for the duration of
// one test. It is not safe for parallel tests, which is why these two are not
// marked t.Parallel().
func captureLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.InitLoggerWithWriter("warn", "text", &buf)
	t.Cleanup(func() { log.InitLoggerWithWriter("info", "text", os.Stderr) })
	return &buf
}

// TestRecordWarnsOnOutOfVocabularyResult: audit.result is tokens only, and
// Record is the boundary where a writer that folds prose in becomes loud. It
// must warn AND still write the row — a mislabelled row is evidence, a lost
// row is not.
func TestRecordWarnsOnOutOfVocabularyResult(t *testing.T) {
	buf := captureLogger(t)

	_, store := newAuditChainBuilder(t)
	ctx := context.Background()
	entry := &state.Audit{
		ID: "vocab-prose", Action: "gate_verify", Actor: "operator", Target: "web-01",
		Result: "passed: disk ok", Timestamp: time.Now().UTC(),
	}
	require.NoError(t, Record(ctx, store, entry))

	out := buf.String()
	assert.Contains(t, out, "audit.result vocabulary",
		"a prose result must be warned about at the write boundary")
	assert.Contains(t, out, "passed: disk ok", "the warning must name the offending value")

	written, err := store.GetAudit(ctx, "vocab-prose")
	require.NoError(t, err)
	require.NotNil(t, written, "the row must still be written: a lost row is worse than a mislabelled one")
	assert.Equal(t, "passed: disk ok", written.Result)
}

// TestRecordQuietOnTheTwoDocumentedFamilies: the check must not cry wolf on the
// values the column legitimately carries, or it becomes noise nobody reads.
func TestRecordQuietOnTheTwoDocumentedFamilies(t *testing.T) {
	buf := captureLogger(t)

	_, store := newAuditChainBuilder(t)
	ctx := context.Background()
	results := []string{
		state.AuditResultSuccess,
		state.AuditResultQuorumPending,
		runstatus.StatusRolledBack,
		runstatus.StatusArchived,
	}
	for i, r := range results {
		entry := &state.Audit{
			ID: fmt.Sprintf("vocab-ok-%d", i), Action: "approve", Actor: "operator", Target: "run-1",
			Result: r, Timestamp: time.Now().UTC(),
		}
		require.NoError(t, Record(ctx, store, entry))
	}
	assert.NotContains(t, buf.String(), "vocabulary",
		"legitimate results must not produce the warning; got: %s", buf.String())
}
