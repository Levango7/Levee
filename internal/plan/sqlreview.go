// sqlreview.go carries the plan-time SQL review verdict into the approved
// artifact. The types mirror internal/sqlreview's result shape field by field
// (the same artifact-mirror pattern as RiskFactor) so the persisted spelling
// is owned by the plan package and the artifact keeps a stable shape even if
// the review engine's internal types evolve.

package plan

// SQLReviewSummary is the aggregated verdict of reviewing every mysql.query
// step's SQL at plan time. It is stamped onto the plan BEFORE hashing, so the
// approved change's review verdict is exactly the executed one (plan_hash
// binds them, canonicalPlanV2 covers this field).
//
// nil means the workflow has no reviewable SQL surface (no mysql.query step
// with a non-empty sql argument) — not "reviewed and clean". The distinction
// matters for the audit report: absence of a verdict must never render as a
// pass.
type SQLReviewSummary struct {
	// Verdict is the worst verdict across the reviewed steps: fail > warn >
	// pass. A fail never reaches the artifact — the plan funnel refuses
	// before generation — so persisted values are pass or warn only.
	Verdict string `json:"verdict"`

	FailCount int `json:"fail_count"`
	WarnCount int `json:"warn_count"`

	// Findings is every finding from every reviewed step, anchored to the
	// workflow step name (Statement is the 1-based index within that step's
	// SQL batch; 0 marks a batch-level rule).
	Findings []SQLReviewFinding `json:"findings,omitempty"`
}

// SQLReviewFinding mirrors sqlreview.Finding plus the step anchor. Rule IDs
// are the review engine's stable SR-* vocabulary.
type SQLReviewFinding struct {
	Step      string `json:"step"`
	Rule      string `json:"rule"`
	Severity  string `json:"severity"`
	Statement int    `json:"statement"`
	Message   string `json:"message"`
}
