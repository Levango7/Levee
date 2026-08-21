package recommend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- NewKnowledgeBase --------------------------------------------------------

func TestNewKnowledgeBase(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NotNil(t, kb)
	s := kb.Stats()
	assert.Equal(t, 0, s.Incidents)
	assert.Equal(t, 0, s.Runbooks)
	assert.Equal(t, 0, s.Patterns)
}

func TestNewKnowledgeBaseWithDefaults(t *testing.T) {
	kb := NewKnowledgeBaseWithDefaults()
	s := kb.Stats()
	assert.Equal(t, 5, s.Incidents, "expected 5 built-in incidents")
	assert.Equal(t, 3, s.Runbooks, "expected 3 built-in runbooks")
	assert.Equal(t, 3, s.Patterns, "expected 3 built-in patterns")
	assert.Greater(t, s.TotalOccurrences, 0)
}

func TestSetLogger(t *testing.T) {
	kb := NewKnowledgeBase()
	// Should not panic with nil; falls back to singleton.
	kb.SetLogger(nil)
	assert.NotNil(t, kb.log)
}

// --- Add ---------------------------------------------------------------------

func TestAddIncident(t *testing.T) {
	kb := NewKnowledgeBase()
	inc := HistoricalIncident{
		ID:       "inc-1",
		Title:    "test",
		Tags:     []string{"Java", "OOM", "oom"},
		Severity: "critical",
	}
	require.NoError(t, kb.AddIncident(inc))
	s := kb.Stats()
	assert.Equal(t, 1, s.Incidents)

	// Tags should be normalised (lower-cased, de-duped, sorted).
	require.Len(t, kb.incidents, 1)
	assert.Equal(t, []string{"java", "oom"}, kb.incidents[0].Tags)
	// CreatedAt should be set when zero.
	assert.False(t, kb.incidents[0].CreatedAt.IsZero())
}

func TestAddIncidentEmptyID(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.AddIncident(HistoricalIncident{Title: "no-id"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyID)
}

func TestAddIncidentDuplicate(t *testing.T) {
	kb := NewKnowledgeBase()
	inc := HistoricalIncident{ID: "dup", Title: "first"}
	require.NoError(t, kb.AddIncident(inc))
	err := kb.AddIncident(HistoricalIncident{ID: "dup", Title: "second"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDuplicateID)
}

func TestAddRunbook(t *testing.T) {
	kb := NewKnowledgeBase()
	rb := Runbook{ID: "rb-1", Name: "test", Tags: []string{"Disk", "disk"}}
	require.NoError(t, kb.AddRunbook(rb))
	assert.Equal(t, 1, kb.Stats().Runbooks)
	require.Len(t, kb.runbooks, 1)
	assert.Equal(t, []string{"disk"}, kb.runbooks[0].Tags)
}

func TestAddRunbookEmptyID(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.AddRunbook(Runbook{Name: "no-id"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyID)
}

func TestAddRunbookDuplicate(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddRunbook(Runbook{ID: "dup"}))
	err := kb.AddRunbook(Runbook{ID: "dup"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDuplicateID)
}

func TestAddPattern(t *testing.T) {
	kb := NewKnowledgeBase()
	p := FixPattern{ID: "p-1", Name: "test", Condition: "(?i)oom", Tags: []string{"Java", "java"}}
	require.NoError(t, kb.AddPattern(p))
	assert.Equal(t, 1, kb.Stats().Patterns)
	require.Len(t, kb.patterns, 1)
	assert.Equal(t, []string{"java"}, kb.patterns[0].Tags)
	require.Len(t, kb.compiledPatterns, 1)
	require.NotNil(t, kb.compiledPatterns[0])
}

func TestAddPatternEmptyID(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.AddPattern(FixPattern{Name: "no-id", Condition: "x"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyID)
}

func TestAddPatternEmptyCondition(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.AddPattern(FixPattern{ID: "p-1", Condition: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty condition")
}

func TestAddPatternBadRegex(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.AddPattern(FixPattern{ID: "p-1", Condition: "("})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compile condition")
}

func TestAddPatternDuplicate(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddPattern(FixPattern{ID: "dup", Condition: "x"}))
	err := kb.AddPattern(FixPattern{ID: "dup", Condition: "y"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDuplicateID)
}

// --- Remove ------------------------------------------------------------------

func TestRemoveIncident(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{ID: "x"}))
	require.NoError(t, kb.RemoveIncident("x"))
	assert.Equal(t, 0, kb.Stats().Incidents)
}

func TestRemoveIncidentNotFound(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.RemoveIncident("missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRemoveRunbook(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddRunbook(Runbook{ID: "x"}))
	require.NoError(t, kb.RemoveRunbook("x"))
	assert.Equal(t, 0, kb.Stats().Runbooks)
}

func TestRemoveRunbookNotFound(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.RemoveRunbook("missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRemovePattern(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddPattern(FixPattern{ID: "x", Condition: "x"}))
	require.NoError(t, kb.AddPattern(FixPattern{ID: "y", Condition: "y"}))
	require.NoError(t, kb.RemovePattern("x"))
	assert.Equal(t, 1, kb.Stats().Patterns)
	// Compiled cache should be rebuilt.
	require.Len(t, kb.compiledPatterns, 1)
	assert.NotNil(t, kb.compiledPatterns[0])
	// Remaining pattern "y" should still match.
	matches, err := kb.MatchPatterns("y", nil, nil)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "y", matches[0].ID)
}

func TestRemovePatternNotFound(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.RemovePattern("missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- Match: incidents --------------------------------------------------------

func TestMatchIncidentsEmpty(t *testing.T) {
	kb := NewKnowledgeBase()
	out, err := kb.MatchIncidents("anything", nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestMatchIncidentsTagOverlap(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "inc-1", Title: "java oom",
		Tags: []string{"java", "oom"}, Severity: "critical",
	}))
	out, err := kb.MatchIncidents("", nil, []string{"java", "oom"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "inc-1", out[0].ID)
	// All tags overlap => tag score 1 => final score 0.4.
	assert.InDelta(t, 0.4, out[0].Score, 1e-9)
}

func TestMatchIncidentsSymptomOverlap(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "inc-1", Title: "java oom",
		Symptoms: []string{"java lang outofmemory", "heap space"},
		Tags:     []string{"java"}, Severity: "critical",
	}))
	out, err := kb.MatchIncidents("", []string{"outofmemory", "heap"}, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	// Symptom overlap: needle tokens {outofmemory, heap}; haystack tokens
	// {java, lang, outofmemory, heap, space}; both hit => score 1.
	// Final = 0.3 * 1 = 0.3.
	assert.InDelta(t, 0.3, out[0].Score, 1e-9)
}

func TestMatchIncidentsRootCauseOverlap(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "inc-1", Title: "disk full",
		RootCause: "disk volume filled by logs",
		Tags:      []string{"disk"}, Severity: "critical",
	}))
	out, err := kb.MatchIncidents("disk volume filled", nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	// Root-cause overlap: needle {disk, volume, filled}; haystack {disk,
	// volume, filled, logs}; all 3 hit => score 1. Final = 0.3 * 1 = 0.3.
	assert.InDelta(t, 0.3, out[0].Score, 1e-9)
}

func TestMatchIncidentsOccurrenceBoost(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "inc-1", Title: "java oom",
		Tags: []string{"java", "oom"}, Severity: "critical",
		Occurrences: 8, // log2(8) = 3 => boost 0.03
	}))
	out, err := kb.MatchIncidents("", nil, []string{"java", "oom"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	// 0.4 (tags) + 0.03 (occurrence boost for 8 occurrences).
	assert.InDelta(t, 0.43, out[0].Score, 1e-9)
}

func TestMatchIncidentsOccurrenceBoostCapped(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "inc-1", Title: "java oom",
		Tags: []string{"java", "oom"}, Severity: "critical",
		Occurrences: 1 << 30, // huge => boost capped at 0.1
	}))
	out, err := kb.MatchIncidents("", nil, []string{"java", "oom"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	// 0.4 (tags) + 0.1 (capped boost).
	assert.InDelta(t, 0.5, out[0].Score, 1e-9)
}

func TestMatchIncidentsSortedDescending(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "low", Title: "low", Tags: []string{"a"}, Severity: "info",
	}))
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "high", Title: "high", Tags: []string{"a", "b", "c"}, Severity: "critical",
	}))
	out, err := kb.MatchIncidents("", nil, []string{"a", "b", "c"})
	require.NoError(t, err)
	require.Len(t, out, 2)
	// "high" has more tag overlap => higher score => comes first.
	assert.Equal(t, "high", out[0].ID)
	assert.Equal(t, "low", out[1].ID)
	assert.GreaterOrEqual(t, out[0].Score, out[1].Score)
}

func TestMatchIncidentsNoMatchExcluded(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "inc-1", Title: "java oom", Tags: []string{"java"}, Severity: "critical",
	}))
	out, err := kb.MatchIncidents("disk full", []string{"disk space"}, []string{"disk"})
	require.NoError(t, err)
	// No overlap on any signal => score 0 => excluded.
	assert.Empty(t, out)
}

// --- Match: runbooks ---------------------------------------------------------

func TestMatchRunbooksTagOverlap(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddRunbook(Runbook{
		ID: "rb-1", Name: "Disk Full Recovery", Trigger: "disk usage above 90",
		Tags: []string{"disk", "storage"},
	}))
	out, err := kb.MatchRunbooks("", nil, []string{"disk", "storage"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "rb-1", out[0].ID)
}

func TestMatchRunbooksTriggerOverlap(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddRunbook(Runbook{
		ID: "rb-1", Name: "Disk Full Recovery", Trigger: "disk usage above 90 percent",
		Tags: []string{"disk"},
	}))
	out, err := kb.MatchRunbooks("disk usage above 90", nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "rb-1", out[0].ID)
}

func TestMatchRunbooksNoMatch(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddRunbook(Runbook{
		ID: "rb-1", Name: "Disk Full Recovery", Trigger: "disk usage",
		Tags: []string{"disk"},
	}))
	out, err := kb.MatchRunbooks("java oom", []string{"heap space"}, []string{"java"})
	require.NoError(t, err)
	assert.Empty(t, out)
}

// --- Match: patterns ---------------------------------------------------------

func TestMatchPatternsRootCause(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddPattern(FixPattern{
		ID: "p-1", Name: "Restart on OOM", Condition: "(?i)oom", RiskLevel: RiskHigh,
	}))
	out, err := kb.MatchPatterns("java oom detected", nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "p-1", out[0].ID)
	assert.InDelta(t, 0.7, out[0].Score, 1e-9)
}

func TestMatchPatternsSymptom(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddPattern(FixPattern{
		ID: "p-1", Name: "Restart on OOM", Condition: "(?i)oom", RiskLevel: RiskHigh,
	}))
	out, err := kb.MatchPatterns("disk full", []string{"java oom error"}, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "p-1", out[0].ID)
}

func TestMatchPatternsTagBoost(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddPattern(FixPattern{
		ID: "p-1", Name: "Restart on OOM", Condition: "(?i)oom",
		Tags: []string{"java", "oom"}, RiskLevel: RiskHigh,
	}))
	out, err := kb.MatchPatterns("oom", nil, []string{"java", "oom"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	// 0.7 base + 0.3 * 1.0 tag overlap = 1.0.
	assert.InDelta(t, 1.0, out[0].Score, 1e-9)
}

func TestMatchPatternsNoMatch(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddPattern(FixPattern{
		ID: "p-1", Name: "Restart on OOM", Condition: "(?i)oom", RiskLevel: RiskHigh,
	}))
	out, err := kb.MatchPatterns("disk full", []string{"no space left"}, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// --- Match: combined ---------------------------------------------------------

func TestMatchCombined(t *testing.T) {
	kb := NewKnowledgeBaseWithDefaults()
	out, err := kb.Match(
		"java outofmemory error heap space",
		[]string{"GC overhead limit exceeded", "high RSS"},
		[]string{"java", "oom", "memory"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	// Top result should reference the Java OOM incident or pattern.
	assert.Contains(t, []string{"INC-JAVA-OOM-001", "FP-OOM-RESTART-001"}, out[0].ID)
	// Scores should be sorted descending.
	for i := 1; i < len(out); i++ {
		assert.GreaterOrEqual(t, out[i-1].Score, out[i].Score,
			"matches should be sorted by descending score")
	}
}

func TestMatchCombinedEmpty(t *testing.T) {
	kb := NewKnowledgeBase()
	out, err := kb.Match("anything", nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// --- Persistence -------------------------------------------------------------

func TestSaveAndLoadJSON(t *testing.T) {
	kb := NewKnowledgeBaseWithDefaults()
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.json")
	require.NoError(t, kb.Save(path))

	// File should exist and be valid JSON.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, data)
	var form persistedForm
	require.NoError(t, json.Unmarshal(data, &form))
	assert.Equal(t, 5, len(form.Incidents))

	// Load into a fresh KB.
	kb2 := NewKnowledgeBase()
	require.NoError(t, kb2.LoadFromJSON(path))
	s := kb2.Stats()
	assert.Equal(t, 5, s.Incidents)
	assert.Equal(t, 3, s.Runbooks)
	assert.Equal(t, 3, s.Patterns)
}

func TestSaveEmptyPath(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.Save("")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyPath)
}

func TestLoadJSONEmptyPath(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.LoadFromJSON("")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyPath)
}

func TestLoadJSONMissingFile(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.LoadFromJSON(filepath.Join(t.TempDir(), "nope.json"))
	require.Error(t, err)
}

func TestLoadJSONBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	kb := NewKnowledgeBase()
	err := kb.LoadFromJSON(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}

func TestSaveOverwrite(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{ID: "a", Title: "first"}))
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.json")
	require.NoError(t, kb.Save(path))

	kb2 := NewKnowledgeBase()
	require.NoError(t, kb2.AddIncident(HistoricalIncident{ID: "b", Title: "second"}))
	require.NoError(t, kb2.Save(path))

	kb3 := NewKnowledgeBase()
	require.NoError(t, kb3.LoadFromJSON(path))
	assert.Equal(t, 1, kb3.Stats().Incidents)
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.yaml")
	form := persistedForm{
		Incidents: []HistoricalIncident{{ID: "y1", Title: "yaml incident", Tags: []string{"a"}}},
		Runbooks:  []Runbook{{ID: "yr1", Name: "yaml runbook"}},
		Patterns:  []FixPattern{{ID: "yp1", Name: "yaml pattern", Condition: "x"}},
	}
	data, err := yaml.Marshal(form)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	kb := NewKnowledgeBase()
	require.NoError(t, kb.LoadFromYAML(path))
	s := kb.Stats()
	assert.Equal(t, 1, s.Incidents)
	assert.Equal(t, 1, s.Runbooks)
	assert.Equal(t, 1, s.Patterns)
}

func TestLoadFromYAMLEmptyPath(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.LoadFromYAML("")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyPath)
}

func TestLoadFromYAMLMissingFile(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.LoadFromYAML(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
}

func TestLoadFromYAMLBadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte(": not: valid: yaml: ["), 0o644))
	kb := NewKnowledgeBase()
	err := kb.LoadFromYAML(path)
	require.Error(t, err)
}

func TestLoadFromDir(t *testing.T) {
	dir := t.TempDir()

	// Write two JSON files and one YAML file.
	form1 := persistedForm{
		Incidents: []HistoricalIncident{{ID: "a", Title: "first", Tags: []string{"x"}}},
	}
	form2 := persistedForm{
		Incidents: []HistoricalIncident{{ID: "b", Title: "second"}},
		Runbooks:  []Runbook{{ID: "r1", Name: "rb"}},
	}
	form3 := persistedForm{
		Patterns: []FixPattern{{ID: "p1", Name: "pat", Condition: "x"}},
	}
	j1, _ := json.MarshalIndent(form1, "", "  ")
	j2, _ := json.MarshalIndent(form2, "", "  ")
	y3, _ := yaml.Marshal(form3)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1.json"), j1, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2.json"), j2, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "3.yaml"), y3, 0o644))
	// Non-catalogue file should be ignored.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644))

	kb := NewKnowledgeBase()
	require.NoError(t, kb.LoadFromDir(dir))
	s := kb.Stats()
	assert.Equal(t, 2, s.Incidents)
	assert.Equal(t, 1, s.Runbooks)
	assert.Equal(t, 1, s.Patterns)
}

func TestLoadFromDirEmptyPath(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.LoadFromDir("")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyPath)
}

func TestLoadFromDirMissing(t *testing.T) {
	kb := NewKnowledgeBase()
	err := kb.LoadFromDir(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err)
}

func TestLoadFromDirBadFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{nope"), 0o644))
	kb := NewKnowledgeBase()
	err := kb.LoadFromDir(dir)
	require.Error(t, err)
}

func TestLoadFromDirDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	form1 := persistedForm{Incidents: []HistoricalIncident{{ID: "dup", Title: "first"}}}
	form2 := persistedForm{Incidents: []HistoricalIncident{{ID: "dup", Title: "second"}}}
	j1, _ := json.MarshalIndent(form1, "", "  ")
	j2, _ := json.MarshalIndent(form2, "", "  ")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1.json"), j1, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2.json"), j2, 0o644))

	kb := NewKnowledgeBase()
	require.NoError(t, kb.LoadFromDir(dir))
	// Second occurrence of "dup" should be skipped.
	assert.Equal(t, 1, kb.Stats().Incidents)
	require.Len(t, kb.incidents, 1)
	assert.Equal(t, "first", kb.incidents[0].Title)
}

// --- Stats -------------------------------------------------------------------

func TestStats(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{ID: "a", Occurrences: 3}))
	require.NoError(t, kb.AddIncident(HistoricalIncident{ID: "b", Occurrences: 5}))
	require.NoError(t, kb.AddRunbook(Runbook{ID: "r"}))
	require.NoError(t, kb.AddPattern(FixPattern{ID: "p", Condition: "x"}))
	s := kb.Stats()
	assert.Equal(t, 2, s.Incidents)
	assert.Equal(t, 1, s.Runbooks)
	assert.Equal(t, 1, s.Patterns)
	assert.Equal(t, 8, s.TotalOccurrences)
}

// --- Scoring helpers ---------------------------------------------------------

func TestNormalizeTags(t *testing.T) {
	out := normalizeTags([]string{"Java", "java", "OOM", "", "  oom  "})
	assert.Equal(t, []string{"java", "oom"}, out)
}

func TestNormalizeTagsEmpty(t *testing.T) {
	assert.Nil(t, normalizeTags(nil))
	assert.Nil(t, normalizeTags([]string{}))
	assert.Nil(t, normalizeTags([]string{"", "  "}))
}

func TestJaccard(t *testing.T) {
	// Both empty => 0.
	assert.InDelta(t, 0, jaccard(nil, nil), 1e-9)
	// Disjoint => 0.
	assert.InDelta(t, 0, jaccard([]string{"a"}, []string{"b"}), 1e-9)
	// Identical => 1.
	assert.InDelta(t, 1, jaccard([]string{"a", "b"}, []string{"a", "b"}), 1e-9)
	// Half overlap => 1/3.
	assert.InDelta(t, 1.0/3.0, jaccard([]string{"a", "b"}, []string{"a", "c"}), 1e-9)
}

func TestTokenize(t *testing.T) {
	out := tokenize("Java OOM: heap space!")
	// Should produce lowercase tokens, dropping 1-char fragments and
	// punctuation. Order is preserved (insertion order).
	assert.Contains(t, out, "java")
	assert.Contains(t, out, "oom")
	assert.Contains(t, out, "heap")
	assert.Contains(t, out, "space")
}

func TestTokenizeEmpty(t *testing.T) {
	assert.Nil(t, tokenize(""))
	assert.Nil(t, tokenize("   ! ? ."))
}

func TestTokenizeStrings(t *testing.T) {
	out := tokenizeStrings([]string{"java oom", "heap space"})
	assert.Contains(t, out, "java")
	assert.Contains(t, out, "oom")
	assert.Contains(t, out, "heap")
	assert.Contains(t, out, "space")
}

func TestTokenOverlap(t *testing.T) {
	// Empty needle => 0.
	assert.InDelta(t, 0, tokenOverlap(nil, []string{"a"}), 1e-9)
	// All hit => 1.
	assert.InDelta(t, 1, tokenOverlap([]string{"a", "b"}, []string{"a", "b", "c"}), 1e-9)
	// Half hit => 0.5.
	assert.InDelta(t, 0.5, tokenOverlap([]string{"a", "b"}, []string{"a"}), 1e-9)
	// None hit => 0.
	assert.InDelta(t, 0, tokenOverlap([]string{"a"}, []string{"b"}), 1e-9)
}

func TestOccurrenceBoost(t *testing.T) {
	assert.InDelta(t, 0, occurrenceBoost(0), 1e-9)
	assert.InDelta(t, 0, occurrenceBoost(1), 1e-9)
	// 2 => log2=1 => 0.01.
	assert.InDelta(t, 0.01, occurrenceBoost(2), 1e-9)
	// 8 => log2=3 => 0.03.
	assert.InDelta(t, 0.03, occurrenceBoost(8), 1e-9)
	// Huge => capped at 0.1.
	assert.InDelta(t, 0.1, occurrenceBoost(1<<30), 1e-9)
}

func TestSortMatches(t *testing.T) {
	ms := []*Match{
		{ID: "a", Score: 0.3},
		{ID: "b", Score: 0.5},
		{ID: "c", Score: 0.5},
	}
	sortMatches(ms)
	assert.Equal(t, "b", ms[0].ID) // tie broken by ID
	assert.Equal(t, "c", ms[1].ID)
	assert.Equal(t, "a", ms[2].ID)
}

// --- Concurrency -------------------------------------------------------------

func TestConcurrentAddAndMatch(t *testing.T) {
	kb := NewKnowledgeBase()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = kb.AddIncident(HistoricalIncident{
				ID:       "inc-" + string(rune('a'+n)),
				Title:    "concurrent",
				Tags:     []string{"java"},
				Severity: "warning",
			})
			_, _ = kb.Match("java", []string{"oom"}, []string{"java"})
			_ = kb.Stats()
		}(i)
	}
	wg.Wait()
	// Should not panic; should have 20 incidents.
	assert.Equal(t, 20, kb.Stats().Incidents)
}

// --- Defaults ----------------------------------------------------------------

func TestDefaultIncidentsWellFormed(t *testing.T) {
	for _, inc := range defaultIncidents {
		assert.NotEmpty(t, inc.ID)
		assert.NotEmpty(t, inc.Title)
		assert.NotEmpty(t, inc.RootCause)
		assert.NotEmpty(t, inc.Resolution)
		assert.NotEmpty(t, inc.Severity)
		assert.NotEmpty(t, inc.Tags)
		assert.False(t, inc.CreatedAt.IsZero())
	}
}

func TestDefaultRunbooksWellFormed(t *testing.T) {
	for _, rb := range defaultRunbooks {
		assert.NotEmpty(t, rb.ID)
		assert.NotEmpty(t, rb.Name)
		assert.NotEmpty(t, rb.Trigger)
		assert.NotEmpty(t, rb.Steps)
		for _, s := range rb.Steps {
			assert.Greater(t, s.Order, 0)
			assert.NotEmpty(t, s.Action)
			assert.NotEmpty(t, s.Description)
		}
	}
}

func TestDefaultPatternsWellFormed(t *testing.T) {
	for _, p := range defaultPatterns {
		assert.NotEmpty(t, p.ID)
		assert.NotEmpty(t, p.Name)
		assert.NotEmpty(t, p.Condition)
		assert.NotEmpty(t, p.RiskLevel)
	}
}

func TestDefaultsMatchRealisticDiagnosis(t *testing.T) {
	kb := NewKnowledgeBaseWithDefaults()

	// Scenario 1: Java OOM.
	out, err := kb.Match(
		"java outofmemory error heap space exhausted",
		[]string{"GC overhead limit exceeded", "high RSS", "java.lang.OutOfMemoryError"},
		[]string{"java", "oom", "memory", "jvm"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	top := out[0]
	assert.Contains(t, []string{"INC-JAVA-OOM-001", "FP-OOM-RESTART-001"}, top.ID)

	// Scenario 2: Disk full.
	out, err = kb.Match(
		"disk volume filled by unbounded logs",
		[]string{"no space left on device", "disk usage above 90"},
		[]string{"disk", "storage", "logs"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	top = out[0]
	assert.Contains(t, []string{"INC-DISK-FULL-001", "FP-DISK-CLEAN-001", "RB-DISK-FULL-001"}, top.ID)

	// Scenario 3: DB pool.
	out, err = kb.Match(
		"connection pool exhausted under peak load",
		[]string{"unable to acquire connection", "request timeout"},
		[]string{"database", "pool", "connection"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	top = out[0]
	assert.Contains(t, []string{"INC-DB-POOL-001", "FP-DB-POOL-001"}, top.ID)
}

// --- Match.Source round-trip -------------------------------------------------

func TestMatchSourceRoundTrip(t *testing.T) {
	kb := NewKnowledgeBase()
	inc := HistoricalIncident{
		ID: "inc-1", Title: "java oom", Tags: []string{"java", "oom"},
		Severity: "critical", Occurrences: 2,
	}
	require.NoError(t, kb.AddIncident(inc))
	require.NoError(t, kb.AddRunbook(Runbook{ID: "rb-1", Name: "Restart", Trigger: "oom", Tags: []string{"oom"}}))
	require.NoError(t, kb.AddPattern(FixPattern{ID: "p-1", Name: "Restart on OOM", Condition: "(?i)oom", Tags: []string{"oom"}}))

	out, err := kb.Match("oom", []string{"oom"}, []string{"oom"})
	require.NoError(t, err)
	require.Len(t, out, 3)

	// Each Source should be the concrete value type.
	for _, m := range out {
		switch m.Type {
		case MatchTypeIncident:
			_, ok := m.Source.(HistoricalIncident)
			assert.True(t, ok, "incident source should be HistoricalIncident")
		case MatchTypeRunbook:
			_, ok := m.Source.(Runbook)
			assert.True(t, ok, "runbook source should be Runbook")
		case MatchTypePattern:
			_, ok := m.Source.(FixPattern)
			assert.True(t, ok, "pattern source should be FixPattern")
		default:
			t.Fatalf("unexpected match type %q", m.Type)
		}
		assert.NotEmpty(t, m.Reason)
	}
}

// --- Save / Load idempotence -------------------------------------------------

func TestSaveLoadIdempotent(t *testing.T) {
	kb := NewKnowledgeBaseWithDefaults()
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.json")
	require.NoError(t, kb.Save(path))

	kb2 := NewKnowledgeBase()
	require.NoError(t, kb2.LoadFromJSON(path))
	require.NoError(t, kb2.Save(path))

	kb3 := NewKnowledgeBase()
	require.NoError(t, kb3.LoadFromJSON(path))

	assert.Equal(t, kb.Stats(), kb3.Stats())
}

// --- Match with empty inputs -------------------------------------------------

func TestMatchAllEmpty(t *testing.T) {
	kb := NewKnowledgeBaseWithDefaults()
	out, err := kb.Match("", nil, nil)
	require.NoError(t, err)
	// With no input signals, only pattern regexes that match the empty
	// string would score; the built-in patterns all require non-empty
	// matches, so the result should be empty.
	for _, m := range out {
		assert.Greater(t, m.Score, 0.0)
	}
}

func TestMatchIncidentsReasonFormatted(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddIncident(HistoricalIncident{
		ID: "inc-1", Title: "java oom", Tags: []string{"java", "oom"},
		Severity: "critical", Occurrences: 4,
	}))
	out, err := kb.MatchIncidents("", nil, []string{"java", "oom"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Contains(t, out[0].Reason, "tags=")
	assert.Contains(t, out[0].Reason, "symptoms=")
	assert.Contains(t, out[0].Reason, "root=")
	assert.Contains(t, out[0].Reason, "occ=")
}

func TestMatchRunbooksReasonFormatted(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddRunbook(Runbook{
		ID: "rb-1", Name: "Restart", Trigger: "oom", Tags: []string{"oom"},
	}))
	out, err := kb.MatchRunbooks("oom", nil, []string{"oom"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Contains(t, out[0].Reason, "tags=")
	assert.Contains(t, out[0].Reason, "trigger=")
}

func TestMatchPatternsReasonFormatted(t *testing.T) {
	kb := NewKnowledgeBase()
	require.NoError(t, kb.AddPattern(FixPattern{
		ID: "p-1", Name: "Restart on OOM", Condition: "(?i)oom", Tags: []string{"oom"},
	}))
	out, err := kb.MatchPatterns("oom", nil, []string{"oom"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Contains(t, out[0].Reason, "regex=")
	assert.Contains(t, out[0].Reason, "tags=")
}

// --- Time-related ------------------------------------------------------------

func TestAddIncidentPreservesCreatedAt(t *testing.T) {
	kb := NewKnowledgeBase()
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, kb.AddIncident(HistoricalIncident{ID: "x", CreatedAt: ts}))
	require.Len(t, kb.incidents, 1)
	assert.Equal(t, ts, kb.incidents[0].CreatedAt)
}
