// vocabulary_ownership_test.go — the approval record's status list must cover
// exactly the constants in service.go.
//
// Same reasoning as internal/state/vocabulary_ownership_test.go: docgen writes
// web/src/types/vocabulary.ts from StatusNames(), so a constant added here without
// joining that list would put a value on the wire with no name on the console side.
// The check is a scan of this package's own declarations, not a restated list —
// restating is what lets the new constant pass quietly.
package approval

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func approvalStatusConsts(t *testing.T) map[string]string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	var raw []byte
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "internal", "approval", "service.go")
		if b, rerr := os.ReadFile(p); rerr == nil {
			raw = b
			break
		}
		dir = filepath.Dir(dir)
	}
	require.NotEmpty(t, raw, "cannot locate internal/approval/service.go from the test working directory")

	out := map[string]string{}
	// Typed constants: `StatusApproved Status = "approved"`. The type name is part
	// of the pattern on purpose — untyped ones in this package alias other
	// vocabularies (Level* tiers alias dsl), and matching them here would make the
	// guard claim ownership of a set it does not own.
	re := regexp.MustCompile(`(?m)^[ \t]*(Status[A-Z]\w*)[ \t]+Status[ \t]*=[ \t]*"([^"]+)"$`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out, "no `Status… Status = \"…\"` constants parsed from service.go — the const block shape changed and this guard went blind")
	return out
}

func TestStatusListCoversEveryStatusConstant(t *testing.T) {
	declared := approvalStatusConsts(t)
	listed := StatusNames()
	require.Len(t, listed, len(declared),
		"statusOrder lists %d values while service.go declares %d — a new approval status would reach the console with no name",
		len(listed), len(declared))

	seen := map[string]bool{}
	for _, v := range listed {
		assert.Falsef(t, seen[v], "statusOrder names %q twice", v)
		seen[v] = true
	}
	for name, value := range declared {
		assert.Truef(t, seen[value], "%s (%q) is declared in service.go but missing from statusOrder", name, value)
	}
	// The set is four, and one of them is the interesting member: `expired` is
	// named here, while the prose in schema.sql / pgschema.sql / proto advertises
	// `timeout` for the same idea. Neither spelling is allowed to quietly double up.
	assert.Equal(t, 4, len(listed), "the approval-record vocabulary is four members; see the note in vocabulary.go about which one has no production caller")
	assert.NotContains(t, listed, "timeout",
		"`timeout` is the spellings the SQL comments use for this state; naming it alongside `expired` would give one idea two members and the console two tones for one fact")
}

func TestStatusNamesIsACopy(t *testing.T) {
	got := StatusNames()
	require.NotEmpty(t, got)
	got[0] = "clobbered"
	assert.NotEqual(t, "clobbered", StatusNames()[0],
		"StatusNames handed out the owner's own slice; a caller could reorder the vocabulary docgen reads")
}

// TestCheckExpiryIsTheOnlyProducerOfExpired records the asymmetry that makes this
// list more than a name bag: three of the four members are written by the decision
// path, while `expired` is produced only by CheckExpiry — and that method has no
// production caller. Pinned as a test so the gap cannot be forgotten: if a driver
// is ever wired up, this fails and the note in vocabulary.go gets updated with it.
func TestCheckExpiryIsTheOnlyProducerOfExpired(t *testing.T) {
	dir, err := os.Getwd()
	require.NoError(t, err)
	var sources []string
	for i := 0; i < 8; i++ {
		root := filepath.Join(dir, "internal")
		if st, serr := os.Stat(root); serr == nil && st.IsDir() {
			sources = append(sources, root)
			cmdRoot := filepath.Join(dir, "cmd")
			if st2, serr2 := os.Stat(cmdRoot); serr2 == nil && st2.IsDir() {
				sources = append(sources, cmdRoot)
			}
			break
		}
		dir = filepath.Dir(dir)
	}
	require.NotEmpty(t, sources, "cannot locate internal/ from the test working directory")

	callSites := 0
	writers := 0
	for _, root := range sources {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
			if werr != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, line := range strings.Split(string(b), "\n") {
				t := strings.TrimSpace(line)
				if strings.HasPrefix(t, "//") {
					continue
				}
				if strings.Contains(t, "CheckExpiry(") && path != filepath.Join(root, "approval", "service.go") {
					callSites++
				}
				if strings.Contains(t, "= StatusExpired") {
					writers++
				}
			}
			return nil
		})
	}
	assert.Equal(t, 1, writers, "StatusExpired should be assigned in exactly one place (CheckExpiry); found %d", writers)
	assert.Equal(t, 0, callSites,
		"CheckExpiry gained %d production caller(s) — the note in vocabulary.go says it has none, so an expired approval would now actually be reachable; update the comment and this assertion together", callSites)
}
