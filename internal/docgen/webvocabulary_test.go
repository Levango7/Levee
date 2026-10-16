package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// committedWebVocabulary reads the generated target from the repository so the
// tests below speak about the artefact CI checks, not about a fixture.
func committedWebVocabulary(t *testing.T) string {
	t.Helper()
	path, err := locateWebVocabulary(repoRootForTest(t))
	require.NoError(t, err)
	data, err := os.ReadFile(path.path)
	require.NoError(t, err)
	return string(data)
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	return root
}

// TestWebVocabularyListsMatchTheirOwners is the direct equality check: every
// value the owners export must appear, and nothing outside them may.
func TestWebVocabularyListsMatchTheirOwners(t *testing.T) {
	body := renderWebVocabulary()

	for _, s := range runstatus.All {
		assert.Containsf(t, body, "'"+s+"',", "run status %q missing from the generated vocabulary", s)
	}
	for _, p := range state.PriorityNames() {
		assert.Containsf(t, body, "'"+p+"',", "priority %q missing from the generated vocabulary", p)
	}

	// A status that was retired must not survive in the generated file either.
	assert.NotContains(t, body, "'pending_approval'",
		"the retired status reappeared in the generated vocabulary")
}

// TestWebVocabularyIsStaleWhenAnOwnerGainsAValue is the positive control that
// makes every other assertion here worth something: it plants a new status in
// the owner, and requires that the committed file no longer equals the
// generator's output. Without this, a guard that parses nothing would still
// report "up to date".
func TestWebVocabularyIsStaleWhenAnOwnerGainsAValue(t *testing.T) {
	original := runstatus.All
	t.Cleanup(func() { runstatus.All = original })

	planted := append(append([]string(nil), original...), "zzz_planted_status")
	runstatus.All = planted

	fresh := renderWebVocabulary()
	require.Contains(t, fresh, "'zzz_planted_status'",
		"the generator ignores a status added to its owner — it is not reading runstatus.All")

	committed := committedWebVocabulary(t)
	assert.NotContains(t, committed, "'zzz_planted_status'",
		"the committed vocabulary already contains the planted status; this control proves nothing")

	// And the check mode would actually catch it, not just the renderer.
	updated, err := replaceBlockMarked(committed, block{"vocabulary", renderWebVocabulary}, tsBeginFmt, tsEndFmt)
	require.NoError(t, err)
	assert.NotEqual(t, committed, updated,
		"docgen would report the repository as up to date after a status was added")
}

// TestRenderedBlockEqualsTheCommittedFile is what `docgen -check` asserts, run
// from inside the test suite so a developer sees the same failure before push.
func TestRenderedBlockEqualsTheCommittedFile(t *testing.T) {
	committed := committedWebVocabulary(t)
	updated, err := replaceBlockMarked(committed, block{"vocabulary", renderWebVocabulary}, tsBeginFmt, tsEndFmt)
	require.NoError(t, err)
	assert.Equal(t, committed, updated,
		"web/src/types/vocabulary.ts is stale — run: go run ./internal/docgen")
}

// TestUnionsAreDerivedNotRepeated keeps the generated block from growing a
// second copy of its own lists: exactly one `as const` array per vocabulary and
// one derived union per name, and no inline union member list anywhere.
func TestUnionsAreDerivedNotRepeated(t *testing.T) {
	body := renderWebVocabulary()

	assert.Equal(t, 2, strings.Count(body, "] as const"),
		"expected exactly one as-const array per generated vocabulary")
	assert.Contains(t, body, "export type ChangeStatus = (typeof CHANGE_STATUSES)[number]")
	assert.Contains(t, body, "export type Priority = (typeof PRIORITIES)[number]")
	// The derived form is the only union form allowed, so a pipe-separated
	// literal list anywhere in the block is a second copy of the same set.
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "| '") {
			t.Errorf("hand-copied union members in generated output: %s", strings.TrimSpace(line))
		}
	}
}

// TestReplaceBlockMarkedHandlesTsMarkers: the TypeScript target must use line
// comments, and the same idempotence and missing-anchor properties hold.
func TestReplaceBlockMarkedHandlesTsMarkers(t *testing.T) {
	const doc = "// header\n\n// BEGIN GENERATED: x\nstale\n// END GENERATED: x\n\n// footer\n"
	b := block{"x", func() string { return "fresh\n" }}

	once, err := replaceBlockMarked(doc, b, tsBeginFmt, tsEndFmt)
	require.NoError(t, err)
	twice, err := replaceBlockMarked(once, b, tsBeginFmt, tsEndFmt)
	require.NoError(t, err)

	assert.Equal(t, once, twice, "running the generator twice must not change bytes")
	assert.Contains(t, once, "fresh")
	assert.NotContains(t, once, "stale")
	assert.Contains(t, once, "// header")
	assert.Contains(t, once, "// footer", "text outside the markers must survive")

	// The HTML marker style must not be accepted for a TS target: silently
	// writing `<!-- ... -->` into a .ts file yields a file that cannot compile.
	_, err = replaceBlockMarked(doc, b, beginFmt, endFmt)
	require.Error(t, err, "HTML markers must not match a TypeScript block")
}

// TestLocateWebVocabularyRefusesDecoys mirrors the spec-locate guard for the new
// target: a same-named file that is not ours must be rejected, not overwritten.
func TestLocateWebVocabularyRefusesDecoys(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "web", "src", "types")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	// A file with the right name and the wrong contents.
	decoy := filepath.Join(dir, "vocabulary.ts")
	require.NoError(t, os.WriteFile(decoy, []byte("export const CHANGE_STATUSES = ['invented'] as const\n"), 0o644))
	_, err := locateWebVocabulary(root)
	require.Error(t, err, "a decoy vocabulary.ts without our markers must be refused")
	assert.Contains(t, err.Error(), "missing generated-block markers")

	// No file at all.
	require.NoError(t, os.Remove(decoy))
	_, err = locateWebVocabulary(root)
	require.Error(t, err, "a missing target file must be an error, not a silent skip")

	// The real repository root resolves.
	got, err := locateWebVocabulary(repoRootForTest(t))
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(got.path, filepath.Join("web", "src", "types", "vocabulary.ts")), got.path)
}

// TestTargetsCoverBothFiles pins the registry itself: adding a renderer without
// registering its target would leave the new artefact unguarded, which is the
// same "generator that quietly stops finding its anchor" failure in a different
// costume.
func TestTargetsCoverBothFiles(t *testing.T) {
	names := make([]string, 0, len(targets()))
	for _, tg := range targets() {
		names = append(names, tg.name)
	}
	assert.ElementsMatch(t,
		[]string{"docs/leveelang-spec.md", "web/src/types/vocabulary.ts"}, names)

	// Every block must render non-empty: an empty body would silently erase a
	// managed section while -check still reported "up to date".
	for _, tg := range targets() {
		require.NotEmpty(t, tg.blocks, tg.name)
		for _, b := range tg.blocks {
			assert.NotEmpty(t, strings.TrimSpace(b.body()), "block %q renders nothing", b.name)
		}
	}
}

// TestTsQuoteKeepsAwkwardValuesParseable covers the escape path: today every
// vocabulary value is lowercase snake_case, so the plain branch is what runs.
// The quoting branch must not be dead code that only fails in production.
func TestTsQuoteKeepsAwkwardValuesParseable(t *testing.T) {
	assert.Equal(t, "'rolled_back_partial'", tsQuote("rolled_back_partial"))
	// A value containing a single quote falls back to Go's %q, i.e. a
	// double-quoted literal — still one valid TypeScript string, whereas naive
	// '"'+v+'"' quoting would emit `'it's'` and break the generated file.
	assert.Equal(t, `"it's"`, tsQuote("it's"))
	assert.Equal(t, `"back\\slash"`, tsQuote(`back\slash`))
	// A bare double quote stays inside single quotes: still parseable.
	assert.Equal(t, `'say"hi'`, tsQuote(`say"hi`))
}
