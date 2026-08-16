// log_analyzer_test.go exercises LogAnalyzer with constructed LogBatch
// inputs. It covers the built-in pattern library, error clustering, root-
// cause determination and the various edge cases (empty batch, no matches,
// invalid regex, multiple patterns on one line, ...).

package diagnosis

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ----------------------------------------------------------------

// makeLine builds a LogLine with the given message and source.
func makeLine(source, msg string) LogLine {
	return LogLine{Source: source, Message: msg, Raw: msg}
}

// makeTimedLine builds a LogLine with a timestamp.
func makeTimedLine(ts time.Time, source, msg string) LogLine {
	return LogLine{Timestamp: ts, Source: source, Message: msg, Raw: msg}
}

// defaultAnalyzer builds a LogAnalyzer with the built-in pattern library.
func defaultAnalyzer(t *testing.T) *LogAnalyzer {
	t.Helper()
	a := NewDefaultLogAnalyzer()
	require.NotNil(t, a)
	return a
}

// fixedTime returns a deterministic time for tests.
func fixedTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("fixedTime: bad input: " + s)
	}
	return t
}

// --- NewLogAnalyzer --------------------------------------------------------

func TestNewLogAnalyzer_InvalidRegex(t *testing.T) {
	patterns := []ErrorPattern{
		{ID: "good", Name: "g", Regex: "ok"},
		{ID: "bad", Name: "b", Regex: "(unclosed"},
	}
	a, invalid := NewLogAnalyzer(patterns)
	require.NotNil(t, a)
	require.Len(t, invalid, 1)
	assert.Equal(t, "bad", invalid[0])
}

func TestNewLogAnalyzer_AllValid(t *testing.T) {
	a, invalid := NewLogAnalyzer(DefaultPatterns())
	require.NotNil(t, a)
	assert.Empty(t, invalid)
}

func TestNewDefaultLogAnalyzer(t *testing.T) {
	a := defaultAnalyzer(t)
	// The default analyzer must carry the eight built-in patterns.
	assert.NotEmpty(t, a.patterns)
}

// --- DefaultPatterns -------------------------------------------------------

func TestDefaultPatterns_Count(t *testing.T) {
	srcs := DefaultPatterns()
	require.Len(t, srcs, 8, "DefaultPatterns must return exactly 8 built-in patterns")
}

func TestDefaultPatterns_FreshCopy(t *testing.T) {
	a := DefaultPatterns()
	a[0].ID = "mutated"
	b := DefaultPatterns()
	assert.NotEqual(t, "mutated", b[0].ID, "DefaultPatterns must return a fresh slice")
}

func TestDefaultPatterns_ExpectedIDs(t *testing.T) {
	want := map[string]bool{
		"OOM": true, "GC_PAUSE": true, "CONN_TIMEOUT": true, "DISK_FULL": true,
		"PERMISSION_DENIED": true, "SEGFAULT": true, "DB_CONN_FAILED": true,
		"NETWORK_DOWN": true,
	}
	for _, p := range DefaultPatterns() {
		assert.True(t, want[p.ID], "unexpected pattern ID %q", p.ID)
		delete(want, p.ID)
	}
	assert.Empty(t, want, "missing patterns: %v", want)
}

// --- Analyze: empty / nil inputs -------------------------------------------

func TestAnalyze_NilBatch(t *testing.T) {
	a := defaultAnalyzer(t)
	r := a.Analyze(nil)
	require.NotNil(t, r)
	assert.Equal(t, 0, r.TotalLines)
	assert.Equal(t, 0, r.ErrorLines)
	assert.Empty(t, r.ErrorPatterns)
	assert.Empty(t, r.ErrorClusters)
	assert.Empty(t, r.Timeline)
	assert.Equal(t, 0.0, r.Confidence)
	assert.Contains(t, r.Summary, "no log batch")
}

func TestAnalyze_EmptyBatch(t *testing.T) {
	a := defaultAnalyzer(t)
	r := a.Analyze(&LogBatch{Target: "h"})
	require.NotNil(t, r)
	assert.Equal(t, 0, r.TotalLines)
	assert.Equal(t, 0, r.ErrorLines)
	assert.Contains(t, r.Summary, "no errors matched")
}

func TestAnalyze_NoMatches(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("app", "all good here"),
			makeLine("app", "nothing to see"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, 2, r.TotalLines)
	assert.Equal(t, 0, r.ErrorLines)
	assert.Empty(t, r.ErrorPatterns)
	assert.Equal(t, 0.0, r.Confidence)
}

// --- Analyze: single pattern matches --------------------------------------

func TestAnalyze_OOMMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("kernel", "Out of memory: killed process 1234 (java)"),
		},
	}
	r := a.Analyze(batch)
	require.Len(t, r.ErrorPatterns, 1)
	assert.Equal(t, "OOM", r.ErrorPatterns[0].Pattern.ID)
	assert.Equal(t, 1, r.ErrorPatterns[0].Count)
	assert.Equal(t, "OOM", r.RootCause.ID)
	assert.Greater(t, r.Confidence, 0.0)
}

func TestAnalyze_DiskFullMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("app", "write failed: No space left on device"),
		},
	}
	r := a.Analyze(batch)
	require.Len(t, r.ErrorPatterns, 1)
	assert.Equal(t, "DISK_FULL", r.RootCause.ID)
}

func TestAnalyze_SegfaultMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("app", "Segmentation fault (core dumped)"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, "SEGFAULT", r.RootCause.ID)
}

func TestAnalyze_PermissionDeniedMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("app", "open /etc/shadow: permission denied"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, "PERMISSION_DENIED", r.RootCause.ID)
}

func TestAnalyze_DBConnFailedMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("app", "FATAL Can't connect to MySQL server on 'db:3306'"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, "DB_CONN_FAILED", r.RootCause.ID)
}

func TestAnalyze_NetworkDownMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("app", "read: connection reset by peer"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, "NETWORK_DOWN", r.RootCause.ID)
}

func TestAnalyze_ConnTimeoutMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("app", "dial tcp: i/o timeout"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, "CONN_TIMEOUT", r.RootCause.ID)
}

func TestAnalyze_GCPauseMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("app", "GC pause 1.2s"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, "GC_PAUSE", r.RootCause.ID)
}

// --- Analyze: all 8 built-in patterns -------------------------------------

func TestAnalyze_AllBuiltInPatternsMatch(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("k", "out of memory"),
			makeLine("a", "gc pause 5s"),
			makeLine("a", "connection timeout"),
			makeLine("a", "no space left on device"),
			makeLine("a", "permission denied"),
			makeLine("a", "segmentation fault"),
			makeLine("a", "too many connections"),
			makeLine("a", "connection reset by peer"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, 8, r.ErrorLines)
	require.Len(t, r.ErrorPatterns, 8)

	// Root cause must be one of the fatal-severity patterns (OOM or SEGFAULT).
	assert.Contains(t, []string{"OOM", "SEGFAULT"}, r.RootCause.ID)
	// severityWeight = 4/4 = 1.0, countWeight = min(1,10)/10 = 0.1,
	// product = 0.1 (no cluster boost because each line is its own cluster).
	assert.GreaterOrEqual(t, r.Confidence, 0.1)
}

// --- Analyze: root-cause selection ----------------------------------------

func TestAnalyze_RootCausePrefersHigherSeverity(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			// Many warn-severity GC pauses.
			makeLine("a", "gc pause 1s"),
			makeLine("a", "gc pause 2s"),
			makeLine("a", "gc pause 3s"),
			// A single fatal OOM.
			makeLine("k", "out of memory"),
		},
	}
	r := a.Analyze(batch)
	// OOM (fatal) must win over GC_PAUSE (warn) despite lower count.
	assert.Equal(t, "OOM", r.RootCause.ID)
}

func TestAnalyze_RootCausePrefersHigherCountOnTie(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			// Two fatal patterns with different counts.
			makeLine("k", "out of memory"),
			makeLine("a", "segmentation fault"),
			makeLine("a", "segmentation fault"),
		},
	}
	r := a.Analyze(batch)
	// Both are fatal; SEGFAULT has count 2 > OOM count 1.
	assert.Equal(t, "SEGFAULT", r.RootCause.ID)
}

func TestAnalyze_ConfidenceBounds(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("k", "out of memory"),
		},
	}
	r := a.Analyze(batch)
	assert.GreaterOrEqual(t, r.Confidence, 0.0)
	assert.LessOrEqual(t, r.Confidence, 1.0)
}

func TestAnalyze_ConfidenceSaturatesAtOne(t *testing.T) {
	a := defaultAnalyzer(t)
	// 20 OOM lines: count weight = min(20,10)/10 = 1.0,
	// severity weight = 4/4 = 1.0, product = 1.0.
	lines := make([]LogLine, 20)
	for i := range lines {
		lines[i] = makeLine("k", "out of memory")
	}
	r := a.Analyze(&LogBatch{Target: "h", Lines: lines})
	assert.Equal(t, "OOM", r.RootCause.ID)
	assert.GreaterOrEqual(t, r.Confidence, 0.99)
}

// --- Analyze: clustering ---------------------------------------------------

func TestAnalyze_ClustersByPrefix(t *testing.T) {
	a := defaultAnalyzer(t)
	// Use a long shared prefix so the first ClusterPrefixLen characters
	// are identical across the three lines; the varying tail (instance-N)
	// falls after the signature boundary.
	shared := "connection timeout to database server with id "
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("a", shared+"0001 in region us-east-1"),
			makeLine("a", shared+"0002 in region us-east-1"),
			makeLine("a", shared+"0003 in region us-east-1"),
		},
	}
	r := a.Analyze(batch)
	require.NotEmpty(t, r.ErrorClusters)
	// All three lines share the leading ClusterPrefixLen characters and
	// must land in the same cluster.
	top := r.ErrorClusters[0]
	assert.Equal(t, 3, top.Count)
}

func TestAnalyze_ClustersSortedByCountDesc(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			// Cluster A: 1 line.
			makeLine("a", "out of memory"),
			// Cluster B: 3 lines (same prefix).
			makeLine("a", "connection timeout to db1"),
			makeLine("a", "connection timeout to db2"),
			makeLine("a", "connection timeout to db3"),
		},
	}
	r := a.Analyze(batch)
	require.GreaterOrEqual(t, len(r.ErrorClusters), 2)
	// The largest cluster must come first.
	assert.GreaterOrEqual(t, r.ErrorClusters[0].Count, r.ErrorClusters[1].Count)
}

func TestAnalyze_ClusterCarriesPatternIDs(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("a", "connection timeout to db1"),
			makeLine("a", "connection timeout to db2"),
		},
	}
	r := a.Analyze(batch)
	require.NotEmpty(t, r.ErrorClusters)
	top := r.ErrorClusters[0]
	assert.Contains(t, top.PatternIDs, "CONN_TIMEOUT")
}

func TestAnalyze_ClusterSeverity(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("a", "out of memory"), // fatal
		},
	}
	r := a.Analyze(batch)
	require.NotEmpty(t, r.ErrorClusters)
	assert.Equal(t, SeverityFatal, r.ErrorClusters[0].Severity)
}

// --- Analyze: timeline -----------------------------------------------------

func TestAnalyze_TimelineSortedByTimestamp(t *testing.T) {
	a := defaultAnalyzer(t)
	t1 := fixedTime("2024-06-01T12:01:00Z")
	t2 := fixedTime("2024-06-01T12:00:00Z") // earlier
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeTimedLine(t1, "a", "out of memory"),
			makeTimedLine(t2, "a", "segmentation fault"),
		},
	}
	r := a.Analyze(batch)
	require.Len(t, r.Timeline, 2)
	// The earlier event must come first.
	assert.True(t, r.Timeline[0].Timestamp.Before(r.Timeline[1].Timestamp))
}

func TestAnalyze_TimelineCarriesPatternID(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("a", "out of memory"),
		},
	}
	r := a.Analyze(batch)
	require.Len(t, r.Timeline, 1)
	assert.Equal(t, "OOM", r.Timeline[0].PatternID)
}

func TestAnalyze_TimelineOnlyMatchedLines(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("a", "all good"),
			makeLine("a", "out of memory"),
		},
	}
	r := a.Analyze(batch)
	require.Len(t, r.Timeline, 1)
}

// --- Analyze: matched pattern stats ---------------------------------------

func TestAnalyze_MatchedPatternFirstLastSeen(t *testing.T) {
	a := defaultAnalyzer(t)
	t1 := fixedTime("2024-06-01T12:00:00Z")
	t2 := fixedTime("2024-06-01T12:05:00Z")
	t3 := fixedTime("2024-06-01T12:10:00Z")
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeTimedLine(t2, "a", "out of memory"),
			makeTimedLine(t1, "a", "out of memory"),
			makeTimedLine(t3, "a", "out of memory"),
		},
	}
	r := a.Analyze(batch)
	require.Len(t, r.ErrorPatterns, 1)
	mp := r.ErrorPatterns[0]
	assert.Equal(t, t1, mp.FirstSeen)
	assert.Equal(t, t3, mp.LastSeen)
}

func TestAnalyze_MatchedPatternSampleLinesBounded(t *testing.T) {
	a := defaultAnalyzer(t)
	lines := make([]LogLine, 50)
	for i := range lines {
		lines[i] = makeLine("a", "out of memory")
	}
	r := a.Analyze(&LogBatch{Target: "h", Lines: lines})
	require.Len(t, r.ErrorPatterns, 1)
	assert.LessOrEqual(t, len(r.ErrorPatterns[0].SampleLines), SampleLinesMax)
}

func TestAnalyze_MatchedPatternsSortedByCountDesc(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			// 3 GC pauses.
			makeLine("a", "gc pause 1s"),
			makeLine("a", "gc pause 2s"),
			makeLine("a", "gc pause 3s"),
			// 1 OOM.
			makeLine("k", "out of memory"),
		},
	}
	r := a.Analyze(batch)
	require.Len(t, r.ErrorPatterns, 2)
	// GC_PAUSE has count 3, OOM has count 1; GC_PAUSE must come first.
	assert.Equal(t, "GC_PAUSE", r.ErrorPatterns[0].Pattern.ID)
	assert.Equal(t, "OOM", r.ErrorPatterns[1].Pattern.ID)
}

// --- Analyze: multiple patterns on one line -------------------------------

func TestAnalyze_MultiplePatternsOnOneLine(t *testing.T) {
	a := defaultAnalyzer(t)
	// "out of memory" + "connection reset by peer" in one line.
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("a", "out of memory then connection reset by peer"),
		},
	}
	r := a.Analyze(batch)
	// Both patterns must match.
	ids := make(map[string]bool, len(r.ErrorPatterns))
	for _, mp := range r.ErrorPatterns {
		ids[mp.Pattern.ID] = true
	}
	assert.True(t, ids["OOM"])
	assert.True(t, ids["NETWORK_DOWN"])
	// Two timeline entries (one per match).
	assert.Len(t, r.Timeline, 2)
}

// --- Analyze: case-insensitivity ------------------------------------------

func TestAnalyze_MatchIsCaseInsensitive(t *testing.T) {
	a := defaultAnalyzer(t)
	batch := &LogBatch{
		Target: "h",
		Lines: []LogLine{
			makeLine("a", "OUT OF MEMORY"),
		},
	}
	r := a.Analyze(batch)
	assert.Equal(t, "OOM", r.RootCause.ID)
}

// --- Analyze: custom patterns ---------------------------------------------

func TestAnalyze_CustomPattern(t *testing.T) {
	custom := []ErrorPattern{
		{ID: "CUSTOM", Name: "custom", Severity: SeverityError, Regex: `widget.*broken`},
	}
	a, _ := NewLogAnalyzer(custom)
	r := a.Analyze(&LogBatch{
		Target: "h",
		Lines:  []LogLine{makeLine("a", "widget 42 is broken")},
	})
	assert.Equal(t, "CUSTOM", r.RootCause.ID)
}

// --- Severity.String ------------------------------------------------------

func TestSeverityString(t *testing.T) {
	assert.Equal(t, "info", SeverityInfo.String())
	assert.Equal(t, "warn", SeverityWarn.String())
	assert.Equal(t, "error", SeverityError.String())
	assert.Equal(t, "fatal", SeverityFatal.String())
	assert.Contains(t, Severity(99).String(), "severity(99)")
}

// --- clusterSignature / clusterID -----------------------------------------

func TestClusterSignature_TrimsAndLowercases(t *testing.T) {
	sig := clusterSignature("  ERROR: Foo Bar  ")
	assert.Equal(t, "error: foo bar", sig)
}

func TestClusterSignature_Truncates(t *testing.T) {
	long := strings.Repeat("x", ClusterPrefixLen+10)
	sig := clusterSignature(long)
	assert.Len(t, sig, ClusterPrefixLen)
}

func TestClusterSignature_Empty(t *testing.T) {
	assert.Empty(t, clusterSignature(""))
	assert.Empty(t, clusterSignature("   "))
}

func TestClusterID_Stable(t *testing.T) {
	id1 := clusterID("error: foo bar baz")
	id2 := clusterID("error: foo bar baz")
	assert.Equal(t, id1, id2)
}

func TestClusterID_NoSpacesOrColons(t *testing.T) {
	id := clusterID("error: foo: bar baz")
	assert.NotContains(t, id, " ")
	assert.NotContains(t, id, ":")
}

// --- collapseWhitespace ---------------------------------------------------

func TestCollapseWhitespace(t *testing.T) {
	assert.Equal(t, "a b c", collapseWhitespace("a   b\t\nc"))
	assert.Equal(t, "abc", collapseWhitespace("abc"))
}
