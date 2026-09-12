// cmd_serve.go implements the `levee serve` sub-command, which runs the
// LEVEE gRPC server in the current process. It is the deployment entry point
// for the cluster形态: a long-lived daemon that exposes all five protobuf
// services (Change, Template, Target, Audit, System) over a single gRPC
// listener.
//
// Usage:
//
//	levee serve [--addr :9090] [--tls-cert cert.pem --tls-key key.pem] [--token <bearer>]
//	           [--cluster --pg-dsn <postgres-dsn> --node-id <id> --node-addr <addr>]
//
// The server reuses the in-process service implementations from internal/grpc,
// backed by the same SQLite store the CLI uses in local mode. This keeps the
// daemon and the CLI a single binary: `levee serve` is just `levee` with the
// gRPC listener attached.
//
// In cluster mode (--cluster --pg-dsn ...) the store is backed by PostgreSQL
// (state.PGStore) and a ClusterManager coordinates node membership, heartbeats
// and distributed locks across the cluster. Without --cluster the server
// stays in single-node SQLite mode (the original behaviour).
//
// Shutdown is graceful: on SIGINT / SIGTERM the server waits for in-flight
// RPCs to complete (up to a 30s deadline) before stopping, so that long-running
// ApplyChange calls are not cancelled mid-flight.

package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/auth"
	"github.com/nexus/levee/internal/channel"
	sshchannel "github.com/nexus/levee/internal/channel/ssh"
	"github.com/nexus/levee/internal/cluster"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/credential"
	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/dispatch"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/metrics"
	"github.com/nexus/levee/internal/push"
	"github.com/nexus/levee/internal/recommend"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/takeover"
	"github.com/nexus/levee/internal/tracing"
	"github.com/nexus/levee/internal/wiring"

	"github.com/spf13/cobra"
)

// serveOptAddr / serveOptTLSCert / serveOptTLSKey hold the values of the
// `levee serve` flags. They are package-level so tests can reset them.
var (
	serveOptAddr    string
	serveOptTLSCert string
	serveOptTLSKey  string
	serveOptToken   string
	// serveOptHTTPAddr is the REST gateway listen address, separate from
	// the gRPC --addr so both listeners can run side by side.
	serveOptHTTPAddr string
	// serveOptInsecure is the explicit opt-out for the startup safety
	// checks: empty --token and wildcard CORS. Development convenience
	// only; production deployments must not set it.
	serveOptInsecure bool
	// serveOptCORSOrigins lists allowed CORS origins for the REST gateway.
	// Empty means no cross-origin access unless --insecure is set.
	serveOptCORSOrigins []string
	// serveOptRateLimit / serveOptRateBurst configure the REST gateway's
	// global token bucket. A negative --rate-limit disables limiting.
	serveOptRateLimit float64
	serveOptRateBurst int

	// Cluster-mode flags. When serveOptCluster is false the server runs in
	// single-node SQLite mode (the default). When true the server requires a
	// PostgreSQL DSN and joins the cluster as the node identified by
	// serveOptNodeID at serveOptNodeAddr.
	serveOptCluster  bool
	serveOptPGDSN    string
	serveOptNodeID   string
	serveOptNodeAddr string
	serveOptNodeRole string
	// serveOptClusterTakeoverInterval is the failover-takeover sweep
	// period (cluster mode). <= 0 disables the LOOP only — execution
	// fencing (leases) stays active whenever the engine is enabled.
	serveOptClusterTakeoverInterval time.Duration
	// serveOptClusterExecLeaseTTL bounds an execution lease. Renewals run
	// at TTL/3; the TTL bounds post-crash detection latency, not
	// execution duration.
	serveOptClusterExecLeaseTTL time.Duration
	// serveOptClusterDispatchInterval is the cross-node dispatch sweep period
	// (cluster mode). <= 0 disables the dispatch loop; takeover and fencing
	// are unaffected.
	serveOptClusterDispatchInterval time.Duration
	// serveOptClusterDispatchWorkerCapacity bounds how many runs a single
	// worker executes concurrently before the dispatcher treats it saturated.
	serveOptClusterDispatchWorkerCapacity int

	// serveOptAuthTokens holds repeatable --auth-token name=secret pairs that
	// map each named bearer token to the subject it authenticates as.
	serveOptAuthTokens []string
	// serveOptMetricsPublic opts out of authenticating the /metrics route.
	serveOptMetricsPublic bool
	// serveOptEngineEnabled wires the execution engine (plan generation +
	// apply execution) into the ChangeService. Off by default: a deployment
	// stays status-only unless the operator explicitly opts in.
	serveOptEngineEnabled bool
	// serveOptEngineMaxParallel caps concurrently executing runs inside
	// this process; applies beyond the cap fast-fail.
	serveOptEngineMaxParallel int
	// serveOptEngineGatePrometheus is the Prometheus HTTP base URL consumed
	// by declared slo verification gates (empty = slo gates fail closed).
	serveOptEngineGatePrometheus string
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
			"--token to require Bearer authentication.\n\n" +
			"Pass --cluster --pg-dsn <dsn> to enable cluster mode: the store " +
			"moves to PostgreSQL and a ClusterManager coordinates node " +
			"membership, heartbeats and distributed locks. Without --cluster " +
			"the server stays in single-node SQLite mode.",
		Args: cobra.NoArgs,
		RunE: runServe,
	}
	cmd.Flags().StringVar(&serveOptAddr, "addr", grpc.DefaultListenAddr, "Listen address")
	cmd.Flags().StringVar(&serveOptHTTPAddr, "http-addr", ":8080", "REST gateway / Web UI listen address")
	cmd.Flags().StringVar(&serveOptTLSCert, "tls-cert", "", "TLS certificate path (optional)")
	cmd.Flags().StringVar(&serveOptTLSKey, "tls-key", "", "TLS key path (optional)")
	cmd.Flags().StringVar(&serveOptToken, "token", "", "Bearer token required from clients (empty = no auth)")
	cmd.Flags().BoolVar(&serveOptInsecure, "insecure", false, "Allow running without --token and with wildcard CORS (development only)")
	cmd.Flags().StringSliceVar(&serveOptCORSOrigins, "cors-origin", nil, "Allowed CORS origins for the REST gateway (repeatable)")
	cmd.Flags().Float64Var(&serveOptRateLimit, "rate-limit", grpc.DefaultRatePerSec, "REST gateway rate limit (req/s); negative disables")
	cmd.Flags().IntVar(&serveOptRateBurst, "rate-burst", grpc.DefaultRateBurst, "REST gateway rate burst size")
	cmd.Flags().BoolVar(&serveOptCluster, "cluster", false, "Enable cluster mode (PostgreSQL store + cluster coordination)")
	cmd.Flags().StringVar(&serveOptPGDSN, "pg-dsn", "", "PostgreSQL DSN (required with --cluster)")
	cmd.Flags().StringVar(&serveOptNodeID, "node-id", "", "Cluster node ID (required with --cluster)")
	cmd.Flags().StringVar(&serveOptNodeAddr, "node-addr", "", "Cluster node address (required with --cluster)")
	cmd.Flags().StringVar(&serveOptNodeRole, "node-role", "worker", "Cluster node role: master|worker")
	cmd.Flags().DurationVar(&serveOptClusterTakeoverInterval, "cluster-takeover-interval", takeover.DefaultInterval, "Failover takeover sweep period; <=0 disables the takeover loop (fencing stays active) (cluster mode)")
	cmd.Flags().DurationVar(&serveOptClusterExecLeaseTTL, "cluster-exec-lease-ttl", cluster.DefaultExecLeaseTTL, "Execution-lease TTL: bounds post-crash takeover detection latency; renewals run at TTL/3 (cluster mode)")
	cmd.Flags().DurationVar(&serveOptClusterDispatchInterval, "cluster-dispatch-interval", dispatch.DefaultInterval, "Cross-node dispatch sweep period; <=0 disables the dispatch loop (cluster mode)")
	cmd.Flags().IntVar(&serveOptClusterDispatchWorkerCapacity, "cluster-dispatch-capacity", dispatch.DefaultWorkerCapacity, "Max concurrent runs per worker node; the dispatcher treats a node at capacity as saturated (cluster mode)")
	cmd.Flags().StringArrayVar(&serveOptAuthTokens, "auth-token", nil, "Named bearer token name=secret (repeatable); the name becomes the authenticated actor")
	cmd.Flags().BoolVar(&serveOptMetricsPublic, "metrics-public", false, "Expose /metrics without authentication (default: requires a token when auth is enabled)")
	cmd.Flags().BoolVar(&serveOptEngineEnabled, "engine-enabled", false, "Wire the execution engine: PlanChange generates and persists real plans and ApplyChange executes approved changes (targets must be registered in the inventory; set LEVEE_MASTER_PASSWORD for credentialed channels)")
	cmd.Flags().IntVar(&serveOptEngineMaxParallel, "engine-max-parallel-runs", wiring.DefaultMaxParallelRuns, "Concurrently executing runs allowed by the engine; additional applies fast-fail (requires --engine-enabled)")
	cmd.Flags().StringVar(&serveOptEngineGatePrometheus, "engine-gate-prometheus", "", "Prometheus HTTP base URL used by slo verification gates; empty means slo gates fail closed (requires --engine-enabled)")
	return cmd
}

// resolveServeToken returns the bearer token from --token or the
// LEVEE_TOKEN environment variable (flag wins).
func resolveServeToken() string {
	if serveOptToken != "" {
		return serveOptToken
	}
	return os.Getenv("LEVEE_TOKEN")
}

// parseNamedTokens converts --auth-token name=secret pairs into
// grpc.TokenIdentity values. Malformed entries (missing '=' or an empty
// name/secret) are rejected so a typo cannot silently drop an identity.
func parseNamedTokens(pairs []string) ([]grpc.TokenIdentity, error) {
	out := make([]grpc.TokenIdentity, 0, len(pairs))
	for _, p := range pairs {
		name, secret, ok := strings.Cut(p, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" || secret == "" {
			return nil, fmt.Errorf("--auth-token expects name=secret, got %q", p)
		}
		out = append(out, grpc.TokenIdentity{Token: secret, Subject: name})
	}
	return out, nil
}

// buildOIDCVerifier converts the auth.oidc config section into an
// auth.Verifier, performing IdP discovery. It returns (nil, nil) when OIDC
// is disabled. Discovery runs under a 10s timeout so a hanging IdP cannot
// stall startup indefinitely.
func buildOIDCVerifier(ctx context.Context, cfg *config.Config) (*auth.Verifier, error) {
	oc := cfg.Auth.OIDC
	if !oc.Enabled {
		return nil, nil
	}
	acfg := auth.OIDCConfig{
		Enabled:       true,
		IssuerURL:     oc.IssuerURL,
		ClientID:      oc.ClientID,
		Audience:      oc.Audience,
		UsernameClaim: oc.UsernameClaim,
		RoleClaim:     oc.RoleClaim,
		RoleMap:       oc.RoleMap,
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return auth.NewVerifier(vctx, acfg)
}

// buildGitHubAuth converts the auth.github config section into the OAuth
// authorizer plus the session manager for SSO-minted tokens. Both are nil
// when GitHub login is disabled. The session manager is shared by both SSO
// providers (only GitHub exists today) so all browser logins verify under
// one key.
func buildGitHubAuth(cfg *config.Config) (*auth.GitHubAuthorizer, *auth.SessionManager, error) {
	if !cfg.Auth.GitHub.Enabled {
		return nil, nil, nil
	}
	gh, err := auth.NewGitHubAuthorizer(auth.GitHubConfig{
		Enabled:      true,
		ClientID:     cfg.Auth.GitHub.ClientID,
		ClientSecret: cfg.Auth.GitHub.ClientSecret,
		Org:          cfg.Auth.GitHub.Org,
		TeamRoleMap:  cfg.Auth.GitHub.TeamRoleMap,
	})
	if err != nil {
		return nil, nil, err
	}
	// The session manager keys browser session tokens. When GitHub is the
	// only SSO provider it is only needed once it is enabled; a configured
	// secret is validated eagerly so a too-short value fails startup.
	sessions, err := auth.NewSessionManager(cfg.Auth.GitHub.SessionSecret)
	if err != nil {
		return nil, nil, err
	}
	return gh, sessions, nil
}

// newServeDiagEngine builds the diagnosis engine for serve mode with both the
// log pipeline and the health prober wired, using the in-process local
// executor — the same self-diagnosis capability `levee diagnose` provides.
func newServeDiagEngine() (*diagnosis.DiagEngine, error) {
	cfg := diagnosis.DiagEngineConfig{
		LogWindow: 15 * time.Minute,
		Timeout:   60 * time.Second,
	}
	executor := newLocalExecutor()
	collector, err := diagnosis.NewLogCollector(executor)
	if err != nil {
		return nil, fmt.Errorf("build log collector: %w", err)
	}
	cfg.Collector = collector
	cfg.Analyzer = diagnosis.NewDefaultLogAnalyzer()
	cfg.Prober = diagnosis.NewHealthProber(diagnosis.HealthProberConfig{Executor: executor})
	return diagnosis.NewDiagEngine(cfg), nil
}

// newServeConvEngine builds the conversation engine for serve mode with the
// built-in recommend engine wired, mirroring the `levee converse` defaults so
// /recommend works out of the box over the API.
func newServeConvEngine() *conversation.ConversationEngine {
	recEngine := recommend.NewRecommendEngine(recommend.RecommendEngineConfig{
		Timeout: 30 * time.Second,
	})
	return conversation.NewConversationEngine(conversation.ConversationEngineConfig{
		Recommend: recEngine,
		Timeout:   60 * time.Second,
	})
}

// setupServeTracing builds the process-wide tracer from the tracing config
// section. Any construction error (e.g. an unsupported exporter) degrades
// to the noop tracer with a warning instead of keeping the daemon from
// starting. It returns a shutdown function for the caller to defer.
func setupServeTracing(cfg *config.Config) func() {
	tracer, tracingShutdown, err := tracing.New(tracing.Config{
		Enabled:     cfg.Tracing.Enabled,
		Exporter:    cfg.Tracing.Exporter,
		Endpoint:    cfg.Tracing.Endpoint,
		ServiceName: "levee",
	})
	if err != nil {
		log.Warn("tracing construction failed, falling back to noop tracer", "error", err)
		tracer, tracingShutdown, _ = tracing.New(tracing.Config{Enabled: false})
	}
	tracing.SetDefault(tracer)
	if cfg.Tracing.Enabled {
		log.Info("tracing enabled", "exporter", cfg.Tracing.Exporter)
	} else {
		log.Info("tracing disabled (configure tracing.enabled to enable)")
	}
	return func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tracingShutdown(stopCtx); err != nil {
			log.Warn("tracing shutdown failed", "error", err)
		}
		tracing.SetDefault(nil)
	}
}

// startTakeoverAndFencing sets up the execution-fencing guard and the
// failover-takeover loop (design-cluster-failover.md) in cluster mode.
// Both are nil in single-node mode. Returns a started takeover loop (or nil
// when the interval disables it) and the execGuard the engine assembly needs.
func startTakeoverAndFencing(takeoverLoop **takeover.Loop, execGuard **cluster.ExecutionGuard,
	clusterMgr *cluster.ClusterManager, store state.Store, nodeID string,
	engineEnabled bool, interval time.Duration, ctx context.Context) error {

	*execGuard = cluster.NewExecutionGuard(pgStoreDB(store))
	if engineEnabled {
		log.Info("execution fencing enabled", "lease_ttl", serveOptClusterExecLeaseTTL)
	}
	if interval > 0 {
		*takeoverLoop = takeover.NewLoop(clusterMgr, *execGuard, store, nodeID, interval)
		if err := (*takeoverLoop).Start(ctx); err != nil {
			return fmt.Errorf("start takeover loop: %w", err)
		}
		log.Info("failover takeover loop enabled (leader-only)",
			"interval", interval, "lease_ttl", serveOptClusterExecLeaseTTL)
	} else {
		log.Info("failover takeover loop disabled (--cluster-takeover-interval<=0); execution fencing remains active")
	}
	return nil
}

// startDispatchAndWorkerLoops wires up the two cluster-dispatch loops
// (design-cluster-dispatch.md): the leader-only dispatch loop that assigns
// approved runs to idle workers, and the per-node worker loop that executes
// the runs assigned to this node. Both require cluster mode and the
// execution engine; when either is disabled the loops are simply not started
// and the single-node apply path is untouched.
func startDispatchAndWorkerLoops(dispatchLoop **dispatch.Loop, workerLoop **dispatch.WorkerLoop,
	clusterMgr *cluster.ClusterManager, store state.Store, changeSvc interface {
		ApplyChange(ctx context.Context, req *pb.ApplyChangeRequest) (*pb.ApplyResponse, error)
		RetryChange(ctx context.Context, req *pb.RetryRequest) (*pb.Change, error)
	}, nodeID string, engineEnabled bool, interval time.Duration, capacity int, ctx context.Context) error {

	if !engineEnabled {
		return nil
	}
	if interval > 0 {
		*dispatchLoop = dispatch.NewLoop(clusterMgr, store, nodeID, interval, capacity)
		if err := (*dispatchLoop).Start(ctx); err != nil {
			return fmt.Errorf("start dispatch loop: %w", err)
		}
		log.Info("cross-node dispatch loop enabled (leader-only)",
			"interval", interval, "worker_capacity", capacity)
	} else {
		log.Info("cross-node dispatch loop disabled (--cluster-dispatch-interval<=0)")
	}

	*workerLoop = dispatch.NewWorkerLoop(store, &grpcChangeExecutor{svc: changeSvc, store: store}, nodeID, 2*time.Second)
	if err := (*workerLoop).Start(ctx); err != nil {
		return fmt.Errorf("start dispatch worker loop: %w", err)
	}
	log.Info("dispatch worker loop enabled", "node_id", nodeID, "interval", 2*time.Second)
	return nil
}

// runServe executes the `levee serve` command. The assembly is split into
// focused helpers (setupServeTracing, openServeStore, buildServeServices,
// buildServeServerOpts); this function keeps the sequential wiring and owns
// every resource's defer-based cleanup.
func runServe(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The bearer token comes from --token or the LEVEE_TOKEN environment
	// variable (12-factor style; keeps container images startable without
	// baking secrets into CMD). Named identities come from repeatable
	// --auth-token name=secret flags.
	token := resolveServeToken()
	namedTokens, err := parseNamedTokens(serveOptAuthTokens)
	if err != nil {
		return err
	}

	// 0. Load configuration first: the safety gate below needs to know
	//    whether auth.oidc provides an additional credential source.
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	applySecurityConfig(cfg)

	// 0b. OIDC verifier. Discovery is fetched eagerly with a short timeout
	//     so a misconfigured issuer fails the start (fail-fast) instead of
	//     leaving a daemon that accepts no JWTs and cannot explain why.
	oidcVerifier, err := buildOIDCVerifier(ctx, cfg)
	if err != nil {
		return fmt.Errorf("oidc: %w", err)
	}
	if oidcVerifier.Enabled() {
		log.Info("oidc authentication enabled", "issuer", cfg.Auth.OIDC.IssuerURL)
	}

	// 0b2. GitHub SSO. The OAuth app credentials are validated up front and
	//      the session manager keys the local tokens minted at login. An
	//      empty session_secret yields an ephemeral per-process key — fine
	//      for a single node, not for a cluster.
	githubAuth, sessionMgr, err := buildGitHubAuth(cfg)
	if err != nil {
		return fmt.Errorf("github auth: %w", err)
	}
	if githubAuth.Enabled() {
		if sessionMgr != nil && sessionMgr.Ephemeral() {
			log.Warn("github sso enabled with an ephemeral session key: sessions die on restart and do not work across nodes; set auth.github.session_secret for multi-node deployments")
		}
		log.Info("github authentication enabled", "org", cfg.Auth.GitHub.Org)
	}

	// 0c. Safety gate: refuse to start with authentication disabled unless
	//    the caller explicitly opted in via --insecure. This prevents an
	//    unauthenticated daemon from reaching production by accident.
	if token == "" && len(namedTokens) == 0 && !oidcVerifier.Enabled() &&
		!githubAuth.Enabled() && !serveOptInsecure {
		return errors.New("refusing to start without --token (or LEVEE_TOKEN / --auth-token / auth.oidc.enabled / auth.github.enabled): all API requests would be unauthenticated. " +
			"Pass --token <secret> for production, or --insecure to accept the risk for local development")
	}

	// 1b. Tracing: construct the process-wide tracer from the tracing
	//     config section (disabled by default).
	shutdownTracing := setupServeTracing(cfg)
	defer shutdownTracing()

	// 2. Open the store. In cluster mode we use PostgreSQL; otherwise we
	//    fall back to the single-node SQLite store.
	store, clusterMgr, err := openServeStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if clusterMgr != nil {
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := clusterMgr.Stop(stopCtx); err != nil {
				log.Warn("cluster manager stop failed", "error", err)
			}
		}()
	}

	// In-cluster coordination handles (created lazily; may be nil in
	// single-node mode). Both loops are cluster-only.
	var dispatchLoop *dispatch.Loop
	var dispatchWorkerLoop *dispatch.WorkerLoop
	var execGuard *cluster.ExecutionGuard
	var takeoverLoop *takeover.Loop
	if clusterMgr != nil {
		if err := startTakeoverAndFencing(&takeoverLoop, &execGuard, clusterMgr, store,
			serveOptNodeID, serveOptEngineEnabled, serveOptClusterTakeoverInterval, ctx); err != nil {
			return err
		}
	}

	// 3. Build the service implementations. We reuse the in-process
	//    implementations so the daemon and CLI share one code path.
	svcs := buildServeServices(store, cfg, execGuard)
	changeSvc, templateSvc, targetSvc := svcs.changeSvc, svcs.templateSvc, svcs.targetSvc
	auditSvc, systemSvc, alertSvc := svcs.auditSvc, svcs.systemSvc, svcs.alertSvc
	diagSvc, convSvc, mobileSvc := svcs.diagSvc, svcs.convSvc, svcs.mobileSvc

	// 3b. Cross-node dispatch (design-cluster-dispatch.md). In cluster mode
	// the leader periodically assigns approved runs to idle workers and
	// every node (including the leader) runs a worker loop that executes
	// the runs assigned to it. Independent of the takeover loop: both run
	// concurrently in cluster mode.
	if err := startDispatchAndWorkerLoops(&dispatchLoop, &dispatchWorkerLoop, clusterMgr, store,
		changeSvc, serveOptNodeID, serveOptEngineEnabled, serveOptClusterDispatchInterval,
		serveOptClusterDispatchWorkerCapacity, ctx); err != nil {
		return err
	}

	// 3c. Build server options.
	serverOpts, tlsCfg, err := buildServeServerOpts(svcs, token, namedTokens, oidcVerifier)
	if err != nil {
		return err
	}

	// 4. Construct and start the server.
	srv := grpc.NewServer(store, serverOpts...)

	// 5. Register extra services (Alert, Diagnosis, Conversation) on the
	//    gRPC server and construct the REST gateway that shares the same
	//    in-process service instances.
	grpc.RegisterExtraServices(srv.GrpcServer(), grpc.ExtraServicesConfig{
		Alert:        alertSvc,
		Diagnosis:    diagSvc,
		Conversation: convSvc,
	})

	// 5b. Inventory service: persistent target groups/import/status/history.
	pb.RegisterInventoryServiceServer(srv.GrpcServer(), grpc.NewInventoryService(store))

	// 5c. Apply the loaded SSH channel configuration (host-key policy +
	// privilege escalation) to the channel factory defaults. Without this
	// the config-file settings would never reach connections.
	sshchannel.SetDefaultConfig(
		cfg.Channel.SSH.StrictHostCheck,
		cfg.Channel.SSH.KnownHosts,
		cfg.Channel.SSH.BecomeMethod,
		cfg.Channel.SSH.BecomeUser,
	) // 5d. Reachability patrol: periodically probe every active inventory
	// target so `reachable` reflects current reality. Disabled by default;
	// enable with inventory.patrol_interval_seconds > 0.
	if interval := cfg.Inventory.PatrolIntervalSeconds; interval > 0 {
		go runReachabilityPatrol(ctx, store, time.Duration(interval)*time.Second)
		log.Info("reachability patrol enabled", "interval_seconds", interval)
	}

	gw := grpc.NewGateway(grpc.ServeGatewayConfig{
		Addr:          serveOptHTTPAddr,
		CORSOrigins:   serveOptCORSOrigins,
		AuthToken:     token,
		AuthTokens:    namedTokens,
		OIDC:          oidcVerifier,
		GitHub:        githubAuth,
		Sessions:      sessionMgr,
		MetricsPublic: serveOptMetricsPublic,
		RatePerSec:    serveOptRateLimit,
		RateBurst:     serveOptRateBurst,
	})
	gw.SetServices(changeSvc, templateSvc, targetSvc, auditSvc, systemSvc, alertSvc, diagSvc, convSvc)
	gw.SetMobileApproval(mobileSvc)
	gw.SetStore(store)
	gw.SetConversationEngine(svcs.convEngine)

	// 5e. Self-observability: expose the process-wide metrics collector as
	//     Prometheus text format on the gateway mux. The route is gated
	//     behind the same bearer auth as the API whenever any token is
	//     configured; pass --metrics-public to allow unauthenticated scraping.
	gw.SetExtraRoute("/metrics", metrics.Default.Handler())

	// 6. Start the REST gateway on THIS instance. Start binds the port
	//    synchronously so a bind failure fails the command instead of
	//    leaving a half-alive daemon; serving continues in the background.
	if err := gw.Start(ctx); err != nil {
		return fmt.Errorf("start gateway: %w", err)
	}
	log.Info("REST gateway listening", "addr", serveOptHTTPAddr)

	// 6. Use the configured address (fall back to listen addr option).
	addr := serveOptAddr
	if addr == "" {
		addr = grpc.DefaultListenAddr
	}

	// 7. Wire signal handling for graceful shutdown. Cancelling ctx stops
	//    the REST gateway's serve loop; GracefulStop drains the gRPC server.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig.String())
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), serveGracefulShutdownTimeout)
		defer shutdownCancel()
		_ = srv.GracefulStop(shutdownCtx)
		_ = gw.Stop(shutdownCtx)
	}()

	// 8. Block until the server stops.
	if tlsCfg == nil && !serveOptInsecure {
		log.Warn("serving without TLS: traffic (including bearer tokens) is plaintext. " +
			"Pass --tls-cert/--tls-key for direct TLS, or terminate TLS at a sidecar/load balancer, " +
			"or pass --insecure to silence this warning for local development")
	}
	log.Info("starting levee gRPC server", "addr", addr, "tls", tlsCfg != nil,
		"auth", token != "" || len(namedTokens) > 0, "named_tokens", len(namedTokens))
	if err := srv.Start(addr); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// serveServices bundles the in-process service implementations the daemon
// (gRPC server + REST gateway) shares.
type serveServices struct {
	changeSvc   *grpc.ChangeService
	templateSvc *grpc.TemplateService
	targetSvc   *grpc.TargetService
	auditSvc    *grpc.AuditService
	systemSvc   *grpc.SystemService
	alertSvc    *grpc.AlertService
	diagSvc     *grpc.DiagnosisService
	convSvc     *grpc.ConversationService
	convEngine  *conversation.ConversationEngine
	mobileSvc   *approval.MobileApprovalService
}

// buildServeServices constructs the in-process service implementations,
// mirroring the CLI code paths. Split out of runServe; behaviour unchanged.
// execGuard is non-nil only in cluster mode: it attaches the execution
// fencing to the engine so cluster-mode executions register leases and
// every write is epoch-checked.
func buildServeServices(store state.Store, cfg *config.Config, execGuard *cluster.ExecutionGuard) serveServices {
	// Credential store first: the engine (when enabled) and target probing
	// share the encrypted store backed by LEVEE_MASTER_PASSWORD. Without it
	// the resolver stays nil (disabled): CheckTarget probes unauthenticated
	// and engine channels dial targets without credentials. Documented
	// limitation: serve mode has no other master-password source today —
	// there is no --master-password flag or keyfile mechanism to wire instead.
	var credResolver *serveCredentialResolver
	if mp := os.Getenv("LEVEE_MASTER_PASSWORD"); mp != "" {
		if credStore, err := credential.NewCredentialStore(store, mp); err != nil {
			log.Warn("credential store unavailable; target checks will probe without credentials",
				"error", err)
		} else {
			credResolver = &serveCredentialResolver{store: credStore}
			log.Info("target checks will resolve stored credentials for probes")
		}
	} else {
		log.Info("LEVEE_MASTER_PASSWORD not set; target checks will probe without credentials " +
			"(no resolver configured)")
	}

	// Execution engine (--engine-enabled, off by default). When wired, the
	// ChangeService gains real plan generation (persisted plan artifacts)
	// and apply execution via internal/wiring; when nil, ApplyChange keeps
	// refusing with FailedPrecondition ("status-only mode") instead of
	// pretending to execute.
	var engine *grpc.EngineAdapter
	if serveOptEngineEnabled {
		var opts []wiring.Option
		if credResolver != nil {
			opts = append(opts, wiring.WithCredentialResolver(credResolver))
			log.Info("execution engine enabled with credential resolution for target channels")
		} else {
			log.Warn("execution engine enabled WITHOUT LEVEE_MASTER_PASSWORD: channels will dial targets without credentials")
		}
		if serveOptEngineMaxParallel > 0 {
			opts = append(opts, wiring.WithMaxParallelRuns(serveOptEngineMaxParallel))
		}
		if serveOptEngineGatePrometheus != "" {
			opts = append(opts, wiring.WithGatePrometheusURL(serveOptEngineGatePrometheus))
			log.Info("execution engine slo gates will query Prometheus", "url", serveOptEngineGatePrometheus)
		} else {
			log.Info("execution engine slo gates have no Prometheus URL: declared slo gates fail closed")
		}
		if execGuard != nil {
			// Cluster mode: executions are fenced by run_execution leases.
			// Begin failure refuses the run outright (no invisible
			// takeover candidates) and losing the lease mid-flight stops
			// the executor without rolling back.
			opts = append(opts,
				wiring.WithExecutionGuard(clusterExecGuardAdapter{g: execGuard}, serveOptNodeID),
				wiring.WithExecLeaseTTL(serveOptClusterExecLeaseTTL))
		}
		engine = wiring.NewEngine(store, opts...).Adapter()
		log.Info("serve: execution engine wired (--engine-enabled); PlanChange generates persisted plans and ApplyChange executes approved changes")
	} else {
		log.Info("serve: execution engine not wired (--engine-enabled=false); ApplyChange RPC returns FailedPrecondition (status-only mode). Plan/approve/status tracking remain fully functional.")
	}
	changeSvc := grpc.NewChangeService(store, engine, nil, nil)
	templateSvc := grpc.NewTemplateService(store, nil)
	targetSvc := grpc.NewTargetService(store, nil)
	if credResolver != nil {
		// Credential-aware probing: CheckTarget probes targets with their
		// stored credentials instead of unauthenticated.
		targetSvc.WithCredentialResolver(credResolver)
	}
	auditSvc := grpc.NewAuditService(store)
	systemSvc := grpc.NewSystemService(
		store, cfg, optConfigPath,
		version, commitHash, buildTime, goVersion, time.Now(),
	)
	// Alert ingestion stays stand-alone here (the AlertService keeps its own
	// bounded ring); run `levee alert serve` for the full gateway with
	// Prometheus/custom adapters. Diagnosis and conversation get real engines
	// so the corresponding RPCs are functional in serve mode instead of
	// returning Unimplemented.
	alertSvc := grpc.NewAlertService(nil, slog.Default())
	diagEngine, diagErr := newServeDiagEngine()
	if diagErr != nil {
		log.Warn("diagnosis engine unavailable; Diagnose RPC will report Unimplemented", "error", diagErr)
	}
	diagSvc := grpc.NewDiagnosisService(diagEngine, slog.Default())
	convEngine := newServeConvEngine()
	convSvc := grpc.NewConversationService(convEngine, slog.Default())

	// Mobile approval: wire the deeplink approve/reject endpoints so the
	// REST gateway's /changes/deeplink/* routes work out of the box. Push
	// delivery stays disabled until a push manager is configured.
	mobileSvc := approval.NewMobileApprovalService(
		approval.NewService(newApprovalStoreAdapter(store)),
		nil,
		push.NewDeepLinkGenerator("levee", "https://levee.local"),
	)
	return serveServices{
		changeSvc: changeSvc, templateSvc: templateSvc, targetSvc: targetSvc,
		auditSvc: auditSvc, systemSvc: systemSvc, alertSvc: alertSvc,
		diagSvc: diagSvc, convSvc: convSvc, convEngine: convEngine, mobileSvc: mobileSvc,
	}
}

// buildServeServerOpts assembles the gRPC server options (services, auth,
// TLS) and returns the TLS config alongside them — runServe inspects it for
// the plaintext-traffic warning. Split out of runServe; behaviour unchanged.
func buildServeServerOpts(svcs serveServices, token string, namedTokens []grpc.TokenIdentity, oidcVerifier *auth.Verifier) ([]grpc.Option, *tls.Config, error) {
	serverOpts := []grpc.Option{
		grpc.WithListenAddr(serveOptAddr),
		grpc.WithChangeService(svcs.changeSvc),
		grpc.WithTemplateService(svcs.templateSvc),
		grpc.WithTargetService(svcs.targetSvc),
		grpc.WithAuditService(svcs.auditSvc),
		grpc.WithSystemService(svcs.systemSvc),
	}
	if token != "" {
		serverOpts = append(serverOpts, grpc.WithAuthToken(token))
	}
	if len(namedTokens) > 0 {
		serverOpts = append(serverOpts, grpc.WithAuthTokens(namedTokens))
	}
	// Nil/disabled verifier is a no-op option: AuthTokens.OIDC stays
	// disabled and only static tokens are accepted.
	serverOpts = append(serverOpts, grpc.WithAuthVerifier(oidcVerifier))
	tlsCfg, err := loadTLSConfig(serveOptTLSCert, serveOptTLSKey)
	if err != nil {
		return nil, nil, fmt.Errorf("load tls: %w", err)
	}
	if tlsCfg != nil {
		serverOpts = append(serverOpts, grpc.WithTLS(tlsCfg))
	}
	return serverOpts, tlsCfg, nil
}

// clusterExecGuardAdapter bridges the cluster package's concrete
// ExecutionGuard to the wiring layer's interface. The wiring layer
// cannot import internal/cluster (it sits below the composition root),
// so the bridge lives here where both sides are already imported. The
// lease it returns re-exposes Owns/Heartbeat/End; the wiring layer's
// SentinelAdapter (applied inside WithExecutionGuard) maps the cluster
// fence errors onto the engine sentinel chain.
type clusterExecGuardAdapter struct{ g *cluster.ExecutionGuard }

func (a clusterExecGuardAdapter) Begin(ctx context.Context, runID string) (wiring.ExecutionLease, error) {
	lease, err := a.g.Register(ctx, runID, serveOptNodeID, serveOptClusterExecLeaseTTL)
	if err != nil {
		return nil, err
	}
	return a.g.LeaseGuard(lease, serveOptClusterExecLeaseTTL), nil
}

// pgStoreDB extracts the *sql.DB from the cluster-mode store. Returns
// nil for the single-node SQLite store (fencing is cluster-only).
func pgStoreDB(store state.Store) *sql.DB {
	if pg, ok := store.(*state.PGStore); ok {
		return pg.DB()
	}
	return nil
}

// openServeStore opens the backend store for serve mode. In cluster mode it
// requires --pg-dsn/--node-id/--node-addr, opens PostgreSQL, joins and
// starts the cluster manager; otherwise it opens the single-node SQLite
// store. The caller owns both returned resources (store.Close,
// clusterMgr.Stop). Split out of runServe; behaviour unchanged.
func openServeStore(ctx context.Context, cfg *config.Config) (state.Store, *cluster.ClusterManager, error) {
	if !serveOptCluster {
		sqliteStore, err := state.NewSQLiteStore(ctx, cfg.Database.Path,
			state.WithSynchronous(cfg.State.SQLiteSynchronous))
		if err != nil {
			return nil, nil, fmt.Errorf("open store: %w", err)
		}
		log.Info("single-node mode (SQLite)")
		return sqliteStore, nil, nil
	}
	if serveOptPGDSN == "" {
		return nil, nil, errors.New("--cluster requires --pg-dsn")
	}
	if serveOptNodeID == "" || serveOptNodeAddr == "" {
		return nil, nil, errors.New("--cluster requires --node-id and --node-addr")
	}
	pgStore, err := state.NewPGStore(ctx, serveOptPGDSN, state.PGPoolConfig{
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres store: %w", err)
	}
	store := state.Store(pgStore)
	clusterMgr := cluster.NewClusterManager(pgStore.DB(), cluster.ManagerConfig{
		SelfID: serveOptNodeID,
	})
	if err := clusterMgr.Join(cluster.Node{
		ID:      serveOptNodeID,
		Address: serveOptNodeAddr,
		Status:  cluster.StatusActive,
		Role:    cluster.NodeRole(serveOptNodeRole),
	}); err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("join cluster: %w", err)
	}
	if err := clusterMgr.Start(ctx); err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("start cluster manager: %w", err)
	}
	log.Info("cluster mode enabled", "node_id", serveOptNodeID, "node_addr", serveOptNodeAddr, "role", serveOptNodeRole)
	log.Info("cluster coordination: shared storage, membership, locking; in-flight changes are fenced (run_execution leases) and the takeover loop settles crashed executors' runs to interrupted")
	return store, clusterMgr, nil
}

// serveCredentialResolver adapts the encrypted credential store to the
// grpc.CredentialResolver interface used by TargetService.CheckTarget. It
// expands a stored credential reference (the credential NAME, e.g. the value
// passed as target credential_ref) into transport credentials for probing.
//
// Stored plaintexts follow one of two conventions:
//  1. a JSON object mirroring channel.CredentialRef (username / password /
//     key_path / key_passphrase) — the structured form;
//  2. any other plaintext is treated as a bare password or API token.
//
// Plaintext bytes are zeroed after decoding; the returned CredentialRef is a
// private copy.
type serveCredentialResolver struct {
	store *credential.CredentialStore
}

// ResolveTargetCredential implements grpc.CredentialResolver.
func (r *serveCredentialResolver) ResolveTargetCredential(ctx context.Context, ref string) (*channel.CredentialRef, error) {
	plaintext, err := r.store.Retrieve(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer credential.SecureZero(plaintext)

	var cred channel.CredentialRef
	if err := json.Unmarshal(plaintext, &cred); err == nil && (cred.Username != "" || cred.Password != "" || cred.KeyPath != "" || cred.KeyPassphrase != "") {
		return &cred, nil
	}
	// SA-011 known residue: channel.CredentialRef.Password is a string, and
	// Go strings cannot be zeroed, so this conversion leaves one copy of the
	// secret behind for the bare-password convention. Erasing it requires
	// changing CredentialRef to []byte across ssh/winrm/grpc, which was
	// scoped out of the hardening round by design; tracked as a known
	// residual in the SA-011 ledger row, not as a fixed issue.
	return &channel.CredentialRef{Password: string(plaintext)}, nil
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

// runReachabilityPatrol probes every active inventory target on the given
// interval until ctx is cancelled, stamping reachability back into the
// store. Probing is a plain TCP dial to hostname:port — sufficient to
// detect host/SSH-stack liveness without holding channel credentials.
func runReachabilityPatrol(ctx context.Context, store state.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	probeAll := func() {
		targets, err := store.ListTargets(ctx, state.TargetFilter{Status: state.StatusActive})
		if err != nil {
			log.Warn("patrol: list targets failed", "err", err)
			return
		}
		now := time.Now().UTC()
		for _, t := range targets {
			if ctx.Err() != nil {
				return
			}
			addr := net.JoinHostPort(t.Hostname, strconv.Itoa(t.Port))
			conn, dialErr := net.DialTimeout("tcp", addr, 5*time.Second)
			if dialErr == nil {
				_ = conn.Close()
			}
			if err := store.SetTargetReachability(ctx, t.ID, dialErr == nil, now); err != nil {
				log.Warn("patrol: set reachability failed", "target", t.ID, "err", err)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeAll()
		}
	}
}
