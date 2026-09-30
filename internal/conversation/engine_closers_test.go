package conversation

// engine_closers_test.go pins the engine's ownership contract for resources it
// did not create at construction time. The CLI bridge is the reason it exists:
// it opens a database on the first confirmed recommendation, long after the
// engine was built, and the engine is what closes it.

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type namedCloser struct {
	name  string
	order *[]string
	err   error
	times int
}

func (c *namedCloser) Close() error {
	c.times++
	*c.order = append(*c.order, c.name)
	return c.err
}

func TestEngineCloseClosesRegisteredResources(t *testing.T) {
	var order []string
	e := NewConversationEngine(ConversationEngineConfig{})
	store := &namedCloser{name: "store", order: &order}
	client := &namedCloser{name: "client", order: &order, err: errors.New("boom")}

	e.AddCloser(store)
	e.AddCloser(client)
	e.AddCloser(nil) // ignored

	err := e.Close()
	require.Error(t, err, "a failing closer must not be swallowed")
	assert.Contains(t, err.Error(), "boom")
	assert.Equal(t, []string{"client", "store"}, order,
		"newest first: a resource opened after another is released before it")
	assert.Equal(t, 1, store.times)

	require.NoError(t, e.Close(), "Close is idempotent: a second call has nothing left to close")
	assert.Equal(t, []string{"client", "store"}, order, "a second Close must not close anything twice")
}
