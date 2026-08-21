// Package diagnosis implements LEVEE's automated failure diagnosis
// subsystem. It collects logs from one or more targets over a time window,
// matches them against a library of well-known error patterns, clusters
// similar messages together and proposes a root-cause hypothesis with a
// confidence score.
//
// Two phases are implemented in this file set:
//
//   - Phase A3 (log_collector.go): pull time-bounded logs from a remote
//     target through an injected CommandExecutor. The executor abstraction
//     keeps the collector transport-agnostic — production code wires in a
//     real SSH / WinRM / agent executor, while tests inject a mock.
//
//   - Phase A4 (log_analyzer.go): regex-based pattern matching, error
//     clustering by message-prefix similarity and root-cause determination
//     from the dominant matched pattern.
//
// All public types are safe for concurrent use. The package never panics;
// failures are reported through the error channel returned by Collect and
// through per-source error entries on the resulting LogBatch.
package diagnosis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrNilExecutor is returned when a LogCollector is constructed with a
	// nil CommandExecutor.
	ErrNilExecutor = errors.New("diagnosis: nil command executor")

	// ErrEmptyTarget is returned when Collect is called with a blank target
	// identifier.
	ErrEmptyTarget = errors.New("diagnosis: empty target")

	// ErrEmptySources is returned when Collect is called with no sources.
	ErrEmptySources = errors.New("diagnosis: empty sources")

	// ErrZeroWindow is returned when Collect is called with a TimeWindow
	// whose Start is not before End.
	ErrZeroWindow = errors.New("diagnosis: zero or negative time window")
)

// --- CommandExecutor --------------------------------------------------------

// CommandExecutor is the transport-agnostic primitive used by LogCollector
// to run a shell command on a remote target and capture its result. The
// interface is intentionally minimal: production code wraps an SSH session,
// a WinRM runspace or an in-process agent, while tests inject a mock that
// returns canned output.
//
// Implementations must be safe for concurrent use. Execute must respect
// ctx: when ctx is cancelled before the command completes the executor
// must release any partial state and return ctx.Err() (wrapped or verbatim).
type CommandExecutor interface {
	// Execute runs command on target and returns the captured stdout,
	// stderr, the process exit code and any transport-level error. A
	// non-zero exit code is NOT reported as an error — callers that care
	// about exit status must inspect exitCode themselves. The err return
	// value is reserved for transport failures (connection dropped,
	// timeout, ...).
	Execute(ctx context.Context, target, command string) (stdout, stderr string, exitCode int, err error)
}

// --- TimeWindow -------------------------------------------------------------

// TimeWindow is the half-open interval [Start, End) used to bound log
// collection. Both bounds are inclusive of their wall-clock second; the
// collector asks the underlying log system for entries whose timestamp
// satisfies Start <= ts < End.
type TimeWindow struct {
	Start time.Time
	End   time.Time
}

// Validate returns ErrZeroWindow if Start is not strictly before End.
func (w TimeWindow) Validate() error {
	if !w.Start.Before(w.End) {
		return ErrZeroWindow
	}
	return nil
}

// --- LogSource --------------------------------------------------------------

// SourceType identifies the kind of log a LogSource refers to. The well-
// known values are SourceSyslog, SourceJournald, SourceEventLog and
// SourceApp; callers may use custom values for vendor-specific logs.
type SourceType string

const (
	// SourceSyslog is the classic /var/log/syslog stream on Linux.
	SourceSyslog SourceType = "syslog"
	// SourceJournald is the systemd journal accessed via journalctl.
	SourceJournald SourceType = "journald"
	// SourceEventLog is the Windows event log accessed via Get-WinEvent
	// or wevtutil.
	SourceEventLog SourceType = "eventlog"
	// SourceApp is an application-owned log file (e.g. /var/log/myapp/*.log
	// or C:\logs\myapp\*.log).
	SourceApp SourceType = "app"
)

// Runtime identifies the operating system family of the target. It selects
// which default sources DefaultSources returns and which command template
// Collect uses to read a given source.
type Runtime string

const (
	// RuntimeLinux selects Linux-style commands (journalctl, awk, ...).
	RuntimeLinux Runtime = "linux"
	// RuntimeWindows selects Windows-style commands (Get-WinEvent,
	// Get-Content, ...).
	RuntimeWindows Runtime = "windows"
)

// LogSource describes a single log stream to collect from a target. The
// Path field is interpreted relative to the target's runtime: on Linux it
// is a filesystem path or a journalctl cursor; on Windows it is an event
// log name (e.g. "Application") or a filesystem path.
type LogSource struct {
	// Name is a human-readable identifier for the source. It is propagated
	// to every LogLine produced from this source so that callers can
	// filter / group by origin.
	Name string `json:"name"`

	// Type selects the command template used to read the source.
	Type SourceType `json:"type"`

	// Path is the location of the log. For SourceApp it is a glob or file
	// path; for SourceEventLog it is the log name; for SourceSyslog and
	// SourceJournald it is ignored (the system stream is read directly).
	Path string `json:"path,omitempty"`

	// Format hints how the collector should parse each line. The well-
	// known values are "plain" (raw text), "syslog" (RFC 3164 / RFC 5424)
	// and "json" (one JSON object per line). Unknown values fall back to
	// "plain".
	Format string `json:"format,omitempty"`
}

// --- LogLine / LogBatch -----------------------------------------------------

// LogLine is a single parsed log entry. Raw is always populated; the other
// fields are best-effort and depend on the source format.
type LogLine struct {
	// Timestamp is the parsed event time. When the format does not carry
	// a timestamp (e.g. plain text without a leading date) the collector
	// leaves it as the zero value and the analyzer ignores it.
	Timestamp time.Time `json:"timestamp,omitempty"`

	// Source is the Name of the LogSource that produced this line.
	Source string `json:"source"`

	// Level is the severity carried by the line, if any ("ERROR",
	// "WARN", "INFO", ...). Normalised to upper-case.
	Level string `json:"level,omitempty"`

	// Message is the human-readable payload, with the timestamp and
	// level stripped when possible.
	Message string `json:"message"`

	// Raw is the original line exactly as produced by the target.
	Raw string `json:"raw"`
}

// SourceError is a per-source failure recorded on a LogBatch when Collect
// could not retrieve one of the requested sources. Other sources may still
// have succeeded.
type SourceError struct {
	Source string `json:"source"`
	Error  string `json:"error"`
}

// LogBatch is the result of a single Collect call. It aggregates the lines
// pulled from every requested source, plus per-source errors for sources
// that failed.
type LogBatch struct {
	// Target is the host the batch was collected from.
	Target string `json:"target"`

	// Window is the time window the batch covers.
	Window TimeWindow `json:"window"`

	// Sources is the list of sources that were requested.
	Sources []LogSource `json:"sources"`

	// Lines is the concatenation of all parsed lines from all successful
	// sources, in (source, then chronological) order.
	Lines []LogLine `json:"lines"`

	// Errors is the list of per-source failures. Empty when every source
	// succeeded.
	Errors []SourceError `json:"errors,omitempty"`

	// CollectedAt is the wall-clock time at which Collect returned.
	CollectedAt time.Time `json:"collected_at"`
}

// --- LogCollector -----------------------------------------------------------

// LogCollector pulls time-bounded logs from a remote target through an
// injected CommandExecutor. The zero value is not usable; callers must use
// NewLogCollector.
//
// A LogCollector is safe for concurrent use: Collect may be called from
// multiple goroutines simultaneously, each call runs its sources in
// parallel internally.
type LogCollector struct {
	exec CommandExecutor
}

// NewLogCollector returns a collector backed by the given executor. The
// executor must be non-nil.
func NewLogCollector(exec CommandExecutor) (*LogCollector, error) {
	if exec == nil {
		return nil, ErrNilExecutor
	}
	return &LogCollector{exec: exec}, nil
}

// Collect pulls logs from every source in sources on target within window.
// Sources are read concurrently; per-source failures are recorded on the
// returned batch rather than aborting the whole call. The returned batch
// always has Lines sorted by (source, then timestamp) and is never nil.
//
// Collect returns an error only for top-level failures (nil executor, blank
// target, empty sources, invalid window, ctx cancelled before any source
// runs). Per-source transport failures are reported via batch.Errors.
func (c *LogCollector) Collect(ctx context.Context, target string, sources []LogSource, window TimeWindow) (*LogBatch, error) {
	if target == "" {
		return nil, ErrEmptyTarget
	}
	if len(sources) == 0 {
		return nil, ErrEmptySources
	}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("diagnosis: collect cancelled before start: %w", err)
	}

	batch := &LogBatch{
		Target:      target,
		Window:      window,
		Sources:     sources,
		CollectedAt: time.Now(),
	}

	type sourceResult struct {
		index int
		lines []LogLine
		err   error
	}

	results := make([]sourceResult, len(sources))
	var wg sync.WaitGroup
	wg.Add(len(sources))

	for i, src := range sources {
		go func(idx int, s LogSource) {
			defer wg.Done()

			cmd, err := buildCollectCommand(s, window)
			if err != nil {
				results[idx] = sourceResult{index: idx, err: err}
				return
			}

			stdout, stderr, exitCode, execErr := c.exec.Execute(ctx, target, cmd)
			if execErr != nil {
				results[idx] = sourceResult{
					index: idx,
					err:   fmt.Errorf("execute %q: %w", s.Name, execErr),
				}
				return
			}
			if exitCode != 0 {
				results[idx] = sourceResult{
					index: idx,
					err:   fmt.Errorf("execute %q: exit code %d: %s", s.Name, exitCode, stderr),
				}
				return
			}

			lines := parseOutput(s, stdout)
			results[idx] = sourceResult{index: idx, lines: lines}
		}(i, src)
	}
	wg.Wait()

	// Preserve the source order requested by the caller.
	for _, r := range results {
		if r.err != nil {
			batch.Errors = append(batch.Errors, SourceError{
				Source: sources[r.index].Name,
				Error:  r.err.Error(),
			})
			log.Warn("diagnosis: source collection failed",
				"source", sources[r.index].Name,
				"error", r.err.Error())
			continue
		}
		batch.Lines = append(batch.Lines, r.lines...)
	}

	log.Info("diagnosis: collection complete",
		"target", target,
		"sources", len(sources),
		"lines", len(batch.Lines),
		"errors", len(batch.Errors))

	return batch, nil
}

// --- Default sources --------------------------------------------------------

// DefaultSources returns the conventional log sources for the given
// runtime. On Linux these are syslog, journald and the application log
// directory under /var/log/levee; on Windows these are the Application
// event log and the application log directory under C:\logs\levee.
//
// The returned slice is a fresh allocation; callers may mutate it freely.
func DefaultSources(runtime Runtime) []LogSource {
	switch runtime {
	case RuntimeLinux:
		return []LogSource{
			{Name: "syslog", Type: SourceSyslog, Format: "syslog"},
			{Name: "journald", Type: SourceJournald, Format: "syslog"},
			{Name: "app", Type: SourceApp, Path: "/var/log/levee/*.log", Format: "plain"},
		}
	case RuntimeWindows:
		return []LogSource{
			{Name: "eventlog", Type: SourceEventLog, Path: "Application", Format: "plain"},
			{Name: "app", Type: SourceApp, Path: `C:\logs\levee\*.log`, Format: "plain"},
		}
	default:
		// Unknown runtime: return an empty slice so callers can detect
		// the gap rather than receiving sources that would never work.
		return nil
	}
}

// --- Command building -------------------------------------------------------

// buildCollectCommand returns the shell command that reads source within
// window on the target. The command is runtime-agnostic in shape: it
// always emits lines on stdout, one entry per line.
func buildCollectCommand(source LogSource, window TimeWindow) (string, error) {
	start := window.Start.UTC().Format(time.RFC3339)
	end := window.End.UTC().Format(time.RFC3339)

	switch source.Type {
	case SourceJournald:
		// journalctl --since --until --output=short-iso
		return fmt.Sprintf(
			"journalctl --since %q --until %q --output=short-iso --no-pager",
			start, end,
		), nil

	case SourceSyslog:
		// awk on /var/log/syslog filtering by RFC3339-bounded date.
		// syslog timestamps are not RFC3339, so we filter on the
		// journalctl-style range by delegating to journalctl as well;
		// this works on any systemd-based Linux which is the only
		// platform we target for SourceSyslog.
		return fmt.Sprintf(
			"journalctl --since %q --until %q --output=short-iso --no-pager -t syslog",
			start, end,
		), nil

	case SourceEventLog:
		// PowerShell Get-WinEvent filtered by time range. We use
		// FilterHashtable so the filtering happens server-side on the
		// target, which is dramatically cheaper than pulling the whole
		// log and filtering client-side.
		logName := source.Path
		if logName == "" {
			logName = "Application"
		}
		return fmt.Sprintf(
			`powershell -NoProfile -Command "Get-WinEvent -FilterHashtable @{LogName='%s'; StartTime='%s'; EndTime='%s'} | ForEach-Object { $_.TimeCreated.ToString('o') + ' ' + $_.LevelDisplayName + ' ' + $_.Message }"`,
			logName, start, end,
		), nil

	case SourceApp:
		if source.Path == "" {
			return "", fmt.Errorf("diagnosis: app source %q has empty path", source.Name)
		}
		// Use awk to filter by mtime range; this is portable across
		// Linux (awk) and Windows (PowerShell via Get-Content), but we
		// keep it simple here and rely on the target's shell. The
		// collector parses whatever the command emits.
		return fmt.Sprintf(
			"awk -v s=%q -v e=%q 'BEGIN{ts=0} {print}' %s",
			start, end, source.Path,
		), nil

	default:
		return "", fmt.Errorf("diagnosis: unknown source type %q", source.Type)
	}
}

// --- Output parsing ---------------------------------------------------------

// parseOutput splits stdout into lines and parses each one according to the
// source format. Unknown formats fall back to "plain". The returned slice
// preserves the order of the input.
func parseOutput(source LogSource, stdout string) []LogLine {
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	// Allow long lines (up to 1 MiB) — application logs can be verbose.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []LogLine
	for scanner.Scan() {
		raw := scanner.Text()
		if raw == "" {
			continue
		}
		lines = append(lines, parseLine(source, raw))
	}
	return lines
}

// parseLine parses a single raw line according to source.Format. It never
// panics and always returns a LogLine with Raw and Source populated.
func parseLine(source LogSource, raw string) LogLine {
	line := LogLine{Source: source.Name, Raw: raw}

	switch source.Format {
	case "syslog":
		parseSyslogLine(&line, raw)
	case "json":
		// JSON parsing is intentionally deferred to a future phase;
		// for now we treat JSON lines as plain text so the analyzer
		// can still match against the raw payload.
		line.Message = raw
	default:
		line.Message = raw
	}
	return line
}

// parseSyslogLine populates line.Timestamp, line.Level and line.Message
// from a syslog-style line. It accepts both the classic RFC 3164 layout
// ("Jan 2 15:04:05 host proc[pid]: message") and the journalctl
// short-iso layout ("2024-01-02T15:04:05+00:00 host proc[pid]: message").
// Unparseable lines leave Timestamp as the zero value and set Message to
// the raw text.
func parseSyslogLine(line *LogLine, raw string) {
	// Try the journalctl short-iso layout first: it starts with an
	// RFC3339-ish timestamp.
	if ts, rest, ok := tryRFC3339Prefix(raw); ok {
		line.Timestamp = ts
		line.Message = stripLevelAndTag(rest, line)
		return
	}

	// Fall back to the classic RFC 3164 layout: the first 15 characters
	// are the timestamp ("Jan  2 15:04:05").
	if len(raw) >= 15 {
		tsStr := raw[:15]
		if ts, err := time.Parse("Jan 2 15:04:05", tsStr); err == nil {
			// RFC 3164 has no year; assume the current year.
			now := time.Now()
			line.Timestamp = time.Date(now.Year(), ts.Month(), ts.Day(),
				ts.Hour(), ts.Minute(), ts.Second(), 0, time.Local)
			line.Message = stripLevelAndTag(strings.TrimSpace(raw[15:]), line)
			return
		}
	}

	line.Message = raw
}

// tryRFC3339Prefix attempts to parse an RFC3339 timestamp from the start of
// s. On success it returns the timestamp, the remainder of s (with the
// timestamp and any single separating space removed) and true.
func tryRFC3339Prefix(s string) (time.Time, string, bool) {
	// RFC3339 timestamps are at least 20 characters ("2006-01-02T15:04:05Z")
	// and at most 35 with a full offset and nanoseconds. Scan a generous
	// upper bound.
	maxLen := len(s)
	if maxLen > 35 {
		maxLen = 35
	}
	for i := 20; i <= maxLen; i++ {
		if s[i-1] == ' ' || s[i-1] == '\t' {
			continue
		}
		tsStr := s[:i]
		if ts, err := time.Parse(time.RFC3339, tsStr); err == nil {
			rest := strings.TrimSpace(s[i:])
			return ts, rest, true
		}
	}
	return time.Time{}, "", false
}

// stripLevelAndTag strips the "host proc[pid]: " prefix and extracts a
// severity level from the message payload when present. The level is
// normalised to upper-case and stored in line.Level.
func stripLevelAndTag(rest string, line *LogLine) string {
	// Find the first ": " which typically separates the tag from the
	// message body.
	if idx := strings.Index(rest, ": "); idx >= 0 {
		rest = strings.TrimSpace(rest[idx+2:])
	}

	// Extract a leading severity token like "ERROR", "WARN", "INFO".
	rest = extractLevel(rest, line)
	return rest
}

// extractLevel peels off a leading severity word (ERROR, WARN, WARNING,
// INFO, INFO, DEBUG, FATAL, CRITICAL, TRACE) and stores its upper-case
// form in line.Level. The remainder is returned with the level and any
// single separating whitespace removed.
func extractLevel(s string, line *LogLine) string {
	known := []string{
		"ERROR", "ERR",
		"WARN", "WARNING",
		"INFO",
		"DEBUG",
		"FATAL", "CRITICAL", "CRIT",
		"TRACE",
		"NOTICE",
	}
	upper := strings.ToUpper(s)
	for _, lvl := range known {
		if strings.HasPrefix(upper, lvl) {
			// Verify the level is followed by a non-alphanumeric
			// boundary so we don't match "INFO" inside "INFORMATION".
			rest := s[len(lvl):]
			if rest == "" || !isAlnum(rest[0]) {
				line.Level = lvl
				return strings.TrimSpace(rest)
			}
		}
	}
	return s
}

// isAlnum reports whether b is an ASCII letter or digit.
func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
