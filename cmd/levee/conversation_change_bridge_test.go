package main

// conversation_change_bridge_test.go covers the CLI bridge's only interesting
// property: it must not open a database until a recommendation is actually
// confirmed, and it must release that database when the command ends.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

func TestLazyLocalChangeCreatorOpensStoreOnFirstUse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "levee-converse-bridge.db")
	opens := 0
	creator := newLazyLocalChangeCreator(func(context.Context) (state.Store, error) {
		opens++
		st, oerr := state.NewSQLiteStore(ctx, path)
		if oerr != nil {
			return nil, oerr
		}
		return st, nil
	})

	// --list / --history / a plain question must never touch a database.
	assert.Zero(t, opens, "constructing the bridge must not open a database")

	id, status, err := creator.CreateChangeDraft(ctx, conversation.ChangeDraft{
		Label:        "converse 建议",
		WorkflowFile: "name: x",
		Params:       map[string]string{"recommendation_id": "rec-1"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)
	assert.Equal(t, runstatus.StatusDraft, status)
	assert.Equal(t, 1, opens)

	// A second confirmation reuses the same store rather than opening another.
	id2, _, err := creator.CreateChangeDraft(ctx, conversation.ChangeDraft{Label: "second"})
	require.NoError(t, err)
	assert.NotEqual(t, id, id2)
	assert.Equal(t, 1, opens, "the store must be opened once, not per confirmation")

	require.NoError(t, creator.Close())
	require.NoError(t, creator.Close(), "Close is safe to call twice")
}

func TestLazyLocalChangeCreatorCloseWithoutUse(t *testing.T) {
	creator := newLazyLocalChangeCreator(func(context.Context) (state.Store, error) {
		t.Error("a read-only invocation must never open the store")
		return nil, nil
	})
	require.NoError(t, creator.Close())
}

func TestLazyLocalChangeCreatorSurfacesOpenFailure(t *testing.T) {
	opens := 0
	creator := newLazyLocalChangeCreator(func(context.Context) (state.Store, error) {
		opens++
		return nil, errors.New("config missing")
	})
	draft := conversation.ChangeDraft{Label: "x"}

	_, _, err := creator.CreateChangeDraft(context.Background(), draft)
	require.Error(t, err, "an unavailable database must surface as a create error, not a panic")
	assert.Contains(t, err.Error(), "config missing")

	_, _, err = creator.CreateChangeDraft(context.Background(), draft)
	require.Error(t, err)
	assert.Equal(t, 1, opens,
		"a broken database is remembered: retrying per confirmation turns one environment problem into a stream of identical errors")
	require.NoError(t, creator.Close())
}
