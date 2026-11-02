// template_remote_test.go pins the second service_factory wiring: `template
// list --remote`.
//
// The claim under test is equivalence, not merely "some output appeared": both
// modes are pointed at the SAME file library, and the three output shapes must
// come out byte-identical. That is only possible because the wire now carries
// the template's id and tags — the ID column had no source remotely and `--tag`
// had nothing to filter on, which is why this command stayed local-only until
// the proto carried them.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/template"
)

// startTemplateTestServer is startTestServer with the template library wired,
// which is what `levee serve` now does (cmd_serve.go).
func startTemplateTestServer(t *testing.T, store state.Store, lib *template.TemplateLibrary) string {
	t.Helper()
	srv := grpc.NewServer(store,
		grpc.WithListenAddr(":0"),
		grpc.WithTemplateService(grpc.NewTemplateService(store, lib)),
	)
	go func() { _ = srv.Start(":0") }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			t.Cleanup(func() { _ = srv.Stop() })
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("template test server did not bind within 2 seconds")
	return ""
}

// templateTestEnv builds a config whose templates dir holds two templates, one
// of them tagged. CreatedAt is pinned to whole seconds: the wire carries int64
// seconds, so anything finer would compare a formatting difference rather than
// a wiring one.
func templateTestEnv(t *testing.T) (cfgPath string, lib *template.TemplateLibrary, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	dbPath = filepath.Join(dataDir, "levee.db")

	lib, err := template.NewTemplateLibrary(filepath.Join(dir, "templates"))
	require.NoError(t, err)

	created := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	require.NoError(t, lib.Save(ctx, &template.Template{
		Name: "b-web-rollout", Description: "rolling nginx deploy",
		Content: "name: web-rollout\nsteps: []\n", CreatedAt: created,
	}))
	require.NoError(t, lib.Save(ctx, &template.Template{
		Name: "a-db-switch", Description: "mysql primary switch",
		Content: "name: db-switch\nsteps: []\n", CreatedAt: created,
		Tags: map[string]string{"tier": "prod"},
	}))

	cfgPath = filepath.Join(dir, "config.yaml")
	cfg := "server:\n  data_dir: " + filepath.ToSlash(dataDir) +
		"\ndatabase:\n  driver: sqlite\n  path: " + filepath.ToSlash(dbPath) + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	return cfgPath, lib, dbPath
}

// TestTemplateListRemoteMatchesLocal is the equivalence proof: same library,
// two modes, identical bytes for every output shape.
func TestTemplateListRemoteMatchesLocal(t *testing.T) {
	cfgPath, lib, _ := templateTestEnv(t)
	store := newTestStore(t)
	addr := startTemplateTestServer(t, store, lib)

	for name, extra := range map[string][]string{
		"human": nil,
		"quiet": {"--quiet"},
		"json":  {"--json"},
	} {
		t.Run(name, func(t *testing.T) {
			localArgs := append(append([]string{"--config", cfgPath}, extra...), "template", "list")
			remoteArgs := append(append([]string{"--config", cfgPath, "--remote", "--server", addr}, extra...), "template", "list")

			local, _, err := runSplit(t, localArgs...)
			require.NoError(t, err)
			remote, _, err := runSplit(t, remoteArgs...)
			require.NoError(t, err)

			assert.Contains(t, local, "a-db-switch", "本地输出没有内容，等价断言就空转了")
			assert.Equal(t, strings.TrimSpace(local), strings.TrimSpace(remote),
				"%s 形状在两种模式下必须逐字节相同", name)
		})
	}
}

func TestTemplateListRemoteTagFilterMatchesLocal(t *testing.T) {
	cfgPath, lib, _ := templateTestEnv(t)
	store := newTestStore(t)
	addr := startTemplateTestServer(t, store, lib)

	local, _, err := runSplit(t, "--config", cfgPath, "template", "list", "--tag", "tier=prod")
	require.NoError(t, err)
	remote, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "template", "list", "--tag", "tier=prod")
	require.NoError(t, err)

	assert.Contains(t, local, "a-db-switch")
	assert.NotContains(t, local, "b-web-rollout", "本地过滤没生效，等价断言会变成两边都空")
	assert.Equal(t, strings.TrimSpace(local), strings.TrimSpace(remote),
		"--tag 是 tags 走线上的直接消费者：过滤结果必须一致")
}

// TestTemplateListRemoteCreatesNoLocalDatabase: asking a server for its
// templates must not touch the local data directory.
func TestTemplateListRemoteCreatesNoLocalDatabase(t *testing.T) {
	cfgPath, lib, dbPath := templateTestEnv(t)
	store := newTestStore(t)
	addr := startTemplateTestServer(t, store, lib)

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "template", "list")
	require.NoError(t, err)
	assert.NoFileExists(t, dbPath, "--remote template list 不该打开（或创建）本地库")
}

// TestServeServesTheFileLibrary pins the half the equivalence test cannot see:
// that test hands the server its own library, so it would stay green even if
// `levee serve` still built the service with a nil library — and then a real
// deployment would answer "no templates" to an operator whose templates are on
// disk. Source-reading is the same guard shape the change-constructor drift
// check uses, for the same reason: the alternative is booting the whole daemon
// for one structural fact.
func TestServeServesTheFileLibrary(t *testing.T) {
	b, err := os.ReadFile("cmd_serve.go")
	require.NoError(t, err, "reading cmd_serve.go from the package directory")
	src := string(b)

	assert.Contains(t, src, "template.NewTemplateLibrary(templateDir(cfg))",
		"serve 必须打开 CLI 写的那份模板目录，否则远端会对着有模板的部署回答「没有模板」")
	assert.Contains(t, src, "grpc.NewTemplateService(store, tmplLib)",
		"模板服务必须拿到那份 library")
	assert.NotContains(t, src, "grpc.NewTemplateService(store, nil)",
		"nil library 是「只认 RPC 建过的模板、重启即失忆」的旧形状")
}
