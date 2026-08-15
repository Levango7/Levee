// cmd_serve.go implements the `levee serve` sub-command, which runs the
// LEVEE gRPC server in the current process. It is the deployment entry point
// for the cluster形态: a long-lived daemon that exposes all five protobuf
// services (Change, Template, Target, Audit, System) over a single gRPC
// listener.
//
// Usage:
//
//	levee serve [--addr :9090] [--tls-cert cert.pem --tls-key key.pem] [--token <bearer>]
//
// The server reuses the in-process service implementations from internal/grpc,
// backed by the same SQLite store the CLI uses in local mode. This keeps the
// daemon and the CLI a single binary: `levee serve` is just `levee` with the
// gRPC listener attached.
//
// Shutdown is graceful: on SIGINT / SIGTERM the server waits for in-flight
// RPCs to complete (up to a 30s deadline) before stopping, so that long-running
// ApplyChange calls are not cancelled mid-flight.

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"

	"github.com/spf13/cobra"
)

// serveOptAddr / serveOptTLSCert / serveOptTLSKey hold the values of the
// `levee serve` flags. They are package-level so tests can reset them.
var (
	serveOptAddr    string
	serveOptTLSCert string
	serveOptTLSKey  string
	serveOptToken   string
)

// serveGracefulShutdownTimeout is the deadline the server waits for in-flight
// RPCs to drain before forcing a hard stop.
const serveGracefulShutdownTimeout = 30 * time.Second

func init() {
	RegisterCommand(newServeCmd())
}

// newServeCmd builds the `levee serve` sub-command.
func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the LEVEE gRPC server",
		Long: "Run the LEVEE gRPC server in the current process. Exposes all " +
			"five services (Change, Template, Target, Audit, System) over a " +
			"single gRPC listener. Use --tls-cert / --tls-key to enable TLS; " +
			"omit them for plaintext (development or sidecar TLS). Use " +
			"--token to require Bearer authentication.",
		Args: cobra.NoArgs,
		RunE: runServe,
	}
	cmd.Flags().StringVar(&serveOptAddr, "addr", grpc.DefaultListenAddr, "Listen address")
	cmd.Flags().StringVar(&serveOptTLSCert, "tls-cert", "", "TLS certificate path (optional)")
	cmd.Flags().StringVar(&serveOptTLSKey, "tls-key", "", "TLS key path (optional)")
	cmd.Flags().StringVar(&serveOptToken, "token", "", "Bearer token required from clients (empty = no auth)")
	return cmd
}

// runServe executes the `levee serve` command.
func runServe(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// 1. Load configuration and open the store.
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	store, err := state.NewSQLiteStore(ctx, cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Build the service implementations. We reuse the in-process
	//    implementations so the daemon and CLI share one code path.
	changeSvc := grpc.NewChangeService(store, nil, nil, nil)
	templateSvc := grpc.NewTemplateService(store, nil)
	targetSvc := grpc.NewTargetService(nil)
	auditSvc := grpc.NewAuditService(store)
	systemSvc := grpc.NewSystemService(
		store, cfg, optConfigPath,
		version, commitHash, buildTime, goVersion, time.Now(),
	)

	// 3. Build server options.
	serverOpts := []grpc.Option{
		grpc.WithListenAddr(serveOptAddr),
		grpc.WithChangeService(changeSvc),
		grpc.WithTemplateService(templateSvc),
		grpc.WithTargetService(targetSvc),
		grpc.WithAuditService(auditSvc),
		grpc.WithSystemService(systemSvc),
	}
	if serveOptToken != "" {
		serverOpts = append(serverOpts, grpc.WithAuthToken(serveOptToken))
	}
	tlsCfg, err := loadTLSConfig(serveOptTLSCert, serveOptTLSKey)
	if err != nil {
		return fmt.Errorf("load tls: %w", err)
	}
	if tlsCfg != nil {
		serverOpts = append(serverOpts, grpc.WithTLS(tlsCfg))
	}

	// 4. Construct and start the server.
	srv := grpc.NewServer(store, serverOpts...)

	// Use the configured address (fall back to listen addr option).
	addr := serveOptAddr
	if addr == "" {
		addr = grpc.DefaultListenAddr
	}

	// 5. Wire signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), serveGracefulShutdownTimeout)
		defer cancel()
		if err := srv.GracefulStop(shutdownCtx); err != nil {
			log.Warn("graceful stop failed", "error", err)
		}
	}()

	// 6. Block until the server stops.
	log.Info("starting levee gRPC server", "addr", addr, "tls", tlsCfg != nil, "auth", serveOptToken != "")
	if err := srv.Start(addr); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// loadTLSConfig reads the cert/key pair and returns a *tls.Config suitable for
// grpc.WithTLS. When both paths are empty it returns (nil, nil) to keep the
// server in plaintext mode. Supplying only one of the two is an error.
func loadTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("tls-cert and tls-key must both be supplied together")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load x509 key pair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// compile-time guard: ensure the pb package is referenced so that future
// service additions surface as a build error here rather than silently
// producing a server that only serves a subset of the API.
var _ pb.ChangeServiceServer = (*grpc.ChangeService)(nil)
