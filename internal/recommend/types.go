
// Package recommend implements the AI recommendation engine's knowledge base
// and historical incident matching for LEVEE.
//
// The package maintains a curated catalogue of historical incidents, runbooks
// and fix patterns, and exposes a Match API that scores each entry against a
// live diagnosis (root cause, symptoms, tags). The scoring blends three
// signals: tag Jaccard similarity, symptom keyword overlap and root-cause
// keyword overlap. The resulting Match list is sorted by descending score so
// the caller can present the most relevant remediation first.
//
// All public types are safe for concurrent use. The KnowledgeBase never
// panics; load / save errors are propagated through error returns.
package recommend

import (
	"time"
)

// --- RiskLevel ---------------------------------------------------------------

// RiskLevel is the operator-risk classification of a fix pattern. It is
// surfaced to the UI so that high-risk remediations can be gated behind
// explicit approval.
type RiskLevel string

const (
	// RiskLow means the fix is safe to apply automatically.
	RiskLow RiskLevel = "low"
	// RiskMedium means the fix may have side effects and should be
	// reviewed before application.
	RiskMedium RiskLevel = "medium"
	// RiskHigh means the fix affects service availability and requires
	// operator confirmation.
	RiskHigh RiskLevel = "high"
	// RiskCritical means the fix is destructive (e.g. data loss) and
	// requires explicit approval plus audit recording.
	RiskCritical RiskLevel = "critical"
)

// --- HistoricalIncident ------------------------------------------------------

// HistoricalIncident is a single recorded occurrence of a previously
// diagnosed and resolved incident. The knowledge base uses these records to
// suggest remediations for new problems that look similar.
//
// All fields are plain value types so the record serialises cleanly to JSON
// and can be stored, forwarded or rendered without further transformation.
type HistoricalIncident struct {
	// ID is the unique identifier of the incident. It is stable for the
	// lifetime of the record and is used as the Match.ID when the
	// incident is returned by Match.
	ID string `json:"id"`

	// Title is a short human-readable summary, e.g. "Java OOM in
	// order-service".
	Title string `json:"title"`

	// Symptoms is the list of observed symptoms, e.g. ["high RSS",
	// "GC pause > 1s"]. The matcher compares these against the live
	// diagnosis symptoms using keyword overlap.
	Symptoms []string `json:"symptoms"`

	// RootCause is the determined root cause, e.g. "heap exhaustion due
	// to inbound queue backlog". The matcher compares this against the
	// live diagnosis root cause using keyword overlap.
	RootCause string `json:"root_cause"`

	// Resolution is the human-readable description of the action that
	// resolved the incident historically.
	Resolution string `json:"resolution"`

	// Workflow is the LEVEELang workflow that automates the resolution.
	// Empty when no automation exists.
	Workflow string `json:"workflow,omitempty"`

	// Tags is the free-form tag set used for Jaccard similarity, e.g.
	// ["java", "oom", "memory"]. Tags are lower-cased by the knowledge
	// base on insert.
	Tags []string `json:"tags"`

	// Severity is the incident severity, one of "critical", "warning",
	// "info" (matching the alert vocabulary).
	Severity string `json:"severity"`

	// CreatedAt is the wall-clock time at which the record was first
	// created.
	CreatedAt time.Time `json:"created_at"`

	// Occurrences is how many times this incident has been observed.
	// Higher occurrence counts boost the match score slightly because
	// recurring problems are more likely to recur.
	Occurrences int `json:"occurrences"`
}

// --- Runbook -----------------------------------------------------------------

// Runbook is a curated operational playbook that documents how to respond to
// a class of problem. Unlike a HistoricalIncident, a runbook is
// prescriptive: it describes the steps to take rather than the steps that
// were taken.
type Runbook struct {
	// ID is the unique identifier of the runbook.
	ID string `json:"id"`

	// Name is the human-readable name, e.g. "Disk Full Recovery".
	Name string `json:"name"`

	// Description is a longer explanation of when to use the runbook.
	Description string `json:"description"`

	// Trigger is the human-readable trigger condition, e.g.
	// "disk usage > 90%". The matcher compares trigger keywords against
	// the live diagnosis root cause and symptoms.
	Trigger string `json:"trigger"`

	// Steps is the ordered list of actions to take.
	Steps []RunbookStep `json:"steps"`

	// Tags is the free-form tag set used for Jaccard similarity.
	Tags []string `json:"tags"`
}

// RunbookStep is a single action within a runbook. Steps are executed in
// ascending Order.
type RunbookStep struct {
	// Order is the 1-based position of the step within the runbook.
	Order int `json:"order"`

	// Action is a short name for the step, e.g. "clean-logs".
	Action string `json:"action"`

	// Command is the shell command or LEVEELang snippet to execute. It
	// may be empty when the step is purely informational.
	Command string `json:"command,omitempty"`

	// Description is the human-readable explanation of what the step
	// does and why.
	Description string `json:"description"`

	// RiskLevel is the operator-risk classification of the step, one of
	// "low", "medium", "high", "critical".
	RiskLevel string `json:"risk_level"`
}

// --- FixPattern --------------------------------------------------------------

// FixPattern is a reusable remediation pattern that matches a class of
// problems by a regular expression against the root cause or symptoms. When
// the pattern matches, the associated Workflow is offered as a remediation.
type FixPattern struct {
	// ID is the unique identifier of the pattern.
	ID string `json:"id"`

	// Name is the human-readable name, e.g. "Restart Java service".
	Name string `json:"name"`

	// Condition is the regular expression that the matcher tests against
	// the live diagnosis root cause and each symptom. The pattern is
	// compiled lazily on first match and cached.
	Condition string `json:"condition"`

	// Fix is the human-readable description of the fix action.
	Fix string `json:"fix"`

	// Workflow is the LEVEELang workflow that automates the fix.
	// Empty when no automation exists.
	Workflow string `json:"workflow,omitempty"`

	// RiskLevel is the operator-risk classification of the fix.
	RiskLevel RiskLevel `json:"risk_level"`

	// Tags is the free-form tag set used for Jaccard similarity.
	Tags []string `json:"tags"`
}

// --- MatchType ---------------------------------------------------------------

// MatchType identifies the kind of knowledge-base entry a Match refers to.
// It is stored on Match.Type so callers can dispatch on the source type
// without type-asserting Match.Source.
type MatchType string

const (
	// MatchTypeIncident means Match.Source is a HistoricalIncident.
	MatchTypeIncident MatchType = "incident"
	// MatchTypeRunbook means Match.Source is a Runbook.
	MatchTypeRunbook MatchType = "runbook"
	// MatchTypePattern means Match.Source is a FixPattern.
	MatchTypePattern MatchType = "pattern"
)

// --- Match -------------------------------------------------------------------

// Match is a single scored result returned by KnowledgeBase.Match. The list
// returned by Match is sorted by descending Score so the most relevant
// remediation comes first.
type Match struct {
	// Type is the kind of entry: "incident", "runbook" or "pattern".
	Type MatchType `json:"type"`

	// ID is the identifier of the matched entry. It is the same value as
	// the entry's ID field.
	ID string `json:"id"`

	// Title is the human-readable title of the matched entry. For
	// incidents it is the incident title; for runbooks it is the runbook
	// name; for patterns it is the pattern name.
	Title string `json:"title"`

	// Score is the match score in the range [0, 1]. Higher is better.
	// The scoring formula is documented on KnowledgeBase.Match.
	Score float64 `json:"score"`

	// Source is the original matched object (HistoricalIncident, Runbook
	// or FixPattern). Callers can type-switch on Match.Type to access
	// the concrete value.
	Source interface{} `json:"source"`

	// Reason is a human-readable explanation of why the entry matched,
	// e.g. "tags=java,oom; symptoms=2/3; root-cause=0.6". It is
	// intended for display in the recommendation UI.
	Reason string `json:"reason"`
}

// --- Stats -------------------------------------------------------------------

// Stats is the summary statistics returned by KnowledgeBase.Stats. It is a
// snapshot of the knowledge base at the time of the call.
type Stats struct {
	// Incidents is the number of HistoricalIncident records.
	Incidents int `json:"incidents"`
	// Runbooks is the number of Runbook records.
	Runbooks int `json:"runbooks"`
	// Patterns is the number of FixPattern records.
	Patterns int `json:"patterns"`
	// TotalOccurrences is the sum of HistoricalIncident.Occurrences.
	TotalOccurrences int `json:"total_occurrences"`
}

// --- persistedForm -----------------------------------------------------------

// persistedForm is the on-disk JSON representation of the knowledge base.
// It is an internal type used by Save / Load; callers never see it.
type persistedForm struct {
	Incidents []HistoricalIncident `json:"incidents"`
	Runbooks  []Runbook            `json:"runbooks"`
	Patterns  []FixPattern         `json:"patterns"`
}