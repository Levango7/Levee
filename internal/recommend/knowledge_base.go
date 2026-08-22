package recommend

// knowledge_base.go implements Phase B1 of LEVEE's recommendation engine: the
// KnowledgeBase that stores historical incidents, runbooks and fix patterns
// and scores them against a live diagnosis.
//
// The scoring strategy is intentionally lightweight (no embeddings, no ML
// model) so the knowledge base can run in-process on the agent without
// external dependencies. The three signals are:
//
//   - Tag Jaccard similarity: |A ∩ B| / |A ∪ B|.
//   - Symptom keyword overlap: each input symptom is tokenised and matched
//     against the entry's symptom tokens; the score is the fraction of
//     input tokens that appear in the entry.
//   - Root-cause keyword overlap: same tokenisation, applied to the root
//     cause strings.
//
// The final score is a weighted blend:
//
//	score = 0.4*tagScore + 0.3*symptomScore + 0.3*rootCauseScore
//
// Recurring incidents (Occurrences > 1) get a small boost so that frequent
// problems surface earlier. The boost is capped at 0.1 so it cannot dominate
// the textual signals.
//
// All public methods are safe for concurrent use. The KnowledgeBase never
// panics; load / save errors are propagated through error returns.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors ---------------------------------------------------------

var (
	// ErrEmptyID is returned when adding an entry with an empty ID.
	ErrEmptyID = errors.New("recommend: empty id")
	// ErrDuplicateID is returned when adding an entry whose ID already
	// exists in the knowledge base.
	ErrDuplicateID = errors.New("recommend: duplicate id")
	// ErrNotFound is returned when looking up an ID that is not present.
	ErrNotFound = errors.New("recommend: not found")
	// ErrEmptyPath is returned when Load / Save is called with an empty
	// path.
	ErrEmptyPath = errors.New("recommend: empty path")
)

// --- Scoring weights ---------------------------------------------------------

// Scoring weights. Exported as constants so tests and callers can reason
// about the blend; they are not meant to be reconfigured at runtime.
const (
	// weightTags is the weight of the tag Jaccard score in the final
	// blend.
	weightTags = 0.4
	// weightSymptoms is the weight of the symptom overlap score.
	weightSymptoms = 0.3
	// weightRootCause is the weight of the root-cause overlap score.
	weightRootCause = 0.3
	// maxOccurrenceBoost is the cap on the recurrence boost added to
	// the final score.
	maxOccurrenceBoost = 0.1
)

// --- KnowledgeBase -----------------------------------------------------------

// KnowledgeBase is the in-memory store of historical incidents, runbooks and
// fix patterns. It is the entry point for the recommendation engine: callers
// add entries (or load them from disk), then call Match to score them
// against a live diagnosis.
//
// A KnowledgeBase is safe for concurrent use by any number of goroutines.
// The zero value is not usable; callers must use NewKnowledgeBase.
type KnowledgeBase struct {
	mu        sync.RWMutex
	incidents []HistoricalIncident
	runbooks  []Runbook
	patterns  []FixPattern
	// compiledPatterns caches compiled regexes keyed by pattern index
	// in the patterns slice. The cache is invalidated whenever the
	// patterns slice changes. It is protected by mu.
	compiledPatterns []*regexp.Regexp
	log              *slog.Logger
}

// NewKnowledgeBase returns an empty KnowledgeBase initialised with the
// package-level singleton logger. Callers that want a different logger can
// call SetLogger.
func NewKnowledgeBase() *KnowledgeBase {
	return &KnowledgeBase{
		log: log.Logger(),
	}
}

// NewKnowledgeBaseWithDefaults returns a KnowledgeBase preloaded with the
// built-in catalogue of 5 historical incidents and 3 runbooks. It is the
// convenient constructor for production use.
func NewKnowledgeBaseWithDefaults() *KnowledgeBase {
	kb := NewKnowledgeBase()
	for i := range defaultIncidents {
		kb.incidents = append(kb.incidents, defaultIncidents[i])
	}
	for i := range defaultRunbooks {
		kb.runbooks = append(kb.runbooks, defaultRunbooks[i])
	}
	for i := range defaultPatterns {
		kb.patterns = append(kb.patterns, defaultPatterns[i])
	}
	kb.recompilePatterns()
	return kb
}

// SetLogger installs a new structured logger. It is safe to call concurrently
// but should typically only be called once at construction time.
func (kb *KnowledgeBase) SetLogger(l *slog.Logger) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	if l == nil {
		l = log.Logger()
	}
	kb.log = l
}

// --- Add ---------------------------------------------------------------------

// AddIncident adds a historical incident to the knowledge base. It returns
// ErrEmptyID when the incident has no ID and ErrDuplicateID when an incident
// with the same ID already exists. Tags are lower-cased on insert so that
// tag matching is case-insensitive.
func (kb *KnowledgeBase) AddIncident(inc HistoricalIncident) error {
	if inc.ID == "" {
		return ErrEmptyID
	}
	inc.Tags = normalizeTags(inc.Tags)
	if inc.CreatedAt.IsZero() {
		inc.CreatedAt = time.Now().UTC()
	}
	kb.mu.Lock()
	defer kb.mu.Unlock()
	if kb.findIncidentLocked(inc.ID) >= 0 {
		return fmt.Errorf("recommend: add incident %s: %w", inc.ID, ErrDuplicateID)
	}
	kb.incidents = append(kb.incidents, inc)
	kb.log.Debug("recommend: incident added", "id", inc.ID, "tags", inc.Tags)
	return nil
}

// AddRunbook adds a runbook to the knowledge base. It returns ErrEmptyID
// when the runbook has no ID and ErrDuplicateID when a runbook with the
// same ID already exists. Tags are lower-cased on insert.
func (kb *KnowledgeBase) AddRunbook(rb Runbook) error {
	if rb.ID == "" {
		return ErrEmptyID
	}
	rb.Tags = normalizeTags(rb.Tags)
	kb.mu.Lock()
	defer kb.mu.Unlock()
	if kb.findRunbookLocked(rb.ID) >= 0 {
		return fmt.Errorf("recommend: add runbook %s: %w", rb.ID, ErrDuplicateID)
	}
	kb.runbooks = append(kb.runbooks, rb)
	kb.log.Debug("recommend: runbook added", "id", rb.ID, "tags", rb.Tags)
	return nil
}

// AddPattern adds a fix pattern to the knowledge base. It returns ErrEmptyID
// when the pattern has no ID and ErrDuplicateID when a pattern with the
// same ID already exists. The pattern's regex is compiled eagerly so that
// syntax errors are reported at insert time rather than at match time.
func (kb *KnowledgeBase) AddPattern(p FixPattern) error {
	if p.ID == "" {
		return ErrEmptyID
	}
	if p.Condition == "" {
		return fmt.Errorf("recommend: add pattern %s: empty condition", p.ID)
	}
	re, err := regexp.Compile(p.Condition)
	if err != nil {
		return fmt.Errorf("recommend: add pattern %s: compile condition: %w", p.ID, err)
	}
	p.Tags = normalizeTags(p.Tags)
	kb.mu.Lock()
	defer kb.mu.Unlock()
	if kb.findPatternLocked(p.ID) >= 0 {
		return fmt.Errorf("recommend: add pattern %s: %w", p.ID, ErrDuplicateID)
	}
	kb.patterns = append(kb.patterns, p)
	kb.compiledPatterns = append(kb.compiledPatterns, re)
	kb.log.Debug("recommend: pattern added", "id", p.ID, "condition", p.Condition)
	return nil
}

// --- Remove ------------------------------------------------------------------

// RemoveIncident removes the incident with the given ID. It returns
// ErrNotFound when the ID is not present.
func (kb *KnowledgeBase) RemoveIncident(id string) error {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	idx := kb.findIncidentLocked(id)
	if idx < 0 {
		return fmt.Errorf("recommend: remove incident %s: %w", id, ErrNotFound)
	}
	kb.incidents = append(kb.incidents[:idx], kb.incidents[idx+1:]...)
	return nil
}

// RemoveRunbook removes the runbook with the given ID. It returns ErrNotFound
// when the ID is not present.
func (kb *KnowledgeBase) RemoveRunbook(id string) error {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	idx := kb.findRunbookLocked(id)
	if idx < 0 {
		return fmt.Errorf("recommend: remove runbook %s: %w", id, ErrNotFound)
	}
	kb.runbooks = append(kb.runbooks[:idx], kb.runbooks[idx+1:]...)
	return nil
}

// RemovePattern removes the fix pattern with the given ID. It returns
// ErrNotFound when the ID is not present.
func (kb *KnowledgeBase) RemovePattern(id string) error {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	idx := kb.findPatternLocked(id)
	if idx < 0 {
		return fmt.Errorf("recommend: remove pattern %s: %w", id, ErrNotFound)
	}
	kb.patterns = append(kb.patterns[:idx], kb.patterns[idx+1:]...)
	// Rebuild the compiled cache from the remaining patterns. We recompile
	// rather than splice because the conditions are stable strings and
	// the cost is negligible for typical catalogue sizes.
	kb.recompilePatternsLocked()
	return nil
}

// recompilePatterns rebuilds the compiled-pattern cache from kb.patterns.
// The caller must hold kb.mu.
func (kb *KnowledgeBase) recompilePatternsLocked() {
	kb.compiledPatterns = kb.compiledPatterns[:0]
	for _, p := range kb.patterns {
		re, err := regexp.Compile(p.Condition)
		if err != nil {
			// Should not happen because AddPattern validates, but
			// be defensive: skip patterns that no longer compile.
			kb.log.Warn("recommend: pattern no longer compiles", "id", p.ID, "err", err)
			kb.compiledPatterns = append(kb.compiledPatterns, nil)
			continue
		}
		kb.compiledPatterns = append(kb.compiledPatterns, re)
	}
}

// recompilePatterns is the public wrapper used by NewKnowledgeBaseWithDefaults
// which does not yet need the lock.
func (kb *KnowledgeBase) recompilePatterns() {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	kb.recompilePatternsLocked()
}

// --- Lookup helpers (caller holds mu) ----------------------------------------

func (kb *KnowledgeBase) findIncidentLocked(id string) int {
	for i := range kb.incidents {
		if kb.incidents[i].ID == id {
			return i
		}
	}
	return -1
}

func (kb *KnowledgeBase) findRunbookLocked(id string) int {
	for i := range kb.runbooks {
		if kb.runbooks[i].ID == id {
			return i
		}
	}
	return -1
}

func (kb *KnowledgeBase) findPatternLocked(id string) int {
	for i := range kb.patterns {
		if kb.patterns[i].ID == id {
			return i
		}
	}
	return -1
}

// --- Match -------------------------------------------------------------------

// Match scores every entry in the knowledge base against the given diagnosis
// and returns the results sorted by descending Score. Entries with a score
// of 0 are excluded from the result.
//
// The inputs are:
//
//   - rootCause: the diagnosis root-cause string (may be empty).
//   - symptoms: the observed symptom strings (may be nil).
//   - tags: the diagnosis tags (may be nil).
//
// The returned slice is newly allocated and safe for the caller to mutate.
// The Source field of each Match points into the knowledge base's storage;
// callers must not mutate it.
func (kb *KnowledgeBase) Match(rootCause string, symptoms []string, tags []string) ([]*Match, error) {
	incidents, err := kb.MatchIncidents(rootCause, symptoms, tags)
	if err != nil {
		return nil, fmt.Errorf("recommend: match incidents: %w", err)
	}
	runbooks, err := kb.MatchRunbooks(rootCause, symptoms, tags)
	if err != nil {
		return nil, fmt.Errorf("recommend: match runbooks: %w", err)
	}
	patterns, err := kb.MatchPatterns(rootCause, symptoms, tags)
	if err != nil {
		return nil, fmt.Errorf("recommend: match patterns: %w", err)
	}

	out := make([]*Match, 0, len(incidents)+len(runbooks)+len(patterns))
	out = append(out, incidents...)
	out = append(out, runbooks...)
	out = append(out, patterns...)
	sortMatches(out)
	return out, nil
}

// MatchIncidents scores historical incidents against the diagnosis. The
// score is the weighted blend described at the top of this file, plus a
// capped recurrence boost. Incidents with a score of 0 are excluded.
func (kb *KnowledgeBase) MatchIncidents(rootCause string, symptoms []string, tags []string) ([]*Match, error) {
	normalizedTags := normalizeTags(tags)
	symptomTokens := tokenizeStrings(symptoms)
	rootCauseTokens := tokenize(rootCause)

	kb.mu.RLock()
	defer kb.mu.RUnlock()

	out := make([]*Match, 0, len(kb.incidents))
	for i := range kb.incidents {
		inc := &kb.incidents[i]
		tagScore := jaccard(normalizedTags, inc.Tags)
		symptomScore := tokenOverlap(symptomTokens, tokenizeStrings(inc.Symptoms))
		rootScore := tokenOverlap(rootCauseTokens, tokenize(inc.RootCause))

		score := weightTags*tagScore + weightSymptoms*symptomScore + weightRootCause*rootScore
		score += occurrenceBoost(inc.Occurrences)
		if score > 1 {
			score = 1
		}
		if score <= 0 {
			continue
		}
		out = append(out, &Match{
			Type:   MatchTypeIncident,
			ID:     inc.ID,
			Title:  inc.Title,
			Score:  score,
			Source: *inc,
			Reason: fmt.Sprintf("tags=%.2f symptoms=%.2f root=%.2f occ=%d",
				tagScore, symptomScore, rootScore, inc.Occurrences),
		})
	}
	sortMatches(out)
	return out, nil
}

// MatchRunbooks scores runbooks against the diagnosis. The score blends tag
// Jaccard similarity with trigger keyword overlap (the trigger is matched
// against both the root cause and the symptoms). Runbooks with a score of 0
// are excluded.
func (kb *KnowledgeBase) MatchRunbooks(rootCause string, symptoms []string, tags []string) ([]*Match, error) {
	normalizedTags := normalizeTags(tags)
	diagTokens := newTokenSet()
	diagTokens.addAll(tokenize(rootCause))
	diagTokens.addAll(tokenizeStrings(symptoms))

	kb.mu.RLock()
	defer kb.mu.RUnlock()

	out := make([]*Match, 0, len(kb.runbooks))
	for i := range kb.runbooks {
		rb := &kb.runbooks[i]
		tagScore := jaccard(normalizedTags, rb.Tags)
		triggerTokens := tokenize(rb.Trigger)
		// Also tokenise the runbook name and description so that a
		// runbook named "Disk Full Recovery" matches a "disk full"
		// root cause even when the trigger field is empty.
		triggerTokens = append(triggerTokens, tokenize(rb.Name)...)
		triggerScore := tokenOverlap(diagTokens.tokens, triggerTokens)

		// Runbook blend: tags 0.5, trigger 0.5. The weights differ
		// from the incident blend because runbooks have no symptom or
		// root-cause field; the trigger is the single textual signal.
		score := 0.5*tagScore + 0.5*triggerScore
		if score <= 0 {
			continue
		}
		out = append(out, &Match{
			Type:   MatchTypeRunbook,
			ID:     rb.ID,
			Title:  rb.Name,
			Score:  score,
			Source: *rb,
			Reason: fmt.Sprintf("tags=%.2f trigger=%.2f", tagScore, triggerScore),
		})
	}
	sortMatches(out)
	return out, nil
}

// MatchPatterns scores fix patterns against the diagnosis. A pattern matches
// when its regular expression matches the root cause or any symptom. The
// score is 1.0 for a full match, scaled by tag Jaccard similarity so that
// patterns with overlapping tags rank higher. Patterns that do not match are
// excluded.
func (kb *KnowledgeBase) MatchPatterns(rootCause string, symptoms []string, tags []string) ([]*Match, error) {
	normalizedTags := normalizeTags(tags)

	kb.mu.RLock()
	defer kb.mu.RUnlock()

	out := make([]*Match, 0, len(kb.patterns))
	for i := range kb.patterns {
		p := &kb.patterns[i]
		re := kb.compiledPatternAt(i)
		if re == nil {
			continue
		}
		matched := false
		matchedOn := ""
		if re.MatchString(rootCause) {
			matched = true
			matchedOn = "root-cause"
		}
		if !matched {
			for _, s := range symptoms {
				if re.MatchString(s) {
					matched = true
					matchedOn = "symptom"
					break
				}
			}
		}
		if !matched {
			continue
		}
		tagScore := jaccard(normalizedTags, p.Tags)
		// Base 0.7 for a regex hit, plus up to 0.3 from tag overlap.
		score := 0.7 + 0.3*tagScore
		if score > 1 {
			score = 1
		}
		out = append(out, &Match{
			Type:   MatchTypePattern,
			ID:     p.ID,
			Title:  p.Name,
			Score:  score,
			Source: *p,
			Reason: fmt.Sprintf("regex=%s tags=%.2f", matchedOn, tagScore),
		})
	}
	sortMatches(out)
	return out, nil
}

// compiledPatternAt returns the compiled regex for the pattern at index i,
// or nil when the cache is stale or the pattern failed to compile. The
// caller must hold kb.mu in read mode.
func (kb *KnowledgeBase) compiledPatternAt(i int) *regexp.Regexp {
	if i < 0 || i >= len(kb.compiledPatterns) {
		return nil
	}
	return kb.compiledPatterns[i]
}

// --- Persistence -------------------------------------------------------------

// Save writes the knowledge base to a JSON file at the given path. The file
// is truncated and rewritten atomically-ish: it is written to a sibling
// temp file and then renamed, so a crash mid-write leaves the previous
// file intact. The parent directory must exist.
func (kb *KnowledgeBase) Save(path string) error {
	if path == "" {
		return ErrEmptyPath
	}
	kb.mu.RLock()
	form := persistedForm{
		Incidents: append([]HistoricalIncident(nil), kb.incidents...),
		Runbooks:  append([]Runbook(nil), kb.runbooks...),
		Patterns:  append([]FixPattern(nil), kb.patterns...),
	}
	kb.mu.RUnlock()

	data, err := json.MarshalIndent(form, "", "  ")
	if err != nil {
		return fmt.Errorf("recommend: save %s: marshal: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".levee-kb-*.json")
	if err != nil {
		return fmt.Errorf("recommend: save %s: create temp: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("recommend: save %s: write: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("recommend: save %s: close: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("recommend: save %s: rename: %w", path, err)
	}
	kb.log.Debug("recommend: saved", "path", path, "incidents", len(form.Incidents),
		"runbooks", len(form.Runbooks), "patterns", len(form.Patterns))
	return nil
}

// LoadFromJSON reads a JSON file produced by Save and replaces the entire
// in-memory catalogue with its contents. Existing entries are discarded.
func (kb *KnowledgeBase) LoadFromJSON(path string) error {
	if path == "" {
		return ErrEmptyPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("recommend: load %s: %w", path, err)
	}
	var form persistedForm
	if err := json.Unmarshal(data, &form); err != nil {
		return fmt.Errorf("recommend: load %s: unmarshal: %w", path, err)
	}
	kb.installForm(form)
	kb.log.Debug("recommend: loaded json", "path", path,
		"incidents", len(form.Incidents), "runbooks", len(form.Runbooks),
		"patterns", len(form.Patterns))
	return nil
}

// LoadFromYAML reads a YAML file with the same shape as persistedForm and
// replaces the in-memory catalogue with its contents.
func (kb *KnowledgeBase) LoadFromYAML(path string) error {
	if path == "" {
		return ErrEmptyPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("recommend: load yaml %s: %w", path, err)
	}
	var form persistedForm
	if err := yaml.Unmarshal(data, &form); err != nil {
		return fmt.Errorf("recommend: load yaml %s: unmarshal: %w", path, err)
	}
	kb.installForm(form)
	kb.log.Debug("recommend: loaded yaml", "path", path,
		"incidents", len(form.Incidents), "runbooks", len(form.Runbooks),
		"patterns", len(form.Patterns))
	return nil
}

// LoadFromDir scans dir for *.json and *.yaml/*.yml files and loads each
// one. The files are loaded in lexical order so the result is deterministic.
// Each file may contain either a full persistedForm or a partial one (e.g.
// only incidents); entries are appended rather than replaced. Duplicate IDs
// across files are skipped with a warning.
func (kb *KnowledgeBase) LoadFromDir(dir string) error {
	if dir == "" {
		return ErrEmptyPath
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("recommend: load dir %s: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".json" && ext != ".yaml" && ext != ".yml" {
			continue
		}
		full := filepath.Join(dir, name)
		var form persistedForm
		data, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("recommend: load dir %s: read %s: %w", dir, name, err)
		}
		if ext == ".json" {
			if err := json.Unmarshal(data, &form); err != nil {
				return fmt.Errorf("recommend: load dir %s: parse %s: %w", dir, name, err)
			}
		} else {
			if err := yaml.Unmarshal(data, &form); err != nil {
				return fmt.Errorf("recommend: load dir %s: parse %s: %w", dir, name, err)
			}
		}
		kb.appendForm(form)
		kb.log.Debug("recommend: loaded file", "path", full,
			"incidents", len(form.Incidents), "runbooks", len(form.Runbooks),
			"patterns", len(form.Patterns))
	}
	return nil
}

// installForm replaces the entire catalogue with form. Tags are normalised
// and patterns are recompiled.
func (kb *KnowledgeBase) installForm(form persistedForm) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	kb.incidents = form.Incidents
	kb.runbooks = form.Runbooks
	kb.patterns = form.Patterns
	for i := range kb.incidents {
		kb.incidents[i].Tags = normalizeTags(kb.incidents[i].Tags)
	}
	for i := range kb.runbooks {
		kb.runbooks[i].Tags = normalizeTags(kb.runbooks[i].Tags)
	}
	for i := range kb.patterns {
		kb.patterns[i].Tags = normalizeTags(kb.patterns[i].Tags)
	}
	kb.recompilePatternsLocked()
}

// appendForm appends form to the catalogue, skipping duplicate IDs. The
// caller-supplied tags are normalised.
func (kb *KnowledgeBase) appendForm(form persistedForm) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	for _, inc := range form.Incidents {
		if inc.ID == "" || kb.findIncidentLocked(inc.ID) >= 0 {
			continue
		}
		inc.Tags = normalizeTags(inc.Tags)
		kb.incidents = append(kb.incidents, inc)
	}
	for _, rb := range form.Runbooks {
		if rb.ID == "" || kb.findRunbookLocked(rb.ID) >= 0 {
			continue
		}
		rb.Tags = normalizeTags(rb.Tags)
		kb.runbooks = append(kb.runbooks, rb)
	}
	for _, p := range form.Patterns {
		if p.ID == "" || kb.findPatternLocked(p.ID) >= 0 {
			continue
		}
		p.Tags = normalizeTags(p.Tags)
		kb.patterns = append(kb.patterns, p)
	}
	kb.recompilePatternsLocked()
}

// --- Stats -------------------------------------------------------------------

// Stats returns a snapshot of the knowledge base counts. It is safe to call
// concurrently with Add / Remove / Match.
func (kb *KnowledgeBase) Stats() Stats {
	kb.mu.RLock()
	defer kb.mu.RUnlock()
	s := Stats{
		Incidents: len(kb.incidents),
		Runbooks:  len(kb.runbooks),
		Patterns:  len(kb.patterns),
	}
	for i := range kb.incidents {
		s.TotalOccurrences += kb.incidents[i].Occurrences
	}
	return s
}

// --- Scoring helpers ---------------------------------------------------------

// normalizeTags lower-cases and de-duplicates a tag slice. Empty tags are
// dropped. The result is sorted for deterministic comparison. Returns nil
// when no tags remain so callers can distinguish "no tags" from "empty
// tag set".
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// jaccard computes the Jaccard similarity |A ∩ B| / |A ∪ B| of two tag
// slices. Both inputs are assumed to be normalised (lower-cased, de-duped,
// sorted). The result is 0 when both inputs are empty.
func jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	intersection := 0
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			intersection++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// tokenSet is a small helper for accumulating unique tokens.
type tokenSet struct {
	seen   map[string]struct{}
	tokens []string
}

func newTokenSet() *tokenSet {
	return &tokenSet{seen: make(map[string]struct{})}
}

func (s *tokenSet) add(tok string) {
	tok = strings.ToLower(strings.TrimSpace(tok))
	if tok == "" {
		return
	}
	if _, ok := s.seen[tok]; ok {
		return
	}
	s.seen[tok] = struct{}{}
	s.tokens = append(s.tokens, tok)
}

func (s *tokenSet) addAll(toks []string) {
	for _, t := range toks {
		s.add(t)
	}
}

// tokenize splits a string into lowercase word tokens. Non-alphanumeric
// characters are treated as separators. Tokens shorter than 2 characters
// are dropped to avoid noise (e.g. "of", "a" — wait, those are 2; we drop
// 1-char tokens like "s" or digits-only fragments like "1").
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	set := newTokenSet()
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') //nolint:staticcheck // QF1001: single negation is more readable than De Morgan split
	})
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		set.add(f)
	}
	return set.tokens
}

// tokenizeStrings tokenises a slice of strings and returns the union of
// tokens.
func tokenizeStrings(ss []string) []string {
	set := newTokenSet()
	for _, s := range ss {
		set.addAll(tokenize(s))
	}
	return set.tokens
}

// tokenOverlap computes the fraction of tokens in needle that appear in
// haystack. The result is 0 when needle is empty. Order and duplicates in
// haystack do not matter; we build a set on the fly.
func tokenOverlap(needle, haystack []string) float64 {
	if len(needle) == 0 {
		return 0
	}
	present := make(map[string]struct{}, len(haystack))
	for _, t := range haystack {
		present[t] = struct{}{}
	}
	hit := 0
	for _, t := range needle {
		if _, ok := present[t]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(needle))
}

// occurrenceBoost returns a small boost proportional to the number of
// occurrences, capped at maxOccurrenceBoost. The boost is 0 for the first
// occurrence and grows logarithmically so that 100 occurrences do not
// dominate the textual signals.
func occurrenceBoost(occurrences int) float64 {
	if occurrences <= 1 {
		return 0
	}
	// log2(occurrences) / 100, capped at maxOccurrenceBoost.
	boost := float64(occurrences)
	// Use a simple integer log2 to avoid importing math.
	log2 := 0
	for boost >= 2 {
		boost /= 2
		log2++
	}
	b := float64(log2) / 100.0
	if b > maxOccurrenceBoost {
		b = maxOccurrenceBoost
	}
	return b
}

// sortMatches sorts a slice of matches by descending Score, breaking ties
// by ID for deterministic output.
func sortMatches(ms []*Match) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Score != ms[j].Score {
			return ms[i].Score > ms[j].Score
		}
		return ms[i].ID < ms[j].ID
	})
}
