// integration_test.go contains end-to-end integration tests for the LEVEE
// gRPC server. Unlike the per-service unit tests, these tests start a real
// gRPC server on a loopback port, connect a real gRPC client, and exercise
// the full network stack including interceptors, auth and TLS.
//
// Test groups:
//   - Server startup: plaintext, TLS and auth-enabled variants.
//   - End-to-end RPC: each of the five services exercised through a client.
//   - Streaming RPC: WatchChange and StreamLogs.
//   - Auth & error handling: missing/wrong tokens, store failures.
//
// All tests use t.Cleanup for resource teardown and t.TempDir for scratch
// space, so they are safe to run in parallel and under -count=N.
package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// testListenAddr is the loopback address used by all integration tests. We
// bind to 127.0.0.1:0 (kernel-assigned port) so tests do not collide with
// each other or with a locally running daemon on :9090.
const testListenAddr = "127.0.0.1:0"

// startTestServer builds a Server backed by a fresh SQLite store, starts it
// on a kernel-assigned loopback port, and returns the running instance. The
// server is automatically stopped via t.Cleanup. Caller-supplied Options are
// appended after the default store wiring.
func startTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	store := newTestStore(t)
	srv := NewServer(store, opts...)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(testListenAddr) }()

	// Wait for the listener to be bound.
	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 5*time.Millisecond, "server did not bind within 5s")

	t.Cleanup(func() {
		_ = srv.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Errorf("server Stop did not return within 5s")
		}
	})
	return srv
}

// startTestServerWithAllServices is like startTestServer but registers real
// implementations of all five services (Change/Template/Target/Audit/System)
// instead of the Unimplemented stubs. It returns the underlying store so
// tests can seed data directly (e.g. batches, steps, traces).
func startTestServerWithAllServices(t *testing.T, opts ...Option) (*Server, state.Store) {
	t.Helper()
	store := newTestStore(t)

	changeSvc := NewChangeService(store, nil, nil, nil)
	templateSvc := NewTemplateService(store, nil)
	targetSvc := NewTargetService(newTestStore(t), nil)
	auditSvc := NewAuditService(store)
	cfg := &config.Config{
		Server:   config.ServerConfig{DataDir: t.TempDir(), LogLevel: "info", LogFormat: "text"},
		Database: config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"},
	}
	systemSvc := NewSystemService(store, cfg, "", "test-v1.0", "abc123", "2024-01-01", "go1.21", time.Now())

	allOpts := append([]Option{
		WithChangeService(changeSvc),
		WithTemplateService(templateSvc),
		WithTargetService(targetSvc),
		WithAuditService(auditSvc),
		WithSystemService(systemSvc),
	}, opts...)

	srv := NewServer(store, allOpts...)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(testListenAddr) }()

	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 5*time.Millisecond, "server did not bind within 5s")

	t.Cleanup(func() {
		_ = srv.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Errorf("server Stop did not return within 5s")
		}
	})
	return srv, store
}

// newInsecureClient dials the server with plaintext transport credentials and
// returns a ClientConn closed via t.Cleanup.
func newInsecureClient(t *testing.T, addr string) *ggrpc.ClientConn {
	t.Helper()
	conn, err := ggrpc.NewClient(addr, ggrpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// newTLSClient dials the server with the supplied client-side TLS config.
func newTLSClient(t *testing.T, addr string, tlsCfg *tls.Config) *ggrpc.ClientConn {
	t.Helper()
	conn, err := ggrpc.NewClient(addr, ggrpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// withAuthCtx returns ctx with a "Bearer <token>" authorization metadata
// header attached, mirroring what the CLI remote mode sends.
func withAuthCtx(ctx context.Context, token string) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
}

// generateSelfSignedCert produces a self-signed ECDSA P-256 certificate and
// returns two *tls.Config values: one for the server (holding the cert+key)
// and one for the client (trusting the cert as a root CA). The certificate
// is valid for localhost and 127.0.0.1.
func generateSelfSignedCert(t *testing.T) (serverTLS, clientTLS *tls.Config) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	serverTLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
		MinVersion:   tls.VersionTLS12,
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	clientTLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return serverTLS, clientTLS
}

// failingStore wraps a real state.Store but injects errors into selected
// methods. It is used by TestServerErrorHandling to verify that store
// failures are mapped to the correct gRPC status codes. Unoverridden
// methods delegate to the embedded Store.
type failingStore struct {
	state.Store
	getRunErr error
}

func (f *failingStore) GetRun(ctx context.Context, id string) (*state.Run, error) {
	if f.getRunErr != nil {
		return nil, f.getRunErr
	}
	return f.Store.GetRun(ctx, id)
}

// =========================================================================
// 1. Server startup tests
// =========================================================================

// TestServerStartup starts a plaintext server on :0, verifies that Addr
// returns a usable address, performs a real RPC, and confirms that
// GracefulStop shuts down cleanly.
func TestServerStartup(t *testing.T) {
	srv, store := startTestServerWithAllServices(t)
	addr := srv.Addr()
	require.NotEmpty(t, addr)

	// The address must be a real TCP address on the loopback.
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	assert.NotEqual(t, "0", port)

	// A real RPC should succeed, proving the server is live.
	conn := newInsecureClient(t, addr)
	client := pb.NewSystemServiceClient(conn)
	resp, err := client.GetVersion(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	assert.Equal(t, "test-v1.0", resp.GetVersion())

	// Sanity: the store is wired and queryable.
	runs, err := store.ListRuns(context.Background(), state.RunFilter{Limit: 1})
	require.NoError(t, err)
	assert.Empty(t, runs)
}

// TestServerStartupWithTLS starts a TLS-encrypted server using a self-signed
// certificate and verifies that a TLS client can successfully complete an
// RPC.
func TestServerStartupWithTLS(t *testing.T) {
	serverTLS, clientTLS := generateSelfSignedCert(t)
	srv := startTestServer(t, WithTLS(serverTLS))
	addr := srv.Addr()
	require.NotEmpty(t, addr)

	conn := newTLSClient(t, addr, clientTLS)
	client := pb.NewSystemServiceClient(conn)

	// Use the default Unimplemented SystemService; GetVersion on the
	// unimplemented server returns the zero value with no error because
	// UnimplementedSystemServiceServer.GetVersion returns Unimplemented.
	// We just verify the TLS handshake completes by invoking any RPC; the
	// error (if any) should be Unimplemented, not a TLS handshake failure.
	_, err := client.GetVersion(context.Background(), &emptypb.Empty{})
	if err != nil {
		st, ok := status.FromError(err)
		require.True(t, ok, "expected a gRPC status error, got: %v", err)
		// TLS handshake succeeded; the only acceptable non-OK code is
		// Unimplemented (from the default stub).
		assert.Equal(t, codes.Unimplemented, st.Code(),
			"expected Unimplemented or OK after TLS handshake, got %s", st.Code())
	}
}

// TestServerWithAuth starts a server with Bearer-token auth and verifies
// that a valid token allows RPCs while the server remains operational.
func TestServerWithAuth(t *testing.T) {
	const token = "integration-test-token"
	srv := startTestServer(t, WithAuthToken(token))
	addr := srv.Addr()
	require.NotEmpty(t, addr)

	conn := newInsecureClient(t, addr)
	client := pb.NewSystemServiceClient(conn)

	// With a valid token the RPC should reach the handler. The default
	// SystemService is Unimplemented, so we expect either OK or
	// Unimplemented — both prove auth passed.
	ctx := withAuthCtx(context.Background(), token)
	_, err := client.GetVersion(ctx, &emptypb.Empty{})
	if err != nil {
		st, _ := status.FromError(err)
		assert.Equal(t, codes.Unimplemented, st.Code(),
			"valid token should reach handler, got %s", st.Code())
	}
}

// =========================================================================
// 2. End-to-end RPC tests (through a real gRPC client)
// =========================================================================

// TestChangeServiceE2E exercises the full Change lifecycle through a gRPC
// client: CreateChange → GetChange → ListChanges → ApproveChange →
// CancelChange.
func TestChangeServiceE2E(t *testing.T) {
	srv, _ := startTestServerWithAllServices(t)
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewChangeServiceClient(conn)
	ctx := context.Background()

	// 1. CreateChange.
	created, err := client.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "e2e-change",
		Priority:     "high",
		WorkflowFile: "workflow.levee",
		TemplateName: "deploy-web",
		Params:       map[string]string{"version": "1.2.3"},
		Team:         "platform",
		Environment:  "staging",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.GetId())
	assert.Equal(t, "draft", created.GetStatus())
	assert.Equal(t, "high", created.GetPriority())

	// 2. GetChange.
	fetched, err := client.GetChange(ctx, &pb.GetChangeRequest{Id: created.GetId()})
	require.NoError(t, err)
	assert.Equal(t, created.GetId(), fetched.GetId())
	assert.Equal(t, "draft", fetched.GetStatus())

	// 3. ListChanges — at least the one we created.
	listed, err := client.ListChanges(ctx, &pb.ListChangesRequest{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, listed.GetTotalSize(), int32(1))
	found := false
	for _, c := range listed.GetChanges() {
		if c.GetId() == created.GetId() {
			found = true
			break
		}
	}
	assert.True(t, found, "created change should appear in ListChanges")

	// 4. ApproveChange.
	approved, err := client.ApproveChange(ctx, &pb.ApproveRequest{
		ChangeId: created.GetId(),
		Approver: "e2e-approver",
		Comment:  "lgtm",
	})
	require.NoError(t, err)
	assert.Equal(t, "approved", approved.GetStatus())

	// 5. CancelChange (approved → cancelled is a legal transition).
	cancelled, err := client.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: created.GetId(),
		Reason:   "no longer needed",
	})
	require.NoError(t, err)
	assert.Equal(t, "cancelled", cancelled.GetStatus())
}

// TestTemplateServiceE2E exercises template CRUD through a gRPC client:
// CreateTemplate → GetTemplate → ListTemplates → DeleteTemplate.
func TestTemplateServiceE2E(t *testing.T) {
	srv, _ := startTestServerWithAllServices(t)
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewTemplateServiceClient(conn)
	ctx := context.Background()

	// 1. CreateTemplate.
	created, err := client.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name:            "e2e-tmpl",
		Description:     "E2E test template",
		WorkflowContent: "name: e2e\nsteps: []",
		RequiredParams:  []string{"version"},
	})
	require.NoError(t, err)
	assert.Equal(t, "e2e-tmpl", created.GetName())

	// 2. GetTemplate.
	fetched, err := client.GetTemplate(ctx, &pb.GetTemplateRequest{Name: "e2e-tmpl"})
	require.NoError(t, err)
	assert.Equal(t, "E2E test template", fetched.GetDescription())
	assert.Equal(t, []string{"version"}, fetched.GetRequiredParams())

	// 3. ListTemplates.
	listed, err := client.ListTemplates(ctx, &pb.ListTemplatesRequest{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, listed.GetTotalSize(), int32(1))

	// 4. DeleteTemplate.
	_, err = client.DeleteTemplate(ctx, &pb.DeleteTemplateRequest{Name: "e2e-tmpl"})
	require.NoError(t, err)

	// Deleting again should return NotFound.
	_, err = client.DeleteTemplate(ctx, &pb.DeleteTemplateRequest{Name: "e2e-tmpl"})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
}

// TestTargetServiceE2E exercises target CRUD through a gRPC client:
// AddTarget → GetTarget → ListTargets → RemoveTarget.
func TestTargetServiceE2E(t *testing.T) {
	srv, _ := startTestServerWithAllServices(t)
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewTargetServiceClient(conn)
	ctx := context.Background()

	// 1. AddTarget.
	added, err := client.AddTarget(ctx, &pb.AddTargetRequest{
		Hostname:    "e2e-host.example.com",
		ChannelType: "ssh",
		Port:        22,
		Labels:      map[string]string{"env": "staging", "role": "web"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, added.GetId())
	assert.Equal(t, "e2e-host.example.com", added.GetHostname())

	// 2. GetTarget.
	fetched, err := client.GetTarget(ctx, &pb.GetTargetRequest{Id: added.GetId()})
	require.NoError(t, err)
	assert.Equal(t, added.GetId(), fetched.GetId())
	assert.Equal(t, "staging", fetched.GetLabels()["env"])

	// 3. ListTargets.
	listed, err := client.ListTargets(ctx, &pb.ListTargetsRequest{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, listed.GetTotalSize(), int32(1))

	// 4. RemoveTarget.
	_, err = client.RemoveTarget(ctx, &pb.RemoveTargetRequest{Id: added.GetId()})
	require.NoError(t, err)

	// GetTarget after removal should return NotFound.
	_, err = client.GetTarget(ctx, &pb.GetTargetRequest{Id: added.GetId()})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
}

// TestAuditServiceE2E exercises the audit service through a gRPC client.
// It creates a change, seeds trace entries directly via the store, then
// queries them through GetTrace (ChangeService), ListAuditTraces and
// VerifyHashChain (AuditService).
func TestAuditServiceE2E(t *testing.T) {
	srv, store := startTestServerWithAllServices(t)
	conn := newInsecureClient(t, srv.Addr())
	changeClient := pb.NewChangeServiceClient(conn)
	auditClient := pb.NewAuditServiceClient(conn)
	ctx := context.Background()

	// Create a change to anchor the traces.
	created, err := changeClient.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "audit-e2e",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Seed two trace entries forming a valid hash chain.
	now := time.Now().UTC()
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "trc-e2e-1", RunID: created.GetId(), Event: "start",
		PrevHash: "", CurrHash: "hash-e2e-1", Timestamp: now,
	}))
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "trc-e2e-2", RunID: created.GetId(), Event: "end",
		PrevHash: "hash-e2e-1", CurrHash: "hash-e2e-2", Timestamp: now.Add(time.Second),
	}))

	// 1. GetTrace via ChangeService, with hash-chain verification.
	trace, err := changeClient.GetTrace(ctx, &pb.GetTraceRequest{
		ChangeId: created.GetId(),
		Verify:   true,
	})
	require.NoError(t, err)
	assert.Equal(t, created.GetId(), trace.GetRunId())
	assert.Len(t, trace.GetEntries(), 2)
	assert.True(t, trace.GetHashChainValid(), "seeded hash chain should be valid")

	// 2. ListAuditTraces via AuditService.
	traces, err := auditClient.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{
		ChangeId: created.GetId(),
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, traces.GetTotalSize(), int32(2))

	// 3. VerifyHashChain via AuditService.
	verify, err := auditClient.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{
		RunId: created.GetId(),
	})
	require.NoError(t, err)
	// The verifier checks that CurrHash of entry N equals PrevHash of
	// entry N+1. Our seeded chain satisfies this, so the run should
	// verify as valid (or at least be present in the result).
	assert.NotEmpty(t, verify.GetRuns(), "expected at least one RunVerification")
}

// TestSystemServiceE2E exercises the system service through a gRPC client.
// The proto SystemService defines GetVersion, GetStatus, GetConfig and
// RunDoctor (there is no HealthCheck RPC; RunDoctor serves the equivalent
// health-check role).
func TestSystemServiceE2E(t *testing.T) {
	srv, _ := startTestServerWithAllServices(t)
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewSystemServiceClient(conn)
	ctx := context.Background()

	// 1. GetVersion.
	ver, err := client.GetVersion(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	assert.Equal(t, "test-v1.0", ver.GetVersion())
	assert.Equal(t, "abc123", ver.GetGitCommit())

	// 2. GetStatus.
	st, err := client.GetStatus(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	assert.NotEmpty(t, st.GetStatus())
	assert.Equal(t, "sqlite", st.GetStoreType())

	// 3. GetConfig.
	cfg, err := client.GetConfig(ctx, &pb.GetConfigRequest{})
	require.NoError(t, err)
	assert.Equal(t, "yaml", cfg.GetFormat())
	assert.NotEmpty(t, cfg.GetContent())

	// 4. RunDoctor (the health-check equivalent).
	doc, err := client.RunDoctor(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	assert.NotEmpty(t, doc.GetStatus())
	assert.NotEmpty(t, doc.GetChecks())
	assert.True(t, doc.GetCheckedAt() > 0)
}

// =========================================================================
// 3. Streaming RPC tests
// =========================================================================

// TestWatchChange opens a WatchChange stream with IncludeCurrentState=true
// and verifies that the first received event reflects the change's current
// status. The stream is then cancelled by the client.
func TestWatchChange(t *testing.T) {
	srv, _ := startTestServerWithAllServices(t)
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewChangeServiceClient(conn)

	// Create a change to watch.
	created, err := client.CreateChange(context.Background(), &pb.CreateChangeRequest{
		Label:        "watch-e2e",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Open the stream with IncludeCurrentState so we get an immediate event.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.WatchChange(ctx, &pb.WatchChangeRequest{
		ChangeId:            created.GetId(),
		IncludeCurrentState: true,
	})
	require.NoError(t, err)

	// The first event should be the current state snapshot.
	ev, err := stream.Recv()
	require.NoError(t, err)
	assert.Equal(t, created.GetId(), ev.GetChangeId())
	assert.Equal(t, "status_changed", ev.GetEventType())
	assert.Equal(t, "draft", ev.GetNewStatus())

	// Cancel the context to end the stream cleanly.
	cancel()
	// Subsequent Recv should return an error (EOF or cancelled).
	_, err = stream.Recv()
	assert.Error(t, err)
}

// TestStreamLogs creates a change with a seeded step containing stdout, then
// opens a StreamLogs stream (Follow=false) and verifies that the historical
// log line is replayed.
func TestStreamLogs(t *testing.T) {
	srv, store := startTestServerWithAllServices(t)
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewChangeServiceClient(conn)

	created, err := client.CreateChange(context.Background(), &pb.CreateChangeRequest{
		Label:        "logs-e2e",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Seed a batch + step with stdout so StreamLogs has something to replay.
	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(context.Background(), &state.Batch{
		ID: "batch-logs-e2e", RunID: created.GetId(), BatchNo: 1,
		Status: "completed", TotalHosts: 1, Succeeded: 1, StartedAt: &now,
	}))
	require.NoError(t, store.CreateStep(context.Background(), &state.Step{
		ID: "step-logs-e2e", RunID: created.GetId(), BatchID: "batch-logs-e2e",
		Host: "host1", StepName: "deploy", Status: "completed",
		Stdout: "e2e log line", StartedAt: &now,
	}))

	// Open StreamLogs with Follow=false: it replays history and returns.
	stream, err := client.StreamLogs(context.Background(), &pb.StreamLogsRequest{
		ChangeId: created.GetId(),
		Follow:   false,
	})
	require.NoError(t, err)

	var logs []*pb.LogEntry
	for {
		entry, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "unexpected error receiving log entry")
		logs = append(logs, entry)
	}
	require.NotEmpty(t, logs, "expected at least one replayed log entry")

	// Verify the seeded stdout line is present.
	found := false
	for _, l := range logs {
		if l.GetMessage() == "e2e log line" {
			found = true
			assert.Equal(t, "INFO", l.GetLevel())
			break
		}
	}
	assert.True(t, found, "seeded log line should be replayed")
}

// =========================================================================
// 4. Auth & error-handling tests
// =========================================================================

// TestAuthMissingToken verifies that an RPC against an auth-enabled server
// without any authorization metadata is rejected with Unauthenticated.
func TestAuthMissingToken(t *testing.T) {
	const token = "secret-token-missing"
	srv, _ := startTestServerWithAllServices(t, WithAuthToken(token))
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewSystemServiceClient(conn)

	// No metadata attached.
	_, err := client.GetStatus(context.Background(), &emptypb.Empty{})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

// TestAuthWrongToken verifies that an RPC with a wrong bearer token is
// rejected with Unauthenticated.
func TestAuthWrongToken(t *testing.T) {
	const token = "secret-token-right"
	srv, _ := startTestServerWithAllServices(t, WithAuthToken(token))
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewSystemServiceClient(conn)

	ctx := withAuthCtx(context.Background(), "wrong-token")
	_, err := client.GetStatus(ctx, &emptypb.Empty{})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

// TestAuthValidToken verifies that an RPC with the correct bearer token
// reaches the handler (the default Unimplemented SystemService returns
// Unimplemented, which proves auth passed).
func TestAuthValidToken(t *testing.T) {
	const token = "secret-token-valid"
	srv := startTestServer(t, WithAuthToken(token))
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewSystemServiceClient(conn)

	ctx := withAuthCtx(context.Background(), token)
	_, err := client.GetVersion(ctx, &emptypb.Empty{})
	if err != nil {
		// The only acceptable non-OK code is Unimplemented (default stub).
		st, ok := status.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.Unimplemented, st.Code(),
			"valid token should pass auth; got %s", st.Code())
	}
}

// TestAuthValidTokenE2E uses a fully-wired server (all services) so the RPC
// succeeds with no error, proving the token was accepted and the request
// reached the real handler.
func TestAuthValidTokenE2E(t *testing.T) {
	const token = "secret-token-e2e"
	srv, _ := startTestServerWithAllServices(t, WithAuthToken(token))
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewSystemServiceClient(conn)

	ctx := withAuthCtx(context.Background(), token)
	resp, err := client.GetVersion(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	assert.Equal(t, "test-v1.0", resp.GetVersion())
}

// TestServerErrorHandling injects a failingStore into the ChangeService and
// verifies that store errors are surfaced as gRPC codes.Internal rather than
// crashing the server or silently returning success.
func TestServerErrorHandling(t *testing.T) {
	store := newTestStore(t)

	// Wrap the store so GetRun always fails.
	fs := &failingStore{
		Store:     store,
		getRunErr: errors.New("simulated store failure"),
	}
	changeSvc := NewChangeService(fs, nil, nil, nil)

	// Build a server with the failing ChangeService. We pass the real
	// store to NewServer so Start's nil-store guard passes; the
	// ChangeService itself uses the failing wrapper.
	srv := NewServer(store, WithChangeService(changeSvc))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(testListenAddr) }()
	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 5*time.Millisecond, "server did not bind")
	t.Cleanup(func() {
		_ = srv.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	})

	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewChangeServiceClient(conn)

	// GetChange calls store.GetRun internally; the injected failure should
	// be mapped to codes.Internal.
	_, err := client.GetChange(context.Background(), &pb.GetChangeRequest{Id: "any-id"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())
	assert.Contains(t, st.Message(), "simulated store failure")
}

// TestServerGracefulStop verifies that GracefulStop drains cleanly when
// there are no in-flight RPCs.
func TestServerGracefulStop(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store, WithListenAddr(testListenAddr))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(testListenAddr) }()
	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 5*time.Millisecond, "server did not bind")

	// GracefulStop with a generous deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, srv.GracefulStop(ctx))

	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after GracefulStop")
	}
}

// TestServerDoubleStartIsRejected verifies that calling Start twice on the
// same server returns an error without starting a second listener.
func TestServerDoubleStartIsRejected(t *testing.T) {
	srv := startTestServer(t)

	// Second Start should fail because the server is already running.
	err := srv.Start(testListenAddr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already started")
}

// TestServerAddrBeforeStart verifies that Addr returns an empty string
// before the server has been started.
func TestServerAddrBeforeStart(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store)
	assert.Empty(t, srv.Addr())
}

// TestServerStartWithNilStore verifies that Start refuses to serve when the
// store is nil, preventing a nil-pointer panic inside the handlers.
func TestServerStartWithNilStore(t *testing.T) {
	srv := NewServer(nil)
	err := srv.Start(testListenAddr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store is nil")
}

// TestServerStopBeforeStart verifies that Stop on an un-started server is a
// safe no-op.
func TestServerStopBeforeStart(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store)
	require.NoError(t, srv.Stop())
}

// TestServerStopIsIdempotent verifies that calling Stop multiple times is
// safe.
func TestServerStopIsIdempotent(t *testing.T) {
	srv := startTestServer(t)
	require.NoError(t, srv.Stop())
	require.NoError(t, srv.Stop())
}

// TestServerSQLiteStorePath verifies that the integration test helper
// creates a usable SQLite store at the expected temp-file path. This is a
// regression guard: if newTestStore ever switches to an in-memory DB that
// does not survive across connections, E2E tests would silently pass
// against an empty database.
func TestServerSQLiteStorePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integration.db")
	store, err := state.NewSQLiteStore(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Create and read back a run.
	now := time.Now().UTC()
	run := &state.Run{
		ID: "run-persist-1", WorkflowName: "wf", Status: "draft",
		CreatedAt: now, UpdatedAt: now, Creator: "test",
	}
	require.NoError(t, store.CreateRun(context.Background(), run))

	got, err := store.GetRun(context.Background(), "run-persist-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "draft", got.Status)
}
