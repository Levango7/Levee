// server_extra_test.go covers Server behaviours the earlier integration
// suite left open: the grpc.health.v1 probe lifecycle (SERVING while up,
// flipped to NOT_SERVING on shutdown, unreachable afterwards), the
// WithListenAddr default-address path, bare-port address normalisation,
// listen-failure reporting, the GrpcServer accessor and the GracefulStop
// deadline fallback to a hard Stop when an RPC refuses to drain.
package grpc

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// --- health check lifecycle -----------------------------------------------------

func TestServerHealthServingWhileUp(t *testing.T) {
	srv := startTestServer(t)
	conn := newInsecureClient(t, srv.Addr())
	healthClient := healthpb.NewHealthClient(conn)

	resp, err := healthClient.Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err, "overall health must be queryable without auth")
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
}

func TestServerHealthFlipsToNotServingOnShutdown(t *testing.T) {
	srv := startTestServer(t)
	conn := newInsecureClient(t, srv.Addr())
	healthClient := healthpb.NewHealthClient(conn)
	ctx := context.Background()

	before, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, before.GetStatus())

	// Perform the exact flip Stop() does, while the transport is still
	// alive, so the client observably transitions SERVING -> NOT_SERVING
	// over the wire (Stop() closes the transport immediately after
	// flipping, which makes post-hoc probing impossible).
	srv.healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	during, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, during.GetStatus())

	// After a real Stop() the listener is gone: probes must fail rather
	// than ever report SERVING again.
	require.NoError(t, srv.Stop())
	_, err = healthClient.Check(ctx, &healthpb.HealthCheckRequest{})
	require.Error(t, err)
	assert.NotEqual(t, codes.OK, status.Code(err))
}

// --- address handling --------------------------------------------------------------

func TestStartWithEmptyAddrUsesConfiguredListenAddr(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store, WithListenAddr("127.0.0.1:0"))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start("") }()

	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 5*time.Millisecond, "server did not bind using WithListenAddr")
	t.Cleanup(func() {
		_ = srv.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Errorf("server Stop did not return within 5s")
		}
	})

	assert.NotEmpty(t, srv.Addr(), "Addr must report the bound listener")
	host, _, err := net.SplitHostPort(srv.Addr())
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
}

func TestStartNormalizesBarePortAddress(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store)

	errCh := make(chan error, 1)
	// "0" has no colon; Start must fix it up to ":0" instead of failing.
	go func() { errCh <- srv.Start("0") }()

	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 5*time.Millisecond, "bare-port address was not normalized")
	t.Cleanup(func() {
		_ = srv.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Errorf("server Stop did not return within 5s")
		}
	})

	_, portStr, err := net.SplitHostPort(srv.Addr())
	require.NoError(t, err)
	assert.NotEqual(t, "0", portStr, "a kernel-assigned port must have been bound")
}

func TestStartListenFailureIsReported(t *testing.T) {
	// Occupy a port first so the server cannot bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	addr := ln.Addr().String()

	srv := NewServer(newTestStore(t))
	err = srv.Start(addr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen")
	assert.Empty(t, srv.Addr(), "failed Start must not leave a listener behind")
}

func TestGrpcServerAccessorReturnsInstance(t *testing.T) {
	srv := NewServer(newTestStore(t))
	g := srv.GrpcServer()
	require.NotNil(t, g)
	assert.Same(t, g, srv.GrpcServer(), "accessor must return the underlying instance")
}

// --- GracefulStop deadline fallback ---------------------------------------------------

// gateStore blocks ListRuns until released, simulating a long-running RPC
// that prevents a graceful drain from completing.
type gateStore struct {
	state.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateStore) ListRuns(ctx context.Context, filter state.RunFilter) ([]*state.Run, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.Store.ListRuns(ctx, filter)
}

func TestGracefulStopTimeoutFallsBackToHardStop(t *testing.T) {
	realStore := newTestStore(t)
	gate := &gateStore{
		Store:   realStore,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	changeSvc := NewChangeService(gate, nil, nil, nil)
	srv := NewServer(realStore, WithChangeService(changeSvc))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(testListenAddr) }()
	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 5*time.Millisecond, "server did not bind")
	t.Cleanup(func() { close(gate.release) })

	// Fire a PauseAll RPC which will block inside the gated ListRuns.
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewChangeServiceClient(conn)
	rpcDone := make(chan error, 1)
	go func() {
		_, err := client.PauseAll(context.Background(), &pb.PauseAllRequest{})
		rpcDone <- err
	}()

	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("PauseAll never reached the store")
	}

	// The graceful drain cannot finish while the RPC is parked, so the
	// deadline must expire and force a hard stop.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := srv.GracefulStop(ctx)
	require.NoError(t, err)
	assert.Less(t, time.Since(started), 3*time.Second,
		"timeout fallback must not wait for the blocked RPC")

	select {
	case <-rpcDone:
	case <-time.After(5 * time.Second):
		t.Error("blocked RPC never returned after hard stop")
	}
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Error("Start did not return after forced stop")
	}
}
