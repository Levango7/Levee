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

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/diagnosis/llm_diag"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/permission"

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

	// authorizer polices Diagnose before it runs anything. Diagnose is not a
	// read: the engine executes commands on the requested target through the
	// platform's own credentials (see internal/diagnosis/health_probe.go's
	// Executor calls), so it is judged as `apply` on that target's
	// environment — the same axis that decides whether you may change the
	// host at all. Nil means no policy is configured and the gate short-circuits,
	// like every other service's authorizer.
	authorizer *authz.Authorizer

	// targetEnv resolves a request's target into the environment the policy
	// judges, reporting whether the target is known to the inventory at all.
	// `known=false` is a refusal, not a fallback: see gateTarget.
	targetEnv func(ctx context.Context, target string) (env string, known bool)

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

// WithAuthorizer installs the policy authorizer consulted by Diagnose and
// GetDiagnosis, matching the other services' pattern.
func (s *DiagnosisService) WithAuthorizer(a *authz.Authorizer) *DiagnosisService {
	s.authorizer = a
	return s
}

// WithTargetEnvResolver installs the target → environment lookup the gate
// needs. It is a function rather than a state.Store so this service keeps no
// persistence dependency of its own; cmd/levee supplies one backed by the
// target registry.
func (s *DiagnosisService) WithTargetEnvResolver(fn func(ctx context.Context, target string) (env string, known bool)) *DiagnosisService {
	s.targetEnv = fn
	return s
}

// gateTarget answers the one question the resolvers cannot: whether this
// request may act on that host at all.
//
// Both failure modes are refused rather than guessed, and each for its own
// reason. An authorizer without a resolver is a wiring bug: falling back to
// env "" would judge a prod host against permission.default_env, so an
// operator would read a grant or a refusal about an environment nobody asked
// about — cmd_serve takes the same line against a malformed policy file, which
// aborts startup rather than degrading to "not enforced". A target the
// inventory does not know is refused for the mirrored reason: we cannot
// attribute it to an environment, so we cannot license running commands on it
// with the platform's credentials.
func (s *DiagnosisService) gateTarget(ctx context.Context, target, action, rpc string) error {
	if s.authorizer == nil {
		return nil
	}
	if s.targetEnv == nil {
		return status.Errorf(codes.FailedPrecondition,
			"%s cannot be authorized: no target→environment resolver is installed, and judging %q against the default environment would silently apply the wrong policy",
			rpc, target)
	}
	env, known := s.targetEnv(ctx, target)
	if !known {
		return status.Errorf(codes.PermissionDenied,
			"%s denied for target %q: the inventory does not know this host, so no environment can be attributed to it and the policy cannot license running diagnostics there",
			rpc, target)
	}
	return authorizeResource(ctx, s.authorizer, env, action, rpc)
}

// gateTargetRead is the read-side twin of gateTarget: `view`, with the
// unattributable caller admitted by the shared read rule rather than by
// omission here.
func (s *DiagnosisService) gateTargetRead(ctx context.Context, target, rpc string) error {
	if s.authorizer == nil {
		return nil
	}
	if s.targetEnv == nil {
		return status.Errorf(codes.FailedPrecondition,
			"%s cannot be authorized: no target→environment resolver is installed, and judging %q against the default environment would silently apply the wrong policy",
			rpc, target)
	}
	env, known := s.targetEnv(ctx, target)
	if !known {
		// An unknown target here means a cached report whose host has left the
		// inventory. Refusing is the safe reading, and it is also the honest
		// one: the alternative is serving a report about nobody-knows-which
		// environment under whichever default is configured.
		return status.Errorf(codes.PermissionDenied,
			"%s denied for target %q: the inventory no longer knows this host, so its report cannot be attributed to an environment",
			rpc, target)
	}
	return authorizeResourceRead(ctx, s.authorizer, env, rpc)
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

	// Authorization precedes every side effect, the same order the change
	// writes keep: a refused diagnosis must not have run anything on the host,
	// must not spend a stored credential, and must not leave a cached report.
	if err := s.gateTarget(ctx, req.GetTarget(), permission.ActionApply, "Diagnose"); err != nil {
		return nil, err
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
	// The read gate runs AFTER the lookup, and the reason is stated rather than
	// assumed: the environment being judged belongs to the report's target,
	// which is only knowable once the id resolves. Judging env "" instead would
	// silently fall back to permission.default_env — a refusal (or a grant)
	// about an environment the caller never touched. The accepted cost is that
	// a caller can tell "no such report" from "not yours to read", so report
	// ids remain a weak existence oracle; they are engine-minted rather than
	// enumerable, which is why that is the lesser of the two errors.
	if err := s.gateTargetRead(ctx, r.Target, "GetDiagnosis"); err != nil {
		return nil, err
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
