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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	kb *recommend.KnowledgeBase
	// records holds POINTERS: Learn stamps PatternID onto the stored record,
	// so the record returned by Record must be the stored element itself.
	// (It used to store a value copy and return a pointer to the local copy —
	// Learn wrote the PatternID into a record nobody could read back, and
	// GetRecord/ListRecords served records with a permanently empty
	// PatternID.)
	records []*FeedbackRecord
	// stats aggregates per-pattern counters. The key is PatternID.
	stats map[string]*PatternStat
	// patterns stores the FixPattern values that the learner
	// synthesised from successful outcomes, keyed by PatternID. It
	// is the source for ExportPatterns.
	patterns map[string]recommend.FixPattern
	// incidents stores the HistoricalIncident values synthesised alongside
	// the patterns — the other half of what Learn feeds the knowledge base,
	// and without which a restart would lose the incident-side matches.
	incidents map[string]recommend.HistoricalIncident
	// persistPath is the JSON snapshot file. Empty disables persistence
	// (the learner stays an in-memory object, useful for tests).
	persistPath string
	mu          sync.RWMutex
	log         *slog.Logger
}

// FeedbackLearnerConfig is the configuration for NewFeedbackLearner.
type FeedbackLearnerConfig struct {
	// KnowledgeBase is the target knowledge base that the learner
	// feeds new patterns and incidents into. It must be non-nil.
	KnowledgeBase *recommend.KnowledgeBase

	// Logger is the optional structured logger. When nil the
	// package-level singleton logger is used.
	Logger *slog.Logger

	// PersistPath is the optional JSON snapshot path. When non-empty the
	// learner loads existing state at construction (a missing file is a
	// fresh start, not an error) and writes a snapshot after every
	// successful Record / Learn, so a restart no longer forgets every
	// learned pattern — the residual the roadmap called out
	// ("全内存、进程重启即失忆"). A corrupt or unreadable file is logged
	// and treated as a fresh start: the learner is best-effort learning
	// state, never the system of record (runs and the audit chain are).
	PersistPath string
}

// NewFeedbackLearner returns a learner ready to record outcomes. The
// KnowledgeBase in cfg must be non-nil; a nil KnowledgeBase causes the
// returned learner to return errors on every Record call (it does not
// panic). When PersistPath is set the existing snapshot (if any) is loaded
// best-effort — see FeedbackLearnerConfig.PersistPath.
func NewFeedbackLearner(cfg FeedbackLearnerConfig) *FeedbackLearner {
	l := cfg.Logger
	if l == nil {
		l = log.Logger()
	}
	lrn := &FeedbackLearner{
		kb:          cfg.KnowledgeBase,
		stats:       make(map[string]*PatternStat),
		patterns:    make(map[string]recommend.FixPattern),
		incidents:   make(map[string]recommend.HistoricalIncident),
		persistPath: cfg.PersistPath,
		log:         l,
	}
	if lrn.persistPath != "" {
		lrn.loadPersisted()
	}
	return lrn
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
	l.records = append(l.records, &rec)
	stored := l.records[len(l.records)-1]
	// PatternID is not set by Record (it is assigned by Learn when a
	// new pattern is synthesised), so there is nothing to aggregate
	// here. Stats are updated in Learn once the pattern relationship
	// is known.
	l.log.Debug("feedback: record stored",
		"id", stored.ID,
		"target", stored.Outcome.Target,
		"success", stored.Outcome.Success,
		"pattern_id", stored.PatternID,
	)
	l.persistLocked()
	return stored, nil
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
		l.incidents[incident.ID] = incident
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
	l.persistLocked()
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
	for i, rec := range l.records {
		out[i] = *rec
	}
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
	for _, rec := range l.records {
		if rec.ID == id {
			cp := *rec
			return &cp, nil
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
	for i, rec := range l.records {
		out[i] = *rec
	}
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

// --- Persistence -------------------------------------------------------------

// feedbackSnapshot is the on-disk shape of the learner's state. It carries
// everything needed to rebuild the in-memory learner AND refill the shared
// KnowledgeBase after a restart: the records (audit of what was learned
// from), the per-pattern counters, and the synthesised patterns + incidents.
type feedbackSnapshot struct {
	Records   []*FeedbackRecord                       `json:"records"`
	Stats     map[string]*PatternStat                 `json:"stats"`
	Patterns  map[string]recommend.FixPattern         `json:"patterns"`
	Incidents map[string]recommend.HistoricalIncident `json:"incidents"`
}

// loadPersisted reads the snapshot (best-effort) and rebuilds the learner
// state, re-adding synthesised patterns and incidents to the shared
// KnowledgeBase so learned matches survive a restart. A missing file is a
// fresh start; a corrupt file is logged and treated as one — the learner is
// auxiliary learning state, never the system of record, so a bad snapshot
// must not keep the daemon from starting.
func (l *FeedbackLearner) loadPersisted() {
	raw, err := os.ReadFile(l.persistPath)
	if err != nil {
		if !os.IsNotExist(err) {
			l.log.Warn("feedback: cannot read snapshot; starting fresh",
				"path", l.persistPath, "error", err)
		}
		return
	}
	var snap feedbackSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		l.log.Warn("feedback: snapshot unreadable; starting fresh",
			"path", l.persistPath, "error", err)
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = snap.Records
	if snap.Stats != nil {
		l.stats = snap.Stats
	}
	if snap.Patterns != nil {
		l.patterns = snap.Patterns
	}
	if snap.Incidents != nil {
		l.incidents = snap.Incidents
	}
	if l.kb != nil {
		for _, p := range l.patterns {
			if err := l.kb.AddPattern(p); err != nil {
				l.log.Warn("feedback: re-add pattern failed", "pattern_id", p.ID, "error", err)
			}
		}
		for _, inc := range l.incidents {
			if err := l.kb.AddIncident(inc); err != nil {
				l.log.Warn("feedback: re-add incident failed", "incident_id", inc.ID, "error", err)
			}
		}
	}
	l.log.Info("feedback: snapshot loaded",
		"path", l.persistPath,
		"records", len(l.records),
		"patterns", len(l.patterns),
		"incidents", len(l.incidents))
}

// persistLocked writes the snapshot atomically (temp file + rename). The
// caller must hold l.mu. Failures are logged, never returned: persistence is
// durability of LEARNING, and a failed write must not fail the run outcome
// that produced it.
func (l *FeedbackLearner) persistLocked() {
	if l.persistPath == "" {
		return
	}
	snap := feedbackSnapshot{
		Records:   l.records,
		Stats:     l.stats,
		Patterns:  l.patterns,
		Incidents: l.incidents,
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		l.log.Warn("feedback: marshal snapshot failed", "error", err)
		return
	}
	tmp := l.persistPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		l.log.Warn("feedback: write snapshot failed", "path", tmp, "error", err)
		return
	}
	if err := os.Rename(tmp, l.persistPath); err != nil {
		l.log.Warn("feedback: rename snapshot failed", "path", l.persistPath, "error", err)
	}
}

// LoadInto restores a persisted snapshot into kb without constructing a
// learner — for read-only consumers (the CLI) that want learned patterns in
// their recommend engine but never record outcomes themselves (the serve
// path owns the effect-learning loop). Empty path or a missing/corrupt file
// is a no-op-with-log, same best-effort contract as the learner's own load.
func LoadInto(kb *recommend.KnowledgeBase, path string) {
	if kb == nil || path == "" {
		return
	}
	l := &FeedbackLearner{
		kb:          kb,
		stats:       make(map[string]*PatternStat),
		patterns:    make(map[string]recommend.FixPattern),
		incidents:   make(map[string]recommend.HistoricalIncident),
		persistPath: path,
		log:         log.Logger(),
	}
	l.loadPersisted()
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
