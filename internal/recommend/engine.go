package recommend

// engine.go implements Phase B3 of LEVEE's recommendation engine: the
// RecommendEngine that turns a DiagnosticReport into a concrete fix
// recommendation. The engine blends two signals:
//
//   - Knowledge base: historical incidents, runbooks and fix patterns are
//     scored against the diagnosis root cause, symptoms and tags. The best
//     matches drive the recommendation when no LLM is available.
//   - LLM: when configured, the engine asks the LLM to propose a fix given
//     the diagnosis and the knowledge-base matches. The LLM response is
//     parsed into a fixProposal and merged with the knowledge-base evidence.
//
// When the LLM is not configured or fails, the engine gracefully degrades to
// pure knowledge-base mode. All LLM prompts are sanitised before being sent
// to prevent leakage of credentials, IPs and other sensitive data.
//
// The engine is safe for concurrent use. It never panics; errors are
// propagated through error returns.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrNilReport is returned when Recommend is called with a nil report.
	ErrNilReport = errors.New("recommend: nil diagnostic report")
	// ErrEmptyReport is returned when the report has no target, no root cause
	// and no findings — there is nothing to recommend on.
	ErrEmptyReport = errors.New("recommend: empty diagnostic report")
)

// --- Defaults ---------------------------------------------------------------

const (
	// DefaultRecommendTimeout is the per-Recommend timeout when the config
	// does not specify one.
	DefaultRecommendTimeout = 30 * time.Second
	// minMatchScore is the threshold below which a knowledge-base match is
	// considered too weak to drive a recommendation.
	minMatchScore = 0.1
	// maxAlternatives is the maximum number of alternative approaches to
	// attach to a recommendation.
	maxAlternatives = 3
)

// --- RecommendationSource ---------------------------------------------------

// RecommendationSource indicates where the recommendation came from.
type RecommendationSource string

const (
	// SourceKnowledgeBase means the recommendation was derived purely from
	// knowledge-base matches.
	SourceKnowledgeBase RecommendationSource = "knowledge_base"
	// SourceLLM means the recommendation was derived purely from the LLM
	// (no knowledge-base matches were available).
	SourceLLM RecommendationSource = "llm"
	// SourceHybrid means the recommendation blends knowledge-base matches
	// with an LLM proposal.
	SourceHybrid RecommendationSource = "hybrid"
)

// --- Alternative ------------------------------------------------------------

// Alternative is a backup fix approach surfaced alongside the primary
// recommendation so the operator can choose a less risky path.
type Alternative struct {
	Summary    string    `json:"summary"`
	Approach   string    `json:"approach"`
	RiskLevel  RiskLevel `json:"risk_level"`
	Confidence float64   `json:"confidence"`
}

// --- Recommendation ---------------------------------------------------------

// Recommendation is the complete fix recommendation produced by
// RecommendEngine.Recommend. It is a value object: once constructed it is
// not mutated.
type Recommendation struct {
	ID            string               `json:"id"`
	DiagnosisID   string               `json:"diagnosis_id"`
	Target        string               `json:"target"`
	Summary       string               `json:"summary"`
	Approach      string               `json:"approach"`
	WorkflowDraft string               `json:"workflow_draft"`
	RiskLevel     RiskLevel            `json:"risk_level"`
	Confidence    float64              `json:"confidence"`
	Alternatives  []Alternative        `json:"alternatives"`
	PreConditions []string             `json:"pre_conditions"`
	RollbackPlan  string               `json:"rollback_plan"`
	Source        RecommendationSource `json:"source"`
	Matches       []*Match             `json:"matches"`
	CreatedAt     time.Time            `json:"created_at"`
}

// --- fixProposal (LLM response shape) ---------------------------------------

// fixProposal is the internal representation of an LLM-generated fix proposal.
// The LLM is asked to return JSON in this shape.
type fixProposal struct {
	Summary       string        `json:"summary"`
	Approach      string        `json:"approach"`
	RiskLevel     RiskLevel     `json:"risk_level"`
	Confidence    float64       `json:"confidence"`
	Steps         []FixStep     `json:"steps"`
	PreConditions []string      `json:"pre_conditions"`
	RollbackPlan  string        `json:"rollback_plan"`
	Alternatives  []Alternative `json:"alternatives"`
}

// --- RecommendEngineConfig --------------------------------------------------

// RecommendEngineConfig configures a RecommendEngine.
type RecommendEngineConfig struct {
	KnowledgeBase *KnowledgeBase
	LLMClient     LLMClient // nil = pure knowledge-base mode
	Timeout       time.Duration
	Logger        *slog.Logger
}

// --- RecommendEngine --------------------------------------------------------

// RecommendEngine generates fix recommendations from diagnostic reports.
// It is safe for concurrent use.
type RecommendEngine struct {
	kb        *KnowledgeBase
	llm       LLMClient
	sanitizer *Sanitizer
	wfGen     *WorkflowGenerator
	log       *slog.Logger
	timeout   time.Duration
}

// NewRecommendEngine creates a RecommendEngine. When cfg.KnowledgeBase is nil
// a default knowledge base is loaded. When cfg.Timeout is zero
// DefaultRecommendTimeout is applied.
func NewRecommendEngine(cfg RecommendEngineConfig) *RecommendEngine {
	kb := cfg.KnowledgeBase
	if kb == nil {
		kb = NewKnowledgeBaseWithDefaults()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultRecommendTimeout
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.With("component", "recommend_engine")
	}
	return &RecommendEngine{
		kb:        kb,
		llm:       cfg.LLMClient,
		sanitizer: NewSanitizer(),
		wfGen:     NewWorkflowGenerator(),
		log:       lg,
		timeout:   timeout,
	}
}

// Recommend generates a fix recommendation from a diagnostic report. It
// returns ErrNilReport for a nil report and ErrEmptyReport for a report with
// no target, root cause or findings.
func (e *RecommendEngine) Recommend(ctx context.Context, report *diagnosis.DiagnosticReport) (*Recommendation, error) {
	if report == nil {
		return nil, ErrNilReport
	}
	if report.Target == "" && report.RootCause == "" && !report.HasFindings() {
		return nil, ErrEmptyReport
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Apply the engine timeout when the caller has not set a deadline.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	rootCause, symptoms, tags := extractDiagnosisSignals(report)

	// 1. Knowledge-base match.
	matches, err := e.kb.Match(rootCause, symptoms, tags)
	if err != nil {
		return nil, fmt.Errorf("recommend: knowledge base match: %w", err)
	}
	strongMatches := filterStrongMatches(matches, minMatchScore)

	// 2. LLM proposal (if configured). Errors degrade to knowledge-base mode.
	proposal, llmErr := e.tryLLM(ctx, report, rootCause, symptoms, tags, strongMatches)

	// 3. Build the recommendation.
	rec := e.buildRecommendation(report, rootCause, strongMatches, proposal, llmErr)

	// 4. Generate the LEVEELang workflow draft.
	rec.WorkflowDraft = e.generateWorkflow(report.Target, strongMatches, proposal)

	return rec, nil
}

// extractDiagnosisSignals pulls root cause, symptoms and tags from a report.
// Findings contribute their title and description as symptoms and their
// category as a tag; matched log patterns contribute their name and
// description as symptoms.
func extractDiagnosisSignals(report *diagnosis.DiagnosticReport) (string, []string, []string) {
	rootCause := report.RootCause

	symptoms := make([]string, 0, len(report.Findings)*2)
	for _, f := range report.Findings {
		if f.Title != "" {
			symptoms = append(symptoms, f.Title)
		}
		if f.Description != "" {
			symptoms = append(symptoms, f.Description)
		}
	}
	for _, p := range report.LogAnalysis.ErrorPatterns {
		if p.Pattern.Name != "" {
			symptoms = append(symptoms, p.Pattern.Name)
		}
		if p.Pattern.Description != "" {
			symptoms = append(symptoms, p.Pattern.Description)
		}
	}

	tags := make([]string, 0, len(report.Findings)+1)
	for _, f := range report.Findings {
		if f.Category != "" {
			tags = append(tags, f.Category)
		}
	}
	if report.Trigger != "" {
		tags = append(tags, string(report.Trigger))
	}

	return rootCause, symptoms, tags
}

// filterStrongMatches drops matches below the threshold.
func filterStrongMatches(matches []*Match, threshold float64) []*Match {
	out := make([]*Match, 0, len(matches))
	for _, m := range matches {
		if m.Score >= threshold {
			out = append(out, m)
		}
	}
	return out
}

// tryLLM calls the LLM with a sanitised prompt and parses the response.
// Returns a nil proposal (and a nil error) when no LLM is configured. On
// LLM or parse failure a nil proposal and the error are returned so the
// caller can degrade to knowledge-base mode.
func (e *RecommendEngine) tryLLM(ctx context.Context, report *diagnosis.DiagnosticReport, rootCause string, symptoms, tags []string, matches []*Match) (*fixProposal, error) {
	if e.llm == nil {
		return nil, nil
	}
	systemPrompt := llmSystemPrompt()
	userPrompt := buildUserPrompt(report, rootCause, symptoms, tags, matches)

	messages := []LLMMessage{{Role: "user", Content: userPrompt}}
	messages = e.sanitizer.SanitizeMessages(messages)
	sanitisedSystem := e.sanitizer.Sanitize(systemPrompt)

	resp, err := e.llm.ChatWithSystem(ctx, sanitisedSystem, messages)
	if err != nil {
		e.log.Warn("recommend: llm call failed, degrading to knowledge base", "err", err)
		return nil, err
	}
	proposal, perr := parseFixProposal(resp)
	if perr != nil {
		e.log.Warn("recommend: llm response parse failed, degrading to knowledge base", "err", perr)
		return nil, perr
	}
	return proposal, nil
}

// parseFixProposal parses an LLM response as a fixProposal JSON object.
// Markdown code fences are stripped first so models that ignore the "no
// fences" instruction still parse.
func parseFixProposal(response string) (*fixProposal, error) {
	jsonStr := extractJSONBlock(response)
	if strings.TrimSpace(jsonStr) == "" {
		return nil, fmt.Errorf("recommend: empty llm response")
	}
	var p fixProposal
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return nil, fmt.Errorf("recommend: parse fix proposal: %w", err)
	}
	return &p, nil
}

// extractJSONBlock strips markdown code fences if present.
func extractJSONBlock(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line.
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	}
	// Drop the closing fence.
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// buildRecommendation assembles the final Recommendation from the report,
// knowledge-base matches and (optionally) the LLM proposal.
func (e *RecommendEngine) buildRecommendation(report *diagnosis.DiagnosticReport, rootCause string, matches []*Match, proposal *fixProposal, llmErr error) *Recommendation {
	rec := &Recommendation{
		ID:          uuid.NewString(),
		DiagnosisID: report.ID,
		Target:      report.Target,
		CreatedAt:   time.Now().UTC(),
		Matches:     matches,
	}

	if proposal != nil && llmErr == nil {
		rec.Summary = proposal.Summary
		rec.Approach = proposal.Approach
		rec.RiskLevel = normaliseRiskLevel(proposal.RiskLevel)
		rec.Confidence = clamp01(proposal.Confidence)
		rec.PreConditions = proposal.PreConditions
		rec.RollbackPlan = proposal.RollbackPlan
		rec.Alternatives = proposal.Alternatives
		if len(matches) > 0 {
			rec.Source = SourceHybrid
			// Blend LLM confidence with the best knowledge-base score.
			rec.Confidence = 0.6*rec.Confidence + 0.4*matches[0].Score
		} else {
			rec.Source = SourceLLM
		}
	} else {
		rec.Source = SourceKnowledgeBase
		e.populateFromMatches(rec, report, rootCause, matches)
	}

	// Fill in any blanks with sensible defaults.
	if rec.Summary == "" {
		rec.Summary = defaultSummary(report, rootCause)
	}
	if rec.Approach == "" {
		rec.Approach = defaultApproach(matches)
	}
	if rec.RiskLevel == "" {
		rec.RiskLevel = riskFromMatches(matches)
	}
	if rec.Confidence == 0 {
		rec.Confidence = confidenceFromMatches(matches, report.Confidence)
	}
	if len(rec.PreConditions) == 0 {
		rec.PreConditions = defaultPreConditions(rec.RiskLevel)
	}
	if rec.RollbackPlan == "" {
		rec.RollbackPlan = defaultRollbackPlan(rec.RiskLevel)
	}
	return rec
}

// populateFromMatches fills the recommendation from the best knowledge-base
// match and builds alternatives from the remaining matches.
func (e *RecommendEngine) populateFromMatches(rec *Recommendation, report *diagnosis.DiagnosticReport, rootCause string, matches []*Match) {
	if len(matches) == 0 {
		return
	}
	best := matches[0]
	rec.Summary = fmt.Sprintf("Based on similar past incident %q", best.Title)
	switch best.Type {
	case MatchTypeIncident:
		if inc, ok := best.Source.(HistoricalIncident); ok {
			rec.Approach = inc.Resolution
			rec.RiskLevel = riskFromSeverity(inc.Severity)
			rec.Confidence = best.Score
		}
	case MatchTypeRunbook:
		if rb, ok := best.Source.(Runbook); ok {
			rec.Approach = rb.Description
			rec.RiskLevel = riskFromRunbook(rb)
			rec.Confidence = best.Score
		}
	case MatchTypePattern:
		if p, ok := best.Source.(FixPattern); ok {
			rec.Approach = p.Fix
			rec.RiskLevel = p.RiskLevel
			rec.Confidence = best.Score
		}
	}
	limit := maxAlternatives
	if len(matches)-1 < limit {
		limit = len(matches) - 1
	}
	for i := 1; i <= limit; i++ {
		alt := alternativeFromMatch(matches[i])
		if alt.Summary != "" {
			rec.Alternatives = append(rec.Alternatives, alt)
		}
	}
}

// alternativeFromMatch builds an Alternative from a Match.
func alternativeFromMatch(m *Match) Alternative {
	alt := Alternative{
		Summary:    m.Title,
		Confidence: m.Score,
	}
	switch m.Type {
	case MatchTypeIncident:
		if inc, ok := m.Source.(HistoricalIncident); ok {
			alt.Approach = inc.Resolution
			alt.RiskLevel = riskFromSeverity(inc.Severity)
		}
	case MatchTypeRunbook:
		if rb, ok := m.Source.(Runbook); ok {
			alt.Approach = rb.Description
			alt.RiskLevel = riskFromRunbook(rb)
		}
	case MatchTypePattern:
		if p, ok := m.Source.(FixPattern); ok {
			alt.Approach = p.Fix
			alt.RiskLevel = p.RiskLevel
		}
	}
	return alt
}

// generateWorkflow produces the LEVEELang YAML draft. LLM-provided steps are
// preferred; otherwise the best knowledge-base match is used; otherwise a
// placeholder workflow is emitted.
func (e *RecommendEngine) generateWorkflow(target string, matches []*Match, proposal *fixProposal) string {
	if target == "" {
		target = "unknown"
	}
	if proposal != nil && len(proposal.Steps) > 0 {
		if yaml, err := e.wfGen.Generate(target, proposal.Steps); err == nil {
			return yaml
		} else {
			e.log.Warn("recommend: workflow gen from proposal failed", "err", err)
		}
	}
	if len(matches) > 0 {
		if yaml, err := e.wfGen.GenerateFromMatch(target, matches[0]); err == nil {
			return yaml
		} else {
			e.log.Warn("recommend: workflow gen from match failed", "err", err)
		}
	}
	return placeholderWorkflow(target)
}

// placeholderWorkflow returns a minimal workflow when no steps could be
// derived. The batches and rollback sections are empty so the LEVEELang
// runtime treats it as a manual-review placeholder.
func placeholderWorkflow(target string) string {
	return fmt.Sprintf("name: auto-fix-%s-%d\ndescription: No automated fix available; manual review required.\ntarget:\n  hosts:\n    - %s\nwindow:\n  duration: 30m\n  approval: manual\nbatches: []\nrollback: []\n",
		sanitizeName(target), time.Now().Unix(), target)
}

// --- Prompt construction ----------------------------------------------------

// llmSystemPrompt returns the system prompt that instructs the LLM to act as
// a senior SRE and return a JSON fix proposal.
func llmSystemPrompt() string {
	return `You are a senior Site Reliability Engineer. Given a diagnostic report and a list of similar historical incidents, propose a concrete fix.

Return a JSON object with the following shape:
{
  "summary": "short one-line summary",
  "approach": "detailed fix approach",
  "risk_level": "low" | "medium" | "high" | "critical",
  "confidence": 0.0-1.0,
  "steps": [
    {"name": "step-name", "module": "shell|svc|file|pkg", "action": "restart|stop|start|copy|remove|run", "target": "...", "args": {"key": "value"}, "description": "..."}
  ],
  "pre_conditions": ["condition 1", "condition 2"],
  "rollback_plan": "how to undo the fix",
  "alternatives": [
    {"summary": "...", "approach": "...", "risk_level": "low", "confidence": 0.7}
  ]
}

Only return the JSON object, no markdown fences, no explanation.`
}

// buildUserPrompt constructs the user prompt from the report and matches.
func buildUserPrompt(report *diagnosis.DiagnosticReport, rootCause string, symptoms, tags []string, matches []*Match) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Diagnostic Report:\n")
	fmt.Fprintf(&b, "  target: %s\n", report.Target)
	fmt.Fprintf(&b, "  status: %s\n", report.Status)
	fmt.Fprintf(&b, "  root_cause: %s\n", rootCause)
	fmt.Fprintf(&b, "  confidence: %.2f\n", report.Confidence)
	fmt.Fprintf(&b, "  summary: %s\n", report.Summary)
	if len(symptoms) > 0 {
		fmt.Fprintf(&b, "  symptoms:\n")
		for _, s := range symptoms {
			fmt.Fprintf(&b, "    - %s\n", s)
		}
	}
	if len(tags) > 0 {
		fmt.Fprintf(&b, "  tags: %s\n", strings.Join(tags, ", "))
	}
	if len(matches) > 0 {
		fmt.Fprintf(&b, "Similar past incidents:\n")
		for i, m := range matches {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "  %d. [%s] %s (score=%.2f)\n", i+1, m.Type, m.Title, m.Score)
		}
	}
	fmt.Fprintf(&b, "\nPropose a fix following the JSON schema in the system prompt.")
	return b.String()
}

// --- Default helpers --------------------------------------------------------

func defaultSummary(report *diagnosis.DiagnosticReport, rootCause string) string {
	if rootCause != "" {
		return fmt.Sprintf("Fix proposal for %s", rootCause)
	}
	if report.HasFindings() {
		return fmt.Sprintf("Fix proposal for %s on %s", report.Findings[0].Title, report.Target)
	}
	return fmt.Sprintf("Fix proposal for target %s", report.Target)
}

func defaultApproach(matches []*Match) string {
	if len(matches) == 0 {
		return "Manual investigation required; no matching historical incident found."
	}
	return fmt.Sprintf("Apply the resolution from similar incident %q.", matches[0].Title)
}

// riskFromMatches derives a risk level from the best match.
func riskFromMatches(matches []*Match) RiskLevel {
	if len(matches) == 0 {
		return RiskMedium
	}
	best := matches[0]
	switch best.Type {
	case MatchTypeIncident:
		if inc, ok := best.Source.(HistoricalIncident); ok {
			return riskFromSeverity(inc.Severity)
		}
	case MatchTypeRunbook:
		if rb, ok := best.Source.(Runbook); ok {
			return riskFromRunbook(rb)
		}
	case MatchTypePattern:
		if p, ok := best.Source.(FixPattern); ok {
			return p.RiskLevel
		}
	}
	return RiskMedium
}

// riskFromSeverity maps an incident severity or runbook step risk string to
// a RiskLevel. It accepts both vocabularies: incident severities
// ("critical"/"warning"/"info") and runbook step risks
// ("low"/"medium"/"high").
func riskFromSeverity(sev string) RiskLevel {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical", "fatal":
		return RiskHigh
	case "high":
		return RiskHigh
	case "warning", "warn", "medium":
		return RiskMedium
	case "info", "low":
		return RiskLow
	default:
		return RiskMedium
	}
}

// riskFromRunbook returns the worst risk across the runbook's steps.
func riskFromRunbook(rb Runbook) RiskLevel {
	worst := RiskLow
	for _, s := range rb.Steps {
		r := riskFromSeverity(s.RiskLevel)
		if riskRank(r) > riskRank(worst) {
			worst = r
		}
	}
	return worst
}

// riskRank returns a numeric rank for a RiskLevel so higher values mean more
// severe.
func riskRank(r RiskLevel) int {
	switch r {
	case RiskLow:
		return 1
	case RiskMedium:
		return 2
	case RiskHigh:
		return 3
	case RiskCritical:
		return 4
	default:
		return 0
	}
}

// normaliseRiskLevel coerces a RiskLevel (which may have come from JSON as a
// raw string) into one of the canonical constants.
func normaliseRiskLevel(r RiskLevel) RiskLevel {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return r
	}
	switch strings.ToLower(string(r)) {
	case "low":
		return RiskLow
	case "medium":
		return RiskMedium
	case "high":
		return RiskHigh
	case "critical":
		return RiskCritical
	}
	return RiskMedium
}

// confidenceFromMatches blends the best match score with the diagnosis
// confidence.
func confidenceFromMatches(matches []*Match, diagConf float64) float64 {
	if len(matches) == 0 {
		return clamp01(diagConf)
	}
	return clamp01(0.5*matches[0].Score + 0.5*diagConf)
}

// defaultPreConditions returns the standard pre-conditions, augmented for
// high-risk fixes.
func defaultPreConditions(risk RiskLevel) []string {
	base := []string{
		"Confirm the target is reachable.",
		"Snapshot current state for rollback.",
	}
	if risk == RiskHigh || risk == RiskCritical {
		base = append(base, "Obtain explicit operator approval.")
	}
	return base
}

// defaultRollbackPlan returns a rollback plan appropriate for the risk level.
func defaultRollbackPlan(risk RiskLevel) string {
	switch risk {
	case RiskCritical:
		return "Restore from snapshot; verify data integrity; restart affected services."
	case RiskHigh:
		return "Restart the affected service with the previous configuration."
	case RiskMedium:
		return "Revert the applied change and verify the target recovers."
	default:
		return "Revert the applied change."
	}
}

// clamp01 clamps v to the [0, 1] range.
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
