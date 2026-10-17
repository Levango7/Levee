package grpc

// conversation_policy_test.go covers the gates on the remediation dialogue —
// both its gRPC surface (SendMessage / SubscribeConversation) and the REST
// session endpoints, which talk to the engine directly and were therefore not
// covered by anything on the gRPC side.
//
// The dialogue carries no environment in any of its requests (session, user,
// text), so everything here is judged in permission.default_env — dev in this
// fixture. The subjects are chosen so a gate that resolved nothing still shows
// up: alice holds plan+view in dev, carol holds only approve (so not even a
// read), mallory is not registered at all.
//
// Each refusal is paired with a state assertion (sessions not created, no
// subscriber registered) and with the same call succeeding for a subject who
// holds the grant. Without the pair, "nothing happened" would be equally true
// of an RPC that refuses everyone.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc/pb"
)

func newPolicyConversationService(t *testing.T) (*ConversationService, *conversation.ConversationEngine) {
	t.Helper()
	engine := conversation.NewConversationEngine(conversation.ConversationEngineConfig{})
	svc := NewConversationService(engine, nil).WithAuthorizer(newServiceAuthorizer(t))
	return svc, engine
}

func subscriberCount(svc *ConversationService) int {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return len(svc.subscribers)
}

func TestSendMessage_PolicedInDefaultEnv(t *testing.T) {
	svc, engine := newPolicyConversationService(t)

	// carol holds `approve` in dev and nothing else: driving the dialogue is
	// beyond her, even though dev is the environment everything lands in.
	_, err := svc.SendMessage(asSubject("carol"), &pb.SendMessageRequest{UserId: "carol", Text: "what is broken?"})
	require.Error(t, err, "a subject with no plan grant must not be able to drive the dialogue")
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err))
	assert.Contains(t, err.Error(), "plan", "the refusal must name the action it judged")
	assert.Contains(t, err.Error(), "dev", "an env-less dialogue request is judged in the default env, and the refusal must say so")
	assert.Equal(t, 0, engine.SessionCount(), "a refused message must not have opened a session")

	// mallory is not registered: refused as well, and by the same rule.
	_, err = svc.SendMessage(asSubject("mallory"), &pb.SendMessageRequest{UserId: "mallory", Text: "hi"})
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err))

	// alice does hold plan in dev — the allowed path has to work, or the two
	// refusals above would just be a broken RPC.
	reply, err := svc.SendMessage(asSubject("alice"), &pb.SendMessageRequest{UserId: "alice", Text: "hello"})
	require.NoError(t, err, "alice has plan in the default env (dev)")
	require.NotNil(t, reply)
	assert.NotEmpty(t, reply.GetSessionId(), "the accepted path creates the session the refused ones did not")
	assert.Equal(t, 1, engine.SessionCount())
}

// TestSendMessage_SubjectBeatsAssertedUser pins the interaction between the two
// identities the handler carries: the authenticated subject is both the actor
// and the policy input, so a caller cannot ask for plan as "alice" while the
// session is opened for somebody else.
func TestSendMessage_SubjectBeatsAssertedUser(t *testing.T) {
	svc, engine := newPolicyConversationService(t)

	_, err := svc.SendMessage(asSubject("carol"), &pb.SendMessageRequest{UserId: "alice", Text: "hi"})
	require.Error(t, err, "asserting a granted user_id must not carry an ungranted subject through the gate")
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err))
	assert.Equal(t, 0, engine.SessionCount())

	reply, err := svc.SendMessage(asSubject("alice"), &pb.SendMessageRequest{UserId: "carol", Text: "hi"})
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Equal(t, 1, engine.SessionCount())
	assert.Empty(t, engine.ListSessions("carol"), "the session belongs to the subject, not the asserted name")
}

func TestSendMessage_NoAuthorizerMeansNoGate(t *testing.T) {
	svc := NewConversationService(
		conversation.NewConversationEngine(conversation.ConversationEngineConfig{}), nil)

	_, err := svc.SendMessage(asSubject("mallory"), &pb.SendMessageRequest{UserId: "mallory", Text: "hi"})
	require.NoError(t, err, "no policy configured means nothing to decide against, as in every other service")
}

// endedStream makes the subject's context one a server-stream will not run on:
// the gate is what the test is about, so a stream that gets past it must end at
// once instead of occupying the test until its deadline. That is also what makes
// a missing gate fail rather than hang — the call returns nil, and the assertion
// on the refusal fires immediately.
func endedStream(subject string) context.Context {
	ctx, cancel := context.WithCancel(asSubject(subject))
	cancel()
	return ctx
}

func TestSubscribeConversation_PolicedAsARead(t *testing.T) {
	svc, _ := newPolicyConversationService(t)

	// An open stream keeps receiving replies, so it is a read and must be
	// judged as one. carol has no `view` anywhere.
	err := svc.SubscribeConversation(&pb.SubscribeRequest{}, &fakeConvStream{ctx: endedStream("carol")})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err))
	assert.Equal(t, 0, subscriberCount(svc), "a refused subscribe must not register a subscriber")

	// alice may (view in dev), and the stream ends without ever sending — so
	// her refusal above is a policy answer, not a broken handler.
	require.NoError(t, svc.SubscribeConversation(&pb.SubscribeRequest{}, &fakeConvStream{ctx: endedStream("alice")}),
		"alice holds view in the default env; her stream must open and close cleanly")
	assert.Equal(t, 0, subscriberCount(svc), "the stream's own defer must have removed its subscriber")
}

func TestSubscribeConversation_NoAuthorizerMeansNoGate(t *testing.T) {
	svc := NewConversationService(
		conversation.NewConversationEngine(conversation.ConversationEngineConfig{}), nil)

	require.NoError(t, svc.SubscribeConversation(&pb.SubscribeRequest{}, &fakeConvStream{ctx: endedStream("mallory")}))
}

// --- the REST surface ------------------------------------------------------

// convRESTGateway is a gateway whose only capability is the dialogue, with the
// same authorizer the gRPC tests use. dispatchConversation is called directly:
// the middleware chain is already covered by rest_conversation_owner_test.go,
// and what is under test here is the gate inside the dispatcher.
func convRESTGateway(t *testing.T) (*Gateway, *conversation.ConversationEngine) {
	t.Helper()
	engine := conversation.NewConversationEngine(conversation.ConversationEngineConfig{})
	gw := NewGateway(ServeGatewayConfig{})
	gw.SetConversationEngine(engine)
	gw.WithAuthorizer(newServiceAuthorizer(t))
	return gw, engine
}

func doConvREST(gw *Gateway, subject, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	if subject != "" {
		req = req.WithContext(ContextWithSubject(req.Context(), subject))
	}
	// restRoute hands the dispatcher the path with the query string stripped,
	// so do the same — a test then carries query parameters like a real request.
	gw.dispatchConversation(w, req, method, req.URL.Path)
	return w
}

func TestConversationREST_WriteRequiresPlan(t *testing.T) {
	gw, engine := convRESTGateway(t)

	w := doConvREST(gw, "carol", http.MethodPost, "/conversation/sessions")
	assert.Equal(t, http.StatusForbidden, w.Code,
		"opening a session over REST is the same act as SendMessage and must not be a way around it")
	assert.Contains(t, w.Body.String(), "plan")
	assert.Equal(t, 0, engine.SessionCount(), "the refused request must not have reached the engine")

	// closing somebody's session is a write too, not a read because it is a DELETE
	// on a resource the caller cannot even see.
	w = doConvREST(gw, "carol", http.MethodDelete, "/conversation/sessions/sess-1")
	assert.Equal(t, http.StatusForbidden, w.Code)

	w = doConvREST(gw, "alice", http.MethodPost, "/conversation/sessions")
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"alice passes the gate and reaches the handler, which then rejects the empty body — so the 403 above was a policy answer, not a broken route")
	assert.Equal(t, 0, engine.SessionCount())
}

func TestConversationREST_ListRequiresView(t *testing.T) {
	gw, _ := convRESTGateway(t)

	w := doConvREST(gw, "carol", http.MethodGet, "/conversation/sessions")
	assert.Equal(t, http.StatusForbidden, w.Code, "listing sessions is a read and needs view")
	assert.Contains(t, w.Body.String(), "view", "the read gate names the action it judged, so a 403 here is not mistaken for a missing session")

	// alice may, which is what makes carol's 403 a policy answer.
	w = doConvREST(gw, "alice", http.MethodGet, "/conversation/sessions")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestConversationREST_UnauthenticatedCallerKeepsTheSharedPosture(t *testing.T) {
	gw, engine := convRESTGateway(t)

	// No subject at all: the read rule admits the unattributable caller by
	// design (authz.AdmitsRead), the write rule does not. Pinning both halves
	// keeps the read exemption from being used to justify an open write.
	// The 200/400 split is what makes this readable: a 403 on the GET would
	// mean the gate refused, which is the posture this clause denies.
	w := doConvREST(gw, "", http.MethodGet, "/conversation/sessions?user_id=dev-user")
	assert.Equal(t, http.StatusOK, w.Code,
		"an unattributable caller keeps the read posture the shared rule defines")
	assert.Equal(t, 0, engine.SessionCount())

	w = doConvREST(gw, "", http.MethodPost, "/conversation/sessions")
	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"a write from an unattributable caller must land on Unauthenticated, not pass")
	assert.Equal(t, 0, engine.SessionCount())
}

func TestConversationREST_NoAuthorizerMeansNoGate(t *testing.T) {
	engine := conversation.NewConversationEngine(conversation.ConversationEngineConfig{})
	gw := NewGateway(ServeGatewayConfig{})
	gw.SetConversationEngine(engine)

	w := doConvREST(gw, "mallory", http.MethodPost, "/conversation/sessions")
	assert.NotEqual(t, http.StatusForbidden, w.Code, "the deployment with no policy keeps the endpoints it had before the gate")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
