// sources_test.go pins the loader posture for alert-sources.yaml: missing is a
// normal state (no ingress identity), and everything the operator wrote must be
// complete — half a credential is worse than none.
package alert

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSources(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, SourcesFileName), []byte(body), 0o600))
	return dir
}

func TestLoadSources_MissingFileIsEmptyNotAnError(t *testing.T) {
	reg, err := LoadSources(t.TempDir())
	require.NoError(t, err, "an unconfigured deployment is a normal state")
	assert.False(t, reg.Configured())
	_, ok := reg.Lookup("anything")
	assert.False(t, ok)

	// An empty data dir is the same answer.
	reg, err = LoadSources("")
	require.NoError(t, err)
	assert.False(t, reg.Configured())
}

func TestLoadSources_ParsesAndTrims(t *testing.T) {
	dir := writeSources(t, `
sources:
  - name: "  prom-dev  "
    environment: " dev "
    secret: secret-dev
  - name: prom-prod
    environment: prod
    secret: secret-prod
`)
	reg, err := LoadSources(dir)
	require.NoError(t, err)
	require.True(t, reg.Configured())
	assert.Len(t, reg.Names(), 2)

	src, ok := reg.Lookup("prom-dev")
	require.True(t, ok, "the name is trimmed on load, so lookups use the trimmed spelling")
	assert.Equal(t, "dev", src.Environment)
	assert.Equal(t, "secret-dev", src.Secret)
}

func TestLoadSources_RejectsIncompleteEntries(t *testing.T) {
	cases := map[string]string{
		"no name":   "sources:\n  - environment: dev\n    secret: s\n",
		"no env":    "sources:\n  - name: prom\n    secret: s\n",
		"no secret": "sources:\n  - name: prom\n    environment: dev\n",
	}
	for name, body := range cases {
		body := body
		t.Run(name, func(t *testing.T) {
			_, err := LoadSources(writeSources(t, body))
			require.Error(t, err, "an incomplete credential must stop the load, not be dropped")
		})
	}
}

func TestLoadSources_RejectsDuplicates(t *testing.T) {
	dir := writeSources(t, `
sources:
  - name: prom
    environment: dev
    secret: a
  - name: prom
    environment: prod
    secret: b
`)
	_, err := LoadSources(dir)
	require.Error(t, err, "the same source name declared twice cannot be resolved to one credential")
	assert.Contains(t, err.Error(), "declared twice")
}

func TestLoadSources_MalformedIsAnError(t *testing.T) {
	_, err := LoadSources(writeSources(t, "sources: [this is not a list of maps\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), SourcesFileName)
}
