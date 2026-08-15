// system_service_test.go tests the SystemService gRPC handler. It uses
// a real SQLite store and a minimal config struct so the tests exercise
// the full read-only code path without external dependencies.
package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"
)

// newTestSystemService returns a SystemService backed by a fresh test
// store and a minimal config. Build info fields are set to test values.
func newTestSystemService(t *testing.T) *SystemService {
	t.Helper()
	store := newTestStore(t)
	cfg := &config.Config{
		Server: config.ServerConfig{
			DataDir:   t.TempDir(),
			LogLevel:  "info",
			LogFormat: "text",
		},
		Database: config.DatabaseConfig{
			Driver: "sqlite",
			Path:   ":memory:",
		},
	}
	svc := NewSystemService(
		store, cfg, "/tmp/test-config.yaml",
		"test-v1.0", "abc123", "2024-01-01", "go1.21",
		time.Now().Add(-time.Minute),
	)
	return svc
}

// =========================================================================
// GetVersion
// =========================================================================

func TestGetVersion_Success(t *testing.T) {
	svc := newTestSystemService(t)
	ctx := context.Background()

	resp, err := svc.GetVersion(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "test-v1.0", resp.GetVersion())
	assert.Equal(t, "abc123", resp.GetGitCommit())
	assert.Equal(t, "2024-01-01", resp.GetBuildDate())
	assert.Equal(t, "go1.21", resp.GetGoVersion())
}

func TestGetVersion_Defaults(t *testing.T) {
	store := newTestStore(t)
	svc := NewSystemService(store, nil, "", "", "", "", "", time.Time{})

	resp, err := svc.GetVersion(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	assert.Equal(t, "dev", resp.GetVersion())
	assert.Equal(t, "unknown", resp.GetGitCommit())
	assert.Equal(t, "unknown", resp.GetBuildDate())
	assert.NotEmpty(t, resp.GetGoVersion())
}

// =========================================================================
// GetStatus
// =========================================================================

func TestGetStatus_Healthy(t *testing.T) {
	svc := newTestSystemService(t)
	ctx := context.Background()

	resp, err := svc.GetStatus(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "healthy", resp.GetStatus())
	assert.Equal(t, int32(0), resp.GetActiveRuns())
	assert.Equal(t, int32(0), resp.GetPausedRuns())
	assert.Equal(t, "sqlite", resp.GetStoreType())
	assert.True(t, resp.GetUptimeSeconds() >= 0)
}

func TestGetStatus_WithRuns(t *testing.T) {
	svc := newTestSystemService(t)
	ctx := context.Background()

	// Create a running run and a paused run via the store.
	now := time.Now().UTC()
	run1 := &state.Run{
		ID: "run-active-1", WorkflowName: "wf", Status: "running",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, svc.store.CreateRun(ctx, run1))

	run2 := &state.Run{
		ID: "run-paused-1", WorkflowName: "wf", Status: "paused",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, svc.store.CreateRun(ctx, run2))

	resp, err := svc.GetStatus(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	assert.Equal(t, int32(1), resp.GetActiveRuns())
	assert.Equal(t, int32(1), resp.GetPausedRuns())
}

func TestGetStatus_NoStore(t *testing.T) {
	svc := NewSystemService(nil, nil, "", "v", "c", "d", "g", time.Now())

	resp, err := svc.GetStatus(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	assert.Equal(t, "degraded", resp.GetStatus())
	assert.Contains(t, resp.GetWarnings(), "store not configured")
}

// =========================================================================
// GetConfig
// =========================================================================

func TestGetConfig_Success(t *testing.T) {
	svc := newTestSystemService(t)
	ctx := context.Background()

	resp, err := svc.GetConfig(ctx, &pb.GetConfigRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "yaml", resp.GetFormat())
	assert.NotEmpty(t, resp.GetContent())
	assert.Equal(t, "/tmp/test-config.yaml", resp.GetSourcePath())
	// Content should contain the config fields.
	content := string(resp.GetContent())
	assert.Contains(t, content, "sqlite")
}

func TestGetConfig_NilConfig(t *testing.T) {
	store := newTestStore(t)
	svc := NewSystemService(store, nil, "", "v", "c", "d", "g", time.Now())

	_, err := svc.GetConfig(context.Background(), &pb.GetConfigRequest{})
	require.Error(t, err)
}

func TestGetConfig_RedactSecrets(t *testing.T) {
	svc := newTestSystemService(t)
	ctx := context.Background()

	resp, err := svc.GetConfig(ctx, &pb.GetConfigRequest{RedactSecrets: true})
	require.NoError(t, err)
	// The redacted content should not contain raw secret values.
	// Since our test config has no secrets, just verify it still works.
	assert.NotEmpty(t, resp.GetContent())
}

func TestGetConfig_Section(t *testing.T) {
	svc := newTestSystemService(t)
	ctx := context.Background()

	resp, err := svc.GetConfig(ctx, &pb.GetConfigRequest{Section: "server"})
	require.NoError(t, err)
	content := string(resp.GetContent())
	assert.Contains(t, content, "server:")
}

func TestGetConfig_SectionNotFound(t *testing.T) {
	svc := newTestSystemService(t)
	ctx := context.Background()

	_, err := svc.GetConfig(ctx, &pb.GetConfigRequest{Section: "nonexistent"})
	require.Error(t, err)
}

// =========================================================================
// RunDoctor
// =========================================================================

func TestRunDoctor_AllPass(t *testing.T) {
	svc := newTestSystemService(t)
	ctx := context.Background()

	resp, err := svc.RunDoctor(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "pass", resp.GetStatus())
	assert.NotEmpty(t, resp.GetChecks())
	assert.True(t, resp.GetCheckedAt() > 0)

	// Each check should have a name and status.
	for _, c := range resp.GetChecks() {
		assert.NotEmpty(t, c.GetName())
		assert.NotEmpty(t, c.GetStatus())
	}
}

func TestRunDoctor_NoStore(t *testing.T) {
	svc := NewSystemService(nil, nil, "", "v", "c", "d", "g", time.Now())

	resp, err := svc.RunDoctor(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)

	// Should fail because store and config are nil.
	assert.Equal(t, "fail", resp.GetStatus())

	// Find the store check.
	var storeCheck *pb.DoctorCheck
	for _, c := range resp.GetChecks() {
		if c.GetName() == "store" {
			storeCheck = c
		}
	}
	require.NotNil(t, storeCheck)
	assert.Equal(t, "fail", storeCheck.GetStatus())
}

func TestRunDoctor_NoConfig(t *testing.T) {
	store := newTestStore(t)
	svc := NewSystemService(store, nil, "", "v", "c", "d", "g", time.Now())

	resp, err := svc.RunDoctor(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)

	// Config check should fail.
	var configCheck *pb.DoctorCheck
	for _, c := range resp.GetChecks() {
		if c.GetName() == "config" {
			configCheck = c
		}
	}
	require.NotNil(t, configCheck)
	assert.Equal(t, "fail", configCheck.GetStatus())
}

// Ensure strings import is used.
var _ = strings.TrimSpace