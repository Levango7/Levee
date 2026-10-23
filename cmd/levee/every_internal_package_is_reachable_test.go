package main

// every_internal_package_is_reachable_test.go pins a property the repo reached
// by hand and then stopped guarding: no package under internal/ is unreachable
// from the binaries.
//
// Why it needs a guard: 2026-09-29 an audit found EIGHT internal packages that
// nothing linked into ./cmd/levee — 4,691 lines of non-test code whose own
// tests passed (90.4–95.4% coverage) while no caller existed. They were worked
// down to zero that day, and "未链入包 8→0" is now recorded in the ledger as a
// finished item. Nothing checks it, so the next package that stops being linked
// — or is added and never linked — arrives silently, and the ledger would go on
// claiming zero.
//
// The check is deliberately about REACHABILITY, not about dead code inside a
// linked package. A file within cmd/levee that production never calls is a
// different question (and this repo has a decided position on it: golangci's
// `unused` runs, and test-only helpers are explicitly exempted in .golangci.yml,
// so "only tests use it" is allowed by design). `go list -deps` answers exactly
// the question this guard is for: can the compiler reach this package at all?

import (
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unlinkedByDesign names the packages that are intentionally NOT part of a
// binary, each with the reason it is not. It is an ALLOWLIST rather than a
// count, because "how many" would let a package leave while another enters.
//
// Both entries are `go run` tools: they are invoked as programs, so linking
// them into `levee` would ship their code to every deployment for nothing.
var unlinkedByDesign = map[string]string{
	"github.com/nexus/levee/internal/agentboard": "board validator, run as `go run ./internal/agentboard -dir ...` (see AGENTS.md)",
	"github.com/nexus/levee/internal/docgen":     "docs/vocabulary generator, run as `go run ./internal/docgen` by the docs CI gate",
}

// goListPackages runs `go list` and returns the package import paths.
//
// Import paths, not relative patterns (`./cmd/levee`), because go test runs with
// the package directory as the working directory — a relative pattern would
// resolve against cmd/levee and find nothing, which is how a scan quietly
// becomes an empty scan.
func goListPackages(t *testing.T, args ...string) []string {
	t.Helper()
	full := append([]string{"list"}, args...)
	out, err := exec.Command("go", full...).Output()
	require.NoError(t, err, "go %s must succeed: without it this guard cannot answer anything", strings.Join(full, " "))
	var pkgs []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			pkgs = append(pkgs, line)
		}
	}
	return pkgs
}

func TestEveryInternalPackageIsReachableFromTheBinary(t *testing.T) {
	// Non-vacuity first: an empty or truncated listing would make the difference
	// below look like "everything is linked". The floors are well under the
	// measured values (744 deps / 68 internal packages, 2026-10-24) so they do
	// not need touching when the tree grows.
	deps := goListPackages(t, "-deps", "github.com/nexus/levee/cmd/levee")
	require.Greater(t, len(deps), 400, "go list -deps returned %d packages; the listing looks truncated", len(deps))

	internal := goListPackages(t, "github.com/nexus/levee/internal/...")
	require.Greater(t, len(internal), 40, "go list ./internal/... returned %d packages; the listing looks truncated", len(internal))

	linked := make(map[string]bool, len(deps))
	for _, p := range deps {
		linked[p] = true
	}

	var unlinked []string
	for _, p := range internal {
		if !linked[p] {
			unlinked = append(unlinked, p)
		}
	}
	sort.Strings(unlinked)

	// Two directions, and both matter:
	//   - a package that is NOT in the allowlist but is unlinked → the regression
	//     this guard exists for;
	//   - an allowlisted package that IS linked now → the reason went stale, and
	//     a stale exemption is how an exception table stops meaning anything.
	var expected []string
	for p := range unlinkedByDesign {
		expected = append(expected, p)
	}
	sort.Strings(expected)

	assert.Equal(t, expected, unlinked,
		"internal packages not reachable from ./cmd/levee: got %v, want %v (an entry in unlinkedByDesign that is now linked must be deleted from it, with the reason)",
		unlinked, expected)
}
