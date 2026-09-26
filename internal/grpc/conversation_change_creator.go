package grpc

// conversation_change_creator.go adapts ChangeService to
// conversation.ChangeCreator, so a serve-mode conversation can hand a
// confirmed recommendation to the real change path: CreateChange records it
// as a draft, and everything after that (plan, approval, apply) is the
// standard governance chain.
//
// The adapter lives here rather than in cmd/levee for two reasons. It is a
// thin wrapper around this service, so it belongs next to the service it
// wraps; and internal/grpc already depends on internal/conversation, which
// means the adapter is reachable from the end-to-end test that drives the
// real store (internal/wiring/inline_change_plan_test.go) instead of having
// to be re-implemented as a test double — the whole point of that test is
// that the real adapter is what runs.

import (
	"context"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc/pb"
)

// conversationChangeCreator implements conversation.ChangeCreator over
// *ChangeService.
type conversationChangeCreator struct {
	svc *ChangeService
}

// NewConversationChangeCreator returns a conversation.ChangeCreator backed by
// svc. A nil svc yields a nil creator, which leaves the conversation engine on
// its pre-bridge behaviour: approvals are recorded and the reply says plainly
// that nothing was submitted.
func NewConversationChangeCreator(svc *ChangeService) conversation.ChangeCreator {
	if svc == nil {
		return nil
	}
	return conversationChangeCreator{svc: svc}
}

// CreateChangeDraft records one draft change and returns its id and status.
func (c conversationChangeCreator) CreateChangeDraft(ctx context.Context, draft conversation.ChangeDraft) (string, string, error) {
	ch, err := c.svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        draft.Label,
		WorkflowFile: draft.WorkflowFile,
		Params:       draft.Params,
	})
	if err != nil {
		return "", "", err
	}
	return ch.GetId(), ch.GetStatus(), nil
}
