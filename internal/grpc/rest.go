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

// Stop gracefully shuts down the HTTP server.
func (gw *Gateway) Stop(ctx context.Context) error {
	if gw.httpServer == nil {
		return nil
	}
	return gw.httpServer.Shutdown(ctx)
}

func (gw *Gateway) serve(ctx context.Context) error {
	mux := http.NewServeMux()
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

// readProto reads JSON body into a proto message.
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
