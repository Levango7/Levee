package errors

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The catalogue and the emitters drift apart in BOTH directions, independently.
//
//   1. Registered with no emitter: LE102 was once withdrawn because nothing
//      produced it, then internal/dsl/validator.go began emitting it for a
//      fixed step count that is not positive — a code users could hit that was
//      documented nowhere and carried no severity.
//   2. Emitted but not registered: eleven codes sat in the catalogue for
//      features that were never implemented (label expressions, DAG cycles,
//      step output references, a required rollback block ...). Each one read as
//      something the compiler can emit, and none of them could.
//
// Neither direction is caught by a count, a duplicate check, or a reviewer:
// they are found by scanning. This test is that scan, wired so it runs on
// every `go test ./...` instead of whenever somebody remembers.
//
// What counts as an EMITTER: the code passed to an error constructor
// (errors.New / errors.Wrap / errors.WrapWith, or dsl's newError), or a string
// literal holding the code anywhere in production code (the dsl package
// declares local code constants like `codeEnumIllegal = "LE003"`).
//
// What does NOT count: the failure classifier's rule table
// (internal/engine/failure.go) lists codes to CLASSIFY them. Those lines carry
// no constructor call, which is the syntactic difference this test relies on —
// a bare `errors.LE061,` in a slice literal is a consumer, not an emitter.

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

var codeLiteral = regexp.MustCompile(`"(LE\d{3})"`)
var codeRef = regexp.MustCompile(`errors\.(LE\d{3})`)
var ctorCall = regexp.MustCompile(`errors\.(?:New|Wrap|WrapWith)\(`)

func stripComments(src string) string {
	// Line comments only: the repo has no /* */ blocks in these files, and a
	// code mentioned in a comment must not count as an emitter (the catalogue
	// file itself explains LE102's history in prose).
	out := make([]string, 0, strings.Count(src, "\n")+1)
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// emitterFiles returns production Go sources that may emit a code: everything
// under the repo except tests, this package (it *defines* the codes), and the
// vendor/build output directories.
func emitterFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "internal/errors/errors.go" {
			return nil // the catalogue itself names every code
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) < 20 {
		t.Fatalf("found only %d emitter files under %s — the scan is not seeing the tree (a walk bug, not a clean repo)", len(files), root)
	}
	return files
}

// emittedBy returns the files that emit code, and why.
func emittedBy(code string, files []string) []string {
	var hits []string
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // unreadable file: the walk already filtered the set
		}
		src := stripComments(string(b))
		flat := strings.Join(strings.Fields(src), " ")

		// A literal is an emitter wherever it sits: the dsl package's local
		// code constants are exactly this shape.
		if codeLiteral.MatchString(src) && strings.Contains(src, `"`+code+`"`) {
			hits = append(hits, filepath.Base(p)+" (literal)")
			continue
		}
		// A reference counts only inside a constructor call. Collapsing
		// whitespace first keeps multi-line calls readable.
		if strings.Contains(flat, "errors."+code) {
			for _, m := range ctorCall.FindAllStringIndex(flat, -1) {
				window := flat[m[0]:]
				if len(window) > 400 {
					window = window[:400]
				}
				if strings.Contains(window, "errors."+code) {
					hits = append(hits, filepath.Base(p)+" (constructor)")
					break
				}
			}
		}
	}
	return hits
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestEveryCatalogueCodeHasAProducer is direction 1: a registered code with no
// emitter is worse than no code — it reads as something the compiler can emit.
func TestEveryCatalogueCodeHasAProducer(t *testing.T) {
	files := emitterFiles(t)
	for _, ci := range AllCodes() {
		hits := emittedBy(ci.Code, files)
		if len(hits) == 0 {
			t.Errorf("code %s (%s) is in the catalogue but nothing emits it: "+
				"either implement the check that produces it, or withdraw the code "+
				"(see the withdrawn-codes note in errors.go — LE102's round trip is why)",
				ci.Code, ci.Description)
		}
	}
}

// TestEveryEmittedCodeIsCatalogue is direction 2: an emitted code that is not
// registered reaches the operator with no severity, no category and no
// documentation.
func TestEveryEmittedCodeIsCatalogue(t *testing.T) {
	files := emitterFiles(t)
	registered := map[string]bool{}
	for _, ci := range AllCodes() {
		registered[ci.Code] = true
	}
	seen := map[string][]string{}
	for _, p := range files {
		src := stripComments(readFile(t, p))
		for _, m := range codeLiteral.FindAllStringSubmatch(src, -1) {
			seen[m[1]] = append(seen[m[1]], filepath.Base(p))
		}
		flat := strings.Join(strings.Fields(src), " ")
		if !ctorCall.MatchString(flat) {
			continue
		}
		for _, m := range codeRef.FindAllStringSubmatch(flat, -1) {
			seen[m[1]] = append(seen[m[1]], filepath.Base(p))
		}
	}
	for code, where := range seen {
		if registered[code] {
			continue
		}
		t.Errorf("code %s is emitted (%s) but is not in the catalogue: "+
			"register it, or stop emitting it", code, strings.Join(uniq(where), ", "))
	}
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
