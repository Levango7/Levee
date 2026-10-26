package chatops

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChatOpsEventProducerInventory pins, as a machine-checked fact, which
// chatops event types the production code actually publishes.
//
// Why this exists: `EventStateChange` was registered as unproduced when the
// run-transition funnel landed (the four run_* types used to be unproduced too).
// That registration lived only in prose — CHANGELOG and
// docs/product-roadmap.md — and this repository has been bitten repeatedly by
// prose that nobody re-reads: a status written today silently becomes a lie
// after the next change (`StatusExpired`, `--local` authz, `runToPB`'s Label).
// The same fix used for `approval.StatusExpired`
// (TestCheckExpiryIsTheOnlyProducerOfExpired) is applied here: turn the
// asymmetry into a test that goes red the moment it stops being true.
//
// The check is deliberately source-shaped rather than behavioural: there is no
// runtime hook that says "type X was never published" (a type with no producer
// is, by definition, never seen by any test). Scanning ignores _test.go, so a
// producer that exists only inside tests does not count — the exact distinction
// the CHANGELOG calls out between "type exists" and "call site exists".
//
// When this test fails, the fix is one of:
//   - a type gained a producer: move it to the produced half of `produced` and
//     correct the "unproduced" prose in CHANGELOG/roadmap, or
//   - a producer was removed: restore it, or move the type to the unproduced
//     half and register that fact where the run-transition funnel is described.
//
// Do not just flip the map to make the test green: the map is the record.
func TestChatOpsEventProducerInventory(t *testing.T) {
	// The constant identifiers are read from their declaration site (bot.go)
	// rather than hand-copied, so a newly declared EventType shows up here
	// automatically and fails the completeness check below until it is
	// registered. Scanning uses the identifier (e.g. "EventRunStarted"), not
	// the wire value ("run_started") — the value collides with unrelated
	// strings such as conversation's own "state_change".
	declared := declaredEventTypeIdentifiers(t)

	// Expected producer status, keyed by constant identifier.
	//
	// `EventStateChange` is the one deliberate omission: the chat room only
	// gets the three named run moments plus approval/gate cards, and
	// intermediate hops are mirrored to the outbound webhook channel instead
	// (notify.RunLifecycleMirror) — a card per hop was rejected as noise.
	// There is intentionally no chat producer for it today.
	produced := map[string]bool{
		"EventApprovalRequested": true,
		"EventApprovalDecision":  true,
		"EventApprovalExpired":   true,
		"EventGateResult":        true,
		"EventRunStarted":        true,
		"EventRunCompleted":      true,
		"EventRunFailed":         true,
		"EventStateChange":       false, // no producer by design; see above
	}

	// Completeness, both directions: every declared constant must be
	// registered, and nothing may be registered that is not declared. This is
	// what stops the map from silently drifting away from the constants —
	// adding a constant fails here, and deleting a key fails the length check.
	require.Len(t, produced, len(declared),
		"every declared EventType constant must be registered in the produced map")
	for _, ident := range declared {
		_, ok := produced[ident]
		require.True(t, ok, "EventType %s is declared but missing from the produced map", ident)
	}

	root := repoRoot(t)
	var scanned int
	found := make(map[string]int, len(declared))

	for _, top := range []string{"internal", "cmd"} {
		dir := filepath.Join(root, top)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, werr error) error {
			if werr != nil || info.IsDir() {
				return nil
			}
			slash := filepath.ToSlash(path)
			if !strings.HasSuffix(slash, ".go") || strings.HasSuffix(slash, "_test.go") {
				return nil
			}
			// Producers must live OUTSIDE this package: bot.go declares the
			// constants and card.go renders them — neither is a producer.
			if strings.Contains(slash, "/internal/chatops/") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			scanned++
			for _, line := range strings.Split(string(b), "\n") {
				code := strings.TrimSpace(line)
				if strings.HasPrefix(code, "//") {
					continue
				}
				for _, ident := range declared {
					if strings.Contains(code, ident) {
						found[ident]++
					}
				}
			}
			return nil
		})
		require.NoError(t, err)
	}

	// Non-vacuity floor: a truncated or mis-rooted walk must not read as
	// "nothing is produced". 40 is far below the real count and only guards
	// against the list being empty.
	require.Greater(t, scanned, 40,
		"the producer scan read suspiciously few Go files; the walk is probably mis-rooted")
	require.NotEmpty(t, found,
		"no event producer found anywhere — the scan is broken, not the codebase")

	for _, ident := range declared {
		want := produced[ident]
		got := found[ident] > 0
		if want && !got {
			t.Errorf("%s is recorded as produced but no non-test producer outside internal/chatops references it; "+
				"either restore the producer or move it to the unproduced half of this map and record the fact", ident)
		}
		if !want && got {
			t.Errorf("%s is recorded as unproduced but %d producer reference(s) now exist; "+
				"move it to the produced half and delete the stale 'unproduced' prose in CHANGELOG/roadmap", ident, found[ident])
		}
	}
}

// declaredEventTypeIdentifiers parses bot.go for the `Event<Name> EventType = "..."`
// declarations and returns the identifiers (left-hand sides) in source order.
// Reading the declaration site keeps the list from becoming a second hand-copy
// that drifts; if the parse finds nothing the test fails rather than scanning
// for an empty set (which would be vacuously green).
func declaredEventTypeIdentifiers(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "internal", "chatops", "bot.go"))
	require.NoError(t, err, "the declaration site bot.go must be readable")

	re := regexp.MustCompile(`^\s*(Event[A-Za-z0-9]+)\s+EventType\s*=\s*"`)
	var idents []string
	for _, line := range strings.Split(string(src), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			idents = append(idents, m[1])
		}
	}
	require.NotEmpty(t, idents, "no EventType constants parsed from bot.go — the regex or the declaration shape changed")
	return idents
}

// repoRoot walks up from the test's working directory until it finds the
// directory that holds both internal/ and cmd/ (the module root).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		in, ierr := os.Stat(filepath.Join(dir, "internal"))
		cm, cerr := os.Stat(filepath.Join(dir, "cmd"))
		if ierr == nil && in.IsDir() && cerr == nil && cm.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("cannot locate the module root (a directory holding internal/ and cmd/) from %s", dir)
	return ""
}
