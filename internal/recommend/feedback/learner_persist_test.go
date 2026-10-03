// learner_persist_test.go — the two closes of the roadmap's feedback
// residual: the PatternID stamp used to land on a copy nobody could read
// back, and the learner forgot everything on restart.
package feedback

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/recommend"
)

// TestRecordAndLearn_StampedPatternIDIsReadable is the regression net for
// the copy-vs-pointer defect: Record used to store a value copy and return
// a pointer to the LOCAL copy, so Learn wrote the PatternID into a record
// that GetRecord/ListRecords could never show. The stored record must carry
// the stamp.
func TestRecordAndLearn_StampedPatternIDIsReadable(t *testing.T) {
	l := newTestLearner(t)

	rec, err := l.RecordAndLearn(sampleOutcome(true))
	require.NoError(t, err)
	require.NotEmpty(t, rec.PatternID, "Learn must stamp the record it returns")

	got, err := l.GetRecord(rec.ID)
	require.NoError(t, err)
	assert.Equal(t, rec.PatternID, got.PatternID,
		"the STORED record must carry the PatternID — Learn's stamp used to land on a copy")

	listed := l.ListRecords(10)
	require.Len(t, listed, 1)
	assert.Equal(t, rec.PatternID, listed[0].PatternID)
}

// TestFeedbackLearner_PersistsAcrossRestart closes "进程重启即失忆": the
// snapshot must restore the records, the stats, the synthesised pattern,
// AND refill the shared knowledge base so learned matches survive.
func TestFeedbackLearner_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.json")

	kb1 := recommend.NewKnowledgeBase()
	l1 := NewFeedbackLearner(FeedbackLearnerConfig{KnowledgeBase: kb1, PersistPath: path})
	rec, err := l1.RecordAndLearn(FixOutcome{
		Target: "checkout-api", Symptoms: "connection refused", RootCause: "runaway cron",
		FixAction: "kill stray cron job", Success: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, rec.PatternID)
	require.FileExists(t, path, "a successful RecordAndLearn must write the snapshot")

	// Restart: a fresh learner (and a fresh KB, as after a process boot)
	// over the same file must come back with everything.
	kb2 := recommend.NewKnowledgeBase()
	l2 := NewFeedbackLearner(FeedbackLearnerConfig{KnowledgeBase: kb2, PersistPath: path})

	stats := l2.GetStats()
	assert.Equal(t, 1, stats.TotalRecords, "records must survive the restart")

	patterns := l2.ExportPatterns()
	require.Len(t, patterns, 1, "the synthesised pattern must survive the restart")

	// And it participates in matching again — the point of persisting at all.
	matches, err := kb2.MatchPatterns("runaway cron", nil, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, matches, "the shared knowledge base must be refilled after the restart")

	// The record's stamped PatternID round-trips too.
	got, err := l2.GetRecord(rec.ID)
	require.NoError(t, err)
	assert.Equal(t, rec.PatternID, got.PatternID)
}

// TestFeedbackLearner_CorruptSnapshotStartsFresh pins the best-effort load
// contract: a corrupt snapshot must not fail construction (the learner is
// auxiliary learning state, never the system of record).
func TestFeedbackLearner_CorruptSnapshotStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	l := NewFeedbackLearner(FeedbackLearnerConfig{
		KnowledgeBase: recommend.NewKnowledgeBase(),
		PersistPath:   path,
	})
	assert.Equal(t, 0, l.GetStats().TotalRecords, "a corrupt snapshot must start fresh, not crash")
}

// TestFeedbackLearner_NoPersistPathStaysInMemory pins the previous
// behaviour for callers that never opt in.
func TestFeedbackLearner_NoPersistPathStaysInMemory(t *testing.T) {
	l := newTestLearner(t)
	_, err := l.RecordAndLearn(sampleOutcome(true))
	require.NoError(t, err)
	assert.Equal(t, 1, l.GetStats().TotalRecords)
}
