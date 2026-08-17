// diagnosis_service_test.go tests the DiagnosisService gRPC handler.
// Tests call the service methods directly; the DiagnosisService has no
// streaming RPCs so no in-process gRPC server is needed.
package grpc

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestDiagEngine returns a DiagEngine with no collector/analyzer/prober.
// Diagnose against such an engine still returns a complete report; the
// status is DiagUnknown because no evidence was gathered.
func newTestDiagEngine() *diagnosis.DiagEngine {
	return diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{})
}

// =========================================================================
// Diagnose
// =========================================================================

// TestDiagnose_Success verifies a diagnosis run returns a report.
func TestDiagnose_Success(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil)
	ctx := context.Background()

	resp, err := svc.Diagnose(ctx, &pb.DiagnoseRequest{
		Target: "host-01.example.com",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetId())
	assert.Equal(t, "host-01.example.com", resp.GetTarget())
	assert.Equal(t, string(diagnosis.TriggerManual), resp.Trigger)
}

// TestDiagnose_NilEngine returns Unimplemented.
func TestDiagnose_NilEngine(t *testing.T) {
	svc := NewDiagnosisService(nil, nil)
	_, err := svc.Diagnose(context.Background(), &pb.DiagnoseRequest{
		Target: "host-01",
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unimplemented, st.Code())
}

// TestDiagnose_InvalidArgument returns InvalidArgument for empty target.
func TestDiagnose_InvalidArgument(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil)
	_, err := svc.Diagnose(context.Background(), &pb.DiagnoseRequest{})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestDiagnose_NilRequest returns InvalidArgument.
func TestDiagnose_NilRequest(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil)
	_, err := svc.Diagnose(context.Background(), nil)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestDiagnose_WithAlertID verifies the alert id is recorded on the report.
func TestDiagnose_WithAlertID(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil)
	resp, err := svc.Diagnose(context.Background(), &pb.DiagnoseRequest{
		Target:  "host-02",
		AlertId: "alert-xyz",
	})
	require.NoError(t, err)
	assert.Equal(t, "alert-xyz", resp.AlertId)
	assert.Equal(t, string(diagnosis.TriggerAlert), resp.Trigger)
}

// =========================================================================
// GetDiagnosis
// =========================================================================

// TestGetDiagnosis_Success verifies a cached report can be retrieved.
func TestGetDiagnosis_Success(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil)
	ctx := context.Background()

	// Run a diagnosis to populate the cache.
	diag, err := svc.Diagnose(ctx, &pb.DiagnoseRequest{Target: "host-03"})
	require.NoError(t, err)
	require.NotEmpty(t, diag.GetId())

	// Retrieve it.
	resp, err := svc.GetDiagnosis(ctx, &pb.GetDiagnosisRequest{Id: diag.GetId()})
	require.NoError(t, err)
	assert.Equal(t, diag.GetId(), resp.GetId())
	assert.Equal(t, "host-03", resp.GetTarget())
}

// TestGetDiagnosis_NotFound returns NotFound for an unknown id.
func TestGetDiagnosis_NotFound(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil)
	_, err := svc.GetDiagnosis(context.Background(), &pb.GetDiagnosisRequest{
		Id: "no-such-report",
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

// TestGetDiagnosis_InvalidArgument returns InvalidArgument for empty id.
func TestGetDiagnosis_InvalidArgument(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil)
	_, err := svc.GetDiagnosis(context.Background(), &pb.GetDiagnosisRequest{})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestGetDiagnosis_NilRequest returns InvalidArgument.
func TestGetDiagnosis_NilRequest(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil)
	_, err := svc.GetDiagnosis(context.Background(), nil)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}