package main

// conversation_change_creator.go adapts the in-process gRPC ChangeService to
// conversation.ChangeCreator, so a serve-mode conversation can hand a
// confirmed recommendation to the real change path: CreateChange records it as
// a draft, and everything after that (plan, approval, apply) is the standard
// governance chain the operator already trusts.
//
// The adapter is intentionally thin. It adds no persistence and no status
// vocabulary of its own — CreateChange here is the same call `levee change`
// makes, which is what keeps the AI loop from becoming a second, laxer door
// into execution.

import (
	"context"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
)

// conversationChangeCreator implements conversation.ChangeCreator over
// *grpc.ChangeService.
type conversationChangeCreator struct {
	svc *grpc.ChangeService
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
