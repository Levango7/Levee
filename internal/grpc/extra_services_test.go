// extra_services_test.go covers RegisterExtraServices (the single entry
// point that installs the Alert/Diagnosis/Conversation services on a gRPC
// server) and the nil-dependency degradation branches of those three
// services. Registration is verified end-to-end by dialing a real in-process
// gRPC server and asserting Unimplemented for stubs versus real behaviour
// for wired implementations.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// startServerWithExtraServices builds a Server, registers the extra services
// with the supplied config before Start, runs it on a kernel-assigned port
// and returns a client connection. The extra pb services have no generated
// client stubs, so tests invoke them via conn.Invoke with the full method
// names (exactly as the hand-written pb file documents).
func startServerWithExtraServices(t *testing.T, cfg ExtraServicesConfig) *ggrpc.ClientConn {
	t.Helper()
	srv := NewServer(newTestStore(t))
	RegisterExtraServices(srv.GrpcServer(), cfg)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(testListenAddr) }()
	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 5*time.Millisecond, "server did not bind")
	t.Cleanup(func() {
		_ = srv.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Errorf("server Stop did not return within 5s")
		}
	})

	return newInsecureClient(t, srv.Addr())
}

// invokeExtraRPC calls an extra-service method over the wire.
func invokeExtraRPC(ctx context.Context, conn *ggrpc.ClientConn, method string, req, out interface{}) error {
	return conn.Invoke(ctx, method, req, out)
}

const (
	alertReceiveMethod = "/levee.AlertService/ReceiveAlert"
	diagGetMethod      = "/levee.DiagnosisService/GetDiagnosis"
	convSendMethod     = "/levee.ConversationService/SendMessage"
)

// --- RegisterExtraServices -------------------------------------------------------

func TestRegisterExtraServices_NilConfigRegistersStubs(t *testing.T) {
	conn := startServerWithExtraServices(t, ExtraServicesConfig{})
	ctx := context.Background()

	// Every RPC must route to the generated Unimplemented stubs rather than
	// fail with Unavailable or an internal crash.
	err := invokeExtraRPC(ctx, conn, alertReceiveMethod,
		&pb.AlertMessage{Source: "prom", Title: "down"}, &pb.AlertResponse{})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))

	err = invokeExtraRPC(ctx, conn, "/levee.DiagnosisService/Diagnose",
		&pb.DiagnoseRequest{Target: "h1"}, &pb.DiagnosticReportMessage{})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))

	err = invokeExtraRPC(ctx, conn, convSendMethod,
		&pb.SendMessageRequest{UserId: "u1", Text: "/help"}, &pb.ReplyMessage{})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestRegisterExtraServices_RealImplementationsHandleRPCs(t *testing.T) {
	cfg := ExtraServicesConfig{
		Alert:        NewAlertService(nil, nil),
		Diagnosis:    NewDiagnosisService(diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{}), nil),
		Conversation: NewConversationService(conversation.NewConversationEngine(conversation.ConversationEngineConfig{}), nil),
	}
	conn := startServerWithExtraServices(t, cfg)
	ctx := context.Background()

	// Alert service routes to the real implementation: missing source maps
	// to codes.InvalidArgument from ReceiveAlert's own validation.
	err := invokeExtraRPC(ctx, conn, alertReceiveMethod, &pb.AlertMessage{}, &pb.AlertResponse{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	resp := &pb.AlertResponse{}
	require.NoError(t, invokeExtraRPC(ctx, conn, alertReceiveMethod,
		&pb.AlertMessage{Source: "prom", Title: "cpu hot"}, resp))
	assert.Equal(t, "accepted", resp.GetStatus())

	// Diagnosis service runs a real report against the configured engine.
	report := &pb.DiagnosticReportMessage{}
	require.NoError(t, invokeExtraRPC(ctx, conn, "/levee.DiagnosisService/Diagnose",
		&pb.DiagnoseRequest{Target: "host-x"}, report))
	assert.NotEmpty(t, report.GetId())

	// Conversation service rejects empty user ids itself.
	err = invokeExtraRPC(ctx, conn, convSendMethod,
		&pb.SendMessageRequest{Text: "/help"}, &pb.ReplyMessage{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestRegisterExtraServices_PartialConfigMixesStubAndReal(t *testing.T) {
	conn := startServerWithExtraServices(t, ExtraServicesConfig{
		Diagnosis: NewDiagnosisService(nil, nil),
	})
	ctx := context.Background()

	// Diagnosis wired with a nil engine still routes to the real service;
	// the unknown report id comes back as NotFound (not Unimplemented).
	err := invokeExtraRPC(ctx, conn, diagGetMethod,
		&pb.GetDiagnosisRequest{Id: "missing-report"}, &pb.DiagnosticReportMessage{})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))

	// Alert and Conversation fall back to stubs.
	err = invokeExtraRPC(ctx, conn, alertReceiveMethod,
		&pb.AlertMessage{Source: "s", Title: "t"}, &pb.AlertResponse{})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
	err = invokeExtraRPC(ctx, conn, convSendMethod,
		&pb.SendMessageRequest{UserId: "u", Text: "x"}, &pb.ReplyMessage{})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// --- AlertService branches ----------------------------------------------------------

func TestReceiveAlert_InvalidSeverityFallsBackToInfo(t *testing.T) {
	svc := NewAlertService(nil, nil)
	resp, err := svc.ReceiveAlert(context.Background(), &pb.AlertMessage{
		Source:   "prom",
		Title:    "odd severity",
		Severity: "banana",
	})
	require.NoError(t, err)
	assert.Equal(t, "accepted", resp.GetStatus())

	st, err := svc.GetAlertStatus(context.Background(), &pb.GetAlertStatusRequest{Id: resp.GetId()})
	require.NoError(t, err)
	assert.Equal(t, "info", st.GetSeverity())
	assert.Equal(t, "firing", st.GetStatus())
}

func TestReceiveAlert_TimestampsExplicitIDAndResolvedStatus(t *testing.T) {
	svc := NewAlertService(nil, nil)
	const startsAt = int64(1700000000)
	const endsAt = int64(1700003600)
	resp, err := svc.ReceiveAlert(context.Background(), &pb.AlertMessage{
		Id:       "explicit-id",
		Source:   "custom",
		Title:    "resolved alert",
		Status:   "resolved",
		StartsAt: startsAt,
		EndsAt:   endsAt,
	})
	require.NoError(t, err)
	assert.Equal(t, "accepted", resp.GetStatus())
	assert.Equal(t, "explicit-id", resp.GetId())

	st, err := svc.GetAlertStatus(context.Background(), &pb.GetAlertStatusRequest{Id: "explicit-id"})
	require.NoError(t, err)
	assert.EqualValues(t, startsAt, st.StartsAt)
	assert.EqualValues(t, endsAt, st.EndsAt)
	assert.Equal(t, "resolved", st.GetStatus())
}

func TestGetAlertStatus_EvictsOldestWhenRingFull(t *testing.T) {
	svc := NewAlertService(nil, nil)
	ctx := context.Background()

	var firstID string
	for i := 0; i <= maxRecentAlerts; i++ {
		resp, err := svc.ReceiveAlert(ctx, &pb.AlertMessage{
			Source: "load", Title: fmt.Sprintf("alert-%03d", i),
		})
		require.NoError(t, err)
		if i == 0 {
			firstID = resp.GetId()
		}
	}

	// The very first alert has been evicted from the bounded ring.
	_, err := svc.GetAlertStatus(ctx, &pb.GetAlertStatusRequest{Id: firstID})
	assert.Equal(t, codes.NotFound, status.Code(err), "oldest alert should have been evicted")
}

func TestRecordAlert_UpdateInPlaceKeepsLookupStable(t *testing.T) {
	svc := NewAlertService(nil, nil)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, err := svc.ReceiveAlert(ctx, &pb.AlertMessage{Id: "dup-1", Source: "s", Title: "same"})
		require.NoError(t, err)
	}

	st, err := svc.GetAlertStatus(ctx, &pb.GetAlertStatusRequest{Id: "dup-1"})
	require.NoError(t, err)
	assert.NotNil(t, st)
}

func TestAlertToPB_NilReturnsNil(t *testing.T) {
	assert.Nil(t, alertToPB(nil))
}

// alertFixture builds an alert.Alert with the given source/severity.
func alertFixture(source, severity string) *alert.Alert {
	sev, _ := alert.ParseSeverity(severity)
	return &alert.Alert{Source: source, Title: "fixture", Severity: sev}
}

func TestSubscribeAlerts_SourceFilterDropsMismatchedAlerts(t *testing.T) {
	svc := NewAlertService(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &fakeAlertStream{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- svc.SubscribeAlerts(&pb.SubscribeRequest{Source: "prometheus"}, stream)
	}()
	require.Eventually(t, func() bool { return svc.SubscriberCount() == 1 },
		5*time.Second, 5*time.Millisecond, "subscriber not registered in time")

	// Mismatched source — dropped by the filter.
	svc.broadcast(alertFixture("custom", "warning"))
	time.Sleep(30 * time.Millisecond)

	// Matching source — delivered.
	svc.broadcast(alertFixture("Prometheus", "critical"))
	time.Sleep(50 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	require.Len(t, stream.alerts, 1, "only the matching-source alert may be delivered")
	assert.Equal(t, "Prometheus", stream.alerts[0].GetSource(),
		"source matching is case-insensitive but the original casing is preserved")
}

func TestSubscribeAlerts_SeverityFilterCaseInsensitive(t *testing.T) {
	svc := NewAlertService(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &fakeAlertStream{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- svc.SubscribeAlerts(&pb.SubscribeRequest{Severity: " CRITICAL "}, stream)
	}()
	require.Eventually(t, func() bool { return svc.SubscriberCount() == 1 },
		5*time.Second, 5*time.Millisecond, "subscriber not registered in time")

	svc.broadcast(alertFixture("src", "info"))
	svc.broadcast(alertFixture("src", "Critical"))
	time.Sleep(50 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	require.Len(t, stream.alerts, 1)
	assert.Equal(t, "critical", stream.alerts[0].GetSeverity())
}

func TestSubscribeAlerts_NilRequestTreatedAsEmptyFilter(t *testing.T) {
	svc := NewAlertService(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &fakeAlertStream{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- svc.SubscribeAlerts(nil, stream)
	}()
	require.Eventually(t, func() bool { return svc.SubscriberCount() == 1 },
		5*time.Second, 5*time.Millisecond, "subscriber not registered in time")

	svc.broadcast(alertFixture("any", "info"))
	time.Sleep(50 * time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	require.Len(t, stream.alerts, 1)
}

// fakeAlertStream is a test double for pb.AlertService_SubscribeAlertsServer.
type fakeAlertStream struct {
	ctx    context.Context
	alerts []*pb.AlertMessage
}

func (f *fakeAlertStream) Context() context.Context { return f.ctx }

func (f *fakeAlertStream) Send(a *pb.AlertMessage) error {
	f.alerts = append(f.alerts, a)
	return nil
}

func (f *fakeAlertStream) SendMsg(interface{}) error    { return nil }
func (f *fakeAlertStream) RecvMsg(interface{}) error    { return nil }
func (f *fakeAlertStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeAlertStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeAlertStream) SetTrailer(metadata.MD)       {}

// --- DiagnosisService branches --------------------------------------------------------

func TestDiagnose_WithTimeoutSecondsSucceeds(t *testing.T) {
	svc := NewDiagnosisService(diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{}), nil)
	resp, err := svc.Diagnose(context.Background(), &pb.DiagnoseRequest{
		Target:         "timed-host",
		TimeoutSeconds: 2,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetId())
}

func TestDiagnosisCache_EvictionAndEmptyIDSkip(t *testing.T) {
	svc := NewDiagnosisService(nil, nil)

	// Reports without an ID are skipped by cacheReport.
	svc.cacheReport(diagnosis.DiagnosticReport{ID: ""})

	for i := 0; i < maxRecentReports+10; i++ {
		svc.cacheReport(diagnosis.DiagnosticReport{ID: fmt.Sprintf("rep-%03d", i)})
	}

	// The oldest entries were evicted; recent ones remain.
	_, err := svc.GetDiagnosis(context.Background(), &pb.GetDiagnosisRequest{Id: "rep-000"})
	assert.Equal(t, codes.NotFound, status.Code(err))

	newest := fmt.Sprintf("rep-%03d", maxRecentReports+9)
	got, err := svc.GetDiagnosis(context.Background(), &pb.GetDiagnosisRequest{Id: newest})
	require.NoError(t, err)
	assert.Equal(t, newest, got.GetId())
}

func TestDiagnosisToPB_MapsFindingsAndFields(t *testing.T) {
	rep := diagnosis.DiagnosticReport{
		ID:              "rep-full",
		Target:          "h1",
		Trigger:         diagnosis.TriggerManual,
		Status:          diagnosis.DiagUnhealthy,
		RootCause:       "disk full",
		Confidence:      0.87,
		Summary:         "summary text",
		Recommendations: []string{"clean /var"},
		Errors:          []string{"probe timeout"},
		StartedAt:       time.Unix(1700000000, 0),
		Duration:        1500 * time.Millisecond,
		Findings: []diagnosis.Finding{{
			ID: "f1", Category: "disk", Severity: "high",
			Title: "usage 98%", Description: "nearly full",
			Evidence: "df -h", Suggestion: "rotate logs",
		}},
	}
	svc := NewDiagnosisService(nil, nil)
	svc.cacheReport(rep)

	got, err := svc.GetDiagnosis(context.Background(), &pb.GetDiagnosisRequest{Id: "rep-full"})
	require.NoError(t, err)
	assert.Equal(t, "rep-full", got.Id)
	assert.Equal(t, "h1", got.Target)
	assert.Equal(t, string(diagnosis.TriggerManual), got.Trigger)
	assert.Equal(t, string(diagnosis.DiagUnhealthy), got.Status)
	assert.Equal(t, "disk full", got.GetRootCause())
	assert.InDelta(t, 0.87, got.Confidence, 0.0001)
	assert.Equal(t, []string{"clean /var"}, got.Recommendations)
	assert.Equal(t, []string{"probe timeout"}, got.Errors)
	assert.EqualValues(t, 1500, got.DurationMs)
	assert.EqualValues(t, 1700000000, got.StartedAt)
	require.Len(t, got.GetFindings(), 1)
	f := got.GetFindings()[0]
	assert.Equal(t, "f1", f.Id)
	assert.Equal(t, "disk", f.Category)
	assert.Equal(t, "high", f.Severity)
	assert.Equal(t, "usage 98%", f.Title)
	assert.Equal(t, "rotate logs", f.Suggestion)
}

// --- ConversationService branches ------------------------------------------------------

func TestConvErrToGRPCTable(t *testing.T) {
	wrap := func(e error) error { return fmt.Errorf("conversation: handle: %w", e) }
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"session not found", wrap(conversation.ErrSessionNotFound), codes.NotFound},
		{"session closed", wrap(conversation.ErrSessionClosed), codes.NotFound},
		{"empty message", wrap(conversation.ErrEmptyMessage), codes.InvalidArgument},
		{"invalid state", wrap(conversation.ErrInvalidState), codes.FailedPrecondition},
		{"nil recommend", wrap(conversation.ErrNilRecommend), codes.Unimplemented},
		{"unknown error", errors.New("boom"), codes.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, status.Code(convErrToGRPC(tc.err)))
		})
	}
}

func TestReplyToPB(t *testing.T) {
	t.Run("nil reply yields session-only message", func(t *testing.T) {
		msg := replyToPB(nil, "sess-1")
		assert.Empty(t, msg.GetText())
		assert.Equal(t, "sess-1", msg.GetSessionId())
	})

	t.Run("action fields are mapped", func(t *testing.T) {
		msg := replyToPB(&conversation.Reply{
			Text: "running remediation",
			Action: &conversation.Action{
				Type:    conversation.ActionExecute,
				Payload: map[string]string{"playbook": "pb-42"},
			},
		}, "sess-2")
		assert.Equal(t, "running remediation", msg.GetText())
		assert.Equal(t, string(conversation.ActionExecute), msg.GetActionType())
		assert.Equal(t, "pb-42", msg.ActionPayload["playbook"])
	})
}

func TestBroadcastReply_SessionFilterMismatchDropped(t *testing.T) {
	svc := NewConversationService(nil, slog.Default())

	ch := make(chan *pb.ReplyMessage, 4)
	id := svc.addSubscriber(&convSubscriber{sessionID: "sess-a", ch: ch})
	defer svc.removeSubscriber(id)

	// Reply for another session must be filtered out.
	svc.broadcastReply(&pb.ReplyMessage{SessionId: "sess-b", Text: "not yours"})
	select {
	case msg := <-ch:
		t.Fatalf("mismatched session leaked through: %+v", msg)
	default:
	}

	// Matching session passes.
	svc.broadcastReply(&pb.ReplyMessage{SessionId: "sess-a", Text: "yours"})
	select {
	case msg := <-ch:
		assert.Equal(t, "yours", msg.GetText())
	default:
		t.Fatal("matching reply was not delivered")
	}
}

func TestSubscribeConversation_SessionFilter(t *testing.T) {
	engine := conversation.NewConversationEngine(conversation.ConversationEngineConfig{})
	svc := NewConversationService(engine, nil)

	sessA, err := engine.NewSession("user-a")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &fakeConvStream{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- svc.SubscribeConversation(&pb.SubscribeRequest{Source: sessA.ID}, stream)
	}()
	time.Sleep(50 * time.Millisecond)

	// A reply for a different session must not reach the subscriber.
	svc.broadcastReply(&pb.ReplyMessage{SessionId: "other-session", Text: "noise"})
	time.Sleep(30 * time.Millisecond)

	// A reply for the subscribed session must arrive.
	svc.broadcastReply(&pb.ReplyMessage{SessionId: sessA.ID, Text: "hello"})
	time.Sleep(50 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	require.Len(t, stream.replies, 1)
	assert.Equal(t, "hello", stream.replies[0].GetText())
}

// fakeConvStream is a test double for pb.ConversationService_SubscribeConversationServer.
type fakeConvStream struct {
	ctx     context.Context
	replies []*pb.ReplyMessage
}

func (f *fakeConvStream) Context() context.Context { return f.ctx }

func (f *fakeConvStream) Send(m *pb.ReplyMessage) error {
	f.replies = append(f.replies, m)
	return nil
}

func (f *fakeConvStream) SendMsg(interface{}) error    { return nil }
func (f *fakeConvStream) RecvMsg(interface{}) error    { return nil }
func (f *fakeConvStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeConvStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeConvStream) SetTrailer(metadata.MD)       {}
