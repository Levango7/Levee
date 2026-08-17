
// conversation_service.go implements pb.ConversationServiceServer, the
// gRPC service that drives multi-turn remediation dialogues with the
// operator.
//
// The service wraps an optional *conversation.ConversationEngine. When
// the engine is nil the RPCs return codes.Unimplemented, keeping the
// server usable in reduced-functionality deployments. SendMessage is a
// thin adapter: it forwards the text to the engine and converts the
// returned Reply to a pb.ReplyMessage. SubscribeConversation opens a
// server-stream that emits replies for a specific session; the stream
// stays open until the client disconnects.
//
// All errors are mapped to gRPC codes via the status package. The service
// is safe for concurrent use and immutable after construction.

package grpc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ConversationService implements pb.ConversationServiceServer.
type ConversationService struct {
	pb.UnimplementedConversationServiceServer

	// engine is the conversation engine. May be nil.
	engine *conversation.ConversationEngine

	// log is the structured logger. When nil the package-level singleton
	// from internal/log is used.
	log *slog.Logger

	// mu guards subscribers.
	mu sync.RWMutex

	// subscribers is the set of active SubscribeConversation goroutines.
	// Each subscriber owns a buffered channel; SendMessage pushes a copy
	// of every reply to every subscriber whose session filter matches.
	subscribers map[int64]*convSubscriber
	nextSubID   int64
}

// convSubscriber is a single active SubscribeConversation stream.
type convSubscriber struct {
	// sessionID is the optional session filter; empty means all sessions.
	sessionID string
	ch        chan *pb.ReplyMessage
}

// NewConversationService constructs a ConversationService. Both engine
// and logger are optional; passing nil for either is supported.
func NewConversationService(engine *conversation.ConversationEngine, lg *slog.Logger) *ConversationService {
	if lg == nil {
		lg = log.With("component", "conversation_service")
	}
	return &ConversationService{
		engine:      engine,
		log:         lg,
		subscribers: make(map[int64]*convSubscriber),
	}
}

// --- SendMessage -----------------------------------------------------------

// SendMessage forwards a single user message to the conversation engine
// and returns the engine's reply. When session_id is empty a new session
// is created for the user; otherwise the existing session is reused.
func (s *ConversationService) SendMessage(ctx context.Context, req *pb.SendMessageRequest) (*pb.ReplyMessage, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}
	if strings.TrimSpace(req.GetUserId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	if strings.TrimSpace(req.GetText()) == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	if s.engine == nil {
		return nil, status.Error(codes.Unimplemented, "conversation engine not configured")
	}

	sessionID := req.GetSessionId()
	if sessionID == "" {
		// Create a new session on the fly. This makes SendMessage usable
		// without a prior CreateSession RPC (which the engine does not
		// expose via this service).
		sess, err := s.engine.NewSession(req.GetUserId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "new session: %v", err)
		}
		sessionID = sess.ID
	}

	reply, err := s.engine.HandleMessage(ctx, sessionID, req.GetUserId(), req.GetText())
	if err != nil {
		return nil, convErrToGRPC(err)
	}

	pbReply := replyToPB(reply, sessionID)
	s.broadcastReply(pbReply)
	return pbReply, nil
}

// --- SubscribeConversation -------------------------------------------------

// SubscribeConversation opens a server-stream of replies. The optional
// Source field of the SubscribeRequest is interpreted as a session id
// filter; when empty all replies are streamed.
func (s *ConversationService) SubscribeConversation(req *pb.SubscribeRequest, stream pb.ConversationService_SubscribeConversationServer) error {
	if req == nil {
		req = &pb.SubscribeRequest{}
	}
	ctx := stream.Context()

	ch := make(chan *pb.ReplyMessage, 32)
	subID := s.addSubscriber(&convSubscriber{
		sessionID: req.GetSource(),
		ch:        ch,
	})
	defer s.removeSubscriber(subID)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-ch:
			if msg == nil {
				continue
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// --- internal helpers ------------------------------------------------------

// broadcastReply sends msg to every active subscriber whose session
// filter matches. Slow subscribers (full channel) silently drop the
// reply to protect the hot path.
func (s *ConversationService) broadcastReply(msg *pb.ReplyMessage) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sub := range s.subscribers {
		if sub.sessionID != "" && sub.sessionID != msg.GetSessionId() {
			continue
		}
		select {
		case sub.ch <- msg:
		default:
			// channel full; drop to avoid blocking the producer.
		}
	}
}

// addSubscriber registers sub and returns its id.
func (s *ConversationService) addSubscriber(sub *convSubscriber) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSubID++
	id := s.nextSubID
	s.subscribers[id] = sub
	return id
}

// removeSubscriber removes the subscriber with the given id.
func (s *ConversationService) removeSubscriber(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub, ok := s.subscribers[id]; ok {
		close(sub.ch)
		delete(s.subscribers, id)
	}
}

// convErrToGRPC maps a conversation engine error to a gRPC status error.
func convErrToGRPC(err error) error {
	switch {
	case errors.Is(err, conversation.ErrSessionNotFound):
		return status.Errorf(codes.NotFound, "%v", err)
	case errors.Is(err, conversation.ErrSessionClosed):
		return status.Errorf(codes.NotFound, "%v", err)
	case errors.Is(err, conversation.ErrEmptyMessage):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, conversation.ErrInvalidState):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	case errors.Is(err, conversation.ErrNilRecommend):
		return status.Errorf(codes.Unimplemented, "%v", err)
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}

// --- conversion helpers ----------------------------------------------------

// replyToPB converts a conversation.Reply to a pb.ReplyMessage.
func replyToPB(r *conversation.Reply, sessionID string) *pb.ReplyMessage {
	if r == nil {
		return &pb.ReplyMessage{SessionId: sessionID}
	}
	msg := &pb.ReplyMessage{
		Text:      r.Text,
		SessionId: sessionID,
	}
	if r.Action != nil {
		msg.ActionType = string(r.Action.Type)
		msg.ActionPayload = r.Action.Payload
	}
	return msg
}