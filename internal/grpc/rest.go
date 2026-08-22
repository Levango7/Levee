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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	"github.com/nexus/levee/internal/grpc/pb"
)

// ServeGatewayConfig configures the REST-to-gRPC gateway.
type ServeGatewayConfig struct {
	// Addr is the HTTP listen address, e.g. ":8080".
	Addr string
	// CORSOrigins lists allowed origins. "*" or an empty slice allows all.
	CORSOrigins []string
}

// ServeGateway starts an HTTP server on cfg.Addr that proxies /api/v1/*
// requests to the in-process gRPC service implementations.
func ServeGateway(ctx context.Context, cfg ServeGatewayConfig) error {
	gw := NewGateway(cfg)
	go func() {
		if err := gw.serve(ctx); err != nil {
			// Log is not available here; errors are non-fatal for the gateway.
			_ = err
		}
	}()
	return nil
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

// Stop gracefully shuts down the HTTP server.
func (gw *Gateway) Stop(ctx context.Context) error {
	if gw.httpServer == nil {
		return nil
	}
	return gw.httpServer.Shutdown(ctx)
}

func (gw *Gateway) serve(ctx context.Context) error {
	mux := http.NewServeMux()
	// RESTful routes take priority over /api/v1/ so the frontend can call
	// /changes, /templates, etc. without the gRPC-style prefix.
	mux.Handle("/", corsMiddleware(gw.cfg.CORSOrigins, gw.restRoute()))
	mux.Handle("/api/v1/", corsMiddleware(gw.cfg.CORSOrigins, gw.route()))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	gw.httpServer = &http.Server{
		Addr:              gw.cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", gw.cfg.Addr)
	if err != nil {
		return fmt.Errorf("gateway: listen %s: %w", gw.cfg.Addr, err)
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
func (gw *Gateway) restRoute() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimRight(r.URL.Path, "/")
		method := r.Method

		switch {
		case strings.HasPrefix(path, "/changes"):
			gw.dispatchChange(w, r, method, path)
		case strings.HasPrefix(path, "/templates"):
			gw.dispatchTemplate(w, r, method, path)
		case strings.HasPrefix(path, "/targets"):
			gw.dispatchTarget(w, r, method, path)
		case path == "/audit/log" && method == "GET":
			gw.handleAuditLog(w, r)
		case path == "/audit/traces" && method == "GET":
			gw.handleAuditTraces(w, r)
		case path == "/audit/verify" && method == "GET":
			gw.handleAuditVerify(w, r)
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
		parts := strings.SplitN(strings.TrimPrefix(path, "/changes"), "/", 2)
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
		case "logs":
			gw.handleChangeLogs(w, r, id)
		case "trace":
			gw.handleChangeTrace(w, r, id)
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
		parts := strings.SplitN(strings.TrimPrefix(path, "/templates"), "/", 2)
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
		parts := strings.SplitN(strings.TrimPrefix(path, "/targets"), "/", 2)
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
	if v := q.Get("team"); v != "" {
		req.Teams = strings.Split(v, ",")
	}
	if v := q.Get("environment"); v != "" {
		req.Environments = strings.Split(v, ",")
	}
	if v := q.Get("labelContains"); v != "" {
		req.LabelContains = v
	}
	if v := q.Get("pageSize"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			req.PageSize = int32(n)
		}
	}
	if v := q.Get("pageToken"); v != "" {
		req.PageToken = v
	}
	if v := q.Get("sortBy"); v != "" {
		req.SortBy = v
	}
	if v := q.Get("sortOrder"); v != "" {
		req.SortOrder = v
	}
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// reason is optional; swallow decode errors gracefully
		_ = err
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// reason is optional; swallow decode errors gracefully
		_ = err
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// purgeArtifacts defaults to false
		_ = err
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
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			req.Limit = int32(n)
		}
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
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			req.PageSize = int32(n)
		}
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
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			req.PageSize = int32(n)
		}
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
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			req.TimeoutSeconds = int32(n)
		}
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
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			req.Since = n
		}
	}
	if v := q.Get("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			req.Until = n
		}
	}
	if v := q.Get("pageSize"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			req.PageSize = int32(n)
		}
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
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			req.Since = n
		}
	}
	if v := q.Get("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			req.Until = n
		}
	}
	if v := q.Get("pageSize"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			req.PageSize = int32(n)
		}
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
	if q.Get("redactSecrets") == "true" {
		req.RedactSecrets = true
	}
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

// readJSONBody reads JSON body into a plain struct (for hand-written types
// that do not implement proto.Message).
func readJSONBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
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
		if err := readJSONBody(r, req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.ReceiveAlert(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeJSON(w, resp)
	case "GetAlertStatus":
		req := &pb.GetAlertStatusRequest{}
		if err := readJSONBody(r, req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetAlertStatus(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeJSON(w, resp)
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
		if err := readJSONBody(r, req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.Diagnose(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeJSON(w, resp)
	case "GetDiagnosis":
		req := &pb.GetDiagnosisRequest{}
		if err := readJSONBody(r, req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.GetDiagnosis(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeJSON(w, resp)
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
		if err := readJSONBody(r, req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := svc.SendMessage(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeJSON(w, resp)
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
func writeGRPCError(w http.ResponseWriter, err error) {
	st, ok := status.FromError(err)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := st.Message()
	switch st.Code() {
	case grpccodes.InvalidArgument:
		writeJSONError(w, http.StatusBadRequest, msg)
	case grpccodes.NotFound:
		writeJSONError(w, http.StatusNotFound, msg)
	case grpccodes.AlreadyExists:
		writeJSONError(w, http.StatusConflict, msg)
	case grpccodes.PermissionDenied:
		writeJSONError(w, http.StatusForbidden, msg)
	case grpccodes.FailedPrecondition:
		writeJSONError(w, http.StatusPreconditionFailed, msg)
	case grpccodes.Unimplemented:
		writeJSONError(w, http.StatusNotImplemented, msg)
	case grpccodes.Unavailable:
		writeJSONError(w, http.StatusServiceUnavailable, msg)
	case grpccodes.Unauthenticated:
		writeJSONError(w, http.StatusUnauthorized, msg)
	case grpccodes.DeadlineExceeded:
		writeJSONError(w, http.StatusGatewayTimeout, msg)
	case grpccodes.ResourceExhausted:
		writeJSONError(w, http.StatusTooManyRequests, msg)
	default:
		writeJSONError(w, http.StatusInternalServerError, msg)
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
func corsMiddleware(origins []string, h http.Handler) http.Handler {
	allowAll := len(origins) == 0
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
		if origin == "" || allowAll || originSet[origin] {
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
