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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/time/rate"

	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	"github.com/nexus/levee/internal/grpc/pb"
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
	cfg        ServeGatewayConfig
	httpServer *http.Server
	change     *ChangeService
	template   *TemplateService
	target     *TargetService
	audit      *AuditService
	system     *SystemService
	alert      pb.AlertServiceServer
	diag       pb.DiagnosisServiceServer
	conv       pb.ConversationServiceServer
	// mobileApproval is optional; non-nil when the mobile approval service
	// is configured. Used by the /changes/deeplink/approve REST endpoint.
	mobileApproval mobileApprovalHandler

	// limiterOnce lazily initialises the gateway-wide rate limiter so
	// that BOTH route trees (RESTful "/" and legacy "/api/v1/") share a
	// single token bucket. limiter stays nil when rate limiting is
	// disabled via a negative RatePerSec.
	limiterOnce sync.Once
	limiter     *rate.Limiter

	// extraRoutes holds handlers registered via SetExtraRoute before
	// Start. They are mounted verbatim on the gateway mux, without
	// auth/CORS/rate-limit middleware, for operational endpoints such
	// as /metrics.
	extraMu     sync.Mutex
	extraRoutes []extraRoute
}

// extraRoute pairs a mux pattern with the handler SetExtraRoute should
// mount for it.
type extraRoute struct {
	pattern string
	handler http.Handler
}

// mobileApprovalHandler wraps the MobileApprovalService for REST routing.
// It is an interface so we can avoid importing the approval package here.
type mobileApprovalHandler interface {
	ApproveViaDeepLink(ctx context.Context, token string) error
	RejectViaDeepLink(ctx context.Context, token string) error
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

// NewGateway constructs a gateway from the given config.
func NewGateway(cfg ServeGatewayConfig) *Gateway {
	return &Gateway{cfg: cfg}
}

// SetServices registers the in-process service implementations with the gateway.
func (gw *Gateway) SetServices(change *ChangeService, template *TemplateService, target *TargetService, audit *AuditService, system *SystemService, alert pb.AlertServiceServer, diag pb.DiagnosisServiceServer, conv pb.ConversationServiceServer) {
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
	// RESTful routes take priority over /api/v1/ so the frontend can call
	// /changes, /templates, etc. without the gRPC-style prefix. Both route
	// trees share ONE rate limiter: separate buckets would let a client
	// double its effective request budget by switching path styles.
	lim := gw.sharedRateLimiter()
	mux.Handle("/", corsMiddleware(gw.cfg.CORSOrigins, gw.authMiddleware(gw.requestIDMiddleware(wrapWithLimiter(lim, gw.restRoute())))))
	mux.Handle("/api/v1/", corsMiddleware(gw.cfg.CORSOrigins, gw.authMiddleware(gw.requestIDMiddleware(wrapWithLimiter(lim, gw.route())))))
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
	// scraping agents that cannot send credentials).
	gw.extraMu.Lock()
	for _, er := range gw.extraRoutes {
		h := er.handler
		if !gw.cfg.MetricsPublic {
			h = gw.authMiddleware(h)
		}
		mux.Handle(er.pattern, h)
	}
	gw.extraMu.Unlock()

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

// route dispatches /api/v1/<Service>/<Method> to the right handler.
func (gw *Gateway) route() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
		path = strings.TrimRight(path, "/")

		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 {
			writeJSONError(w, http.StatusBadRequest, "invalid path: expected /api/v1/<Service>/<Method>")
			return
		}

		switch parts[0] {
		case "ChangeService":
			gw.handleChange(w, r, parts[1])
		case "TemplateService":
			gw.handleTemplate(w, r, parts[1])
		case "TargetService":
			gw.handleTarget(w, r, parts[1])
		case "AuditService":
			gw.handleAudit(w, r, parts[1])
		case "SystemService":
			gw.handleSystem(w, r, parts[1])
		case "AlertService":
			gw.handleAlert(w, r, parts[1])
		case "DiagnosisService":
			gw.handleDiagnosis(w, r, parts[1])
		case "ConversationService":
			gw.handleConversation(w, r, parts[1])
		default:
			writeJSONError(w, http.StatusBadRequest, "unknown service: "+parts[0])
		}
	})
}

// restRoute dispatches RESTful HTTP paths (e.g. /changes, /templates/:id)
// to the corresponding gRPC handlers. It runs BEFORE /api/v1/ so the
// frontend's Axios calls hit this layer first.
//
// Routing matches the FIRST path segment EXACTLY. The previous
// strings.HasPrefix(path, "/changes") test made look-alike paths such as
// "/changesfoo" dispatch into the change handlers instead of 404ing.
func (gw *Gateway) restRoute() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimRight(r.URL.Path, "/")
		method := r.Method

		first := strings.Split(strings.TrimPrefix(path, "/"), "/")[0]
		switch first {
		case "changes":
			gw.dispatchChange(w, r, method, path)
		case "templates":
			gw.dispatchTemplate(w, r, method, path)
		case "targets":
			gw.dispatchTarget(w, r, method, path)
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
		case "system":
			switch {
			case path == "/system/version" && method == "GET":
				gw.handleSystemVersion(w, r)
			case path == "/system/status" && method == "GET":
				gw.handleSystemStatus(w, r)
			case path == "/system/config" && method == "GET":
				gw.handleSystemConfig(w, r)
			case path == "/system/doctor" && method == "POST":
				gw.handleSystemDoctor(w, r)
			default:
				writeJSONError(w, http.StatusNotFound, "not found: "+r.URL.Path)
			}
		default:
			writeJSONError(w, http.StatusNotFound, "not found: "+r.URL.Path)
		}
	})
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
		id, rest := parts[0], ""
		if len(parts) > 1 && parts[1] != "" {
			rest = parts[1]
		}
		switch rest {
		case "":
			switch method {
			case "GET":
				gw.handleChangeGet(w, r, id)
			default:
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case "plan", "apply", "approve", "reject", "pause", "resume", "cancel", "retry", "rollback", "archive":
			// State-changing actions must use POST. Rejecting other verbs
			// prevents crawlers, link prefetchers and cached GETs from
			// mutating change state (e.g. GET /changes/<id>/pause pausing a
			// run).
			if method != http.MethodPost {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: use POST")
				return
			}
			switch rest {
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
		case "logs", "trace":
			if method != http.MethodGet {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: use GET")
				return
			}
			if rest == "logs" {
				gw.handleChangeLogs(w, r, id)
			} else {
				gw.handleChangeTrace(w, r, id)
			}
		default:
			writeJSONError(w, http.StatusBadRequest, "unknown change sub-path: /"+rest)
		}
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
	if err := gw.mobileApproval.ApproveViaDeepLink(ctx, body.Token); err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "approved"})
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
	if err := gw.mobileApproval.RejectViaDeepLink(ctx, body.Token); err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "rejected"})
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
	writeProto(w, resp)
}

// -----------------------------------------------------------------------
// System — RESTful handlers
// -----------------------------------------------------------------------

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

// --- legacy proto-style JSON -------------------------------------------------
//
// The AlertService/DiagnosisService/ConversationService message types in
// pb/levee_extra.pb.go are hand-written and predate protoreflect: they do
// NOT implement the modern proto.Message interface (no ProtoReflect), so
// protojson and writeProto/readProto cannot be used on them directly.
// Their protobuf struct tags still carry the canonical lowerCamelCase JSON
// name (json=<name>), so these helpers derive field names from those tags
// and mirror the protojson behaviour that matters for the REST surface:
// responses use lowerCamelCase keys, requests accept either spelling.

// readProtoJSON decodes a request body into v using the proto-style JSON
// rules below. An empty body leaves v untouched.
func readProtoJSON(v any) func(*http.Request) error {
	return func(r *http.Request) error {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}
		if len(body) == 0 {
			return nil
		}
		if err := protoLikeUnmarshal(body, v); err != nil {
			return fmt.Errorf("invalid request body: %w", err)
		}
		return nil
	}
}

// writeProtoJSON marshals v to JSON with lowerCamelCase keys derived from
// the protobuf tags and writes it as the response.
func writeProtoJSON(w http.ResponseWriter, v any) {
	out, ok, err := protoLikeValue(reflect.ValueOf(v))
	if err != nil || !ok {
		writeJSONError(w, http.StatusInternalServerError, "marshal error")
		return
	}
	b, err := json.Marshal(out)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// protoTagJSONName extracts the canonical json= component from a protobuf
// struct tag ("bytes,2,opt,name=user_id,json=userId,proto3" → "userId").
func protoTagJSONName(tag string) string {
	for _, part := range strings.Split(tag, ",") {
		if strings.HasPrefix(part, "json=") {
			return strings.TrimPrefix(part, "json=")
		}
	}
	return ""
}

// protoLikeFieldName returns the canonical lowerCamelCase wire name of a
// struct field: the protobuf tag's json= name when present, else a
// lowerCamelCased Go field name.
func protoLikeFieldName(f reflect.StructField) string {
	if name := protoTagJSONName(f.Tag.Get("protobuf")); name != "" {
		return name
	}
	r := []rune(f.Name)
	if len(r) == 0 {
		return f.Name
	}
	return string(unicode.ToLower(r[0])) + string(r[1:])
}

// protoTagComponent extracts a key=value component from a protobuf struct
// tag ("bytes,2,opt,name=user_id,json=userId,proto3", "name" → "user_id").
func protoTagComponent(tag, key string) string {
	want := key + "="
	for _, part := range strings.Split(tag, ",") {
		if strings.HasPrefix(part, want) {
			return strings.TrimPrefix(part, want)
		}
	}
	return ""
}

// protoLikeAcceptNames returns every input spelling accepted for a field:
// the canonical camelCase name plus the snake_case proto/json-tag name,
// mirroring protojson's dual-name tolerance.
func protoLikeAcceptNames(f reflect.StructField) []string {
	names := []string{protoLikeFieldName(f)}
	add := func(n string) {
		if n == "" || n == "-" {
			return
		}
		for _, existing := range names {
			if existing == n {
				return
			}
		}
		names = append(names, n)
	}
	if j := f.Tag.Get("json"); j != "" {
		add(strings.Split(j, ",")[0])
	}
	add(protoTagComponent(f.Tag.Get("protobuf"), "name"))
	return names
}

// protoLikeValue converts v into plain JSON-encodable values with camelCase
// object keys. The second return is false when v is a nil pointer (nothing
// to encode).
func protoLikeValue(v reflect.Value) (any, bool, error) {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil, false, nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		out := make(map[string]any, v.NumField())
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}
			val, ok, err := protoLikeValue(v.Field(i))
			if err != nil {
				return nil, false, err
			}
			if !ok {
				continue // nil pointer field is omitted, like protojson
			}
			out[protoLikeFieldName(f)] = val
		}
		return out, true, nil
	case reflect.Slice:
		arr := make([]any, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			val, ok, err := protoLikeValue(v.Index(i))
			if err != nil {
				return nil, false, err
			}
			if !ok {
				arr = append(arr, nil)
				continue
			}
			arr = append(arr, val)
		}
		return arr, true, nil
	case reflect.Map:
		out := make(map[string]any, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			key, _ := iter.Key().Interface().(string)
			val, _, err := protoLikeValue(iter.Value())
			if err != nil {
				return nil, false, err
			}
			out[key] = val
		}
		return out, true, nil
	default:
		return v.Interface(), true, nil
	}
}

// protoLikeUnmarshal decodes JSON data into v using the dual-name field
// matching described above.
func protoLikeUnmarshal(data []byte, v any) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	return assignProtoLike(raw, reflect.ValueOf(v))
}

// assignProtoLike assigns decoded JSON value raw into the settable value v.
func assignProtoLike(raw any, v reflect.Value) error {
	if !v.CanSet() && v.Kind() != reflect.Ptr {
		return fmt.Errorf("cannot assign field")
	}
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		obj, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("expected JSON object")
		}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			for _, name := range protoLikeAcceptNames(f) {
				val, present := obj[name]
				if !present {
					continue
				}
				if err := assignProtoLike(val, v.Field(i)); err != nil {
					return err
				}
				break
			}
		}
		return nil
	case reflect.Slice:
		arr, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("expected JSON array")
		}
		out := reflect.MakeSlice(v.Type(), 0, len(arr))
		for _, el := range arr {
			ev := reflect.New(v.Type().Elem()).Elem()
			if err := assignProtoLike(el, ev); err != nil {
				return err
			}
			out = reflect.Append(out, ev)
		}
		v.Set(out)
		return nil
	case reflect.Map:
		obj, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("expected JSON object")
		}
		out := reflect.MakeMapWithSize(v.Type(), len(obj))
		for k, val := range obj {
			ev := reflect.New(v.Type().Elem()).Elem()
			if err := assignProtoLike(val, ev); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(k), ev)
		}
		v.Set(out)
		return nil
	case reflect.String:
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("expected JSON string")
		}
		v.SetString(s)
		return nil
	case reflect.Int64, reflect.Int32, reflect.Int:
		f, ok := raw.(float64)
		if !ok {
			return fmt.Errorf("expected JSON number")
		}
		v.SetInt(int64(f))
		return nil
	case reflect.Float32, reflect.Float64:
		f, ok := raw.(float64)
		if !ok {
			return fmt.Errorf("expected JSON number")
		}
		v.SetFloat(f)
		return nil
	case reflect.Bool:
		b, ok := raw.(bool)
		if !ok {
			return fmt.Errorf("expected JSON boolean")
		}
		v.SetBool(b)
		return nil
	default:
		return fmt.Errorf("unsupported field kind %s", v.Kind())
	}
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
		if err := readProtoJSON(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ReceiveAlert(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProtoJSON(w, resp)
	case "GetAlertStatus":
		req := &pb.GetAlertStatusRequest{}
		if err := readProtoJSON(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetAlertStatus(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProtoJSON(w, resp)
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
		if err := readProtoJSON(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.Diagnose(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProtoJSON(w, resp)
	case "GetDiagnosis":
		req := &pb.GetDiagnosisRequest{}
		if err := readProtoJSON(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetDiagnosis(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProtoJSON(w, resp)
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
		if err := readProtoJSON(req)(r); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.SendMessage(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeProtoJSON(w, resp)
	case "SubscribeConversation":
		writeJSONError(w, http.StatusNotImplemented, "streaming RPCs not supported")
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown method: "+method)
	}
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// extractAuth reads the Authorization header and returns gRPC metadata.
func extractAuth(r *http.Request) metadata.MD {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return metadata.New(nil)
	}
	return metadata.New(map[string]string{"authorization": auth})
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
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

// authMiddleware validates the Bearer token from the Authorization header
// against the accepted set (cfg.AuthToken plus cfg.AuthTokens). When no token
// is configured, auth is disabled (development mode). Matches the same
// semantics as grpc.AuthInterceptorFor. A named token's authenticated subject
// is injected into the request context (actorKey) so audit attribution uses
// it in preference to any client-asserted X-Acting-As header.
func (gw *Gateway) authMiddleware(h http.Handler) http.Handler {
	tokens := AuthTokens{Legacy: gw.cfg.AuthToken, Named: gw.cfg.AuthTokens}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokens.Enabled() {
			// Auth disabled: let the request through.
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
		subject, ok := tokens.Resolve(bearer)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if subject != "" {
			ctx := context.WithValue(r.Context(), actorKey{}, subject)
			r = r.WithContext(ctx)
		}
		h.ServeHTTP(w, r)
	})
}
