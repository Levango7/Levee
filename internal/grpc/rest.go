// Package grpc implements a lightweight REST-to-gRPC gateway. It translates
// HTTP/JSON requests at /api/v1/<Service>/<Method> into calls against the
// in-process gRPC service implementations, using protojson for serialization.
//
// The gateway runs the same service instances as the gRPC server. It does
// not dial a separate process — all calls are direct method invocations.
//
// Streaming RPCs are not yet supported and return HTTP 501.
package grpc

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/auth"
	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/permission"
	"github.com/nexus/levee/internal/state"
)

// Default gateway rate-limit values. The limiter is a global token
// bucket shared by all clients of one gateway process.
const (
	DefaultRatePerSec = 200.0
	DefaultRateBurst  = 400
)

// ServeGatewayConfig configures the REST-to-gRPC gateway.
type ServeGatewayConfig struct {
	// Addr is the HTTP listen address, e.g. ":8080".
	Addr string
	// CORSOrigins lists allowed origins. "*" or an empty slice allows all.
	CORSOrigins []string
	// AuthToken is the expected Bearer token for client authentication.
	// When empty (and AuthTokens is empty), authentication is disabled
	// (development mode).
	AuthToken string
	// AuthTokens lists additional named bearer tokens, each mapped to the
	// subject it authenticates as. Together with AuthToken they form the
	// accepted set. A request authenticated via a named token carries that
	// token's subject for audit attribution, overriding any client-asserted
	// X-Acting-As header.
	AuthTokens []TokenIdentity
	// OIDC, when non-nil and enabled, additionally admits JWT-shaped bearer
	// tokens verified against the configured OpenID Connect provider.
	// Static tokens take precedence; the verified subject and roles are
	// injected into the request context for audit attribution.
	OIDC *auth.Verifier
	// GitHub, when non-nil and enabled, exposes the server-side half of the
	// GitHub OAuth login flow (POST /auth/github) and announces it via
	// /system/auth-info. The client_secret it carries never leaves the
	// process.
	GitHub *auth.GitHubAuthorizer
	// Sessions verifies the LEVEE-local session tokens minted by the SSO
	// login endpoints. When nil while GitHub is enabled, NewGateway
	// substitutes an ephemeral per-process manager (single-node only;
	// cluster deployments must share a configured session_secret).
	Sessions *auth.SessionManager
	// MetricsPublic, when true, leaves operational extra routes (e.g.
	// /metrics) reachable without a bearer token. By default (false) those
	// routes require authentication whenever any token is configured, so
	// deployment telemetry is not publicly readable.
	MetricsPublic bool
	// RatePerSec is the global token-bucket refill rate for the gateway.
	// Zero selects DefaultRatePerSec; a negative value disables rate
	// limiting entirely.
	RatePerSec float64
	// RateBurst is the token-bucket burst size. Zero selects
	// DefaultRateBurst. Ignored when rate limiting is disabled.
	RateBurst int
	// ClusterEnabled announces that THIS process runs cluster coordination: a
	// cluster manager exists, so membership, leases and the dispatch sweep are
	// live. The gateway cannot discover this by itself — coordination is
	// constructed in cmd/levee, and a manager is not something the store knows
	// about. Leaving it unset reports "not coordinating", which is the honest
	// reading of a gateway nobody wired to a cluster.
	//
	// It is deliberately not derived from the backend label. Now that a
	// single-node PostgreSQL deployment is a supported shape,
	// backend == "postgres" reports only which database answers; reading
	// coordination off it would tell an operator that a deliberate single node
	// is half a cluster.
	ClusterEnabled bool
}

// ServeGateway starts an HTTP server on cfg.Addr that proxies /api/v1/*
// requests to the in-process gRPC service implementations. The gateway
// created here has NO services registered — data endpoints will report 503
// until SetServices is called. Production code should construct a Gateway
// via NewGateway + SetServices and call Start instead.
func ServeGateway(ctx context.Context, cfg ServeGatewayConfig) error {
	return NewGateway(cfg).Start(ctx)
}

// Gateway is the REST-to-gRPC gateway. It holds service references and can
// be started with Serve or manually via start().
type Gateway struct {
	cfg   ServeGatewayConfig
	store state.Store // backend store for direct queries (cluster status)
	// authz is the policy authorizer the REST-only endpoints judge against. It
	// is nil until WithAuthorizer is called, which is the same "no policy
	// configured" posture the services use — an endpoint that judges without it
	// would refuse everything. The gRPC services carry their own authorizer;
	// the gateway needs one too because /system/batch-status and friends are
	// handled here, not by a service.
	authz      *authz.Authorizer
	httpServer *http.Server
	change     *ChangeService
	template   *TemplateService
	target     *TargetService
	audit      *AuditService
	system     *SystemService
	alert      *AlertService
	diag       pb.DiagnosisServiceServer
	conv       pb.ConversationServiceServer
	inventory  *InventoryService
	agents     pb.AgentServiceServer
	// convEngine is the underlying ConversationEngine, used by the REST
	// session-management endpoints (NewSession/ListSessions/etc.) that do
	// not map to a gRPC method. nil when conversation is not configured.
	convEngine *conversation.ConversationEngine
	// mobileApproval is optional; non-nil when the mobile approval service
	// is configured. Used by the /changes/deeplink/approve REST endpoint.
	mobileApproval mobileApprovalHandler
	// gateService is optional; non-nil when ad-hoc gate verification is
	// configured. Used by the POST /gates/verify endpoint (rest_gate.go).
	gateService gateServiceHandler

	// limiterOnce lazily initialises the gateway-wide rate limiter so
	// that BOTH route trees (RESTful "/" and legacy "/api/v1/") share a
	// single token bucket. limiter stays nil when rate limiting is
	// disabled via a negative RatePerSec.
	limiterOnce sync.Once
	limiter     *rate.Limiter

	// extraRoutes holds handlers registered via SetExtraRoute before
	// Start. They are mounted verbatim on the gateway mux: no CORS and
	// no rate limiting, but (unlike earlier revisions of this comment
	// suggested) bearer auth DOES apply whenever any token is
	// configured, unless the route owner opted out via MetricsPublic.
	// See SetExtraRoute for the exact gating semantics.
	extraMu     sync.Mutex
	extraRoutes []extraRoute
}

// extraRoute pairs a mux pattern with the handler SetExtraRoute should
// mount for it. A public route authenticates by its own means and must not
// also demand a bearer the caller cannot hold.
type extraRoute struct {
	pattern string
	handler http.Handler
	public  bool
}

// mobileApprovalHandler wraps the MobileApprovalService for REST routing.
// The route layer only needs the two decision calls plus the evidence read
// behind them, so it depends on this surface rather than on the concrete
// service (which also owns push, history and the approval chain).
//
// The decision methods return the runID the token was bound to so the
// gateway can settle the run from the approval chain state (the settlement
// itself lives on ChangeService — this layer only routes).
type mobileApprovalHandler interface {
	ApproveViaDeepLink(ctx context.Context, token string) (string, error)
	RejectViaDeepLink(ctx context.Context, token string) (string, error)
	// DeeplinkState reports what has already happened to the approval a
	// deep-link token points at, consuming nothing. ok is false when the
	// token is not one this server issued: the caller must then disclose
	// nothing beyond the flat 401, because a forged token proves nothing.
	DeeplinkState(ctx context.Context, token string) (approval.DeeplinkState, bool, error)
}

// GatewayServices bundles the service implementations for NewGateway.
type GatewayServices struct {
	Change   *ChangeService
	Template *TemplateService
	Target   *TargetService
	Audit    *AuditService
	System   *SystemService
	Alert    pb.AlertServiceServer
	Diag     pb.DiagnosisServiceServer
	Conv     pb.ConversationServiceServer
}

// NewGateway constructs a gateway from the given config. When GitHub SSO is
// enabled without a session manager, an ephemeral per-process manager is
// substituted so logins work on single-node deployments (sessions die with
// the process); cluster deployments behind a load balancer must configure a
// shared session_secret instead.
func NewGateway(cfg ServeGatewayConfig) *Gateway {
	if cfg.GitHub.Enabled() && cfg.Sessions == nil {
		if m, err := auth.NewSessionManager(""); err == nil {
			cfg.Sessions = m
		}
	}
	return &Gateway{cfg: cfg}
}

// WithAuthorizer installs the policy authorizer for the REST-only endpoints.
// It mirrors the services' own WithAuthorizer: nil means "no policy
// configured", in which case a gated endpoint admits rather than refuses (see
// authorizeResourceRead).
func (gw *Gateway) WithAuthorizer(a *authz.Authorizer) *Gateway {
	gw.authz = a
	return gw
}

// SetServices registers the in-process service implementations with the gateway.
func (gw *Gateway) SetServices(change *ChangeService, template *TemplateService, target *TargetService, audit *AuditService, system *SystemService, alert *AlertService, diag pb.DiagnosisServiceServer, conv pb.ConversationServiceServer) {
	gw.change = change
	gw.template = template
	gw.target = target
	gw.audit = audit
	gw.system = system
	gw.alert = alert
	gw.diag = diag
	gw.conv = conv
}

// SetMobileApproval registers an optional mobile approval service. When set,
// the gateway exposes POST /changes/deeplink/approve and
// POST /changes/deeplink/reject.
func (gw *Gateway) SetMobileApproval(m mobileApprovalHandler) {
	gw.mobileApproval = m
}

// SetStore sets the backend store the gateway queries directly for endpoints
// that do not map to a gRPC service (e.g. cluster status).
func (gw *Gateway) SetStore(s state.Store) { gw.store = s }

// ClusterEnabled reports what this gateway announces on
// /system/cluster-status about cluster coordination.
//
// The gateway's other exported members are all writers, so this reads as
// out of place until you ask how the announcement can be trusted: `serve`
// builds the config, and a field nothing can read back is a field nobody can
// prove is set. cmd/levee's wiring test uses it to check that the value the
// payload carries is the one derived from the live cluster manager rather
// than a literal left behind when the flag was added.
func (gw *Gateway) ClusterEnabled() bool { return gw.cfg.ClusterEnabled }

// SetConversationEngine registers the ConversationEngine the gateway calls
// directly for the session-management REST endpoints (NewSession/ListSessions
// /GetSession/CloseSession), which do not map to a gRPC method. The engine
// must be set in addition to the gRPC ConversationService.
func (gw *Gateway) SetConversationEngine(e *conversation.ConversationEngine) {
	gw.convEngine = e
}

// SetExtraRoute registers an additional route on the gateway's mux, e.g. an
// operational endpoint such as /metrics. Call it before Start; routes
// registered after the gateway started are not mounted. Unless MetricsPublic
// is set, the route is gated behind the same bearer auth as the API whenever
// any token is configured (CORS and rate limiting still do not apply).
func (gw *Gateway) SetExtraRoute(pattern string, handler http.Handler) {
	gw.extraMu.Lock()
	defer gw.extraMu.Unlock()
	gw.extraRoutes = append(gw.extraRoutes, extraRoute{pattern: pattern, handler: handler})
}

// SetPublicRoute registers an extra route that is exempt from bearer auth
// because it authenticates by its own means — a MAC over the request body
// under a secret the caller and server share, which is the only credential
// a chat platform callback can present (see chatops.CallbackHandler).
//
// The exemption is per route, not a gateway flag, and that distinction is
// the whole point: MetricsPublic leaves EVERY extra route public at once,
// so using it for a callback would also publish /metrics. A public route
// that cannot verify its callers is a hole; this method is only correct for
// handlers whose own check is unconditional.
func (gw *Gateway) SetPublicRoute(pattern string, handler http.Handler) {
	gw.extraMu.Lock()
	defer gw.extraMu.Unlock()
	gw.extraRoutes = append(gw.extraRoutes, extraRoute{pattern: pattern, handler: handler, public: true})
}

// mountExtraRoutes mounts every registered extra route on mux. It is a
// method rather than a block inside serveOn so the auth-per-route decision
// has one home that both the server and its tests exercise — the same
// "one builder for both" rule dataHandler follows.
func (gw *Gateway) mountExtraRoutes(mux *http.ServeMux) {
	gw.extraMu.Lock()
	defer gw.extraMu.Unlock()
	for _, er := range gw.extraRoutes {
		h := er.handler
		if !er.public && !gw.cfg.MetricsPublic {
			h = gw.authMiddleware(h)
		}
		mux.Handle(er.pattern, h)
	}
}

// Stop gracefully shuts down the HTTP server.
func (gw *Gateway) Stop(ctx context.Context) error {
	if gw.httpServer == nil {
		return nil
	}
	return gw.httpServer.Shutdown(ctx)
}

// Start binds the gateway's listen address synchronously and serves in the
// background until ctx is cancelled. Unlike the package-level ServeGateway,
// Start runs on this Gateway instance, so services registered through
// SetServices are honored. A bind failure is returned to the caller.
func (gw *Gateway) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", gw.cfg.Addr)
	if err != nil {
		return fmt.Errorf("gateway: listen %s: %w", gw.cfg.Addr, err)
	}
	go func() { _ = gw.serveOn(ln, ctx) }()
	return nil
}

func (gw *Gateway) serveOn(ln net.Listener, ctx context.Context) error {
	mux := http.NewServeMux()
	// ONE router serves both URL shapes, behind one rate limiter (separate
	// buckets would let a client double its budget by switching path styles):
	//   RESTful    /changes, /templates/:id, /audit/verify, ...
	//   gRPC-style /ChangeService/ListChanges, ...
	// Both also accept the /api/v1 prefix, which is what the SPA sends — its
	// axios client is built with baseURL "/api/v1" and resource paths, e.g.
	// GET /api/v1/changes. The prefix is stripped before auth so the
	// public-path predicates below (which compare bare paths) keep working.
	mux.Handle("/", gw.dataHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if gw.change == nil {
			// Services were never registered: data endpoints would fail.
			// Report unhealthy so orchestrators do not route traffic here.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable","reason":"services not registered"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Mount operational routes registered via SetExtraRoute (e.g. /metrics).
	// Unlike /healthz, these are gated behind the same bearer auth as the API
	// whenever any token is configured, so deployment telemetry is not
	// publicly readable by default. Set MetricsPublic to opt out (e.g. for
	// scraping agents that cannot send credentials). Routes registered via
	// SetPublicRoute skip that gate individually — they carry their own
	// authentication (the ChatOps callback verifies the platform MAC).
	gw.mountExtraRoutes(mux)

	gw.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- gw.httpServer.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = gw.httpServer.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("gateway: serve: %w", err)
		}
		return nil
	}
}

// dataHandler is THE data-plane pipeline: prefix compat → CORS → bearer auth →
// request id → shared rate limiter → one router for both URL shapes.
//
// Both serveOn and the tests mount this, because the previous arrangement had
// the test harness hand-copying the two mounts instead of calling production's
// wiring — so the harness kept passing while every prefixed frontend request 400
// in the real server. One builder for both is the point.
func (gw *Gateway) dataHandler() http.Handler {
	lim := gw.sharedRateLimiter()
	return apiCompatPrefix(corsMiddleware(gw.cfg.CORSOrigins,
		gw.authMiddleware(gw.requestIDMiddleware(wrapWithLimiter(lim, gw.dispatch())))))
}

// apiCompatPrefix drops the optional /api/v1 prefix so one set of handlers and
// one set of path predicates see bare resource paths.
//
// This is not cosmetic. The SPA builds every URL as
// baseURL("/api/v1") + "/changes", and the gateway used to mount that prefix
// on a router that only accepted /api/v1/<Service>/<Method>, so every
// dashboard read answered 400 ("invalid path… / unknown service: changes") —
// and because the strip happens ahead of the auth layer, the public-path
// predicates (/system/auth-info, /auth/github, /changes/deeplink/*) never
// matched either, so the login bootstrap demanded a credential the browser
// cannot hold yet.
func apiCompatPrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == apiPrefix || strings.HasPrefix(p, apiPrefix+"/") {
			stripped := strings.TrimPrefix(p, apiPrefix)
			if stripped == "" {
				stripped = "/"
			}
			r.URL.Path = stripped
			// RawPath only exists when the escaped form differs; keeping a
			// stale copy would hand handlers two answers for one path.
			if r.URL.RawPath != "" {
				if rp := strings.TrimPrefix(r.URL.RawPath, apiPrefix); rp != "" {
					r.URL.RawPath = rp
				} else {
					r.URL.RawPath = ""
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// apiPrefix is the versioned prefix the frontend and the documented HTTP API
// both use. It is optional at the router: /changes and /api/v1/changes reach
// the same handler.
const apiPrefix = "/api/v1"

// apiRelativePath returns the gateway path with the optional /api/v1 prefix
// removed and no surrounding slashes: "ChangeService/ListChanges" or
// "changes/run-1/logs". Routers use it so they behave the same whether they
// are reached through apiCompatPrefix or mounted directly.
//
// The prefix is only recognized at a path boundary: without that rule
// /api/v1x/changes would be stripped to x/changes, disagreeing with
// apiCompatPrefix (which demands the slash) and misrouting the request.
func apiRelativePath(p string) string {
	if p == apiPrefix {
		return ""
	}
	if strings.HasPrefix(p, apiPrefix+"/") {
		p = p[len(apiPrefix):]
	}
	return strings.Trim(p, "/")
}

// dispatch is the gateway's single router: gRPC-style when the first segment
// names a registered service, RESTful otherwise.
func (gw *Gateway) dispatch() http.Handler {
	grpcRouter := gw.route()
	restRouter := gw.restRoute()
	services := gw.grpcServiceHandlers()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := apiRelativePath(r.URL.Path)
		first := rel
		if i := strings.Index(rel, "/"); i >= 0 {
			first = rel[:i]
		}
		if _, ok := services[first]; ok {
			grpcRouter.ServeHTTP(w, r)
			return
		}
		// A segment that LOOKS like a service but isn't registered is a bad API
		// path, not a missing resource: answer 400 with the real reason rather
		// than letting the resource tree report an unhelpful 404.
		if strings.HasSuffix(first, "Service") {
			writeJSONError(w, http.StatusBadRequest, "unknown service: "+first)
			return
		}
		restRouter.ServeHTTP(w, r)
	})
}

// grpcServiceHandlers maps a gRPC-style service name to the handler that takes
// its <Method> path tail.
//
// This table is the single answer to "is this first segment a service?":
// route() dispatches through it, dispatch() routes through it, and the
// frontend-URL guard test reads it. The gateway used to keep the service names
// in a switch AND in a second list, which is how a router and a test can
// disagree while both look correct.
func (gw *Gateway) grpcServiceHandlers() map[string]func(http.ResponseWriter, *http.Request, string) {
	return map[string]func(http.ResponseWriter, *http.Request, string){
		"ChangeService":       gw.handleChange,
		"TemplateService":     gw.handleTemplate,
		"TargetService":       gw.handleTarget,
		"AuditService":        gw.handleAudit,
		"SystemService":       gw.handleSystem,
		"AlertService":        gw.handleAlert,
		"DiagnosisService":    gw.handleDiagnosis,
		"ConversationService": gw.handleConversation,
	}
}

// restResourceHandlers maps the first segment of a RESTful route to its
// dispatcher. The signature is (w, r, method, absolutePath).
func (gw *Gateway) restResourceHandlers() map[string]func(http.ResponseWriter, *http.Request, string, string) {
	return map[string]func(http.ResponseWriter, *http.Request, string, string){
		"changes":      gw.dispatchChange,
		"templates":    gw.dispatchTemplate,
		"targets":      gw.dispatchTarget,
		"groups":       gw.dispatchInventory,
		"agents":       gw.dispatchAgents,
		"alerts":       gw.dispatchAlerts,
		"conversation": gw.dispatchConversation,
		"audit": func(w http.ResponseWriter, r *http.Request, method, path string) {
			gw.dispatchOperationalRest(w, r, "audit", method, path)
		},
		"auth": func(w http.ResponseWriter, r *http.Request, method, path string) {
			gw.dispatchOperationalRest(w, r, "auth", method, path)
		},
		"system": func(w http.ResponseWriter, r *http.Request, method, path string) {
			gw.dispatchOperationalRest(w, r, "system", method, path)
		},
		"gates": func(w http.ResponseWriter, r *http.Request, method, path string) {
			if path == "/gates/verify" && method == http.MethodPost {
				gw.handleGateVerify(w, r)
				return
			}
			writeJSONError(w, http.StatusNotFound, "not found: "+path)
		},
	}
}

// route dispatches <Service>/<Method> (with or without the /api/v1 prefix) to
// the right handler.
func (gw *Gateway) route() http.Handler {
	handlers := gw.grpcServiceHandlers()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := apiRelativePath(r.URL.Path)

		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 {
			writeJSONError(w, http.StatusBadRequest, "invalid path: expected /api/v1/<Service>/<Method>")
			return
		}
		handler, ok := handlers[parts[0]]
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "unknown service: "+parts[0])
			return
		}
		handler(w, r, parts[1])
	})
}

// restRoute dispatches RESTful HTTP paths (e.g. /changes, /templates/:id) to
// the corresponding gRPC handlers.
//
// Routing matches the FIRST path segment EXACTLY. The previous
// strings.HasPrefix(path, "/changes") test made look-alike paths such as
// "/changesfoo" dispatch into the change handlers instead of 404ing.
func (gw *Gateway) restRoute() http.Handler {
	handlers := gw.restResourceHandlers()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The routers accept the path with or without the /api/v1 prefix; the
		// dispatchers below compare absolute paths, so rebuild one.
		rel := apiRelativePath(r.URL.Path)
		path := "/" + rel
		first := rel
		if i := strings.Index(rel, "/"); i >= 0 {
			first = rel[:i]
		}
		handler, ok := handlers[first]
		if !ok {
			writeJSONError(w, http.StatusNotFound, "not found: "+path)
			return
		}
		handler(w, r, r.Method, path)
	})
}

// dispatchOperationalRest handles the audit, authentication, and system
// route families. Keeping these exact path switches out of restRoute prevents
// the top-level dispatcher from growing past the project's complexity gate
// whenever a new operational endpoint is added.
func (gw *Gateway) dispatchOperationalRest(w http.ResponseWriter, r *http.Request, family, method, path string) {
	switch family {
	case "audit":
		switch {
		case path == "/audit/log" && method == "GET":
			gw.handleAuditLog(w, r)
		case path == "/audit/traces" && method == "GET":
			gw.handleAuditTraces(w, r)
		case path == "/audit/verify" && method == "GET":
			gw.handleAuditVerify(w, r)
		default:
			writeJSONError(w, http.StatusNotFound, "not found: "+r.URL.Path)
		}
	case "auth":
		if path == "/auth/github" && method == "POST" {
			gw.handleGitHubLogin(w, r)
			return
		}
		writeJSONError(w, http.StatusNotFound, "not found: "+r.URL.Path)
	case "system":
		switch {
		case path == "/system/auth-info" && method == "GET":
			gw.handleSystemAuthInfo(w, r)
		case path == "/system/version" && method == "GET":
			gw.handleSystemVersion(w, r)
		case path == "/system/status" && method == "GET":
			gw.handleSystemStatus(w, r)
		case path == "/system/cluster-status" && method == "GET":
			gw.handleClusterStatus(w, r)
		case path == "/system/batch-status" && method == "GET":
			gw.handleBatchStatus(w, r)
		case path == "/system/change-trend" && method == "GET":
			gw.handleChangeTrend(w, r)
		case path == "/system/takeover-events" && method == "GET":
			gw.handleTakeoverEvents(w, r)
		case path == "/system/config" && method == "GET":
			gw.handleSystemConfig(w, r)
		case path == "/system/doctor" && method == "POST":
			gw.handleSystemDoctor(w, r)
		default:
			writeJSONError(w, http.StatusNotFound, "not found: "+r.URL.Path)
		}
	}
}

func (gw *Gateway) dispatchChange(w http.ResponseWriter, r *http.Request, method, path string) {
	switch {
	case path == "/changes/deeplink/approve" && method == "POST":
		gw.handleDeeplinkApprove(w, r)
	case path == "/changes/deeplink/reject" && method == "POST":
		gw.handleDeeplinkReject(w, r)
	case path == "/changes" && method == "GET":
		gw.handleChangeList(w, r)
	case path == "/changes" && method == "POST":
		gw.handleChangeCreate(w, r)
	default:
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(path, "/changes"), "/"), "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid change path")
			return
		}
		rest := ""
		if len(parts) > 1 {
			rest = parts[1]
		}
		gw.dispatchChangeSubPath(w, r, method, parts[0], rest)
	}
}

// dispatchChangeSubPath routes /changes/<id>/<rest>. It is its own function
// because the sub-path rules are a small state machine of their own (mutating
// verbs must be POST, read-only verbs must be GET) and folding it into
// dispatchChange made that function exceed the complexity gate the moment a
// third read-only verb was added.
func (gw *Gateway) dispatchChangeSubPath(w http.ResponseWriter, r *http.Request, method, id, rest string) {
	switch rest {
	case "":
		if method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		gw.handleChangeGet(w, r, id)
	case "plan", "apply", "approve", "reject", "pause", "resume", "cancel", "retry", "rollback", "archive":
		// State-changing actions must use POST. Rejecting other verbs
		// prevents crawlers, link prefetchers and cached GETs from
		// mutating change state (e.g. GET /changes/<id>/pause pausing a
		// run).
		if method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: use POST")
			return
		}
		gw.handleChangeAction(w, r, id, rest)
	case "approvals", "logs", "trace", "events":
		if method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: use GET")
			return
		}
		gw.handleChangeRead(w, r, id, rest)
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown change sub-path: /"+rest)
	}
}

// handleChangeAction runs one mutating change action.
func (gw *Gateway) handleChangeAction(w http.ResponseWriter, r *http.Request, id, action string) {
	switch action {
	case "plan":
		gw.handleChangePlan(w, r, id)
	case "apply":
		gw.handleChangeApply(w, r, id)
	case "approve":
		gw.handleChangeApprove(w, r, id)
	case "reject":
		gw.handleChangeReject(w, r, id)
	case "pause":
		gw.handleChangePause(w, r, id)
	case "resume":
		gw.handleChangeResume(w, r, id)
	case "cancel":
		gw.handleChangeCancel(w, r, id)
	case "retry":
		gw.handleChangeRetry(w, r, id)
	case "rollback":
		gw.handleChangeRollback(w, r, id)
	case "archive":
		gw.handleChangeArchive(w, r, id)
	}
}

// handleChangeRead runs one read-only change action. The three reads share the
// verb rule and nothing else, which is why they dispatch through a tagged
// switch instead of a chain of conditions.
func (gw *Gateway) handleChangeRead(w http.ResponseWriter, r *http.Request, id, which string) {
	switch which {
	case "logs":
		gw.handleChangeLogs(w, r, id)
	case "approvals":
		gw.handleChangeApprovals(w, r, id)
	case "trace":
		gw.handleChangeTrace(w, r, id)
	case "events":
		gw.handleChangeEvents(w, r, id)
	}
}

func (gw *Gateway) dispatchTemplate(w http.ResponseWriter, r *http.Request, method, path string) {
	switch {
	case path == "/templates" && method == "GET":
		gw.handleTemplateList(w, r)
	case path == "/templates" && method == "POST":
		gw.handleTemplateCreate(w, r)
	case path == "/templates/instantiate" && method == "POST":
		gw.handleTemplateInstantiate(w, r)
	default:
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(path, "/templates"), "/"), "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid template path")
			return
		}
		name, rest := parts[0], ""
		if len(parts) > 1 && parts[1] != "" {
			rest = parts[1]
		}
		switch rest {
		case "":
			switch method {
			case "GET":
				gw.handleTemplateGet(w, r, name)
			case "DELETE":
				gw.handleTemplateDelete(w, r, name)
			default:
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		default:
			writeJSONError(w, http.StatusBadRequest, "unknown template sub-path: /"+rest)
		}
	}
}

func (gw *Gateway) dispatchTarget(w http.ResponseWriter, r *http.Request, method, path string) {
	switch {
	case path == "/targets" && method == "GET":
		gw.handleTargetList(w, r)
	case path == "/targets/import" && method == "POST":
		gw.handleTargetImport(w, r)
	case path == "/targets" && method == "POST":
		gw.handleTargetAdd(w, r)
	default:
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(path, "/targets"), "/"), "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid target path")
			return
		}
		id, rest := parts[0], ""
		if len(parts) > 1 && parts[1] != "" {
			rest = parts[1]
		}
		switch rest {
		case "":
			switch method {
			case "GET":
				gw.handleTargetGet(w, r, id)
			case "DELETE":
				gw.handleTargetRemove(w, r, id)
			default:
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case "check":
			gw.handleTargetCheck(w, r, id)
		case "history":
			gw.handleTargetHistory(w, r, id)
		case "status":
			gw.handleTargetStatus(w, r, id)
		default:
			writeJSONError(w, http.StatusBadRequest, "unknown target sub-path: /"+rest)
		}
	}
}

// -----------------------------------------------------------------------
// Changes — RESTful handlers
// -----------------------------------------------------------------------

func (gw *Gateway) handleChangeList(w http.ResponseWriter, r *http.Request) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.ListChangesRequest{}
	q := r.URL.Query()
	if v := q.Get("status"); v != "" {
		req.Statuses = strings.Split(v, ",")
	}
	if v := q.Get("labelContains"); v != "" {
		req.LabelContains = v
	}
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid pageSize: "+v)
			return
		}
		req.PageSize = int32(n)
	}
	if v := q.Get("pageToken"); v != "" {
		req.PageToken = v
	}
	// NOTE: team/environment/sortBy/sortOrder query parameters are
	// deliberately NOT parsed here — the backend never supported them,
	// and silently accepting them made clients believe they worked.
	resp, err := svc.ListChanges(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeCreate(w http.ResponseWriter, r *http.Request) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.CreateChangeRequest{}
	if err := readProto(req)(r); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := svc.CreateChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeGet(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.GetChangeRequest{Id: id}
	if r.URL.Query().Get("includePlan") == "true" {
		req.IncludePlan = true
	}
	resp, err := svc.GetChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangePlan(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		TargetHosts []string `json:"targetHosts"`
		DryRun      bool     `json:"dryRun"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.PlanChangeRequest{ChangeId: id, TargetHosts: body.TargetHosts, DryRun: body.DryRun}
	resp, err := svc.PlanChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeApply(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		AutoApprove    bool  `json:"autoApprove"`
		MaxConcurrency int32 `json:"maxConcurrency"`
		DryRun         bool  `json:"dryRun"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.ApplyChangeRequest{ChangeId: id, AutoApprove: body.AutoApprove, MaxConcurrency: body.MaxConcurrency, DryRun: body.DryRun}
	resp, err := svc.ApplyChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeApprove(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		Approver string `json:"approver"`
		Comment  string `json:"comment,omitempty"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.ApproveRequest{ChangeId: id, Approver: body.Approver, Comment: body.Comment}
	resp, err := svc.ApproveChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeReject(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		Rejecter string `json:"rejecter"`
		Reason   string `json:"reason"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.RejectRequest{ChangeId: id, Rejecter: body.Rejecter, Reason: body.Reason}
	resp, err := svc.RejectChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangePause(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		Reason string `json:"reason,omitempty"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		// reason is optional: an empty body is fine, but a malformed one is
		// a client error and must not be silently ignored.
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.PauseRequest{ChangeId: id, Reason: body.Reason}
	resp, err := svc.PauseChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeResume(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		Reason string `json:"reason,omitempty"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		// reason is optional: an empty body is fine, but a malformed one is
		// a client error and must not be silently ignored.
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.PauseRequest{ChangeId: id, Reason: body.Reason}
	resp, err := svc.ResumeChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeCancel(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		Reason string `json:"reason,omitempty"`
		Force  bool   `json:"force"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.CancelRequest{ChangeId: id, Reason: body.Reason, Force: body.Force}
	resp, err := svc.CancelChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeRetry(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		Replan      bool     `json:"replan"`
		TargetHosts []string `json:"targetHosts"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.RetryRequest{ChangeId: id, Replan: body.Replan, TargetHosts: body.TargetHosts}
	resp, err := svc.RetryChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeRollback(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		RunID       string `json:"runId,omitempty"`
		DryRun      bool   `json:"dryRun"`
		AutoApprove bool   `json:"autoApprove"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.RollbackRequest{ChangeId: id, RunId: body.RunID, DryRun: body.DryRun, AutoApprove: body.AutoApprove}
	resp, err := svc.RollbackChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeArchive(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	body := struct {
		PurgeArtifacts bool `json:"purgeArtifacts"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		// purgeArtifacts defaults to false: an empty body is fine, but a
		// malformed one is a client error and must not be silently ignored.
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req := &pb.ArchiveRequest{ChangeId: id, PurgeArtifacts: body.PurgeArtifacts}
	resp, err := svc.ArchiveChange(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeLogs(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.GetLogsRequest{ChangeId: id}
	q := r.URL.Query()
	if v := q.Get("runId"); v != "" {
		req.RunId = v
	}
	if v := q.Get("levels"); v != "" {
		req.Levels = strings.Split(v, ",")
	}
	if v := q.Get("since"); v != "" {
		// Accept an RFC3339 timestamp; forward as a Unix-seconds lower
		// bound. An unparseable value is a client error, not "no filter".
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid since (want RFC3339): "+v)
			return
		}
		req.Since = ts.Unix()
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid limit: "+v)
			return
		}
		req.Limit = int32(n)
	}
	resp, err := svc.GetLogs(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeApprovals(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	resp, err := svc.GetApprovals(ctx, &pb.GetApprovalsRequest{ChangeId: id})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleChangeTrace(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.GetTraceRequest{ChangeId: id}
	q := r.URL.Query()
	if v := q.Get("runId"); v != "" {
		req.RunId = v
	}
	if v := q.Get("verify"); v == "true" {
		req.Verify = true
	}
	resp, err := svc.GetTrace(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleDeeplinkApprove(w http.ResponseWriter, r *http.Request) {
	if gw.mobileApproval == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "mobile approval service not configured")
		return
	}
	body := struct {
		Token string `json:"token"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "token is required")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	runID, err := gw.mobileApproval.ApproveViaDeepLink(ctx, body.Token)
	if err != nil {
		gw.writeDeeplinkFailure(w, ctx, body.Token)
		return
	}
	// Settle the run from the chain state: a completed quorum moves the
	// run to approved; a partial one (min_approvers > 1) leaves it
	// untouched and the response says so.
	out := map[string]string{"status": "approved"}
	if gw.change != nil {
		settled, serr := gw.change.SettleApproval(ctx, runID)
		if serr != nil {
			slog.Default().Warn("deeplink approve: settle failed (decision is durable; run state not mirrored)",
				"run_id", runID, "error", serr)
		} else if settled == SettlePending {
			out["status"] = "recorded; quorum pending"
		}
	}
	writeJSON(w, out)
}

func (gw *Gateway) handleDeeplinkReject(w http.ResponseWriter, r *http.Request) {
	if gw.mobileApproval == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "mobile approval service not configured")
		return
	}
	body := struct {
		Token string `json:"token"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "token is required")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	runID, err := gw.mobileApproval.RejectViaDeepLink(ctx, body.Token)
	if err != nil {
		gw.writeDeeplinkFailure(w, ctx, body.Token)
		return
	}
	// One-vote veto: settle the rejection onto the run.
	if gw.change != nil {
		if _, serr := gw.change.SettleApproval(ctx, runID); serr != nil {
			slog.Default().Warn("deeplink reject: settle failed (decision is durable; run state not mirrored)",
				"run_id", runID, "error", serr)
		}
	}
	writeJSON(w, map[string]string{"status": "rejected"})
}

// writeDeeplinkFailure answers a one-tap whose token can no longer be
// applied. The interesting case is the second tap: the token is single-use,
// so the first tap already spent it, and the operator's question is whether
// their change went through — not whether the token was well-formed. When
// the spent token still binds to a run whose approval is terminal, the
// answer is 409 with that evidence, which is also what stops the re-tap: the
// mobile page renders it as a done notice instead of a retry prompt.
//
// Everything else keeps the flat 401 ("invalid or expired deeplink token").
// That is not lazy: a token this server never issued proves nothing about
// any run, so the evidence path must not double as a way to probe which runs
// exist and what became of them. The token sentinels come from the push
// package and survive the %w wrapping applied by MobileApprovalService.
func (gw *Gateway) writeDeeplinkFailure(w http.ResponseWriter, ctx context.Context, token string) {
	if gw.mobileApproval != nil {
		if st, ok, err := gw.mobileApproval.DeeplinkState(ctx, token); err == nil && ok && st.Decided {
			decidedAt := int64(0)
			if !st.At.IsZero() {
				decidedAt = st.At.Unix()
			}
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error":      "this change has already been decided",
				"decided":    true,
				"decision":   st.Status,
				"run_id":     st.RunID,
				"decided_by": st.By,
				"decided_at": decidedAt,
			})
			return
		}
	}
	writeJSONError(w, http.StatusUnauthorized, "invalid or expired deeplink token")
}

// writeJSONStatus writes v with an explicit status code. writeJSON is
// hard-wired to 200 and writeJSONError to a one-line message; the deeplink
// evidence response needs a non-2xx code AND a structured body, so neither
// fits.
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	out, err := json.Marshal(v)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(out)
}

// -----------------------------------------------------------------------
// Templates — RESTful handlers
// -----------------------------------------------------------------------

func (gw *Gateway) handleTemplateList(w http.ResponseWriter, r *http.Request) {
	svc := gw.template
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.ListTemplatesRequest{}
	q := r.URL.Query()
	if v := q.Get("nameContains"); v != "" {
		req.NameContains = v
	}
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid pageSize: "+v)
			return
		}
		req.PageSize = int32(n)
	}
	if v := q.Get("pageToken"); v != "" {
		req.PageToken = v
	}
	resp, err := svc.ListTemplates(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleTemplateCreate(w http.ResponseWriter, r *http.Request) {
	svc := gw.template
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.CreateTemplateRequest{}
	if err := readProto(req)(r); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := svc.CreateTemplate(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleTemplateGet(w http.ResponseWriter, r *http.Request, name string) {
	svc := gw.template
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.GetTemplateRequest{Name: name}
	resp, err := svc.GetTemplate(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleTemplateDelete(w http.ResponseWriter, r *http.Request, name string) {
	svc := gw.template
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.DeleteTemplateRequest{Name: name}
	if v := r.URL.Query().Get("force"); v == "true" {
		req.Force = true
	}
	_, err := svc.DeleteTemplate(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func (gw *Gateway) handleTemplateInstantiate(w http.ResponseWriter, r *http.Request) {
	svc := gw.template
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.InstantiateTemplateRequest{}
	if err := readProto(req)(r); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := svc.InstantiateTemplate(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

// -----------------------------------------------------------------------
// Targets — RESTful handlers
// -----------------------------------------------------------------------

func (gw *Gateway) handleTargetList(w http.ResponseWriter, r *http.Request) {
	svc := gw.target
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.ListTargetsRequest{}
	q := r.URL.Query()
	// labelSelector: encode as comma-separated k=v pairs
	if v := q.Get("labelSelector"); v != "" {
		req.LabelSelector = parseLabelSelector(v)
	}
	if v := q.Get("channelType"); v != "" {
		req.ChannelType = v
	}
	if q.Get("reachableOnly") == "true" {
		req.ReachableOnly = true
	}
	// groupId: the console's group tree clicks through to one group's hosts.
	if v := q.Get("groupId"); v != "" {
		req.GroupId = v
	}
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid pageSize: "+v)
			return
		}
		req.PageSize = int32(n)
	}
	if v := q.Get("pageToken"); v != "" {
		req.PageToken = v
	}
	resp, err := svc.ListTargets(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func parseLabelSelector(s string) map[string]string {
	m := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			m[kv[0]] = kv[1]
		}
	}
	return m
}

func (gw *Gateway) handleTargetAdd(w http.ResponseWriter, r *http.Request) {
	svc := gw.target
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.AddTargetRequest{}
	if err := readProto(req)(r); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := svc.AddTarget(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleTargetGet(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.target
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.GetTargetRequest{Id: id}
	resp, err := svc.GetTarget(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleTargetRemove(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.target
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.RemoveTargetRequest{Id: id}
	if v := r.URL.Query().Get("force"); v == "true" {
		req.Force = true
	}
	_, err := svc.RemoveTarget(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func (gw *Gateway) handleTargetCheck(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.target
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.CheckTargetRequest{Id: id}
	q := r.URL.Query()
	if q.Get("fresh") == "true" {
		req.Fresh = true
	}
	if v := q.Get("timeoutSeconds"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid timeoutSeconds: "+v)
			return
		}
		req.TimeoutSeconds = int32(n)
	}
	resp, err := svc.CheckTarget(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

// -----------------------------------------------------------------------
// Audit — RESTful handlers
// -----------------------------------------------------------------------

func (gw *Gateway) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	svc := gw.audit
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.GetAuditLogRequest{}
	q := r.URL.Query()
	if v := q.Get("changeId"); v != "" {
		req.ChangeId = v
	}
	if v := q.Get("runId"); v != "" {
		req.RunId = v
	}
	if v := q.Get("actor"); v != "" {
		req.Actor = v
	}
	if v := q.Get("action"); v != "" {
		req.Action = v
	}
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid since: "+v)
			return
		}
		req.Since = n
	}
	if v := q.Get("until"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid until: "+v)
			return
		}
		req.Until = n
	}
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid pageSize: "+v)
			return
		}
		req.PageSize = int32(n)
	}
	if v := q.Get("pageToken"); v != "" {
		req.PageToken = v
	}
	resp, err := svc.GetAuditLog(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleAuditTraces(w http.ResponseWriter, r *http.Request) {
	svc := gw.audit
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.ListAuditTracesRequest{}
	q := r.URL.Query()
	if v := q.Get("changeId"); v != "" {
		req.ChangeId = v
	}
	if v := q.Get("runIds"); v != "" {
		req.RunIds = strings.Split(v, ",")
	}
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid since: "+v)
			return
		}
		req.Since = n
	}
	if v := q.Get("until"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid until: "+v)
			return
		}
		req.Until = n
	}
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid pageSize: "+v)
			return
		}
		req.PageSize = int32(n)
	}
	if v := q.Get("pageToken"); v != "" {
		req.PageToken = v
	}
	resp, err := svc.ListAuditTraces(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

// handleAuditVerify serves GET /audit/verify (the RESTful path; the
// /api/v1/AuditService/VerifyHashChain form still returns the proto shape
// alone).
//
// It returns the per-run TRACE chain result exactly as the gRPC method produces
// it, plus the GLOBAL audit chain under "auditChain". The two are merged into a
// single top-level "valid" because an operator asking "is the audit log
// trustworthy" wants one answer, not two they have to combine by hand.
//
// The audit chain is not part of VerifyHashChainResponse because that message
// is generated from levee.proto, which cannot be regenerated in this
// environment (protoc is unavailable) and whose fields are all run-scoped —
// forcing a single global chain into the per-run RunVerification list would
// misreport what it covers.
func (gw *Gateway) handleAuditVerify(w http.ResponseWriter, r *http.Request) {
	svc := gw.audit
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.VerifyHashChainRequest{}
	q := r.URL.Query()
	if v := q.Get("runId"); v != "" {
		req.RunId = v
	}
	if v := q.Get("changeId"); v != "" {
		req.ChangeId = v
	}
	resp, err := svc.VerifyHashChain(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}

	// Marshal the proto first, then splice the audit chain in as raw JSON, so
	// no protojson field is re-encoded (int64 counts would round-trip through
	// float64 otherwise).
	out, err := protojson.Marshal(resp)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	body := map[string]json.RawMessage{}
	if err := json.Unmarshal(out, &body); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "decode error: "+err.Error())
		return
	}

	if svc.store == nil {
		writeJSONError(w, http.StatusInternalServerError, "store not configured")
		return
	}
	builder, err := audit.NewAuditChainBuilder(svc.store)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create audit chain builder: "+err.Error())
		return
	}
	result, err := builder.Verify(ctx)
	if err != nil {
		writeGRPCError(w, status.Errorf(grpccodes.Internal, "verify audit chain: %v", err))
		return
	}

	chain, err := json.Marshal(auditChainResultBody(result))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	body["auditChain"] = chain

	// Fold the audit chain into the top-level verdict. The trace-chain
	// fields are left untouched: a broken trace chain is a fact about a run,
	// and overwriting it with an audit-chain id would lose that.
	if !result.Valid {
		if resp.Valid {
			body["valid"] = json.RawMessage("false")
		}
		if len(result.Failures) > 0 && resp.BrokenEntryId == "" {
			f := result.Failures[0]
			body["brokenEntryId"] = mustMarshalJSON(f.AuditID)
			body["brokenReason"] = mustMarshalJSON("audit_chain:" + f.Type.String())
		}
	}

	merged, err := json.Marshal(body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(merged)
}

// auditChainFailureBody is the JSON shape of one broken audit row.
type auditChainFailureBody struct {
	AuditID      string `json:"auditId"`
	Index        int    `json:"index"`
	Type         string `json:"type"`
	Expected     string `json:"expected"`
	Actual       string `json:"actual"`
	PrevExpected string `json:"prevExpected"`
	PrevActual   string `json:"prevActual"`
}

// auditChainBody is the JSON shape of the "auditChain" member of the verify
// response. Failures is always a non-nil array so clients can iterate it
// without a null check.
type auditChainBody struct {
	Valid            bool                    `json:"valid"`
	EntriesVerified  int                     `json:"entriesVerified"`
	Unsealed         int                     `json:"unsealed"`
	FirstFailureType string                  `json:"firstFailureType,omitempty"`
	Failures         []auditChainFailureBody `json:"failures"`
}

// auditChainResultBody converts a verification result into its JSON shape.
func auditChainResultBody(r *audit.AuditChainResult) auditChainBody {
	out := auditChainBody{
		Valid:           r.Valid,
		EntriesVerified: r.Count,
		Unsealed:        r.Unsealed,
		Failures:        []auditChainFailureBody{},
	}
	for _, f := range r.Failures {
		if out.FirstFailureType == "" {
			out.FirstFailureType = f.Type.String()
		}
		out.Failures = append(out.Failures, auditChainFailureBody{
			AuditID:      f.AuditID,
			Index:        f.Index,
			Type:         f.Type.String(),
			Expected:     f.Expected,
			Actual:       f.Actual,
			PrevExpected: f.PrevExpected,
			PrevActual:   f.PrevActual,
		})
	}
	return out
}

func mustMarshalJSON(v string) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

// -----------------------------------------------------------------------
// System — RESTful handlers
// -----------------------------------------------------------------------

// handleSystemAuthInfo serves the public authentication descriptor consumed
// by the login page. It is intentionally unauthenticated (exempted in
// authMiddleware): the browser must learn which SSO options are offered
// before it holds any credential. The response carries only non-secret
// discovery data — client_id is public by design in both OAuth and OIDC,
// and endpoint URLs are the providers' own published values.
func (gw *Gateway) handleSystemAuthInfo(w http.ResponseWriter, _ *http.Request) {
	v := gw.cfg.OIDC
	gh := gw.cfg.GitHub
	resp := map[string]any{
		"oidcEnabled": v.Enabled(),
	}
	if authorizeURL, tokenURL := v.Endpoints(); authorizeURL != "" {
		resp["issuerUrl"] = v.IssuerURL()
		resp["clientId"] = v.ClientID()
		resp["authorizeUrl"] = authorizeURL
		resp["tokenUrl"] = tokenURL
	}
	if gh.Enabled() {
		resp["githubEnabled"] = true
		resp["githubClientId"] = gh.ClientID()
		if org := gh.Org(); org != "" {
			resp["githubOrg"] = org
		}
	}
	writeJSON(w, resp)
}

// -----------------------------------------------------------------------
// SSO login — RESTful handlers
// -----------------------------------------------------------------------

// handleGitHubLogin is the server-side half of the GitHub OAuth flow. The
// browser POSTs {"code","state","redirect_uri"} here; the gateway exchanges
// the code with GitHub (client_secret never leaves the process), applies
// the org restriction and team role mapping, mints a LEVEE session token
// and returns {"token","subject","roles"}.
//
// SECURITY: this endpoint is unauthenticated by necessity (the caller holds
// no credential yet). The CSRF state is validated here — the browser sent
// it through GitHub's redirect, and the gateway compares it against the
// value the login page stashed server-side at flow start. A replayed or
// forged code fails GitHub's token exchange; a stolen code is single-use
// and expires in ~10 minutes.
func (gw *Gateway) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if !gw.cfg.GitHub.Enabled() || gw.cfg.Sessions == nil {
		writeJSONError(w, http.StatusNotFound, "github login not enabled")
		return
	}
	var req struct {
		Code        string `json:"code"`
		State       string `json:"state"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := readJSONBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Code == "" || req.State == "" {
		writeJSONError(w, http.StatusBadRequest, "code and state are required")
		return
	}
	// The redirect_uri must match the value used at authorization time
	// (GitHub enforces this on the exchange); the frontend always uses the
	// canonical callback origin, so reconstruct it rather than trusting a
	// client-supplied value.
	redirectURI := canonicalSSOCallbackURL(r)
	if req.RedirectURI != "" && req.RedirectURI != redirectURI {
		writeJSONError(w, http.StatusBadRequest, "redirect_uri mismatch")
		return
	}

	id, err := gw.cfg.GitHub.ExchangeCode(r.Context(), req.Code, redirectURI)
	if err != nil {
		// A failed exchange means a bad/expired code or a rejected user —
		// not the caller's fault; keep it 401 so the frontend can prompt a
		// fresh login.
		writeJSONError(w, http.StatusUnauthorized, err.Error())
		return
	}
	// GitHub is an identity provider, not a tenancy provider: an OAuth
	// exchange proves WHO signed in and nothing about which tenant they belong
	// to. Letting the browser name its own tenant here would make isolation
	// self-asserted, so the session is minted unbound and the tenant resolver
	// refuses it while multi-tenancy is enabled.
	//
	// A multi-tenant deployment that wants GitHub logins must either
	// front them with a tenant-aware IdP (configure auth.oidc.tenant_claim)
	// or use named bearer tokens with auth_tokens[].tenant. Silently guessing
	// a tenant here would be the exact failure this design exists to prevent.
	token, err := gw.cfg.Sessions.Issue(id.Login, id.Roles, "github", "")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "issue session token")
		return
	}
	writeJSON(w, map[string]any{
		"token":   token,
		"subject": id.Login,
		"roles":   id.Roles,
	})
}

// canonicalSSOCallbackURL derives the SSO callback redirect_uri from the
// request's own origin: "<scheme>://<host>/login/callback". Proxies are
// respected via X-Forwarded-Proto / X-Forwarded-Host when present.
func canonicalSSOCallbackURL(r *http.Request) string {
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host + "/login/callback"
}

// readJSONBody decodes a small JSON request body into out with a 1MB cap.
func readJSONBody(r *http.Request, out any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

func (gw *Gateway) handleSystemVersion(w http.ResponseWriter, r *http.Request) {
	svc := gw.system
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	resp, err := svc.GetVersion(ctx, &emptypb.Empty{})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	svc := gw.system
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	resp, err := svc.GetStatus(ctx, &emptypb.Empty{})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

// handleClusterStatus serves a read-only view of the cluster's dispatch
// state: registered nodes and assignment summary (counts per state and
// per-node active load). It queries the store directly rather than going
// through a gRPC service, because the data lives in the state layer that
// the gateway already holds.
func (gw *Gateway) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if gw.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no store configured")
		return
	}
	// The node registry is deployment-wide like groups/templates/config: it
	// declares no environment, so it is judged in permission.default_env rather
	// than by parsing an address. Before this gate the endpoint answered every
	// authenticated caller, because it reads the store directly and never
	// reaches a service's authorizer.
	if err := authorizeResourceRead(ctx, gw.authz, gw.authz.ResolveEnv(""), "GetClusterStatus"); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}

	nodes, err := gw.store.ListClusterNodes(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("list nodes: %v", err))
		return
	}
	summary, err := gw.store.AssignmentSummary(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("assignment summary: %v", err))
		return
	}

	// ?capability=<name> narrows the registry to the nodes that DECLARED that
	// module. The matching rule belongs to internal/capability rather than being
	// re-derived here: the gateway writes nothing about capabilities, and a second
	// copy of "what counts as a match" is how a filter and a card start disagreeing.
	//
	// It filters the node list only. `summary` stays deployment-wide, because
	// assignments are placed by load, not by capability — putting "3 nodes declare
	// pkg" beside "17 active assignments" without saying so would imply the
	// scheduler honours the declaration. It does not; see internal/dispatch.
	nodes = filterNodesByCapability(nodes, strings.TrimSpace(r.URL.Query().Get("capability")))

	// `backend` answers "which database", `cluster_enabled` answers "is
	// coordination running". They part ways the moment a deliberate
	// single-node PostgreSQL deployment asks, and the console has to say so:
	// reading one off the other told those operators their storage was a
	// half-built cluster.
	writeJSON(w, map[string]any{
		"nodes":           nodes,
		"summary":         summary,
		"backend":         backendLabel(gw.store),
		"cluster_enabled": gw.cfg.ClusterEnabled,
	})
}

// filterNodesByCapability keeps the nodes that declare want. An empty want
// returns the list untouched — nilness included — so without the parameter the
// payload is exactly what it has always been, and the only way to be shown a
// narrowed list is to ask for one. With the parameter the result is always a
// slice, so "nothing matches" answers `[]` rather than null, and a client that
// iterates the field cannot crash on a shape only the filtered path produces.
func filterNodesByCapability(nodes []state.ClusterNode, want string) []state.ClusterNode {
	if want == "" {
		return nodes
	}
	kept := make([]state.ClusterNode, 0, len(nodes))
	for _, n := range nodes {
		if capability.Has(n.Capabilities, want) {
			kept = append(kept, n)
		}
	}
	return kept
}

// handleBatchStatus serves the per-batch progress of a run (cluster v2
// observability). Read-only; queries the batches table directly.
func (gw *Gateway) handleBatchStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if gw.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no store configured")
		return
	}

	runID := r.URL.Query().Get("run_id")
	if strings.TrimSpace(runID) == "" {
		writeJSONError(w, http.StatusBadRequest, "run_id query parameter is required")
		return
	}
	// Judged on the RUN's environment, exactly like every other run-scoped read
	// (see ChangeService.authorizeRead): a subject entitled to prod only must
	// not read a staging run's batch progress. The gate loads the run first, so
	// an unknown id is 404 before it is 403 — an id typo is not a policy
	// question (the same ordering the SSE route uses).
	run, err := gw.change.RunForGate(ctx, runID)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	if run == nil {
		// 404 BEFORE the gate: GetRun answers (nil, nil) for an unknown id, and
		// judging a nil run would resolve an empty environment to default_env and
		// refuse a subject who has every right to look — sending the operator
		// hunting a permission problem that does not exist.
		writeJSONError(w, http.StatusNotFound, "change "+runID+" not found")
		return
	}
	if err := authorizeResourceRead(ctx, gw.authz, envOf(run), "GetBatchStatus"); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}

	summary, err := gw.store.BatchSummary(ctx, runID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("batch summary: %v", err))
		return
	}
	if summary == nil {
		// No batches (or missing table): return an empty summary rather than 404
		// so the frontend can render "no batch data" uniformly.
		summary = &state.BatchSummary{Batches: []state.BatchProgress{}}
	}
	// The per-host rows travel with the aggregates: the batch bar says "this
	// batch is 60% accounted for", which is the one question the monitor page
	// could not answer — WHICH host is in which state. The steps table is the
	// only place that is written, and this is its only read path.
	steps, serr := gw.stepRows(ctx, runID)
	if serr != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("step rows: %v", serr))
		return
	}
	writeJSON(w, batchStatusPayload{BatchSummary: summary, Steps: steps})
}

// batchStatusPayload is what /system/batch-status returns: the per-batch
// aggregates (state.BatchSummary, exactly as the store hands them over) plus
// the per-host rows the monitor's target map renders.
type batchStatusPayload struct {
	*state.BatchSummary
	Steps []stepPayload `json:"steps,omitempty"`
}

// stepPayload is one host's row in one batch. ExitCode is omitted when the
// engine did not record one, rather than rendered as 0 (an exit code of 0 is
// a fact, a missing one is the absence of one).
type stepPayload struct {
	Host       string `json:"host"`
	BatchNo    int    `json:"batch_no"`
	StepName   string `json:"step_name"`
	Status     string `json:"status"`
	DurationMs int    `json:"duration_ms"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

// stepRows resolves the steps table's batch_id (a row id) to the batch NUMBER
// the rest of the payload speaks in, so a client can put a host in a column
// without a second request. The join lives here rather than in the store
// because the store has no step↔batch join and the monitor is the only reader.
func (gw *Gateway) stepRows(ctx context.Context, runID string) ([]stepPayload, error) {
	steps, err := gw.store.ListSteps(ctx, state.StepFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, nil
	}
	batches, err := gw.store.ListBatches(ctx, state.BatchFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	noOf := make(map[string]int, len(batches))
	for _, b := range batches {
		noOf[b.ID] = b.BatchNo
	}
	out := make([]stepPayload, 0, len(steps))
	for _, st := range steps {
		if st == nil {
			continue
		}
		out = append(out, stepPayload{
			Host: st.Host, BatchNo: noOf[st.BatchID], StepName: st.StepName,
			Status: st.Status, DurationMs: st.DurationMs, ExitCode: st.ExitCode,
		})
	}
	return out, nil
}

// backendLabel returns a human-readable backend name for the store. It defers
// to state.StoreDriver, which owns the vocabulary, so that this endpoint and
// SystemService.GetStatus cannot report two different backends for one process —
// the disagreement that made `store_type` untrustworthy is exactly a second,
// hand-written copy of this switch.
func backendLabel(s state.Store) string {
	if name := state.StoreDriver(s); name != "" {
		return name
	}
	return "unknown"
}

func (gw *Gateway) handleSystemConfig(w http.ResponseWriter, r *http.Request) {
	svc := gw.system
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	req := &pb.GetConfigRequest{}
	q := r.URL.Query()
	// Redaction is the DEFAULT: the gateway sends redact_secrets=true
	// unless the client explicitly passes ?redactSecrets=false. Omitting
	// the flag must never leak credentials.
	req.RedactSecrets = q.Get("redactSecrets") != "false"
	if v := q.Get("section"); v != "" {
		req.Section = v
	}
	resp, err := svc.GetConfig(ctx, req)
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}

func (gw *Gateway) handleSystemDoctor(w http.ResponseWriter, r *http.Request) {
	svc := gw.system
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	resp, err := svc.RunDoctor(ctx, &emptypb.Empty{})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeProto(w, resp)
}
func readProto(req proto.Message) func(r *http.Request) error {
	return func(r *http.Request) error {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}
		if len(body) == 0 {
			return nil
		}
		return protojson.Unmarshal(body, req)
	}
}

// writeProto marshals a proto message to JSON and writes it as the response.
func writeProto(w http.ResponseWriter, v proto.Message) {
	out, err := protojson.Marshal(v)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// writeJSON writes any struct as JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	out, err := json.Marshal(v)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// -----------------------------------------------------------------------
// ChangeService
// -----------------------------------------------------------------------

func (gw *Gateway) handleChange(w http.ResponseWriter, r *http.Request, method string) { //nolint:gocyclo // inherently complex: 20+ route branches for all REST endpoints
	svc := gw.change
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch method {
	case "CreateChange":
		req := &pb.CreateChangeRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.CreateChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "CloneChange":
		req := &pb.CloneChangeRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.CloneChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "PlanChange":
		req := &pb.PlanChangeRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.PlanChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "ApplyChange":
		req := &pb.ApplyChangeRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ApplyChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "PauseChange":
		req := &pb.PauseRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.PauseChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "ResumeChange":
		req := &pb.PauseRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ResumeChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "PauseAll":
		req := &pb.PauseAllRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.PauseAll(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "ResumeAll":
		req := &pb.PauseAllRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ResumeAll(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "CancelChange":
		req := &pb.CancelRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.CancelChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "RetryChange":
		req := &pb.RetryRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.RetryChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "RetryHost":
		req := &pb.RetryHostRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.RetryHost(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "RollbackChange":
		req := &pb.RollbackRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.RollbackChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "ApproveChange":
		req := &pb.ApproveRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ApproveChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "RejectChange":
		req := &pb.RejectRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.RejectChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetChange":
		req := &pb.GetChangeRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "ListChanges":
		req := &pb.ListChangesRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ListChanges(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "ArchiveChange":
		req := &pb.ArchiveRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ArchiveChange(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetLogs":
		req := &pb.GetLogsRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetLogs(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetDiff":
		req := &pb.GetDiffRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetDiff(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetTrace":
		req := &pb.GetTraceRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetTrace(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// TemplateService
// -----------------------------------------------------------------------

func (gw *Gateway) handleTemplate(w http.ResponseWriter, r *http.Request, method string) {
	svc := gw.template
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch method {
	case "CreateTemplate":
		req := &pb.CreateTemplateRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.CreateTemplate(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetTemplate":
		req := &pb.GetTemplateRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetTemplate(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "ListTemplates":
		req := &pb.ListTemplatesRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ListTemplates(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "DeleteTemplate":
		req := &pb.DeleteTemplateRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		_, err := svc.DeleteTemplate(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case "InstantiateTemplate":
		req := &pb.InstantiateTemplateRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.InstantiateTemplate(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// TargetService
// -----------------------------------------------------------------------

func (gw *Gateway) handleTarget(w http.ResponseWriter, r *http.Request, method string) {
	svc := gw.target
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch method {
	case "AddTarget":
		req := &pb.AddTargetRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.AddTarget(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "RemoveTarget":
		req := &pb.RemoveTargetRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		_, err := svc.RemoveTarget(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case "ListTargets":
		req := &pb.ListTargetsRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ListTargets(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetTarget":
		req := &pb.GetTargetRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetTarget(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "CheckTarget":
		req := &pb.CheckTargetRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.CheckTarget(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// AuditService
// -----------------------------------------------------------------------

func (gw *Gateway) handleAudit(w http.ResponseWriter, r *http.Request, method string) {
	svc := gw.audit
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch method {
	case "GetAuditLog":
		req := &pb.GetAuditLogRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetAuditLog(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "ListAuditTraces":
		req := &pb.ListAuditTracesRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ListAuditTraces(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "VerifyHashChain":
		req := &pb.VerifyHashChainRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.VerifyHashChain(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetRunReport":
		req := &pb.GetRunReportRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetRunReport(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// SystemService
// -----------------------------------------------------------------------

func (gw *Gateway) handleSystem(w http.ResponseWriter, r *http.Request, method string) {
	svc := gw.system
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch method {
	case "GetVersion":
		resp, err := svc.GetVersion(ctx, &emptypb.Empty{})
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetStatus":
		resp, err := svc.GetStatus(ctx, &emptypb.Empty{})
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetConfig":
		req := &pb.GetConfigRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetConfig(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "RunDoctor":
		resp, err := svc.RunDoctor(ctx, &emptypb.Empty{})
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// AlertService (via pb.AlertServiceServer interface)
// -----------------------------------------------------------------------

func (gw *Gateway) handleAlert(w http.ResponseWriter, r *http.Request, method string) {
	svc := gw.alert
	if svc == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "AlertService not configured")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch method {
	case "ReceiveAlert":
		req := &pb.AlertMessage{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ReceiveAlert(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetAlertStatus":
		req := &pb.GetAlertStatusRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetAlertStatus(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "SubscribeAlerts":
		writeJSONError(w, http.StatusNotImplemented, "streaming RPCs not supported")
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// DiagnosisService (via pb.DiagnosisServiceServer interface)
// -----------------------------------------------------------------------

func (gw *Gateway) handleDiagnosis(w http.ResponseWriter, r *http.Request, method string) {
	svc := gw.diag
	if svc == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "DiagnosisService not configured")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch method {
	case "Diagnose":
		req := &pb.DiagnoseRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.Diagnose(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "GetDiagnosis":
		req := &pb.GetDiagnosisRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetDiagnosis(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// ConversationService (via pb.ConversationServiceServer interface)
// -----------------------------------------------------------------------

func (gw *Gateway) handleConversation(w http.ResponseWriter, r *http.Request, method string) {
	svc := gw.conv
	if svc == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ConversationService not configured")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch method {
	case "SendMessage":
		req := &pb.SendMessageRequest{}
		if err := readProto(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.SendMessage(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProto(w, resp)
	case "SubscribeConversation":
		writeJSONError(w, http.StatusNotImplemented, "streaming RPCs not supported")
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// Conversation session management (REST-only: not gRPC methods)
// -----------------------------------------------------------------------

// dispatchConversation routes /conversation/* paths to the appropriate
// session-management handler. These endpoints call the ConversationEngine
// directly rather than going through gRPC, because NewSession/ListSessions/
// GetSession/CloseSession are not part of the ConversationService gRPC API.
func (gw *Gateway) dispatchConversation(w http.ResponseWriter, r *http.Request, method, path string) {
	if gw.convEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "conversation engine not configured")
		return
	}
	// These four handlers talk to the ConversationEngine directly because
	// NewSession/ListSessions/GetSession/CloseSession are not part of the
	// ConversationService gRPC API — which also means SendMessage's gate does
	// not cover them. Judged here, once, from the same authorizer and with the
	// same default-env fallback the gRPC side uses: a GET is a read (`view`),
	// anything that opens or closes a session is `plan`, the token the
	// dialogue's only mutation is policed under anyway.
	rpc := "conversation " + strings.ToLower(method)
	if method == http.MethodGet {
		if err := authorizeResourceRead(r.Context(), gw.authz, gw.authz.ResolveEnv(""), rpc); err != nil {
			writeGRPCError(w, err)
			return
		}
	} else if err := authorizeResource(r.Context(), gw.authz, gw.authz.ResolveEnv(""), permission.ActionPlan, rpc); err != nil {
		writeGRPCError(w, err)
		return
	}
	// /conversation/sessions
	if path == "/conversation/sessions" || path == "/conversation/sessions/" {
		switch method {
		case http.MethodPost:
			gw.handleConversationNewSession(w, r)
		case http.MethodGet:
			gw.handleConversationListSessions(w, r)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, method+" not allowed")
		}
		return
	}
	// /conversation/sessions/{id} and /conversation/sessions/{id}/messages
	rest := strings.TrimPrefix(path, "/conversation/sessions/")
	if idx := strings.Index(rest, "/"); idx > 0 {
		sid := rest[:idx]
		suffix := rest[idx:]
		if suffix == "/messages" {
			gw.handleConversationPostMessage(w, r, sid)
			return
		}
	}
	if rest != "" && !strings.Contains(rest, "/") {
		switch method {
		case http.MethodGet:
			gw.handleConversationGetSession(w, r, rest)
		case http.MethodDelete:
			gw.handleConversationCloseSession(w, r, rest)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, method+" not allowed")
		}
		return
	}
	writeJSONError(w, http.StatusNotFound, "not found: "+path)
}

// conversationPrincipal resolves the user identity that owns conversation
// sessions (P2-2). An authenticated subject — named token / OIDC / web
// session, injected by authMiddleware under actorKey — always wins over any
// client-asserted user_id. The asserted value is the fallback for
// development mode and legacy static-token callers, which carry no verified
// subject (same trust model as actorFromCtx).
func conversationPrincipal(r *http.Request, asserted string) string {
	if v, ok := r.Context().Value(actorKey{}).(string); ok && v != "" {
		return v
	}
	return strings.TrimSpace(asserted)
}

// requireConversationOwner writes a 403 and returns false when principal is
// known and does not own sess. An empty principal (development mode with no
// client assertion) is allowed through, preserving --insecure usability.
func requireConversationOwner(w http.ResponseWriter, sess *conversation.Session, principal string) bool {
	if principal != "" && sess.UserID != principal {
		writeJSONError(w, http.StatusForbidden, "session belongs to another user")
		return false
	}
	return true
}

// handleConversationNewSession creates a new conversation session.
func (gw *Gateway) handleConversationNewSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID  string `json:"user_id"`
		AlertID string `json:"alert_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	principal := conversationPrincipal(r, req.UserID)
	if principal == "" {
		writeJSONError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	var sess *conversation.Session
	var err error
	if req.AlertID != "" {
		sess, err = gw.convEngine.NewSessionFromAlert(principal, req.AlertID)
	} else {
		sess, err = gw.convEngine.NewSession(principal)
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"session": sessToDTO(sess)})
}

// handleConversationListSessions lists sessions for a user.
func (gw *Gateway) handleConversationListSessions(w http.ResponseWriter, r *http.Request) {
	principal := conversationPrincipal(r, r.URL.Query().Get("user_id"))
	if principal == "" {
		writeJSONError(w, http.StatusBadRequest, "user_id query parameter is required")
		return
	}
	sessions := gw.convEngine.ListSessions(principal)
	dtos := make([]map[string]any, 0, len(sessions))
	for _, s := range sessions {
		dtos = append(dtos, sessToDTO(s))
	}
	writeJSON(w, map[string]any{"sessions": dtos})
}

// handleConversationGetSession returns a single session with its history.
func (gw *Gateway) handleConversationGetSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	sess, err := gw.convEngine.GetSession(sessionID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	if !requireConversationOwner(w, sess, conversationPrincipal(r, r.URL.Query().Get("user_id"))) {
		return
	}
	writeJSON(w, map[string]any{"session": sessToDTO(sess)})
}

// handleConversationPostMessage sends a message to a session and returns
// the engine's reply.
func (gw *Gateway) handleConversationPostMessage(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req struct {
		UserID string `json:"user_id"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Ownership (P2-2): the resolved principal (authenticated subject, or
	// the client-asserted fallback in development mode) must own the
	// session; the engine re-checks inside HandleMessage via ErrNotOwner.
	reply, err := gw.convEngine.HandleMessage(r.Context(), sessionID, conversationPrincipal(r, req.UserID), req.Text)
	if err != nil {
		switch {
		case errors.Is(err, conversation.ErrEmptyMessage):
			writeJSONError(w, http.StatusBadRequest, "text is required")
		case errors.Is(err, conversation.ErrSessionNotFound):
			writeJSONError(w, http.StatusNotFound, "session not found")
		case errors.Is(err, conversation.ErrNotOwner):
			writeJSONError(w, http.StatusForbidden, "session belongs to another user")
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, map[string]any{"reply": reply})
}

// handleConversationCloseSession closes (removes) a session.
func (gw *Gateway) handleConversationCloseSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	principal := conversationPrincipal(r, r.URL.Query().Get("user_id"))
	if principal != "" {
		sess, err := gw.convEngine.GetSession(sessionID)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "session not found")
			return
		}
		if !requireConversationOwner(w, sess, principal) {
			return
		}
	}
	if err := gw.convEngine.CloseSession(sessionID); err != nil {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessToDTO builds a JSON-safe view of a conversation.Session (locks the
// session only long enough to copy the needed fields).
func sessToDTO(s *conversation.Session) map[string]any {
	return map[string]any{
		"id":         s.ID,
		"user_id":    s.UserID,
		"alert_id":   s.AlertID,
		"state":      s.GetState().String(),
		"messages":   s.History(),
		"created_at": s.CreatedAt,
		"updated_at": s.UpdatedAt,
	}
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// extractAuth reads the Authorization header and returns gRPC metadata.
func extractAuth(r *http.Request) metadata.MD {
	md := metadata.MD{}
	if auth := r.Header.Get("Authorization"); auth != "" {
		md.Set("authorization", auth)
	}
	// The alert-source ingress identity rides the same metadata as the bearer
	// token, so the in-process AlertService reads one transport shape whether
	// the request arrived over gRPC or through this gateway. Absent headers are
	// simply not set: the service decides what a missing credential means.
	if s := strings.TrimSpace(r.Header.Get(alert.SourceHeaderName)); s != "" {
		md.Set(alert.SourceMetadataKey, s)
	}
	if t := r.Header.Get(alert.TokenHeaderName); t != "" {
		md.Set(alert.TokenMetadataKey, t)
	}
	return md
}

// writeGRPCError maps a gRPC status error to an HTTP error response.
//
// Internal codes are scrubbed: the response body carries a generic
// "internal error" message while the real cause is logged server-side.
// Raw store/driver messages have previously leaked connection strings
// and SQL errors to API clients.
func writeGRPCError(w http.ResponseWriter, err error) {
	st, ok := status.FromError(err)
	if !ok {
		// Not a gRPC status at all: treat as internal, never echo the text.
		slog.Default().Error("rest gateway internal error", "message", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	switch st.Code() {
	case grpccodes.InvalidArgument:
		writeJSONError(w, http.StatusBadRequest, st.Message())
	case grpccodes.NotFound:
		writeJSONError(w, http.StatusNotFound, st.Message())
	case grpccodes.AlreadyExists:
		writeJSONError(w, http.StatusConflict, st.Message())
	case grpccodes.PermissionDenied:
		writeJSONError(w, http.StatusForbidden, st.Message())
	case grpccodes.FailedPrecondition:
		writeJSONError(w, http.StatusPreconditionFailed, st.Message())
	case grpccodes.Unimplemented:
		writeJSONError(w, http.StatusNotImplemented, st.Message())
	case grpccodes.Unavailable:
		writeJSONError(w, http.StatusServiceUnavailable, st.Message())
	case grpccodes.Unauthenticated:
		writeJSONError(w, http.StatusUnauthorized, st.Message())
	case grpccodes.DeadlineExceeded:
		writeJSONError(w, http.StatusGatewayTimeout, st.Message())
	case grpccodes.ResourceExhausted:
		writeJSONError(w, http.StatusTooManyRequests, st.Message())
	default:
		slog.Default().Error("rest gateway internal error", "code", st.Code().String(), "message", st.Message())
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, statusCode int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}

// corsMiddleware wraps h with CORS headers.
//
// SECURITY: an empty origins list denies all cross-origin requests —
// wildcard access requires an explicit "*" entry. Same-origin requests
// (no Origin header) always pass through; CORS does not apply to them.
func corsMiddleware(origins []string, h http.Handler) http.Handler {
	var allowAll bool
	originSet := make(map[string]bool, len(origins))
	for _, o := range origins {
		if o == "*" {
			allowAll = true
			break
		}
		originSet[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allowAll || originSet[origin]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// sharedRateLimiter lazily initialises and returns the gateway-wide token
// bucket shared by BOTH route trees. It returns nil when rate limiting is
// explicitly disabled via a negative RatePerSec.
func (gw *Gateway) sharedRateLimiter() *rate.Limiter {
	gw.limiterOnce.Do(func() {
		rps := gw.cfg.RatePerSec
		burst := gw.cfg.RateBurst
		if rps == 0 {
			rps = DefaultRatePerSec
		}
		if burst == 0 {
			burst = DefaultRateBurst
		}
		if rps < 0 {
			// Explicitly disabled; leave gw.limiter nil.
			return
		}
		gw.limiter = rate.NewLimiter(rate.Limit(rps), burst)
	})
	return gw.limiter
}

// wrapWithLimiter enforces lim against h, responding 429 with a
// Retry-After hint when the bucket is exhausted. A nil limiter means
// rate limiting is disabled and h is returned unchanged.
func wrapWithLimiter(lim *rate.Limiter, h http.Handler) http.Handler {
	if lim == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !lim.Allow() {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware wraps h with the gateway's SHARED token bucket (see
// serveOn: one limiter covers both route trees). Rate limiting is disabled
// when cfg.RatePerSec is negative.
func (gw *Gateway) rateLimitMiddleware(h http.Handler) http.Handler {
	return wrapWithLimiter(gw.sharedRateLimiter(), h)
}

// requestIDHeaderName is the HTTP header used to propagate the request
// id to REST clients (mirrors the gRPC metadata key).
const requestIDHeaderName = "X-Request-Id"

// actingAsHeaderName is the HTTP header used to assert the actor a client
// acts on behalf of; it mirrors the "x-actor" gRPC metadata key.
const actingAsHeaderName = "X-Acting-As"

// requestIDMiddleware assigns a per-request id (honouring a client
// supplied X-Request-Id), echoes it on the response and exposes it to
// handlers through the request context. The actor identity is resolved with
// the authenticated subject taking precedence over the client assertion (see
// SECURITY note).
//
// Both values are sanitized (control characters stripped, length capped);
// an unusable client request id is replaced by a generated one.
//
// SECURITY: when the request authenticates with a named bearer token,
// authMiddleware injects the token's subject into the context and that proven
// identity wins here. Only when no authenticated subject is present do we fall
// back to the client-asserted X-Acting-As header, which is then an ASSERTION
// only — any token holder may claim any name. Use it for audit attribution,
// never for authorization.
func (gw *Gateway) requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := sanitizeHeaderValue(r.Header.Get(requestIDHeaderName))
		if rid == "" {
			rid = newRESTRequestID()
		}
		w.Header().Set(requestIDHeaderName, rid)
		ctx := context.WithValue(r.Context(), requestIDKey{}, rid)
		// Prefer the authenticated subject (set by authMiddleware) over the
		// client-asserted X-Acting-As header.
		if existing, _ := ctx.Value(actorKey{}).(string); existing == "" {
			if actor := sanitizeHeaderValue(r.Header.Get(actingAsHeaderName)); actor != "" {
				ctx = context.WithValue(ctx, actorKey{}, actor)
			}
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newRESTRequestID returns a random 16-hex-char request id.
func newRESTRequestID() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		// SA-012 classification: observational. Request IDs are log
		// correlation labels, not identity/authorization credentials, so a
		// static label is an acceptable degradation.
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

// authMiddleware validates the Bearer token from the Authorization header
// against the accepted credential sources (cfg.AuthToken plus cfg.AuthTokens,
// plus OIDC verification for JWT-shaped tokens when a verifier is
// configured). When no source is configured, auth is disabled (development
// mode). Matches the same semantics as grpc.AuthInterceptorFor. A
// named-token or OIDC-authenticated subject is injected into the request
// context (actorKey, plus rolesKey for OIDC roles) so audit attribution uses
// it in preference to any client-asserted X-Acting-As header.
func (gw *Gateway) authMiddleware(h http.Handler) http.Handler {
	tokens := AuthTokens{
		Legacy:   gw.cfg.AuthToken,
		Named:    gw.cfg.AuthTokens,
		OIDC:     gw.cfg.OIDC,
		Sessions: gw.cfg.Sessions,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokens.Enabled() {
			// Auth disabled: let the request through. With no credential
			// configured the deployment has declared it has no identity
			// model, so the asserted X-Acting-As name is admitted as the
			// governance subject (see admitAssertedIdentity).
			r = r.WithContext(admitAssertedIdentity(
				r.Context(), sanitizeHeaderValue(r.Header.Get(actingAsHeaderName))))
			h.ServeHTTP(w, r)
			return
		}
		// Mobile deep-link endpoints authenticate via a one-time token in
		// the request body (validated by the handler itself), not via
		// Bearer header. Skip Bearer enforcement here; the handler will
		// reject requests with missing or invalid tokens.
		if r.URL.Path == "/changes/deeplink/approve" || r.URL.Path == "/changes/deeplink/reject" {
			h.ServeHTTP(w, r)
			return
		}
		// The auth descriptor and the SSO code-exchange endpoint are
		// public: the login page needs the first to decide what to offer
		// and the second to hand over the one-time authorization code
		// before the browser holds any credential.
		if r.URL.Path == "/system/auth-info" || r.URL.Path == "/auth/github" {
			h.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}
		bearer, err := extractBearerToken(auth)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}
		id, ok := tokens.Resolve(r.Context(), bearer)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if id.Subject != "" || len(id.Roles) > 0 {
			r = r.WithContext(withResolvedIdentity(r.Context(), id))
		}
		h.ServeHTTP(w, r)
	})
}
