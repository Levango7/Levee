// Command agentboard reads and validates a coordination board: a directory where
// one file is one agent's claim on one branch.
//
// It exists because several agents share this repository on one machine and the
// collisions were all discoverable in advance but invisible at the moment they
// happened: worktrees created by one session being pruned by another, two
// sessions landing work in the wrong checkout, a browser check pointed at a dev
// server that belongs to somebody else's tree. None of that is a code defect; it
// is a missing fact.
//
// The board therefore lives on the shared filesystem, NOT in the repository. A
// claim committed to a branch is unreadable by the peer it is meant to warn
// until that peer fetches and checks the branch out, which is exactly the moment
// the collision has already happened; and a merged claim would sit on master
// expiring, turning an abandoned session into a red build for strangers. The
// directory comes from -dir or LEVEE_AGENT_BOARD_DIR, so nothing here hardcodes
// one machine's path.
//
// It is deliberately not a command channel. An entry states what its author is
// doing and nothing more: no priority, no assignment, no request. Merge and push
// authority stays with the human who owns the repository, and a claim is never
// evidence about another agent's work — you still re-run their conclusions
// yourself.
//
// One file per claim, not one append-only board: several writers appending to a
// single file collide on every concurrent branch, which would make the
// coordination device itself a source of conflicts.
//
// Usage:
//
//	agentboard [-dir <path>] [-now <RFC3339>]
//
// Exit 0 with a report when the board is honest; exit 1 naming every entry that
// is malformed, expired, or ambiguous. The vocabulary it accepts is the same one
// docs/agent-board.md describes to whoever has to write a claim.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EnvDir is where an operator points the board. There is no default path: a
// silently invented one would have every agent reading a different directory
// and believing they were coordinated.
const EnvDir = "LEVEE_AGENT_BOARD_DIR"

// ReadmeName is the format document that may live in the board directory and is
// not itself a claim.
const ReadmeName = "README.md"

// States are the only two lifecycle values a claim may carry.
const (
	StateActive = "active"
	StateDone   = "done"
)

// MinTTL and MaxTTL bound how long a claim is allowed to assert anything.
//
// The floor stops a claim from expiring so fast that readers cannot act on it;
// the ceiling stops an abandoned session from holding a branch "open" forever.
// Both bounds exist because the gate's whole value is expiry: an entry that
// never goes stale is a comment, not a claim.
var (
	MinTTL = 5 * time.Minute
	MaxTTL = 24 * time.Hour
)

// clockSlack tolerates a small skew between the machine writing a claim and the
// machine validating it. Beyond it, a future timestamp is a lie about freshness
// (it would make an expired claim look current), so it is refused.
const clockSlack = 5 * time.Minute

// fileNamePattern is the on-disk identity of a claim: lowercase, no slashes, so
// that "which agent wrote this" is answerable from the path alone.
var fileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,80}\.md$`)

// requiredKeys is the minimum a reader needs to judge a claim at all.
var requiredKeys = []string{"agent", "branch", "state", "updated", "ttl-minutes"}

// knownKeys is closed on purpose: an unknown key is a problem rather than being
// ignored. `ttl_minutes` instead of `ttl-minutes` would otherwise silently drop
// the expiry, and the claim would stay readable forever while asserting nothing.
var knownKeys = map[string]bool{
	"agent": true, "branch": true, "state": true, "updated": true,
	"ttl-minutes": true, "worktree": true, "owns": true, "notes": true,
}

// Entry is one parsed claim. File is the board-relative name it came from.
type Entry struct {
	File     string
	Agent    string
	Branch   string
	State    string
	Updated  time.Time
	TTL      time.Duration
	Worktree string
	Owns     []string
	Notes    string
}

// Expiry is the instant after which this entry no longer asserts anything.
func (e Entry) Expiry() time.Time { return e.Updated.Add(e.TTL) }

// Expired reports whether the entry has run out its claim.
func (e Entry) Expired(now time.Time) bool { return now.After(e.Expiry()) }

// Load parses every claim in dir and returns them together with the problems
// found. Problems are always a superset of what a reader may not act on:
// malformed entries, unknown keys, a state that does not parse. Entries that
// were parsed successfully are returned even when other files are broken, so a
// single bad file cannot blind the caller to the rest of the board.
func Load(dir string, now time.Time) ([]Entry, []string) {
	ents, problems := parseAll(dir)
	return ents, append(problems, freshnessIssues(ents, now)...)
}

// parseAll walks the directory in sorted order so reported problem lists are
// deterministic across platforms.
func parseAll(dir string) ([]Entry, []string) {
	names, err := sortedClaimFiles(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("agent-board: cannot read %s: %v", dir, err)}
	}
	var (
		ents     []Entry
		problems []string
	)
	for _, name := range names {
		e, problemsFor := parseFile(filepath.Join(dir, name), name)
		problems = append(problems, problemsFor...)
		if len(problemsFor) == 0 {
			ents = append(ents, e)
		}
	}
	problems = append(problems, duplicateBranchIssues(ents)...)
	return ents, problems
}

// sortedClaimFiles lists claim file names, excluding the format document and
// anything that is not a regular .md file.
func sortedClaimFiles(dir string) ([]string, error) {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range dirents {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			continue
		}
		if d.Name() == ReadmeName {
			continue
		}
		names = append(names, d.Name())
	}
	sort.Strings(names)
	return names, nil
}

// parseFile reads one claim. It accumulates every complaint about the file
// instead of stopping at the first, because whoever broke the board is the only
// person who can fix it and a list is cheaper to act on than a series of
// single-error round trips.
func parseFile(path, name string) (Entry, []string) {
	var problems []string
	if !fileNamePattern.MatchString(name) {
		problems = append(problems, fmt.Sprintf(
			"claim %s: file name must match %s (lowercase, no slashes)",
			name, fileNamePattern.String()))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, append(problems, fmt.Sprintf("claim %s: %v", name, err))
	}

	fields := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"claim %s:%d: not a `key: value` line: %q", name, i+1, trimmed))
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !knownKeys[key] {
			problems = append(problems, fmt.Sprintf(
				"claim %s:%d: unknown key %q (the vocabulary is closed; a typo here drops a rule silently)",
				name, i+1, key))
			continue
		}
		if _, dup := fields[key]; dup {
			problems = append(problems, fmt.Sprintf(
				"claim %s:%d: duplicate key %q", name, i+1, key))
			continue
		}
		fields[key] = value
	}

	for _, key := range requiredKeys {
		if _, ok := fields[key]; !ok {
			problems = append(problems, fmt.Sprintf("claim %s: missing required key %q", name, key))
		}
	}

	e := Entry{
		File:     name,
		Agent:    fields["agent"],
		Branch:   fields["branch"],
		State:    fields["state"],
		Worktree: fields["worktree"],
		Owns:     splitList(fields["owns"]),
		Notes:    fields["notes"],
	}

	if problemsOnly := validateIdentity(&e, name); len(problemsOnly) > 0 {
		problems = append(problems, problemsOnly...)
	}
	if problemsOnly := validateTiming(&e, fields["updated"], fields["ttl-minutes"], name); len(problemsOnly) > 0 {
		problems = append(problems, problemsOnly...)
	}
	return e, problems
}

// validateIdentity checks the two fields a reader uses to decide whether this
// claim is about the work in front of them.
func validateIdentity(e *Entry, name string) []string {
	var problems []string
	if strings.TrimSpace(e.Agent) == "" {
		problems = append(problems, fmt.Sprintf("claim %s: agent must be named", name))
	}
	if e.Branch == "" || strings.HasPrefix(e.Branch, "/") || strings.HasSuffix(e.Branch, "/") ||
		strings.ContainsAny(e.Branch, " \t") {
		problems = append(problems, fmt.Sprintf(
			"claim %s: branch %q is not a usable branch name", name, e.Branch))
	}
	if e.State != StateActive && e.State != StateDone {
		problems = append(problems, fmt.Sprintf(
			"claim %s: state %q must be one of %s|%s", name, e.State, StateActive, StateDone))
	}
	return problems
}

// validateTiming parses and range-checks the freshness pair.
func validateTiming(e *Entry, updatedRaw, ttlRaw, name string) []string {
	var problems []string
	if updatedRaw != "" {
		ts, err := time.Parse(time.RFC3339, updatedRaw)
		if err != nil {
			problems = append(problems, fmt.Sprintf(
				"claim %s: updated %q is not RFC3339 with an offset (e.g. 2026-10-15T03:04:05+08:00)",
				name, updatedRaw))
		} else {
			e.Updated = ts
		}
	}
	if ttlRaw != "" {
		minutes, err := strconv.Atoi(ttlRaw)
		if err != nil {
			problems = append(problems, fmt.Sprintf(
				"claim %s: ttl-minutes %q is not an integer", name, ttlRaw))
		} else {
			e.TTL = time.Duration(minutes) * time.Minute
			if e.TTL < MinTTL || e.TTL > MaxTTL {
				problems = append(problems, fmt.Sprintf(
					"claim %s: ttl-minutes %d is outside [%d, %d]",
					name, minutes, int(MinTTL/time.Minute), int(MaxTTL/time.Minute)))
			}
		}
	}
	return problems
}

// freshnessIssues is the part of the gate that fights decay: an entry that ran
// out its TTL but still says `active` claims authority it no longer has, and a
// `done` entry that outlived its TTL is just noise on the board.
//
// Both are failures rather than warnings. That is the point — whoever finds a
// stale claim is expected to delete it, and the rules for doing that safely are
// in docs/agent-board.md.
func freshnessIssues(ents []Entry, now time.Time) []string {
	var problems []string
	for _, e := range ents {
		if e.Updated.IsZero() || e.TTL == 0 {
			continue // already reported by validateTiming
		}
		if e.Updated.After(now.Add(clockSlack)) {
			problems = append(problems, fmt.Sprintf(
				"claim %s: updated %s is in the future (now %s)",
				e.File, e.Updated.Format(time.RFC3339), now.Format(time.RFC3339)))
			continue
		}
		if e.Expired(now) {
			problems = append(problems, fmt.Sprintf(
				"claim %s: %s claim on %s expired %s (updated %s + %s) — refresh it or delete the file",
				e.File, e.State, e.Branch, now.Sub(e.Expiry()).Round(time.Minute),
				e.Updated.Format(time.RFC3339), e.TTL))
		}
	}
	return problems
}

// duplicateBranchIssues catches the one structural error a per-file board makes
// easy: two agents describing the same branch, so neither file is authoritative.
func duplicateBranchIssues(ents []Entry) []string {
	byBranch := map[string][]string{}
	for _, e := range ents {
		if e.Branch != "" {
			byBranch[e.Branch] = append(byBranch[e.Branch], e.File)
		}
	}
	var branches []string
	for b, files := range byBranch {
		if len(files) > 1 {
			branches = append(branches, b)
		}
	}
	sort.Strings(branches)
	var problems []string
	for _, b := range branches {
		files := byBranch[b]
		sort.Strings(files)
		problems = append(problems, fmt.Sprintf(
			"branch %q is claimed by %d files (%s) — one claim per branch",
			b, len(files), strings.Join(files, ", ")))
	}
	return problems
}

// Overlaps pairs up live claims that say they touch the same path.
//
// This is a report, not a refusal. Two branches touching one file is normal in a
// review queue — #160 and #161 both edit internal/grpc/rest.go legitimately —
// so prohibiting it would turn the board into a blocker and get it ignored. What
// must be impossible is the board LYING about what is live; what must be VISIBLE
// is that someone else is in the same file.
func Overlaps(ents []Entry, now time.Time) []string {
	var out []string
	for i := 0; i < len(ents); i++ {
		for j := i + 1; j < len(ents); j++ {
			a, b := ents[i], ents[j]
			if a.State != StateActive || b.State != StateActive ||
				a.Branch == b.Branch || a.Expired(now) || b.Expired(now) {
				continue
			}
			shared := intersect(a.Owns, b.Owns)
			if len(shared) == 0 {
				continue
			}
			out = append(out, fmt.Sprintf("%s (%s) and %s (%s) both touch: %s",
				a.File, a.Branch, b.File, b.Branch, strings.Join(shared, ", ")))
		}
	}
	sort.Strings(out)
	return out
}

// PathsInUse lists the paths that live claims say they are touching, for a
// reader who wants one answer before editing something hot.
func PathsInUse(ents []Entry, now time.Time) []string {
	seen := map[string]bool{}
	for _, e := range ents {
		if e.State != StateActive || e.Expired(now) {
			continue
		}
		for _, p := range e.Owns {
			seen[p] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// splitList turns `a, b` into ["a","b"] and treats an empty value as no items.
func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, filepath.ToSlash(trimmed))
		}
	}
	return out
}

func intersect(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	seen := make(map[string]bool, len(a))
	var out []string
	for _, s := range a {
		if inB[s] && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// exitProblems and exitUsage are the two failure distinctions a caller acts on:
// "the board is lying" versus "I was not told where the board is". Collapsing
// them would make a missing configuration look like a stale claim.
const (
	exitProblems = 1
	exitUsage    = 2
)

func main() {
	dirFlag := flag.String("dir", "", "board directory (falls back to $"+EnvDir+")")
	nowFlag := flag.String("now", "", "evaluate freshness at this RFC3339 instant instead of the clock")
	flag.Parse()

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentboard:", err)
		os.Exit(exitUsage)
	}
	now := time.Now()
	if strings.TrimSpace(*nowFlag) != "" {
		ts, err := time.Parse(time.RFC3339, strings.TrimSpace(*nowFlag))
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentboard: -now %q is not RFC3339: %v\n", *nowFlag, err)
			os.Exit(exitUsage)
		}
		now = ts
	}

	entries, problems := Load(dir, now)
	fmt.Printf("board %s — %d readable claim(s), evaluated at %s\n",
		dir, len(entries), now.Format(time.RFC3339))
	for _, e := range entries {
		fmt.Printf("  %-24s %-10s %-6s expires %s\n",
			e.Agent, e.Branch, e.State, e.Expiry().Format(time.RFC3339))
	}
	if overlaps := Overlaps(entries, now); len(overlaps) > 0 {
		fmt.Println("overlapping claims (advisory, not a refusal):")
		for _, o := range overlaps {
			fmt.Println("  ", o)
		}
	}
	if paths := PathsInUse(entries, now); len(paths) > 0 {
		fmt.Println("paths someone is actively touching:")
		for _, p := range paths {
			fmt.Println("  ", p)
		}
	}
	if len(problems) > 0 {
		fmt.Println("problems:")
		for _, p := range problems {
			fmt.Println("  ", p)
		}
		os.Exit(exitProblems)
	}
	fmt.Println("board is honest")
}

// resolveDir prefers the flag over the environment so a one-off invocation
// against a scratch board (a test, a review) never depends on what the shell
// happens to export.
func resolveDir(flagValue string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv(EnvDir)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no board directory given: pass -dir or set %s "+
		"(see docs/agent-board.md)", EnvDir)
}
