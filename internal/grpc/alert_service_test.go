// alert_service_test.go tests the AlertService gRPC handler. Most tests
// call the service methods directly; the streaming RPC test uses an
// in-process gRPC server (loopback, kernel-assigned port) plus a raw
// gRPC client stream, because the hand-written pb package does not
// include generated client code.
package grpc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// =========================================================================
// ReceiveAlert
// =========================================================================

// TestReceiveAlert_Success verifies a well-formed alert is accepted.
func TestReceiveAlert_Success(t *testing.T) {
	svc := NewAlertService(nil, nil)
	ctx := context.Background()

	req := &pb.AlertMessage{
		Id:       "alert-001",
		Source:   "prometheus",
		Severity: "critical",
		Title:    "CPU saturation on host-01",
		Target:   "host-01",
	}
	resp, err := svc.ReceiveAlert(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, "accepted", resp.GetStatus())
	assert.Equal(t, "alert-001", resp.GetId())
}

// TestReceiveAlert_NilRequest returns InvalidArgument.
func TestReceiveAlert_NilRequest(t *testing.T) {
	svc := NewAlertService(nil, nil)
	_, err := svc.ReceiveAlert(context.Background(), nil)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestReceiveAlert_InvalidArgument returns InvalidArgument for missing
// source or title.
func TestReceiveAlert_InvalidArgument(t *testing.T) {
	svc := NewAlertService(nil, nil)

	// Missing source.
	_, err := svc.ReceiveAlert(context.Background(), &pb.AlertMessage{
		Title: "x",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())

	// Missing title.
	_, err = svc.ReceiveAlert(context.Background(), &pb.AlertMessage{
		Source: "prometheus",
	})
	require.Error(t, err)
	st, _ = status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestReceiveAlert_AcceptsEmptyID verifies that an alert without an
// explicit id gets a fingerprint assigned.
func TestReceiveAlert_AcceptsEmptyID(t *testing.T) {
	svc := NewAlertService(nil, nil)
	resp, err := svc.ReceiveAlert(context.Background(), &pb.AlertMessage{
		Source: "prometheus",
		Title:  "no id",
	})
	require.NoError(t, err)
	assert.Equal(t, "accepted", resp.GetStatus())
	assert.NotEmpty(t, resp.GetId())
}

// =========================================================================
// GetAlertStatus
// =========================================================================

// TestGetAlertStatus_Success verifies a stored alert can be looked up.
func TestGetAlertStatus_Success(t *testing.T) {
	svc := NewAlertService(nil, nil)
	ctx := context.Background()

	_, err := svc.ReceiveAlert(ctx, &pb.AlertMessage{
		Id:     "alert-002",
		Source: "custom",
		Title:  "disk full",
	})
	require.NoError(t, err)

	resp, err := svc.GetAlertStatus(ctx, &pb.GetAlertStatusRequest{Id: "alert-002"})
	require.NoError(t, err)
	assert.Equal(t, "alert-002", resp.GetId())
	assert.Equal(t, "firing", resp.GetStatus())
}

// TestGetAlertStatus_NotFound returns NotFound for an unknown id.
func TestGetAlertStatus_NotFound(t *testing.T) {
	svc := NewAlertService(nil, nil)
	_, err := svc.GetAlertStatus(context.Background(), &pb.GetAlertStatusRequest{Id: "no-such-alert"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

// TestGetAlertStatus_InvalidArgument returns InvalidArgument for empty id.
func TestGetAlertStatus_InvalidArgument(t *testing.T) {
	svc := NewAlertService(nil, nil)
	_, err := svc.GetAlertStatus(context.Background(), &pb.GetAlertStatusRequest{})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestGetAlertStatus_NilRequest returns InvalidArgument.
func TestGetAlertStatus_NilRequest(t *testing.T) {
	svc := NewAlertService(nil, nil)
	_, err := svc.GetAlertStatus(context.Background(), nil)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// =========================================================================
// SubscribeAlerts (streaming)
// =========================================================================

// TestSubscribeAlerts_ReceivesBroadcast verifies that an alert received
// via ReceiveAlert is delivered to an active subscriber.
func TestSubscribeAlerts_ReceivesBroadcast(t *testing.T) {
	svc := NewAlertService(nil, nil)

	// Start an in-process gRPC server.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := ggrpc.NewServer()
	pb.RegisterAlertServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	// Connect a client.
	conn, err := ggrpc.NewClient(lis.Addr().String(),
		ggrpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Open the subscription stream.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamDesc := &ggrpc.StreamDesc{
		StreamName:    "SubscribeAlerts",
		ServerStreams: true,
	}
	stream, err := conn.NewStream(ctx, streamDesc,
		"/levee.AlertService/SubscribeAlerts", ggrpc.StaticMethod())
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(&pb.SubscribeRequest{}))

	// Send an alert via the client. We use a unary invoke on the same
	// connection so the alert flows through the registered server.
	invokeCtx, invokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer invokeCancel()
	var resp pb.AlertResponse
	err = conn.Invoke(invokeCtx, "/levee.AlertService/ReceiveAlert",
		&pb.AlertMessage{
			Id:     "alert-stream-1",
			Source: "prometheus",
			Title:  "streamed alert",
		}, &resp, ggrpc.StaticMethod())
	require.NoError(t, err)
	assert.Equal(t, "accepted", resp.GetStatus())

	// Receive the alert on the stream.
	var got pb.AlertMessage
	require.NoError(t, stream.RecvMsg(&got))
	assert.Equal(t, "alert-stream-1", got.GetId())
	assert.Equal(t, "prometheus", got.GetSource())
}

// TestSubscribeAlerts_RespectsSeverityFilter verifies that the severity
// filter drops non-matching alerts.
func TestSubscribeAlerts_RespectsSeverityFilter(t *testing.T) {
	svc := NewAlertService(nil, nil)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := ggrpc.NewServer()
	pb.RegisterAlertServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := ggrpc.NewClient(lis.Addr().String(),
		ggrpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamDesc := &ggrpc.StreamDesc{
		StreamName:    "SubscribeAlerts",
		ServerStreams: true,
	}
	stream, err := conn.NewStream(ctx, streamDesc,
		"/levee.AlertService/SubscribeAlerts", ggrpc.StaticMethod())
	require.NoError(t, err)
	require.NoError(t, stream.SendMsg(&pb.SubscribeRequest{Severity: "critical"}))

	// Send a warning alert (should be filtered out).
	var resp pb.AlertResponse
	require.NoError(t, conn.Invoke(context.Background(),
		"/levee.AlertService/ReceiveAlert",
		&pb.AlertMessage{Id: "warn-1", Source: "s", Title: "t", Severity: "warning"},
		&resp, ggrpc.StaticMethod()))

	// Send a critical alert (should pass the filter).
	require.NoError(t, conn.Invoke(context.Background(),
		"/levee.AlertService/ReceiveAlert",
		&pb.AlertMessage{Id: "crit-1", Source: "s", Title: "t", Severity: "critical"},
		&resp, ggrpc.StaticMethod()))

	var got pb.AlertMessage
	require.NoError(t, stream.RecvMsg(&got))
	assert.Equal(t, "crit-1", got.GetId())
	assert.Equal(t, "critical", got.GetSeverity())
}
