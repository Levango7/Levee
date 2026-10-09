package credential

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleRoot walks up from this package to the module root (the directory
// holding go.mod). The guard below judges call sites across the whole
// repository, not just this package: a secret read from cmd/ or internal/grpc
// is the same finding as one read from here.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "walked to the filesystem root without finding go.mod")
		dir = parent
	}
}

// TestDirectRetrieveCallersAreAccountedFor pins SA-011's rule at the call
// sites. Reading a credential through the raw primitive is allowed in exactly
// two shapes:
//
//   - inside RetrieveInto, the one function that must own the buffer (it is
//     the API that zeroes); or
//   - at a site that transfers plaintext ownership upward on purpose, marked
//     with a NOTE(SA-011) line that says so — provider.Resolve and the KMS
//     local-fallback read, both of which hand the bytes to a caller that is
//     documented to own them.
//
// Any other direct call is a secret whose lifetime nobody owns, which is the
// finding itself. The rule used to live only in a doc comment ("new code
// should prefer RetrieveInto"), and a comment cannot fail a build.
func TestDirectRetrieveCallersAreAccountedFor(t *testing.T) {
	root := moduleRoot(t)
	var violations []string
	seen := 0

	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		owner := ""
		for i, line := range lines {
			if strings.HasPrefix(line, "func ") {
				owner = line
			}
			if !strings.Contains(line, ".Retrieve(") {
				continue
			}
			seen++
			if strings.Contains(owner, ") RetrieveInto(") {
				continue // the primitive's owner, the one place that zeroes
			}
			marked := false
			for j := i - 1; j >= 0 && j >= i-5; j-- {
				if strings.Contains(lines[j], "NOTE(SA-011)") {
					marked = true
					break
				}
			}
			if !marked {
				rel, _ := filepath.Rel(root, path)
				violations = append(violations, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	}))

	// A detector that finds nothing passes on everything: if the anchor spread
	// stops matching (renamed method, different receiver), this must be a loud
	// failure rather than an empty success.
	require.Positive(t, seen, "no `.Retrieve(` call sites matched anywhere — the guard's anchor rotted, so it is checking nothing")

	assert.Empty(t, violations,
		"each direct Retrieve call must be RetrieveInto itself or carry a NOTE(SA-011) ownership note; "+
			"otherwise use RetrieveInto, which zeroes the buffer for you")
}
