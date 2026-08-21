// learner.go implements the FeedbackLearner that consumes FixOutcome records,
// persists them as FeedbackRecords, and distils them into KnowledgeBase
// entries.
//
// The learner keeps two indexes alongside the raw record list:
//
//   - stats:    per-PatternID aggregate counters (uses / successes / failures).
//   - patterns: the FixPattern values that the learner itself synthesised
//               from successful outcomes, keyed by PatternID. Patterns that
//               were supplied by the caller (record.PatternID already set)
//               are NOT inserted here; they are assumed to live in the
//               KnowledgeBase already.
//
// All public methods take the write lock when mutating and the read lock when
// reading. The learner never panics; validation errors are returned through
// error returns.

package feedback

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/recommend"
)

// --- Sentinel errors ---------------------------------------------------------

var (
	// ErrEmptyTarget is returned when Record is called with an outcome
	// whose Target is empty.
	ErrEmptyTarget = errors.New("feedback: empty target")
	// ErrEmptyFixAction is returned when Record is called with an
	// outcome whose FixAction is empty.
	ErrEmptyFixAction = errors.New("feedback: empty fix action")
	// ErrRecordNotFound is returned when GetRecord is called with an
	// ID that is not present.
	ErrRecordNotFound = errors.New("feedback: record not found")
)

// --- Limits ------------------------------------------------------------------

const (
	// maxTopPatterns is the maximum number of PatternStat entries
	// returned in LearningStats.TopPatterns.
	maxTopPatterns = 10
	// maxRecentRecords is the maximum number of FeedbackRecord
	// entries returned in LearningStats.RecentRecords.
	maxRecentRecords = 10
	// maxListRecords is the default cap on ListRecords when the
	// caller passes a non-positive limit.
	maxListRecords = 100
)

// --- FeedbackLearner ---------------------------------------------------------

// FeedbackLearner consumes FixOutcome records, persists them as
// FeedbackRecords, and feeds the learned patterns back into a
// recommend.KnowledgeBase.
//
// A FeedbackLearner is safe for concurrent use by any number of goroutines.
// The zero value is not usable; callers must use NewFeedbackLearner.
type FeedbackLearner struct {
	kb      *recommend.KnowledgeBase
	records []FeedbackRecord
	// stats aggregates per-pattern counters. The key is PatternID.
	stats map[string]*PatternStat
	// patterns stores the FixPattern values that the learner
	// synthesised from successful outcomes, keyed by PatternID. It
	// is the source for ExportPatterns.
	patterns map[string]recommend.FixPattern
	mu       sync.RWMutex
	log      *slog.Logger
}

// FeedbackLearnerConfig is the configuration for NewFeedbackLearner.
type FeedbackLearnerConfig struct {
	// KnowledgeBase is the target knowledge base that the learner
	// feeds new patterns and incidents into. It must be non-nil.
	KnowledgeBase *recommend.KnowledgeBase

	// Logger is the optional structured logger. When nil the
	// package-level singleton logger is used.
	Logger *slog.Logger
}

// NewFeedbackLearner returns a learner ready to record outcomes. The
// KnowledgeBase in cfg must be non-nil; a nil KnowledgeBase causes the
// returned learner to return errors on every Record call (it does not
// panic).
func NewFeedbackLearner(cfg FeedbackLearnerConfig) *FeedbackLearner {
	l := cfg.Logger
	if l == nil {
		l = log.Logger()
	}
	return &FeedbackLearner{
		kb:       cfg.KnowledgeBase,
		stats:    make(map[string]*PatternStat),
		patterns: make(map[string]recommend.FixPattern),
		log:      l,
	}
}

// --- Record ------------------------------------------------------------------

// Record validates the outcome, wraps it in a FeedbackRecord with a fresh
// UUID, appends it to the record list, and updates the per-pattern stats.
// It does NOT interact with the KnowledgeBase; call Learn (or
// RecordAndLearn) to feed the record into the knowledge base.
//
// Record returns ErrEmptyTarget when outcome.Target is empty and
// ErrEmptyFixAction when outcome.FixAction is empty.
func (l *FeedbackLearner) Record(outcome FixOutcome) (*FeedbackRecord, error) {
	if outcome.Target == "" {
		return nil, ErrEmptyTarget
	}
	if outcome.FixAction == "" {
		return nil, ErrEmptyFixAction
	}

	now := time.Now().UTC()
	createdAt := outcome.Timestamp
	if createdAt.IsZero() {
		createdAt = now
	}

	rec := FeedbackRecord{
		ID:        uuid.NewString(),
		Outcome:   outcome,
		CreatedAt: createdAt,
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, rec)
	// PatternID is not set by Record (it is assigned by Learn when a
	// new pattern is synthesised), so there is nothing to aggregate
	// here. Stats are updated in Learn once the pattern relationship
	// is known.
	l.log.Debug("feedback: record stored",
		"id", rec.ID,
		"target", rec.Outcome.Target,
		"success", rec.Outcome.Success,
		"pattern_id", rec.PatternID,
	)
	return &rec, nil
}

// bumpPatternStatLocked updates the per-pattern stats for the given pattern.
// desc is stored when the stat is first created; subsequent calls keep the
// existing desc. The caller must hold l.mu.
func (l *FeedbackLearner) bumpPatternStatLocked(patternID, desc string, success bool) {
	st := l.stats[patternID]
	if st == nil {
		st = &PatternStat{
			PatternID:   patternID,
			PatternDesc: desc,
		}
		l.stats[patternID] = st
	}
	st.Uses++
	if success {
		st.Successes++
	} else {
		st.Failures++
	}
	if st.Uses > 0 {
		st.SuccessRate = float64(st.Successes) / float64(st.Uses)
	}
}

// --- Learn -------------------------------------------------------------------

// Learn feeds a record into the KnowledgeBase. The learning rules are:
//
//   - Success and no PatternID: synthesise a new FixPattern and a
//     HistoricalIncident from the outcome, add them to the KnowledgeBase,
//     and stamp the record with the new PatternID. The new pattern is also
//     remembered for ExportPatterns.
//   - Success and PatternID set: increment the pattern's success counter.
//   - Failure and PatternID set: increment the pattern's failure counter.
//   - Failure and no PatternID: log the failure for observability; no
//     KnowledgeBase mutation is performed.
//
// Learn returns an error when the KnowledgeBase rejects the synthesised
// pattern or incident (e.g. duplicate ID, invalid regex). In that case the
// record is left unchanged and the caller may retry or drop the record.
func (l *FeedbackLearner) Learn(record *FeedbackRecord) error {
	if record == nil {
		return errors.New("feedback: learn: nil record")
	}
	if l.kb == nil {
		return errors.New("feedback: knowledge base not configured")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	outcome := record.Outcome
	switch {
	case outcome.Success && record.PatternID == "":
		// Synthesise a new pattern + incident from the outcome.
		pid := uuid.NewString()
		pattern := newPatternFromOutcome(pid, outcome)
		incident := newIncidentFromOutcome(pid, outcome)

		if err := l.kb.AddPattern(pattern); err != nil {
			return fmt.Errorf("feedback: learn: add pattern: %w", err)
		}
		if err := l.kb.AddIncident(incident); err != nil {
			// Best-effort rollback: remove the pattern we just
			// added so the knowledge base does not carry an
			// orphan pattern. A failure here is logged but not
			// returned, because the primary error is the
			// incident add.
			if rmErr := l.kb.RemovePattern(pid); rmErr != nil {
				l.log.Warn("feedback: learn: rollback add pattern failed",
					"pattern_id", pid, "err", rmErr)
			}
			return fmt.Errorf("feedback: learn: add incident: %w", err)
		}

		record.PatternID = pid
		l.patterns[pid] = pattern
		// Record the first successful use of the new pattern in the
		// per-pattern stats. pid is a fresh UUID so no prior stat
		// exists; bumpPatternStatLocked creates one.
		l.bumpPatternStatLocked(pid, pattern.Name, true)
		l.log.Info("feedback: learned new pattern",
			"pattern_id", pid,
			"name", pattern.Name,
			"target", outcome.Target,
		)

	case outcome.Success && record.PatternID != "":
		l.bumpSuccessLocked(record.PatternID)
		l.log.Debug("feedback: pattern success recorded",
			"pattern_id", record.PatternID)

	case !outcome.Success && record.PatternID != "":
		l.bumpFailureLocked(record.PatternID)
		l.log.Debug("feedback: pattern failure recorded",
			"pattern_id", record.PatternID)

	default:
		// Failure with no pattern: nothing to learn.
		l.log.Debug("feedback: failure without pattern; nothing to learn",
			"target", outcome.Target)
	}
	return nil
}

// bumpSuccessLocked increments the success counter for the given pattern.
// The caller must hold l.mu.
func (l *FeedbackLearner) bumpSuccessLocked(patternID string) {
	st := l.stats[patternID]
	if st == nil {
		st = &PatternStat{PatternID: patternID}
		l.stats[patternID] = st
	}
	st.Successes++
	st.Uses = maxInt(st.Uses, st.Successes+st.Failures)
	st.SuccessRate = float64(st.Successes) / float64(st.Uses)
}

// bumpFailureLocked increments the failure counter for the given pattern.
// The caller must hold l.mu.
func (l *FeedbackLearner) bumpFailureLocked(patternID string) {
	st := l.stats[patternID]
	if st == nil {
		st = &PatternStat{PatternID: patternID}
		l.stats[patternID] = st
	}
	st.Failures++
	st.Uses = maxInt(st.Uses, st.Successes+st.Failures)
	st.SuccessRate = float64(st.Successes) / float64(st.Uses)
}

// maxInt returns the larger of a and b.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- RecordAndLearn ----------------------------------------------------------

// RecordAndLearn is the convenience wrapper that calls Record then Learn.
// When Record fails the error is returned and Learn is not called. When
// Learn fails the record has already been stored; the caller receives the
// record together with the Learn error so it can retry Learn if it wishes.
func (l *FeedbackLearner) RecordAndLearn(outcome FixOutcome) (*FeedbackRecord, error) {
	rec, err := l.Record(outcome)
	if err != nil {
		return nil, err
	}
	if lerr := l.Learn(rec); lerr != nil {
		return rec, lerr
	}
	return rec, nil
}

// --- GetStats ----------------------------------------------------------------

// GetStats returns a point-in-time snapshot of the learner. The returned
// value is a deep copy and safe for the caller to mutate.
func (l *FeedbackLearner) GetStats() *LearningStats {
	l.mu.RLock()
	defer l.mu.RUnlock()

	stats := &LearningStats{
		TotalRecords: len(l.records),
	}
	for i := range l.records {
		if l.records[i].Outcome.Success {
			stats.SuccessCount++
		} else {
			stats.FailureCount++
		}
	}
	if stats.TotalRecords > 0 {
		stats.SuccessRate = float64(stats.SuccessCount) / float64(stats.TotalRecords)
	}
	stats.TopPatterns = l.topPatternsLocked()
	stats.RecentRecords = l.recentRecordsLocked()
	return stats
}

// topPatternsLocked returns the top patterns sorted by descending SuccessRate
// then by descending Uses. The caller must hold l.mu in read mode.
func (l *FeedbackLearner) topPatternsLocked() []PatternStat {
	out := make([]PatternStat, 0, len(l.stats))
	for _, st := range l.stats {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SuccessRate != out[j].SuccessRate {
			return out[i].SuccessRate > out[j].SuccessRate
		}
		return out[i].Uses > out[j].Uses
	})
	if len(out) > maxTopPatterns {
		out = out[:maxTopPatterns]
	}
	return out
}

// recentRecordsLocked returns the most recent records ordered by descending
// CreatedAt. The caller must hold l.mu in read mode.
func (l *FeedbackLearner) recentRecordsLocked() []FeedbackRecord {
	out := make([]FeedbackRecord, len(l.records))
	copy(out, l.records)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > maxRecentRecords {
		out = out[:maxRecentRecords]
	}
	return out
}

// --- GetRecord ---------------------------------------------------------------

// GetRecord returns the record with the given ID. It returns
// ErrRecordNotFound when no such record exists.
func (l *FeedbackLearner) GetRecord(id string) (*FeedbackRecord, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for i := range l.records {
		if l.records[i].ID == id {
			rec := l.records[i]
			return &rec, nil
		}
	}
	return nil, fmt.Errorf("feedback: get record %s: %w", id, ErrRecordNotFound)
}

// --- ListRecords -------------------------------------------------------------

// ListRecords returns the most recent records ordered by descending
// CreatedAt, up to limit entries. When limit is non-positive a default cap
// of maxListRecords is applied. The returned slice is a copy and safe for
// the caller to mutate.
func (l *FeedbackLearner) ListRecords(limit int) []FeedbackRecord {
	if limit <= 0 {
		limit = maxListRecords
	}
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]FeedbackRecord, len(l.records))
	copy(out, l.records)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// --- ExportPatterns ----------------------------------------------------------

// ExportPatterns returns the FixPattern values that the learner has
// synthesised from successful outcomes. Patterns that were supplied by the
// caller (record.PatternID already set on Record) are not included. The
// returned slice is ordered by pattern ID for deterministic output and is
// safe for the caller to mutate.
func (l *FeedbackLearner) ExportPatterns() []recommend.FixPattern {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]recommend.FixPattern, 0, len(l.patterns))
	for _, p := range l.patterns {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

// --- Outcome -> KB entry helpers ---------------------------------------------

// newPatternFromOutcome synthesises a FixPattern from a successful outcome.
// The condition is the quoted root cause (or symptoms when the root cause
// is empty) so that the pattern matches future diagnoses with the same
// textual signature.
func newPatternFromOutcome(pid string, o FixOutcome) recommend.FixPattern {
	condition := regexp.QuoteMeta(o.RootCause)
	if condition == "" {
		condition = regexp.QuoteMeta(o.Symptoms)
	}
	if condition == "" {
		// Fall back to a permissive match keyed on the target so
		// the pattern is still useful when both root cause and
		// symptoms are empty.
		condition = regexp.QuoteMeta(o.Target)
	}
	name := o.FixAction
	if len(name) > 60 {
		name = name[:60]
	}
	return recommend.FixPattern{
		ID:        pid,
		Name:      name,
		Condition: condition,
		Fix:       o.FixAction,
		Workflow:  "",
		RiskLevel: recommend.RiskMedium,
		Tags:      []string{strings.ToLower(o.Target)},
	}
}

// newIncidentFromOutcome synthesises a HistoricalIncident from a successful
// outcome. The incident records what was wrong and how it was fixed so the
// matcher can surface it for similar future problems.
func newIncidentFromOutcome(pid string, o FixOutcome) recommend.HistoricalIncident {
	title := o.RootCause
	if title == "" {
		title = o.Symptoms
	}
	if title == "" {
		title = o.Target
	}
	title = fmt.Sprintf("%s on %s", title, o.Target)

	symptoms := []string{}
	if o.Symptoms != "" {
		symptoms = append(symptoms, o.Symptoms)
	}

	severity := "warning"
	if o.RollbackUsed {
		severity = "critical"
	}

	return recommend.HistoricalIncident{
		ID:          pid,
		Title:       title,
		Symptoms:    symptoms,
		RootCause:   o.RootCause,
		Resolution:  o.FixAction,
		Workflow:    "",
		Tags:        []string{strings.ToLower(o.Target)},
		Severity:    severity,
		CreatedAt:   time.Now().UTC(),
		Occurrences: 1,
	}
}
