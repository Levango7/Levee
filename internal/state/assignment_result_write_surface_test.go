// assignment_result_write_surface_test.go — the write surface for
// assignments.result is frozen at completed|failed|rolled_back. The mapping in
// cmd/levee/grpc_change_executor.go used to leak arbitrary run statuses
// (interrupted, running, rolled_back_partial, ...) into this column through its
// default branch; the executor now maps them, and this guard makes the store the
// second line so a future caller cannot reopen the hole.
package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetAssignmentResultRefusesValuesOutsideTheVocabulary(t *testing.T) {
	ctx := context.Background()
	st, err := NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// The empty string is the column's SQL default ("no result recorded yet"),
	// not a fourth verdict, so a terminal write must not carry it either.
	for _, bad := range []string{"", "running", "interrupted", "rolled_back_partial", "done", "Completed"} {
		err := st.SetAssignmentResult(ctx, "run-1", 1, bad)
		require.Errorf(t, err, "result %q must be refused by the write surface", bad)
		assert.Containsf(t, err.Error(), "is not an assignment result", "result %q", bad)
	}
}

func TestSetAssignmentResultAcceptsEveryVerdict(t *testing.T) {
	ctx := context.Background()
	st, err := NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// SQLite deployments have no run_assignment table, so a valid write fails on
	// the UPDATE rather than on the guard. What matters here is that the guard
	// let the value through: any error must NOT be the vocabulary refusal.
	for _, good := range AssignResultNames() {
		err := st.SetAssignmentResult(ctx, "run-1", 1, good)
		if err != nil {
			assert.NotContainsf(t, err.Error(), "is not an assignment result", "%q is a valid verdict", good)
		}
	}
}
