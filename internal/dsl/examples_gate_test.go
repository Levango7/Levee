package dsl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestShippedExamplesCompile is the gate that was missing when three of the
// four shipped examples could not be compiled by the very tool they document.
// An example is documentation users paste into a real change: if it does not
// compile, the vocabulary it teaches is wrong, and the drift is silent because
// no test ever opened the files.
//
// Two document kinds live under examples/ and are deliberately not workflows:
// plugin manifests (examples/plugins/**, whose shape belongs to the plugin
// SDK) and template envelopes (a top-level `workflow:` key plus `params:`,
// rendered by `levee new` before the LEVEELang compiler ever sees them).
// Everything else must parse, validate and type-check in strict mode.
func TestShippedExamplesCompile(t *testing.T) {
	const root = "../../examples"

	var checked, skipped []string
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		if isTemplateEnvelope(t, src) || strings.Contains(filepath.ToSlash(path), "/plugins/") {
			skipped = append(skipped, path)
			return nil
		}

		t.Run(filepath.ToSlash(path), func(t *testing.T) {
			wf, perr := NewParser().ParseBytes(src)
			require.NoError(t, perr, "example must parse")
			verrs := NewValidator().Validate(wf)
			assert.Empty(t, verrs, "example must validate (this is the gate that caught the "+
				"batches.strategy copy that made one-per-target unusable)")
			checker := NewTypeChecker(NewTypeRegistry(), path)
			terrs := checker.CheckWithMode(wf, ModeStrict)
			assert.Empty(t, terrs, "example must type-check in strict mode")
		})
		checked = append(checked, path)
		return nil
	}))

	assert.GreaterOrEqual(t, len(checked), 4,
		"at least the four workflow examples must be covered — a classification rule that skips "+
			"everything would make this gate vacuously green")
	t.Logf("compiled %d example(s); skipped %d non-workflow document(s): %v",
		len(checked), len(skipped), strings.Join(skipped, ", "))
}

// isTemplateEnvelope reports whether the YAML is a template (`params:` +
// `workflow:`) rather than a LEVEELang workflow document.
func isTemplateEnvelope(t *testing.T, src []byte) bool {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return false // let the parser produce the real diagnostic
	}
	_, hasWorkflow := doc["workflow"]
	_, hasParams := doc["params"]
	return hasWorkflow && hasParams
}
