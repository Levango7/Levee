// conversation_service_test.go tests the ConversationService gRPC
// handler. Most tests call the service methods directly; the streaming
// SubscribeConversation test uses an in-process gRPC server.
package grpc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// newTestConversationEngine returns a ConversationEngine with no recommend
// or diagnose dependencies. HandleMessage still works for /help and
// similar no-op commands; commands that need the recommend engine return
// ErrNilRecommend.
func newTestConversationEngine() *conversation.ConversationEngine {
	return conversation.NewConversationEngine(conversation.ConversationEngineConfig{})
}

// =========================================================================
// SendMessage
// =========================================================================

// TestSendMessage_Success verifies a /help command returns a reply.
func TestSendMessage_Success(t *testing.T) {
	engine := newTestConversationEngine()
	svc := NewConversationService(engine, nil)
	ctx := context.Background()

	// Create a session first so we have a valid session id.
	sess, err := engine.NewSession("user-1")
	require.NoError(t, err)

	resp, err := svc.SendMessage(ctx, &pb.SendMessageRequest{
		SessionId: sess.ID,
		UserId:    "user-1",
		Text:      "/help",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetText())
	assert.Equal(t, sess.ID, resp.GetSessionId())
}

// TestSendMessage_NilEngine returns Unimplemented.
func TestSendMessage_NilEngine(t *testing.T) {
	svc := NewConversationService(nil, nil)
	_, err := svc.SendMessage(context.Background(), &pb.SendMessageRequest{
		UserId: "user-1",
		Text:   "hello",
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unimplemented, st.Code())
}

// TestSendMessage_InvalidArgument returns InvalidArgument for missing
// user_id or text.
func TestSendMessage_InvalidArgument(t *testing.T) {
	svc := NewConversationService(newTestConversationEngine(), nil)

	// Missing user_id.
	_, err := svc.SendMessage(context.Background(), &pb.SendMessageRequest{
		Text: "hello",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())

	// Missing text.
	_, err = svc.SendMessage(context.Background(), &pb.SendMessageRequest{
		UserId: "user-1",
	})
	require.Error(t, err)
	st, _ = status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestSendMessage_NilRequest returns InvalidArgument.
func TestSendMessage_NilRequest(t *testing.T) {
	svc := NewConversationService(newTestConversationEngine(), nil)
	_, err := svc.SendMessage(context.Background(), nil)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestSendMessage_SessionNotFound returns NotFound for an unknown session.
func TestSendMessage_SessionNotFound(t *testing.T) {
	svc := NewConversationService(newTestConversationEngine(), nil)
	_, err := svc.SendMessage(context.Background(), &pb.SendMessageRequest{
		SessionId: "no-such-session",
		UserId:    "user-1",
		Text:      "hello",
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

// TestSendMessage_AutoCreateSession verifies that an empty session_id
// creates a new session.
func TestSendMessage_AutoCreateSession(t *testing.T) {
	svc := NewConversationService(newTestConversationEngine(), nil)
	resp, err := svc.SendMessage(context.Background(), &pb.SendMessageRequest{
		UserId: "user-1",
		Text:   "/help",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetSessionId())
	assert.NotEmpty(t, resp.GetText())
}

// =========================================================================
// SubscribeConversation (streaming)
// =========================================================================

// TestSubscribeConversation_ReceivesReply verifies that a reply from
// SendMessage is delivered to an active subscriber.
func TestSubscribeConversation_ReceivesReply(t *testing.T) {
	engine := newTestConversationEngine()
	svc := NewConversationService(engine, nil)

	// Start an in-process gRPC server.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := ggrpc.NewServer()
	pb.RegisterConversationServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := ggrpc.NewClient(lis.Addr().String(),
		ggrpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Create a session.
	sess, err := engine.NewSession("user-2")
	require.NoError(t, err)

	// Open the subscription stream filtered to this session.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamDesc := &ggrpc.StreamDesc{
		StreamName:    "SubscribeConversation",
		ServerStreams: true,
	}
	stream, err := conn.NewStream(ctx, streamDesc,
		"/levee.ConversationService/SubscribeConversation", ggrpc.StaticMethod())
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(&pb.SubscribeRequest{Source: sess.ID}))

	// Give the subscriber goroutine time to register before sending a
	// message. Without this, the reply may be broadcast before the
	// subscriber is registered and the stream would block forever.
	time.Sleep(200 * time.Millisecond)

	// Send a message via the client.
	invokeCtx, invokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer invokeCancel()
	var resp pb.ReplyMessage
	err = conn.Invoke(invokeCtx, "/levee.ConversationService/SendMessage",
		&pb.SendMessageRequest{
			SessionId: sess.ID,
			UserId:    "user-2",
			Text:      "/help",
		}, &resp, ggrpc.StaticMethod())
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetText())

	// Receive the reply on the stream.
	var got pb.ReplyMessage
	require.NoError(t, stream.RecvMsg(&got))
	assert.Equal(t, sess.ID, got.GetSessionId())
	assert.NotEmpty(t, got.GetText())
}