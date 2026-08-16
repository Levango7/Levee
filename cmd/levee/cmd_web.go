// cmd_web.go implements the `levee web` sub-command, which serves the LEVEE
// Web UI. In production mode it serves the embedded static assets (built from
// web/dist) and optionally proxies /api/* to a gRPC-gateway backend. In dev
// mode (--dev) it proxies all non-API traffic to a Vite dev server so that
// frontend changes hot-reload without rebuilding the Go binary.
//
// Usage:
//
//	levee web --port 8080                          # serve embedded SPA
//	levee web --port 8080 --api http://localhost:9091  # also proxy /api to gateway
//	levee web --dev --dev-server http://localhost:5173 # dev mode (Vite HMR)
//
// The command blocks until SIGINT / SIGTERM is received, then drains for up
// to 5 seconds before exiting.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/web"

	"github.com/spf13/cobra"
)

// webOpt* hold the values of the `levee web` flags. They are package-level so
// tests can reset them between runs.
var (
	webOptPort      int
	webOptAddr      string
	webOptAPI       string
	webOptDev       bool
	webOptDevServer string
)

func init() {
	RegisterCommand(newWebCmd())
}

// newWebCmd builds the `levee web` sub-command.
func newWebCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve the LEVEE Web UI",
		Long: "Serve the LEVEE Web UI (Vue 3 SPA). In production mode the " +
			"embedded static assets are served directly. In dev mode " +
			"(--dev) all non-API requests are proxied to a Vite dev server " +
			"for hot reload. Use --api to proxy /api/* to a gRPC-gateway " +
			"backend running separately.",
		Args: cobra.NoArgs,
		RunE: runWeb,
	}
	cmd.Flags().IntVar(&webOptPort, "port", 8080, "HTTP listen port")
	cmd.Flags().StringVar(&webOptAddr, "addr", "", "HTTP listen address (overrides --port, e.g. 0.0.0.0:8080)")
	cmd.Flags().StringVar(&webOptAPI, "api", "", "gRPC-gateway backend URL for /api/* proxying (empty = no proxy)")
	cmd.Flags().BoolVar(&webOptDev, "dev", false, "development mode: proxy to Vite dev server")
	cmd.Flags().StringVar(&webOptDevServer, "dev-server", "http://localhost:5173", "Vite dev server URL (dev mode only)")
	return cmd
}

// runWeb executes the `levee web` command.
func runWeb(cmd *cobra.Command, args []string) error {
	addr := resolveWebAddr()
	if addr == "" {
		return fmt.Errorf("web: invalid listen address [exit=2]")
	}

	cfg := web.ServerConfig{
		Addr:          addr,
		APIBackendURL: webOptAPI,
		DevMode:       webOptDev,
		DevServerURL:  webOptDevServer,
	}

	srv, err := web.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("web: %w [exit=2]", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig.String())
		cancel()
	}()

	log.Info("starting levee web ui",
		"addr", addr,
		"dev", webOptDev,
		"api_backend", webOptAPI,
		"dev_server", webOptDevServer,
	)
	if err := srv.Start(ctx); err != nil {
		return fmt.Errorf("web: serve: %w", err)
	}
	return nil
}

// resolveWebAddr merges --addr and --port into a single listen address. --addr
// wins when set; otherwise we build ":<port>".
func resolveWebAddr() string {
	if webOptAddr != "" {
		return webOptAddr
	}
	if webOptPort <= 0 || webOptPort > 65535 {
		return ""
	}
	return fmt.Sprintf(":%d", webOptPort)
}