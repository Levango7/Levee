// template_show_remote_test.go pins the `template show --remote` wiring.
//
// The claim under test is equivalence, not merely "something printed": both
// modes read the SAME library contents and the three output shapes must come
// out byte-identical. That required carrying the full parameter declarations
// over the wire first — pb.Template.required_params only names the required
// parameters, so a remote show could not print optional ones, their types,
// defaults or descriptions.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/template"
)

// templateShowTestEnv writes two templates straight into the file library (not
// through Save, which stamps UpdatedAt with time.Now and would make a JSON
// byte comparison depend on sub-second precision the wire cannot carry).
func templateShowTestEnv(t *testing.T) (cfgPath, templatesDir string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	templatesDir = filepath.Join(dir, "templates")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	require.NoError(t, os.MkdirAll(templatesDir, 0o755))

	created := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC)
	updated := time.Date(2026, 11, 1, 8, 45, 0, 0, time.UTC)

	web := &template.Template{
		ID:          "tmpl-web",
		Name:        "web",
		Description: "rolling web deploy",
		Content:     "name: web\nsteps: []\n",
		Parameters: []template.TemplateParam{
			{Name: "target", Description: "host to roll", Required: true, Type: "string"},
			{Name: "replicas", Description: "how many to keep", Required: false, Default: "2", Type: "int"},
		},
		Tags:      map[string]string{"tier": "prod"},
		CreatedAt: created,
		UpdatedAt: &updated,
	}
	plain := &template.Template{
		ID:          "tmpl-plain",
		Name:        "plain",
		Description: "no parameters",
		Content:     "name: plain\nsteps: []\n",
		CreatedAt:   created,
	}
	for _, tmpl := range []*template.Template{web, plain} {
		b, err := json.MarshalIndent(tmpl, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(templatesDir, tmpl.Name+".json"), b, 0o644))
	}

	cfgPath = filepath.Join(dir, "config.yaml")
	cfg := "server:\n  data_dir: " + filepath.ToSlash(dataDir) +
		"\ndatabase:\n  driver: sqlite\n  path: " + filepath.ToSlash(filepath.Join(dataDir, "levee.db")) + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	return cfgPath, templatesDir
}

// TestTemplateShowRemoteMatchesLocal is the equivalence proof across both a
// parameter-rich template and a bare one (the second is what catches a
// converter that turns nil slices/maps into empty ones and changes --json).
func TestTemplateShowRemoteMatchesLocal(t *testing.T) {
	cfgPath, templatesDir := templateShowTestEnv(t)
	lib, err := template.NewTemplateLibrary(templatesDir)
	require.NoError(t, err)
	addr := startTemplateTestServer(t, newTestStore(t), lib)

	for _, name := range []string{"web", "plain"} {
		for shape, extra := range map[string][]string{
			"human": nil,
			"quiet": {"--quiet"},
			"json":  {"--json"},
		} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				localArgs := append(append([]string{"--config", cfgPath}, extra...), "template", "show", name)
				remoteArgs := append(append([]string{"--config", cfgPath, "--remote", "--server", addr}, extra...), "template", "show", name)

				local, _, err := runSplit(t, localArgs...)
				require.NoError(t, err)
				remote, _, err := runSplit(t, remoteArgs...)
				require.NoError(t, err)

				assert.Contains(t, local, name, "本地输出没有内容，等价断言就空转了")
				assert.Equal(t, strings.TrimSpace(local), strings.TrimSpace(remote),
					"%s 的 %s 形状在两种模式下必须逐字节相同", name, shape)
			})
		}
	}

	// Control for the fixture itself: the human view must actually show the
	// optional parameter, otherwise "identical" would be identical-and-empty.
	out, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "template", "show", "web")
	require.NoError(t, err)
	assert.Contains(t, out, "replicas (int, optional) default=\"2\"")
	assert.Contains(t, out, "host to roll")
}

// TestTemplateShowRemoteMissingRefusesLikeLocal: a missing template must read
// the same in both modes, not as an rpc error with a NotFound prefix.
func TestTemplateShowRemoteMissingRefusesLikeLocal(t *testing.T) {
	cfgPath, templatesDir := templateShowTestEnv(t)
	lib, err := template.NewTemplateLibrary(templatesDir)
	require.NoError(t, err)
	addr := startTemplateTestServer(t, newTestStore(t), lib)

	_, _, localErr := runSplit(t, "--config", cfgPath, "template", "show", "nope")
	require.Error(t, localErr)
	_, _, remoteErr := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "template", "show", "nope")
	require.Error(t, remoteErr)

	assert.Equal(t, localErr.Error(), remoteErr.Error(), "两种模式必须给出同一条错误")
	assert.Contains(t, remoteErr.Error(), "template: template not found")
}

// TestTemplateShowRemoteDialsTheServer: the equivalence cases read the same
// library contents, so only a dead endpoint shows which branch ran.
func TestTemplateShowRemoteDialsTheServer(t *testing.T) {
	cfgPath, _ := templateShowTestEnv(t)

	stdout, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", "127.0.0.1:1", "template", "show", "web")
	require.Error(t, err, "server 不通时静默退回本地库，就是操作者以为在问 server")
	assert.Empty(t, strings.TrimSpace(stdout))
}

// TestTemplateShowRemoteCreatesNoLocalDatabase: asking a server for a template
// must not touch the local data directory.
func TestTemplateShowRemoteCreatesNoLocalDatabase(t *testing.T) {
	_, templatesDir := templateShowTestEnv(t)
	lib, err := template.NewTemplateLibrary(templatesDir)
	require.NoError(t, err)
	addr := startTemplateTestServer(t, newTestStore(t), lib)

	cfgPath, dbPath := targetTestCfg(t)
	_, _, err = runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "template", "show", "web")
	require.NoError(t, err)
	assert.NoFileExists(t, dbPath, "--remote template show 不该打开（或创建）本地库")
}
