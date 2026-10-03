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
	"github.com/nexus/levee/internal/diagnosis/llm_diag"
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

	// reasoner is the optional multi-turn LLM reasoning engine. Nil keeps
	// the rule-based diagnosis only. When set, a CONVERGED reasoning result
	// refines the report (root cause / confidence / recommendations); a
	// non-converged or failed run leaves the rule-based report untouched.
	reasoner *llm_diag.ReasoningEngine

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

// WithReasoner attaches an optional multi-turn LLM reasoning engine. When
// set, Diagnose runs the rule-based engine first and then lets a CONVERGED
// reasoning result refine the report: root cause, confidence and (when the
// model provided any) recommendations. Convergence is corroborated across
// turns by llm_diag's stability gate — a single self-reported answer is not
// enough — and the written-back confidence is the model's own estimate
// (provenance in ReasoningResult.ConfidenceSource), so callers mapping it
// toward approval tiers must treat it as a hint, never as a verified fact.
// Reasoner failures and non-convergence degrade to the rule-based report
// with a log line: an LLM outage must not take the Diagnose RPC down, and
// the rule engine's answer must not be lost to it.
func (s *DiagnosisService) WithReasoner(r *llm_diag.ReasoningEngine) *DiagnosisService {
	s.reasoner = r
	return s
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
	// Optional LLM refinement (only when converged); graceful on any failure.
	// Runs BEFORE the cache write so GetDiagnosis serves the enriched report.
	s.enrichWithReasoning(ctx, &report)

	s.cacheReport(report)

	return diagnosisToPB(&report), nil
}

// enrichWithReasoning refines a report with the multi-turn reasoner when it
// converges (hypothesis corroborated across turns — the stability gate in
// llm_diag refuses single self-reported answers). Every other outcome is a
// faithful no-op with a log line: non-convergence keeps the rule-based
// values, and a reasoner error must not fail the RPC.
func (s *DiagnosisService) enrichWithReasoning(ctx context.Context, report *diagnosis.DiagnosticReport) {
	if s.reasoner == nil {
		return
	}
	res, err := s.reasoner.Diagnose(ctx, report.Target, report)
	if err != nil {
		s.log.Warn("diagnosis: llm reasoning failed; keeping rule-based report",
			"target", report.Target, "error", err)
		return
	}
	if res.Status != llm_diag.StatusConverged {
		s.log.Info("diagnosis: llm reasoning did not converge; keeping rule-based report",
			"target", report.Target, "status", res.Status.String(), "turns", res.Turns)
		return
	}
	report.RootCause = res.RootCause
	report.Confidence = res.Confidence
	if len(res.Suggestions) > 0 {
		report.Recommendations = res.Suggestions
	}
	s.log.Info("diagnosis: refined by llm reasoning",
		"target", report.Target, "turns", res.Turns, "confidence_source", res.ConfidenceSource)
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
