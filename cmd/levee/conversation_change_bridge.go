package main

// conversation_change_bridge.go wires the recommendation→change bridge for
// `levee converse`, the standalone CLI path.
//
// The serve path gets its bridge from cmd_serve (in-process ChangeService).
// This one is different in a way that shapes the whole design: a CLI command
// owns a store only while it runs, and most converse invocations never need
// one at all (`--list`, `--history`, or a plain question). Opening the state
// database at construction time would make every read-only invocation create
// or lock a database file, and would make the engine untestable without one.
//
// So the store is opened LAZILY, on the first confirmed recommendation, and
// its lifetime is tied to the engine (AddCloser) so the process leaves no
// SQLite handle behind. If the store cannot be opened, the failure surfaces as
// a normal create error and the conversation answers honestly — the operator
// can fix the environment and retry.

import (
	"context"
	"fmt"
	"sync"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// storeOpener opens the CLI's state store. It is a field rather than a direct
// call to openStore so tests can point the bridge at a temporary database.
type storeOpener func(context.Context) (state.Store, error)

// lazyLocalChangeCreator is a conversation.ChangeCreator that opens the local
// state store on first use. It is safe for concurrent use: the bridge may be
// reached from a REPL goroutine and, in tests, from several goroutines at once.
type lazyLocalChangeCreator struct {
	mu      sync.Mutex
	open    storeOpener
	store   state.Store
	svc     *grpc.ChangeService
	openErr error
}

// newLazyLocalChangeCreator returns a creator that opens its store through
// open. The store is not touched until the first CreateChangeDraft call.
func newLazyLocalChangeCreator(open storeOpener) *lazyLocalChangeCreator {
	return &lazyLocalChangeCreator{open: open}
}

// service returns the lazily built ChangeService, opening the store on the
// first call. A failure to open is remembered: retrying a broken database on
// every confirmation would turn one environment problem into a stream of
// identical errors.
func (c *lazyLocalChangeCreator) service() (*grpc.ChangeService, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.svc != nil || c.openErr != nil {
		return c.svc, c.openErr
	}
	st, err := c.open(context.Background())
	if err != nil {
		c.openErr = fmt.Errorf("open state store: %w", err)
		return nil, c.openErr
	}
	c.store = st
	c.svc = grpc.NewChangeService(st, nil, nil, nil)
	return c.svc, nil
}

// CreateChangeDraft records one draft change and returns its id and status.
func (c *lazyLocalChangeCreator) CreateChangeDraft(ctx context.Context, draft conversation.ChangeDraft) (string, string, error) {
	svc, err := c.service()
	if err != nil {
		return "", "", err
	}
	ch, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        draft.Label,
		WorkflowFile: draft.WorkflowFile,
		Params:       draft.Params,
	})
	if err != nil {
		return "", "", err
	}
	return ch.GetId(), ch.GetStatus(), nil
}

// Close releases the store if one was ever opened. It is safe to call when
// nothing was opened (read-only invocations) and safe to call twice.
func (c *lazyLocalChangeCreator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		return nil
	}
	err := c.store.Close()
	c.store = nil
	c.svc = nil
	return err
}
