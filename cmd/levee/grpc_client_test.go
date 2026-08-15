package main

// grpc_client_test.go tests the gRPC client wrapper and the dual-mode
// service factory. The remote-mode tests spin up a real in-process gRPC
// server (via internal/grpc.NewServer) on an ephemeral port and exercise
// the full client → server round trip, so they validate not just the
// adapter plumbing but also the generated stubs and wire format.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"
)

// newTestStore returns a fresh SQLite store backed by a temp file. Each test
// gets its own database so concurrent tests do not interfere.
func newTestStore(t *testing.T) state.Store {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-cli-test.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// startTestServer starts a real gRPC server on an ephemeral port backed by
// the given store, with all five real service implementations registered.
// It returns the server and its dialable address. The server is stopped
// automatically when the test ends.
func startTestServer(t *testing.T, store state.Store, token string) (*grpc.Server, string) {
	t.Helper()

	changeSvc := grpc.NewChangeService(store, nil, nil, nil)
	templateSvc := grpc.NewTemplateService(store, nil)
	targetSvc := grpc.NewTargetService(nil)
	auditSvc := grpc.NewAuditService(store)
	systemSvc := grpc.NewSystemService(
		store, &config.Config{}, "",
		"test", "test-commit", "test-build", "go-test", time.Now(),
	)

	opts := []grpc.Option{
		grpc.WithListenAddr(":0"),
		grpc.WithChangeService(changeSvc),
		grpc.WithTemplateService(templateSvc),
		grpc.WithTargetService(targetSvc),
		grpc.WithAuditService(auditSvc),
		grpc.WithSystemService(systemSvc),
	}
	if token != "" {
		opts = append(opts, grpc.WithAuthToken(token))
	}

	srv := grpc.NewServer(store, opts...)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(":0") }()

	// Wait for the listener to be bound. We poll Addr() until non-empty.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			t.Cleanup(func() { _ = srv.Stop() })
			return srv, addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("test server did not bind within 2 seconds")
	return nil, ""
}

// =========================================================================
// newGRPCClient
// =========================================================================

func TestNewGRPCClient_EmptyAddr(t *testing.T) {
	_, err := newGRPCClient("", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty server address")
}

func TestNewGRPCClient_Success(t *testing.T) {
	// NewClient is non-blocking, so we can construct a client without a
	// running server. The connection is only established on first RPC.
	c, err := newGRPCClient("localhost:9999", "")
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.NotNil(t, c.change)
	assert.NotNil(t, c.template)
	assert.NotNil(t, c.target)
	assert.NotNil(t, c.audit)
	assert.NotNil(t, c.system)
	require.NoError(t, c.close())
}

func TestNewGRPCClient_WithToken(t *testing.T) {
	c, err := newGRPCClient("localhost:9999", "secret-token")
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "secret-token", c.token)
	require.NoError(t, c.close())
}

func TestNewGRPCClient_CloseIdempotent(t *testing.T) {
	c, err := newGRPCClient("localhost:9999", "")
	require.NoError(t, err)
	require.NoError(t, c.close())
	// Second close is a no-op.
	require.NoError(t, c.close())
}

func TestNewGRPCClient_CloseNilSafe(t *testing.T) {
	var c *grpcClient
	require.NoError(t, c.close())
}

// =========================================================================
// bearerTokenCredentials
// =========================================================================

func TestBearerTokenCredentials_GetRequestMetadata(t *testing.T) {
	creds := bearerTokenCredentials{token: "abc123"}
	md, err := creds.GetRequestMetadata(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer abc123", md["authorization"])
}

func TestBearerTokenCredentials_NoTransportSecurityRequired(t *testing.T) {
	creds := bearerTokenCredentials{token: "abc123"}
	assert.False(t, creds.RequireTransportSecurity())
}

// =========================================================================
// serviceFactory — local mode
// =========================================================================

func TestServiceFactoryLocal(t *testing.T) {
	store := newTestStore(t)

	f, err := newServiceFactory(modeLocal, "", "", WithFactoryDeps(&localDeps{
		store: store,
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	assert.Equal(t, modeLocal, f.Mode())
	assert.NotNil(t, f.local)

	// All five adapters should be non-nil and report the local mode.
	cs := f.GetChangeService()
	require.NotNil(t, cs)
	ts := f.GetTemplateService()
	require.NotNil(t, ts)
	tg := f.GetTargetService()
	require.NotNil(t, tg)
	as := f.GetAuditService()
	require.NotNil(t, as)
	ss := f.GetSystemService()
	require.NotNil(t, ss)

	// Exercise a real call through the local adapter to confirm wiring.
	ctx := context.Background()
	_, err = ss.GetVersion(ctx, &emptypb.Empty{})
	// We don't assert on the result (version string is build-specific); we
	// only confirm the call does not return a wiring error.
	assert.NoError(t, err)
}

func TestServiceFactoryLocal_RequiresStore(t *testing.T) {
	// nil deps → error.
	_, err := newServiceFactory(modeLocal, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil store")

	// deps with nil store → error.
	_, err = newServiceFactory(modeLocal, "", "", WithFactoryDeps(&localDeps{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil store")
}

// =========================================================================
// serviceFactory — remote mode
// =========================================================================

func TestServiceFactoryRemote(t *testing.T) {
	store := newTestStore(t)
	_, addr := startTestServer(t, store, "")

	f, err := newServiceFactory(modeRemote, addr, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	assert.Equal(t, modeRemote, f.Mode())
	assert.NotNil(t, f.remote)

	// Wait for the connection to be ready before issuing RPCs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.remote.waitForReady(ctx))

	// Exercise a real call through the remote adapter.
	ss := f.GetSystemService()
	require.NotNil(t, ss)
	resp, err := ss.GetVersion(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	// The test server was constructed with version "test".
	assert.Equal(t, "test", resp.Version)
}

func TestServiceFactoryRemote_WithToken(t *testing.T) {
	store := newTestStore(t)
	_, addr := startTestServer(t, store, "shared-secret")

	// Correct token → calls succeed.
	f, err := newServiceFactory(modeRemote, addr, "shared-secret")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.remote.waitForReady(ctx))

	ss := f.GetSystemService()
	resp, err := ss.GetVersion(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	assert.Equal(t, "test", resp.Version)
}

func TestServiceFactoryRemote_BadTokenRejected(t *testing.T) {
	store := newTestStore(t)
	_, addr := startTestServer(t, store, "shared-secret")

	// Wrong token → calls fail with Unauthenticated.
	f, err := newServiceFactory(modeRemote, addr, "wrong-secret")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// waitForReady may succeed (connection establishes at TCP level) but
	// the RPC itself should fail with Unauthenticated.
	_ = f.remote.waitForReady(ctx)

	ss := f.GetSystemService()
	_, err = ss.GetVersion(ctx, &emptypb.Empty{})
	require.Error(t, err)
}

func TestServiceFactoryRemote_EmptyAddr(t *testing.T) {
	_, err := newServiceFactory(modeRemote, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty server address")
}

// =========================================================================
// serviceMode String()
// =========================================================================

func TestServiceModeString(t *testing.T) {
	assert.Equal(t, "local", modeLocal.String())
	assert.Equal(t, "remote", modeRemote.String())
	assert.Equal(t, "unknown", serviceMode(99).String())
}

// =========================================================================
// resolveServiceMode (root flag → serviceMode)
// =========================================================================

func TestResolveServiceMode_DefaultLocal(t *testing.T) {
	defer resetRootFlags()
	optLocal = true
	optRemote = false
	mode, err := resolveServiceMode()
	require.NoError(t, err)
	assert.Equal(t, modeLocal, mode)
}

func TestResolveServiceMode_Remote(t *testing.T) {
	defer resetRootFlags()
	optLocal = false
	optRemote = true
	mode, err := resolveServiceMode()
	require.NoError(t, err)
	assert.Equal(t, modeRemote, mode)
}

func TestResolveServiceMode_MutuallyExclusive(t *testing.T) {
	defer resetRootFlags()
	optLocal = true
	optRemote = true
	_, err := resolveServiceMode()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestResolveServiceMode_NeitherSetDefaultsLocal(t *testing.T) {
	defer resetRootFlags()
	optLocal = false
	optRemote = false
	mode, err := resolveServiceMode()
	require.NoError(t, err)
	assert.Equal(t, modeLocal, mode)
}

// =========================================================================
// loadTLSConfig
// =========================================================================

func TestLoadTLSConfig_BothEmpty(t *testing.T) {
	cfg, err := loadTLSConfig("", "")
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadTLSConfig_OnlyCert(t *testing.T) {
	_, err := loadTLSConfig("cert.pem", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both be supplied")
}

func TestLoadTLSConfig_OnlyKey(t *testing.T) {
	_, err := loadTLSConfig("", "key.pem")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both be supplied")
}

func TestLoadTLSConfig_NonexistentFiles(t *testing.T) {
	_, err := loadTLSConfig("nonexistent-cert.pem", "nonexistent-key.pem")
	require.Error(t, err)
}

// =========================================================================
// serve sub-command registration
// =========================================================================

func TestServeSubcommandRegistered(t *testing.T) {
	defer resetRootFlags()
	require.NotNil(t, findSub("serve"), "serve subcommand should be registered")
}

func TestServeCmdFlags(t *testing.T) {
	defer resetRootFlags()
	sub := findSub("serve")
	require.NotNil(t, sub)
	for _, name := range []string{"addr", "tls-cert", "tls-key", "token"} {
		f := sub.Flags().Lookup(name)
		require.NotNil(t, f, "missing serve flag %q", name)
	}
}

// =========================================================================
// adapter type sanity — compile-time guarantees that local and remote
// adapters satisfy their interfaces.
// =========================================================================

func TestAdapterInterfacesCompile(t *testing.T) {
	var _ changeServiceAdapter = (*localChangeAdapter)(nil)
	var _ changeServiceAdapter = (*remoteChangeAdapter)(nil)
	var _ templateServiceAdapter = (*localTemplateAdapter)(nil)
	var _ templateServiceAdapter = (*remoteTemplateAdapter)(nil)
	var _ targetServiceAdapter = (*localTargetAdapter)(nil)
	var _ targetServiceAdapter = (*remoteTargetAdapter)(nil)
	var _ auditServiceAdapter = (*localAuditAdapter)(nil)
	var _ auditServiceAdapter = (*remoteAuditAdapter)(nil)
	var _ systemServiceAdapter = (*localSystemAdapter)(nil)
	var _ systemServiceAdapter = (*remoteSystemAdapter)(nil)
}

// silence unused-import warnings if a future refactor drops a reference.
var _ = pb.ChangeServiceClient(nil)
