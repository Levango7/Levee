package main

// Tests for the coordination board. Three things are held at once: the parser
// must refuse a claim that cannot be judged (unknown key, unparseable timestamp,
// a state outside the vocabulary); the freshness rule must refuse a claim that
// has outlived its own assertion; and the written format must not drift from the
// vocabulary the parser actually accepts. The second is what keeps the board from
// becoming decoration, so it is tested against a fixed clock rather than the wall
// clock — a test that passes because time happened to be on its side is not a
// gate.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var boardNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// writeBoard drops claim files into a scratch directory and returns it.
func writeBoard(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	return dir
}

func validClaim(updated, ttl string) string {
	return "agent: qoder-a5c\n" +
		"branch: feat/node-capability-declaration\n" +
		"state: active\n" +
		"updated: " + updated + "\n" +
		"ttl-minutes: " + ttl + "\n" +
		"worktree: F:/Nexus/levee-wt-a5c\n" +
		"owns: internal/grpc/rest.go, web/src/views/ClusterView.vue\n" +
		"notes: 一条事实通告，不是指令\n"
}

func TestLoadValidClaim(t *testing.T) {
	dir := writeBoard(t, map[string]string{
		"qoder-a5c.md": validClaim("2026-10-15T11:50:00Z", "30"),
	})

	entries, problems := Load(dir, boardNow)
	assert.Empty(t, problems, "a well-formed, live claim must not be reported")
	require.Len(t, entries, 1)

	e := entries[0]
	assert.Equal(t, "qoder-a5c", e.Agent)
	assert.Equal(t, "feat/node-capability-declaration", e.Branch)
	assert.Equal(t, StateActive, e.State)
	assert.Equal(t, 30*time.Minute, e.TTL)
	assert.Equal(t, []string{"internal/grpc/rest.go", "web/src/views/ClusterView.vue"}, e.Owns,
		"owns is a comma list normalised to forward slashes")
	assert.Equal(t, boardNow.Add(-10*time.Minute).Add(30*time.Minute), e.Expiry())
	assert.False(t, e.Expired(boardNow))
}

func TestLoadRejectsAnythingThatCannotBeJudged(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			// The failure this rule exists for: `ttl_minutes` parses as prose,
			// the expiry vanishes, and the claim stays "live" forever while
			// asserting nothing. Ignoring unknown keys would let that through.
			name:    "unknown key is a typo that dropped a rule",
			body:    strings.Replace(validClaim("2026-10-15T11:50:00Z", "30"), "ttl-minutes:", "ttl_minutes:", 1),
			wantSub: "unknown key",
		},
		{
			name:    "missing required key",
			body:    strings.Replace(validClaim("2026-10-15T11:50:00Z", "30"), "branch: feat/node-capability-declaration\n", "", 1),
			wantSub: `missing required key "branch"`,
		},
		{
			name:    "duplicate key leaves two answers to one question",
			body:    validClaim("2026-10-15T11:50:00Z", "30") + "agent: other\n",
			wantSub: "duplicate key",
		},
		{
			name:    "state outside the vocabulary",
			body:    strings.Replace(validClaim("2026-10-15T11:50:00Z", "30"), "state: active", "state: in-progress-ish", 1),
			wantSub: "must be one of active|done",
		},
		{
			// A naive `2026-10-15 11:50` would be silently dropped and the entry
			// would never expire.
			name:    "timestamp without offset is not RFC3339",
			body:    strings.Replace(validClaim("2026-10-15T11:50:00Z", "30"), "2026-10-15T11:50:00Z", "2026-10-15 11:50", 1),
			wantSub: "not RFC3339",
		},
		{
			name:    "ttl below the floor expires before anyone can read it",
			body:    validClaim("2026-10-15T11:50:00Z", "1"),
			wantSub: "ttl-minutes 1 is outside",
		},
		{
			name:    "ttl above the ceiling is an abandoned hold",
			body:    validClaim("2026-10-15T11:50:00Z", "99999"),
			wantSub: "outside",
		},
		{
			name:    "prose without a colon",
			body:    "this agent is working on the cluster page\n",
			wantSub: "not a `key: value` line",
		},
		{
			name:    "branch with a space is not a branch",
			body:    strings.Replace(validClaim("2026-10-15T11:50:00Z", "30"), "branch: feat/node-capability-declaration", "branch: feat two", 1),
			wantSub: "not a usable branch name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeBoard(t, map[string]string{"a.md": tc.body})
			entries, problems := Load(dir, boardNow)
			require.NotEmpty(t, problems, "the mutation must be reported")
			assert.Contains(t, strings.Join(problems, "\n"), tc.wantSub)
			assert.Empty(t, entries, "an unjudgeable claim must not be counted as live")
		})
	}
}

func TestLoadFileNameMustBeAnAddressableIdentity(t *testing.T) {
	// Uppercase and separators vary by filesystem and by whoever typed the name;
	// the file name is how a reader says which agent wrote this, so it is pinned.
	for _, name := range []string{"Qoder A5C.md", "with space.md", "..escapes.md"} {
		t.Run(name, func(t *testing.T) {
			dir := writeBoard(t, map[string]string{name: validClaim("2026-10-15T11:50:00Z", "30")})
			_, problems := Load(dir, boardNow)
			assert.Contains(t, strings.Join(problems, "\n"), "file name must match")
		})
	}
}

func TestFreshnessIsJudgedOnTheGivenClock(t *testing.T) {
	t.Run("a live claim past its ttl is a stale assertion", func(t *testing.T) {
		dir := writeBoard(t, map[string]string{"a.md": validClaim("2026-10-15T09:00:00Z", "30")})
		entries, problems := Load(dir, boardNow)
		require.Len(t, entries, 1, "it still parses; only its authority expired")
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "expired")
		assert.Contains(t, problems[0], "refresh it or delete the file",
			"the message has to carry the remedy, not just the verdict")
	})

	t.Run("a done claim outliving its ttl is noise on the board", func(t *testing.T) {
		body := strings.Replace(validClaim("2026-10-15T09:00:00Z", "30"), "state: active", "state: done", 1)
		dir := writeBoard(t, map[string]string{"a.md": body})
		_, problems := Load(dir, boardNow)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "done claim")
	})

	t.Run("a timestamp ahead of the clock is a lie about freshness", func(t *testing.T) {
		dir := writeBoard(t, map[string]string{"a.md": validClaim("2026-10-16T09:00:00Z", "30")})
		_, problems := Load(dir, boardNow)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "in the future")
	})

	t.Run("small clock skew is tolerated", func(t *testing.T) {
		// Written a minute ahead by a second machine: refusing this would make
		// the board depend on every host agreeing about the time.
		dir := writeBoard(t, map[string]string{"a.md": validClaim("2026-10-15T12:01:00Z", "30")})
		_, problems := Load(dir, boardNow)
		assert.Empty(t, problems)
	})

	t.Run("missing board directory is reported not swallowed", func(t *testing.T) {
		_, problems := Load(filepath.Join(t.TempDir(), "nope"), boardNow)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "cannot read")
	})
}

func TestDuplicateBranchClaimsAreAmbiguous(t *testing.T) {
	dir := writeBoard(t, map[string]string{
		"one.md": validClaim("2026-10-15T11:50:00Z", "30"),
		"two.md": strings.Replace(validClaim("2026-10-15T11:55:00Z", "30"),
			"agent: qoder-a5c", "agent: opencode-7", 1),
	})
	entries, problems := Load(dir, boardNow)
	require.Len(t, entries, 2, "both files are individually well-formed")
	require.Len(t, problems, 1, "one complaint per doubled branch")
	assert.Contains(t, problems[0], "claimed by 2 files")
	assert.Contains(t, problems[0], "one.md, two.md")
}

// The three cases below pin that overlap is VISIBLE but never a refusal, and
// that expiry removes a claim from the picture entirely.
func TestOverlapsAreReportedNotEnforced(t *testing.T) {
	shared := "internal/grpc/rest.go"
	a := validClaim("2026-10-15T11:50:00Z", "30")

	t.Run("two live branches in one file", func(t *testing.T) {
		b := strings.Replace(strings.Replace(a,
			"branch: feat/node-capability-declaration", "branch: feat/cluster-enabled-flag-on-wire", 1),
			"owns: internal/grpc/rest.go, web/src/views/ClusterView.vue", "owns: "+shared, 1)
		entries, _ := Load(writeBoard(t, map[string]string{"a.md": a, "b.md": b}), boardNow)
		overlaps := Overlaps(entries, boardNow)
		require.Len(t, overlaps, 1)
		assert.Contains(t, overlaps[0], shared)
	})

	t.Run("an expired neighbour does not overlap", func(t *testing.T) {
		b := strings.Replace(strings.Replace(a,
			"branch: feat/node-capability-declaration", "branch: feat/abandoned", 1),
			"2026-10-15T11:50:00Z", "2026-10-15T07:00:00Z", 1)
		entries, _ := Load(writeBoard(t, map[string]string{"a.md": a, "b.md": b}), boardNow)
		assert.Empty(t, Overlaps(entries, boardNow),
			"a claim that expired says nothing about what it used to touch")
	})

	t.Run("a done neighbour does not overlap", func(t *testing.T) {
		b := strings.Replace(strings.Replace(a,
			"branch: feat/node-capability-declaration", "branch: feat/just-merged", 1),
			"state: active", "state: done", 1)
		b = strings.Replace(b, "2026-10-15T11:50:00Z", "2026-10-15T11:55:00Z", 1)
		entries, _ := Load(writeBoard(t, map[string]string{"a.md": a, "b.md": b}), boardNow)
		assert.Empty(t, Overlaps(entries, boardNow))
	})
}

func TestPathsInUseOnlyCountsLiveClaims(t *testing.T) {
	live := validClaim("2026-10-15T11:50:00Z", "30")
	// The expired claim owns one path nobody else claims. Without that, including
	// expired claims in the answer would be invisible: the live claim below
	// already covers rest.go and ClusterView.vue, so a mutation that stopped
	// filtering by expiry would still return the same three paths and the test
	// would pass while the function lies.
	expired := strings.Replace(live, "2026-10-15T11:50:00Z", "2026-10-15T07:00:00Z", 1)
	expired = strings.Replace(expired, "branch: feat/node-capability-declaration", "branch: feat/old", 1)
	expired = strings.Replace(expired,
		"owns: internal/grpc/rest.go, web/src/views/ClusterView.vue",
		"owns: internal/cluster/pg_registry.go", 1)
	other := strings.Replace(live, "agent: qoder-a5c", "agent: codex-2", 1)
	other = strings.Replace(other, "branch: feat/node-capability-declaration", "branch: feat/other", 1)
	other = strings.Replace(other,
		"owns: internal/grpc/rest.go, web/src/views/ClusterView.vue", "owns: cmd/levee/cmd_serve.go", 1)

	dir := writeBoard(t, map[string]string{"live.md": live, "expired.md": expired, "other.md": other})
	entries, _ := Load(dir, boardNow)
	require.Len(t, entries, 3, "all three parse; only their authority differs")
	assert.Equal(t, []string{"cmd/levee/cmd_serve.go", "internal/grpc/rest.go", "web/src/views/ClusterView.vue"},
		PathsInUse(entries, boardNow),
		"the reader's one-question answer: what is someone touching right now — "+
			"and a path only an expired claim mentions must not appear in it")
}

func TestReadmeIsNotAClaim(t *testing.T) {
	dir := writeBoard(t, map[string]string{
		ReadmeName: "# format\n\nThis file documents the format and is not a claim.\n",
	})
	entries, problems := Load(dir, boardNow)
	assert.Empty(t, entries)
	assert.Empty(t, problems, "the format document must never be validated as a claim")
}

func TestResolveDirPrefersTheFlag(t *testing.T) {
	t.Setenv(EnvDir, "F:/from/environment")

	got, err := resolveDir("F:/from/flag")
	require.NoError(t, err)
	assert.Equal(t, "F:/from/flag", got,
		"a one-off run against a scratch board must not depend on the shell")

	got, err = resolveDir("")
	require.NoError(t, err)
	assert.Equal(t, "F:/from/environment", got)

	// Invented default would have every agent reading a different directory while
	// believing they were coordinated.
	t.Setenv(EnvDir, "")
	_, err = resolveDir("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvDir)
}

// TestFormatDocMatchesTheParser is the part of this that actually belongs in a
// build: the board lives on a shared filesystem, so nothing in CI can tell
// whether anyone posted a claim. What CI CAN refuse is the written format and
// the parser disagreeing — a doc that advertises `ttl_minutes`, or a filename
// rule that drifted, produces claims whose expiry silently never applies. That
// is the same drift this repo has paid for repeatedly, and it is exactly the
// failure mode this tool exists to avoid for other people.
func TestFormatDocMatchesTheParser(t *testing.T) {
	doc := filepath.Join(repoRoot(t), "docs", "agent-board.md")
	raw, err := os.ReadFile(doc)
	require.NoError(t, err, "the board format must be documented")
	text := string(raw)

	// Keys are read out of the document's own table, first column, in backticks.
	declared := map[string]bool{}
	keyRow := regexp.MustCompile(`^\| ` + "`" + `([^` + "`" + `]+)` + "`" + ` \|`)
	for _, line := range strings.Split(text, "\n") {
		if m := keyRow.FindStringSubmatch(line); m != nil {
			declared[m[1]] = true
		}
	}
	require.NotEmpty(t, declared, "the table must be parseable or the gate is vacuous")

	want := map[string]bool{}
	for key := range knownKeys {
		want[key] = true
	}
	assert.Equal(t, want, declared,
		"docs/agent-board.md must list exactly the keys the parser accepts — an undocumented key is a typo waiting to drop a rule")

	assert.Contains(t, text, "`"+fileNamePattern.String()+"`",
		"the filename rule is stated once, in the document, matching the compiled regexp")

	bounds := fmt.Sprintf("%d-%d", int(MinTTL/time.Minute), int(MaxTTL/time.Minute))
	assert.Contains(t, text, bounds,
		"the TTL bounds are advertised as "+bounds+" in the document")

	for _, state := range []string{StateActive, StateDone} {
		assert.Contains(t, text, "`"+state+"`")
	}
}

// repoRoot walks up from the test file to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
