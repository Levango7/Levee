
// diagnosis_service.go implements pb.DiagnosisServiceServer, the gRPC
// service that runs on-demand diagnoses and retrieves stored diagnosis
// reports.
//
// The service wraps an optional *diagnosis.DiagEngine. When the engine is
// nil the RPCs return codes.Unimplemented, keeping the server usable in
// reduced-functionality deployments. Successful Diagnose results are
// cached in an in-memory map keyed by report id so that GetDiagnosis can
// retrieve them later; the cache is bounded by maxRecentReports.
//
// All errors are mapped to gRPC codes via the status package. The service
// is safe for concurrent use and immutable after construction.

package grpc

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxRecentReports caps the in-memory diagnosis report cache.
const maxRecentReports = 256

// DiagnosisService implements pb.DiagnosisServiceServer.
type DiagnosisService struct {
	pb.UnimplementedDiagnosisServiceServer

	// engine is the diagnosis engine. May be nil.
	engine *diagnosis.DiagEngine

	// log is the structured logger. When nil the package-level singleton
	// from internal/log is used.
	log *slog.Logger

	// mu guards reports and order.
	mu      sync.RWMutex
	reports map[string]diagnosis.DiagnosticReport
	order   []string // report ids in insertion order, oldest first
}

// NewDiagnosisService constructs a DiagnosisService. Both engine and
// logger are optional; passing nil for either is supported.
func NewDiagnosisService(engine *diagnosis.DiagEngine, lg *slog.Logger) *DiagnosisService {
	if lg == nil {
		lg = log.With("component", "diagnosis_service")
	}
	return &DiagnosisService{
		engine:  engine,
		log:     lg,
		reports: make(map[string]diagnosis.DiagnosticReport),
	}
}

// --- Diagnose --------------------------------------------------------------

// Diagnose runs a fresh diagnosis on the requested target. When alert_id
// is supplied the diagnosis is tagged with that id in the report. The
// resulting report is cached so GetDiagnosis can retrieve it.
func (s *DiagnosisService) Diagnose(ctx context.Context, req *pb.DiagnoseRequest) (*pb.DiagnosticReportMessage, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}
	if strings.TrimSpace(req.GetTarget()) == "" {
		return nil, status.Error(codes.InvalidArgument, "target is required")
	}
	if s.engine == nil {
		return nil, status.Error(codes.Unimplemented, "diagnosis engine not configured")
	}

	// Apply optional timeout when the caller did not set one.
	if req.GetTimeoutSeconds() > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.GetTimeoutSeconds())*time.Second)
		defer cancel()
	}

	report := s.engine.Diagnose(ctx, req.GetTarget())
	if req.GetAlertId() != "" {
		report.AlertID = req.GetAlertId()
		// Override the trigger to indicate this run was alert-driven.
		report.Trigger = diagnosis.TriggerAlert
	}

	s.cacheReport(report)

	return diagnosisToPB(&report), nil
}

// --- GetDiagnosis ----------------------------------------------------------

// GetDiagnosis returns a previously stored diagnosis report by id. Returns
// codes.NotFound when the report is unknown.
func (s *DiagnosisService) GetDiagnosis(ctx context.Context, req *pb.GetDiagnosisRequest) (*pb.DiagnosticReportMessage, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}
	if strings.TrimSpace(req.GetId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	s.mu.RLock()
	r, ok := s.reports[req.GetId()]
	s.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "diagnosis report %q not found", req.GetId())
	}
	return diagnosisToPB(&r), nil
}

// --- internal helpers ------------------------------------------------------

// cacheReport stores r in the in-memory cache, evicting the oldest entry
// when the cache is full.
func (s *DiagnosisService) cacheReport(r diagnosis.DiagnosticReport) {
	if r.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.reports[r.ID]; !exists {
		s.order = append(s.order, r.ID)
		if len(s.order) > maxRecentReports {
			oldest := s.order[0]
			s.order = s.order[1:]
			delete(s.reports, oldest)
		}
	}
	s.reports[r.ID] = r
}

// --- conversion helpers ----------------------------------------------------

// diagnosisToPB converts a diagnosis.DiagnosticReport to a
// pb.DiagnosticReportMessage.
func diagnosisToPB(r *diagnosis.DiagnosticReport) *pb.DiagnosticReportMessage {
	if r == nil {
		return nil
	}
	findings := make([]*pb.FindingMessage, 0, len(r.Findings))
	for _, f := range r.Findings {
		findings = append(findings, &pb.FindingMessage{
			Id:          f.ID,
			Category:    f.Category,
			Severity:    f.Severity,
			Title:       f.Title,
			Description: f.Description,
			Evidence:    f.Evidence,
			Suggestion:  f.Suggestion,
		})
	}
	return &pb.DiagnosticReportMessage{
		Id:              r.ID,
		Target:          r.Target,
		Trigger:         string(r.Trigger),
		AlertId:         r.AlertID,
		Status:          string(r.Status),
		RootCause:       r.RootCause,
		Confidence:      r.Confidence,
		Summary:         r.Summary,
		Recommendations: r.Recommendations,
		Errors:          r.Errors,
		StartedAt:       r.StartedAt.Unix(),
		DurationMs:      r.Duration.Milliseconds(),
		Findings:        findings,
	}
}