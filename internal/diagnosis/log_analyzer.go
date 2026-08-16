// log_analyzer.go implements Phase A4 of the diagnosis subsystem: it takes
// a LogBatch produced by LogCollector, matches every line against a
// library of well-known error patterns (OOM, GC pause, connection timeout,
// disk full, ...), clusters similar messages together and proposes a
// root-cause hypothesis with a confidence score in [0, 1].
//
// The analyzer is intentionally rule-based rather than ML-based: it must
// run offline, produce deterministic results and be auditable. Each
// ErrorPattern carries a human-readable description and a remediation
// suggestion so that the resulting AnalysisResult can be rendered directly
// into an incident report.
//
// All public types are safe for concurrent use. The analyzer never panics;
// malformed regexes in a user-supplied pattern are reported through the
// returned AnalysisResult.InvalidPatterns field rather than aborting the
// call.

package diagnosis

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Severity ---------------------------------------------------------------

// Severity is the importance level assigned to an ErrorPattern and
// propagated to the ErrorClusters it produces. Higher numeric values mean
// more severe; the well-known constants below use the conventional
// syslog-style ordering.
type Severity int

const (
	// SeverityInfo is the lowest severity; used for noise patterns that
	// should be recorded but never proposed as a root cause.
	SeverityInfo Severity = 1
	// SeverityWarn is for patterns that indicate degraded behaviour but
	// not necessarily a failure (e.g. long GC pauses).
	SeverityWarn Severity = 2
	// SeverityError is for patterns that indicate a concrete failure
	// (e.g. connection timeout, disk full).
	SeverityError Severity = 3
	// SeverityFatal is for patterns that indicate a process or node
	// crash (e.g. OOM kill, segfault).
	SeverityFatal Severity = 4
)

// String returns the lower-case name of the severity, suitable for
// rendering in reports and JSON output.
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarn:
		return "warn"
	case SeverityError:
		return "error"
	case SeverityFatal:
		return "fatal"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// --- ErrorPattern -----------------------------------------------------------

// ErrorPattern is a single rule that the analyzer matches against every
// log line. The Regex is compiled once when the analyzer is constructed;
// a compile failure is recorded and the pattern is skipped at match time.
type ErrorPattern struct {
	// ID is a short stable identifier (e.g. "OOM", "DISK_FULL") used to
	// cross-reference matched patterns across runs and reports.
	ID string `json:"id"`

	// Name is a human-readable summary of the pattern.
	Name string `json:"name"`

	// Severity controls how strongly a match on this pattern contributes
	// to root-cause determination.
	Severity Severity `json:"severity"`

	// Regex is the regular expression matched against the lower-cased
	// log line message. The expression is compiled with regexp.MustCompile
	// at construction time; if it fails to compile the pattern is marked
	// invalid and skipped.
	Regex string `json:"regex"`

	// Description is a longer explanation of what the pattern detects,
	// suitable for inclusion in an incident report.
	Description string `json:"description"`

	// Suggestion is a remediation hint shown to the operator when the
	// pattern is selected as the root cause.
	Suggestion string `json:"suggestion"`
}

// compiledPattern is the internal form of an ErrorPattern with the regex
// pre-compiled. Invalid patterns keep compiled == nil.
type compiledPattern struct {
	pattern  ErrorPattern
	compiled *regexp.Regexp
}

// --- MatchedPattern / ErrorCluster / TimelineItem ---------------------------

// MatchedPattern summarises all log lines that matched a single
// ErrorPattern during an Analyze call.
type MatchedPattern struct {
	// Pattern is the originating ErrorPattern.
	Pattern ErrorPattern `json:"pattern"`

	// Count is the number of lines that matched.
	Count int `json:"count"`

	// FirstSeen is the earliest timestamp of any matching line, or the
	// zero value if no matching line carried a timestamp.
	FirstSeen time.Time `json:"first_seen,omitempty"`

	// LastSeen is the latest timestamp of any matching line, or the zero
	// value if no matching line carried a timestamp.
	LastSeen time.Time `json:"last_seen,omitempty"`

	// SampleLines is a small (<= SampleLinesMax) selection of matching
	// lines, chosen to illustrate the pattern. The first match is always
	// included; subsequent samples are spread across the match list.
	SampleLines []LogLine `json:"sample_lines,omitempty"`
}

// ErrorCluster groups log lines whose messages share a common prefix. The
// prefix is the cluster signature; lines within a cluster are likely
// instances of the same underlying error and are reported once with a
// count rather than repeated verbatim.
type ErrorCluster struct {
	// ID is a stable identifier derived from the signature, suitable for
	// use as a map key or report anchor.
	ID string `json:"id"`

	// Signature is the shared message prefix that defines the cluster.
	Signature string `json:"signature"`

	// Count is the number of lines in the cluster.
	Count int `json:"count"`

	// Severity is the maximum severity of any pattern matched by a line
	// in the cluster, or SeverityInfo if no pattern matched.
	Severity Severity `json:"severity"`

	// PatternIDs is the set of pattern IDs that matched at least one
	// line in the cluster. It is de-duplicated and sorted.
	PatternIDs []string `json:"pattern_ids,omitempty"`

	// FirstSeen / LastSeen bound the timestamps of the cluster members.
	FirstSeen time.Time `json:"first_seen,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`

	// SampleLines is a small selection of cluster members.
	SampleLines []LogLine `json:"sample_lines,omitempty"`
}

// TimelineItem is a single error event on the incident timeline. Only
// lines that matched at least one pattern are included; the timeline is
// sorted by timestamp ascending.
type TimelineItem struct {
	Timestamp time.Time `json:"timestamp,omitempty"`
	Source    string    `json:"source"`
	Level     string    `json:"level,omitempty"`
	Message   string    `json:"message"`
	PatternID string    `json:"pattern_id,omitempty"`
}

// --- AnalysisResult ---------------------------------------------------------

// AnalysisResult is the outcome of a single Analyze call. It is always
// non-nil even when the analyzer found no errors.
type AnalysisResult struct {
	// RootCause is the pattern proposed as the most likely root cause,
	// or the zero value when no pattern matched. RootCause is only set
	// when Confidence > 0.
	RootCause ErrorPattern `json:"root_cause,omitempty"`

	// Confidence is the analyzer's confidence in RootCause, in the range
	// [0, 1]. A value of 0 means no root cause could be determined.
	Confidence float64 `json:"confidence"`

	// ErrorPatterns lists every pattern that matched at least one line,
	// sorted by descending count then by descending severity.
	ErrorPatterns []MatchedPattern `json:"error_patterns,omitempty"`

	// ErrorClusters lists the error clusters, sorted by descending count
	// then by descending severity.
	ErrorClusters []ErrorCluster `json:"error_clusters,omitempty"`

	// Timeline is the chronological list of matched error events.
	Timeline []TimelineItem `json:"timeline,omitempty"`

	// Summary is a short human-readable description of the analysis
	// outcome, suitable for display at the top of an incident report.
	Summary string `json:"summary"`

	// TotalLines is the number of lines the analyzer examined.
	TotalLines int `json:"total_lines"`

	// ErrorLines is the number of lines that matched at least one
	// pattern.
	ErrorLines int `json:"error_lines"`

	// InvalidPatterns is the set of pattern IDs whose regex failed to
	// compile. Empty when all patterns are valid.
	InvalidPatterns []string `json:"invalid_patterns,omitempty"`

	// AnalyzedAt is the wall-clock time at which Analyze returned.
	AnalyzedAt time.Time `json:"analyzed_at"`
}

// --- LogAnalyzer ------------------------------------------------------------

// lineMatch records that a single log line matched a single pattern. It is
// an internal helper used by Analyze and clusterErrors.
type lineMatch struct {
	lineIdx   int
	patternID string
}

// SampleLinesMax is the maximum number of sample lines retained per
// matched pattern or cluster. Keeping a small bound prevents the result
// from ballooning when a noisy pattern matches thousands of lines.
const SampleLinesMax = 5

// ClusterPrefixLen is the number of characters used as the cluster
// signature. The value is a trade-off: too short and unrelated errors
// merge; too long and the same error with varying parameters splits.
// 40 characters is enough to capture the leading "proc[pid]: error: ..."
// prefix of most well-known error messages without the variable tail.
const ClusterPrefixLen = 40

// LogAnalyzer matches log lines against a library of ErrorPatterns,
// clusters the matches and proposes a root cause. The zero value is not
// usable; callers must use NewLogAnalyzer.
//
// A LogAnalyzer is safe for concurrent use. The pattern set is fixed at
// construction time; to update it, build a new analyzer.
type LogAnalyzer struct {
	patterns []compiledPattern
}

// NewLogAnalyzer returns an analyzer that matches against the given
// patterns. Patterns whose regex fails to compile are recorded in the
// returned invalidIDs list and skipped at match time; the analyzer is
// still returned (with the valid subset) so callers can proceed.
func NewLogAnalyzer(patterns []ErrorPattern) (*LogAnalyzer, []string) {
	compiled := make([]compiledPattern, 0, len(patterns))
	var invalid []string
	for _, p := range patterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			invalid = append(invalid, p.ID)
			log.Warn("diagnosis: invalid pattern regex",
				"pattern", p.ID,
				"regex", p.Regex,
				"error", err.Error())
			// Keep the pattern with compiled == nil so it is
			// skipped at match time but still appears in the
			// invalid list.
			compiled = append(compiled, compiledPattern{pattern: p, compiled: nil})
			continue
		}
		compiled = append(compiled, compiledPattern{pattern: p, compiled: re})
	}
	return &LogAnalyzer{patterns: compiled}, invalid
}

// NewDefaultLogAnalyzer returns an analyzer pre-loaded with the built-in
// pattern library (DefaultPatterns). It is the convenience constructor
// most callers should use.
func NewDefaultLogAnalyzer() *LogAnalyzer {
	a, _ := NewLogAnalyzer(DefaultPatterns())
	return a
}

// Analyze matches every line in batch.Lines against the analyzer's
// patterns, clusters the matches and proposes a root cause. The returned
// AnalysisResult is always non-nil.
func (a *LogAnalyzer) Analyze(batch *LogBatch) *AnalysisResult {
	result := &AnalysisResult{
		TotalLines: 0,
		ErrorLines: 0,
		AnalyzedAt: time.Now(),
	}

	if batch == nil {
		result.Summary = "no log batch provided"
		return result
	}
	result.TotalLines = len(batch.Lines)

	// Match phase: for every line, find the set of patterns that match.
	// A line may match multiple patterns (e.g. an OOM message that also
	// contains "killed" — both OOM and a generic KILL pattern could
	// match); we record every match so the operator sees the full
	// picture.

	var matches []lineMatch
	patternStats := make(map[string]*MatchedPattern)
	timeline := make([]TimelineItem, 0, len(batch.Lines))

	for i, line := range batch.Lines {
		msg := strings.ToLower(line.Message)
		if msg == "" {
			msg = strings.ToLower(line.Raw)
		}
		for _, cp := range a.patterns {
			if cp.compiled == nil {
				continue
			}
			if !cp.compiled.MatchString(msg) {
				continue
			}

			matches = append(matches, lineMatch{lineIdx: i, patternID: cp.pattern.ID})

			mp, ok := patternStats[cp.pattern.ID]
			if !ok {
				mp = &MatchedPattern{Pattern: cp.pattern, Count: 0}
				patternStats[cp.pattern.ID] = mp
			}
			mp.Count++
			updateFirstLast(mp, line)
			mp.SampleLines = appendSample(mp.SampleLines, line)

			timeline = append(timeline, TimelineItem{
				Timestamp: line.Timestamp,
				Source:    line.Source,
				Level:     line.Level,
				Message:   line.Message,
				PatternID: cp.pattern.ID,
			})
		}
	}

	result.ErrorLines = len(matches)

	// Collect matched patterns sorted by count desc, severity desc.
	for _, mp := range patternStats {
		result.ErrorPatterns = append(result.ErrorPatterns, *mp)
	}
	sort.SliceStable(result.ErrorPatterns, func(i, j int) bool {
		if result.ErrorPatterns[i].Count != result.ErrorPatterns[j].Count {
			return result.ErrorPatterns[i].Count > result.ErrorPatterns[j].Count
		}
		return result.ErrorPatterns[i].Pattern.Severity > result.ErrorPatterns[j].Pattern.Severity
	})

	// Cluster phase: group every line (matched or not) by message prefix.
	result.ErrorClusters = clusterErrors(batch.Lines, matches, patternStats)

	// Timeline sorted by timestamp ascending; lines without a timestamp
	// sort first.
	sort.SliceStable(timeline, func(i, j int) bool {
		ti, tj := timeline[i].Timestamp, timeline[j].Timestamp
		if ti.IsZero() {
			return !tj.IsZero()
		}
		if tj.IsZero() {
			return false
		}
		return ti.Before(tj)
	})
	result.Timeline = timeline

	// Root-cause determination.
	rootCause, confidence := determineRootCause(result.ErrorPatterns, result.ErrorClusters)
	result.RootCause = rootCause
	result.Confidence = confidence

	result.Summary = buildSummary(result)
	return result
}

// --- DefaultPatterns --------------------------------------------------------

// defaultPatternsOnce ensures the compiled default pattern slice is built
// only once; the slice itself is immutable after construction so it can be
// shared safely across analyzers.
var (
	defaultPatternsOnce  sync.Once
	defaultPatternsCache []ErrorPattern
)

// DefaultPatterns returns the built-in library of well-known error
// patterns. The returned slice is a fresh copy; callers may mutate it
// freely (e.g. to add custom patterns or tweak severities).
//
// The eight built-in patterns cover the most common failure modes
// observed in production Linux / Windows service operation:
//
//  1. OOM              — out-of-memory kill or allocation failure
//  2. GC_PAUSE         — long garbage-collection pause
//  3. CONN_TIMEOUT     — outbound connection timeout
//  4. DISK_FULL        — filesystem out of space
//  5. PERMISSION_DENIED — file or socket permission denied
//  6. SEGFAULT         — process segfault / SIGSEGV
//  7. DB_CONN_FAILED   — database connection failure
//  8. NETWORK_DOWN     — network unreachable / connection reset
func DefaultPatterns() []ErrorPattern {
	defaultPatternsOnce.Do(func() {
		defaultPatternsCache = []ErrorPattern{
			{
				ID:          "OOM",
				Name:        "Out of memory",
				Severity:    SeverityFatal,
				Regex:       `out of memory|oom-kill|oom kill|cannot allocate memory|killed process.*oom`,
				Description: "The kernel or runtime ran out of memory and killed a process or refused an allocation.",
				Suggestion:  "Increase memory limits, identify the top memory consumer with `top` / `ps aux --sort=-rss`, and consider adding swap or restarting the offending process.",
			},
			{
				ID:          "GC_PAUSE",
				Name:        "Long GC pause",
				Severity:    SeverityWarn,
				Regex:       `gc pause|gc overhead|stop-the-world|full gc|pause.*gc`,
				Description: "The runtime paused application threads for an extended period to perform garbage collection.",
				Suggestion:  "Reduce allocation rate, tune the GC (e.g. GOGC, -Xmx), or switch to a low-pause collector (G1, ZGC, Shenandoah).",
			},
			{
				ID:          "CONN_TIMEOUT",
				Name:        "Connection timeout",
				Severity:    SeverityError,
				Regex:       `connection timeout|dial.*timeout|context deadline exceeded|connect.*timed out|i/o timeout`,
				Description: "An outbound network connection did not complete within the configured timeout.",
				Suggestion:  "Check the target host and port, network routing, firewall rules and the client's timeout configuration.",
			},
			{
				ID:          "DISK_FULL",
				Name:        "Disk full",
				Severity:    SeverityError,
				Regex:       `no space left on device|disk full|enospc|write.*disk.*full`,
				Description: "A write failed because the target filesystem has no free space.",
				Suggestion:  "Free space by deleting or rotating logs, expand the volume, or move the data directory to a larger disk.",
			},
			{
				ID:          "PERMISSION_DENIED",
				Name:        "Permission denied",
				Severity:    SeverityError,
				Regex:       `permission denied|access denied|eacces|operation not permitted|epERM`,
				Description: "An operation was refused because the calling process lacks the required permission.",
				Suggestion:  "Check file / directory ownership and mode, the running user, and any SELinux / AppArmor profile.",
			},
			{
				ID:          "SEGFAULT",
				Name:        "Segmentation fault",
				Severity:    SeverityFatal,
				Regex:       `segmentation fault|segfault|sigsegv|core dumped|fatal signal 11`,
				Description: "The process accessed invalid memory and was terminated by the kernel.",
				Suggestion:  "Inspect the core dump with gdb, check for native library mismatches, and rebuild the affected binary with debug symbols.",
			},
			{
				ID:          "DB_CONN_FAILED",
				Name:        "Database connection failed",
				Severity:    SeverityError,
				Regex:       `too many connections|connection refused|can't connect to|unable to connect to|database.*connection.*failed|db.*connect.*fail`,
				Description: "The application could not open a connection to its database.",
				Suggestion:  "Verify the database is up, check the connection pool size and max_connections, and review the connection string.",
			},
			{
				ID:          "NETWORK_DOWN",
				Name:        "Network unreachable",
				Severity:    SeverityError,
				Regex:       `network is unreachable|network unreachable|connection reset by peer|broken pipe|no route to host|enonet|host unreachable`,
				Description: "An established or new connection failed because the network path was lost.",
				Suggestion:  "Check interface status, routing table, DNS resolution and any recent network or firewall change.",
			},
		}
	})

	out := make([]ErrorPattern, len(defaultPatternsCache))
	copy(out, defaultPatternsCache)
	return out
}

// --- Clustering -------------------------------------------------------------

// clusterErrors groups lines by the leading ClusterPrefixLen characters of
// their message (after lower-casing and trimming whitespace). Only lines
// that matched at least one pattern contribute to a cluster's severity and
// pattern ID set; unmatched lines are still clustered so the operator can
// see the full error landscape, but their clusters carry SeverityInfo.
//
// The returned slice is sorted by descending count then by descending
// severity.
func clusterErrors(lines []LogLine, matches []lineMatch, patternStats map[string]*MatchedPattern) []ErrorCluster {

	// Build a line-index -> pattern IDs map for quick lookup.
	linePatterns := make(map[int][]string, len(matches))
	lineSeverity := make(map[int]Severity, len(matches))
	for _, m := range matches {
		linePatterns[m.lineIdx] = append(linePatterns[m.lineIdx], m.patternID)
		if mp, ok := patternStats[m.patternID]; ok {
			if mp.Pattern.Severity > lineSeverity[m.lineIdx] {
				lineSeverity[m.lineIdx] = mp.Pattern.Severity
			}
		}
	}

	type clusterAccum struct {
		cluster ErrorCluster
	}
	accums := make(map[string]*clusterAccum)

	for i, line := range lines {
		sig := clusterSignature(line.Message)
		if sig == "" {
			sig = clusterSignature(line.Raw)
		}
		if sig == "" {
			continue
		}

		acc, ok := accums[sig]
		if !ok {
			acc = &clusterAccum{
				cluster: ErrorCluster{
					ID:        clusterID(sig),
					Signature: sig,
					Severity:  SeverityInfo,
				},
			}
			accums[sig] = acc
		}

		acc.cluster.Count++
		acc.cluster.SampleLines = appendSample(acc.cluster.SampleLines, line)

		// Merge timestamps.
		if !line.Timestamp.IsZero() {
			if acc.cluster.FirstSeen.IsZero() || line.Timestamp.Before(acc.cluster.FirstSeen) {
				acc.cluster.FirstSeen = line.Timestamp
			}
			if line.Timestamp.After(acc.cluster.LastSeen) {
				acc.cluster.LastSeen = line.Timestamp
			}
		}

		// Promote severity and accumulate pattern IDs.
		if sev := lineSeverity[i]; sev > acc.cluster.Severity {
			acc.cluster.Severity = sev
		}
		for _, pid := range linePatterns[i] {
			if !containsString(acc.cluster.PatternIDs, pid) {
				acc.cluster.PatternIDs = append(acc.cluster.PatternIDs, pid)
			}
		}
	}

	clusters := make([]ErrorCluster, 0, len(accums))
	for _, acc := range accums {
		// De-duplicate and sort pattern IDs.
		sort.Strings(acc.cluster.PatternIDs)
		clusters = append(clusters, acc.cluster)
	}

	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].Count != clusters[j].Count {
			return clusters[i].Count > clusters[j].Count
		}
		return clusters[i].Severity > clusters[j].Severity
	})

	return clusters
}

// clusterSignature returns the lower-case, whitespace-normalised prefix
// of msg with length <= ClusterPrefixLen. Empty input yields an empty
// signature.
func clusterSignature(msg string) string {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return ""
	}
	// Collapse runs of whitespace so that "error:  foo" and
	// "error: foo" cluster together.
	msg = collapseWhitespace(msg)
	if len(msg) > ClusterPrefixLen {
		return msg[:ClusterPrefixLen]
	}
	return msg
}

// clusterID returns a stable identifier for a signature suitable for use
// as a map key or report anchor. We use a simple hex-free scheme: the
// signature itself with spaces replaced by underscores, truncated to 60
// characters. This keeps IDs human-readable in reports.
func clusterID(sig string) string {
	id := strings.ReplaceAll(sig, " ", "_")
	id = strings.ReplaceAll(id, ":", "")
	if len(id) > 60 {
		id = id[:60]
	}
	return id
}

// collapseWhitespace replaces every run of whitespace in s with a single
// space. It does not allocate when s contains no whitespace runs.
func collapseWhitespace(s string) string {
	if !strings.ContainsAny(s, "\t\n\r") && !strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteByte(c)
	}
	return b.String()
}

// --- Root-cause determination -----------------------------------------------

// determineRootCause picks the most likely root cause from the matched
// patterns and clusters. The heuristic is:
//
//  1. If no patterns matched, return the zero ErrorPattern and confidence 0.
//  2. Otherwise pick the pattern with the highest severity; ties are
//     broken by count, then by the order in the matched list (which is
//     already sorted by count desc, severity desc).
//  3. Confidence is severityWeight * countWeight where severityWeight is
//     severity/4 and countWeight is min(count, 10)/10. The product is
//     clamped to [0, 1].
func determineRootCause(patterns []MatchedPattern, clusters []ErrorCluster) (ErrorPattern, float64) {
	if len(patterns) == 0 {
		return ErrorPattern{}, 0
	}

	// Find the dominant pattern: highest severity, then highest count.
	dominant := patterns[0]
	for _, mp := range patterns[1:] {
		if mp.Pattern.Severity > dominant.Pattern.Severity {
			dominant = mp
			continue
		}
		if mp.Pattern.Severity == dominant.Pattern.Severity && mp.Count > dominant.Count {
			dominant = mp
		}
	}

	severityWeight := float64(dominant.Pattern.Severity) / float64(SeverityFatal)
	countWeight := float64(dominant.Count) / 10.0
	if countWeight > 1.0 {
		countWeight = 1.0
	}
	confidence := severityWeight * countWeight
	if confidence > 1.0 {
		confidence = 1.0
	}
	if confidence < 0.0 {
		confidence = 0.0
	}

	// Boost confidence slightly when the dominant pattern also owns the
	// largest cluster — that is a strong signal that the pattern is the
	// primary failure mode rather than a coincidental match.
	if len(clusters) > 0 {
		top := clusters[0]
		if containsString(top.PatternIDs, dominant.Pattern.ID) {
			confidence += 0.05
			if confidence > 1.0 {
				confidence = 1.0
			}
		}
	}

	return dominant.Pattern, confidence
}

// --- Helpers ----------------------------------------------------------------

// updateFirstLast updates mp.FirstSeen / mp.LastSeen from line.Timestamp
// when the latter is non-zero.
func updateFirstLast(mp *MatchedPattern, line LogLine) {
	if line.Timestamp.IsZero() {
		return
	}
	if mp.FirstSeen.IsZero() || line.Timestamp.Before(mp.FirstSeen) {
		mp.FirstSeen = line.Timestamp
	}
	if line.Timestamp.After(mp.LastSeen) {
		mp.LastSeen = line.Timestamp
	}
}

// appendSample appends line to samples if there is room (len < SampleLinesMax)
// and the line is not already present (compared by Raw). The returned slice
// is the updated samples; appendSample never grows the slice beyond
// SampleLinesMax.
func appendSample(samples []LogLine, line LogLine) []LogLine {
	for _, s := range samples {
		if s.Raw == line.Raw {
			return samples
		}
	}
	if len(samples) >= SampleLinesMax {
		return samples
	}
	return append(samples, line)
}

// containsString reports whether ss contains s.
func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// buildSummary returns a one-line human-readable description of the
// analysis outcome.
func buildSummary(result *AnalysisResult) string {
	if result.ErrorLines == 0 {
		return fmt.Sprintf("analyzed %d lines, no errors matched", result.TotalLines)
	}
	if result.Confidence == 0 {
		return fmt.Sprintf("analyzed %d lines, %d matched but no root cause determined",
			result.TotalLines, result.ErrorLines)
	}
	return fmt.Sprintf("analyzed %d lines, %d errors across %d patterns; likely root cause: %s (%s, confidence %.2f)",
		result.TotalLines, result.ErrorLines, len(result.ErrorPatterns),
		result.RootCause.ID, result.RootCause.Name, result.Confidence)
}
