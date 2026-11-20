package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/log"
)

// restoreDefaultLogger puts the process logger back on stderr after a test
// that reconfigured it. No test in this package runs in parallel, so the
// window is not shared.
func restoreDefaultLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { log.InitLoggerWithWriter("info", "text", os.Stderr) })
}

func TestResolveLogWriterSinks(t *testing.T) {
	w, err := resolveLogWriter("stdout")
	require.NoError(t, err)
	assert.Equal(t, os.Stdout, w)

	w, err = resolveLogWriter("stderr")
	require.NoError(t, err)
	assert.Equal(t, os.Stderr, w)

	w, err = resolveLogWriter("")
	require.NoError(t, err)
	assert.Equal(t, os.Stderr, w, "an unset output falls back to stderr")

	w, err = resolveLogWriter("STDOUT")
	require.NoError(t, err)
	assert.Equal(t, os.Stdout, w, "the sink names are case-insensitive")
}

func TestResolveLogWriterOpensTheFileInAppendMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "levee.log")

	w, err := resolveLogWriter(path)
	require.NoError(t, err)
	f, ok := w.(*os.File)
	require.True(t, ok, "a file output must be an *os.File the caller can close")
	_, err = f.WriteString("first\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// A second writer on the same path must append, not truncate: a restart
	// may not eat the previous run's logs.
	w2, err := resolveLogWriter(path)
	require.NoError(t, err)
	f2, ok := w2.(*os.File)
	require.True(t, ok)
	_, err = f2.WriteString("second\n")
	require.NoError(t, err)
	require.NoError(t, f2.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "first")
	assert.Contains(t, string(data), "second")
}

func TestResolveLogWriterRefusesAnUnusablePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	// The parent directory cannot be created because a regular file is in
	// the way. The operator named a destination; failing loudly is the point.
	_, err := resolveLogWriter(filepath.Join(blocker, "sub", "levee.log"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log output")
}

func TestApplyLogConfigWritesToTheConfiguredFile(t *testing.T) {
	restoreDefaultLogger(t)

	path := filepath.Join(t.TempDir(), "levee.log")
	cfg := &config.Config{}
	cfg.Log.Level = "debug"
	cfg.Log.Format = "json"
	cfg.Log.Output = path

	require.NoError(t, applyLogConfig(cfg))
	log.Debug("m-marker")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "m-marker")
	assert.Contains(t, string(data), `"level":"DEBUG"`,
		"the configured level and format must reach the logger")
}

// TestApplyLogConfigDoesNotReopenTheSameSink pins the memo: a command may
// load the configuration twice (`levee new` loads it, then opens the store,
// which loads it again). The second load must not open the same log file a
// second time — two append-mode handles on one path is a leaked descriptor
// and two writers racing for the same file.
func TestApplyLogConfigDoesNotReopenTheSameSink(t *testing.T) {
	restoreDefaultLogger(t)

	path := filepath.Join(t.TempDir(), "levee.log")
	cfg := &config.Config{}
	cfg.Log.Level = "info"
	cfg.Log.Format = "text"
	cfg.Log.Output = path

	require.NoError(t, applyLogConfig(cfg))
	first := appliedLog.writer
	require.NotNil(t, first)

	require.NoError(t, applyLogConfig(cfg))
	assert.Same(t, first, appliedLog.writer,
		"applying the same settings twice must reuse the sink")

	// A different configuration still reconfigures the logger.
	cfg.Log.Level = "debug"
	require.NoError(t, applyLogConfig(cfg))
	assert.NotSame(t, first, appliedLog.writer,
		"a different configuration must reconfigure the logger")
}

func TestApplyLogConfigFailsWhenTheOutputCannotBeOpened(t *testing.T) {
	restoreDefaultLogger(t)

	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	cfg := &config.Config{}
	cfg.Log.Output = filepath.Join(blocker, "sub", "levee.log")
	err := applyLogConfig(cfg)
	require.Error(t, err,
		"an operator who named a log file must not silently get their logs somewhere else")
	assert.Contains(t, err.Error(), "log output")
}

// TestLoadConfigAppliesTheLogSettings is the end-to-end half: a config file
// that says "debug + json + this file" must produce exactly that, through the
// same path every command takes.
func TestLoadConfigAppliesTheLogSettings(t *testing.T) {
	restoreDefaultLogger(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "levee.log")
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "server:\n  data_dir: " + filepath.ToSlash(dir) + "\n" +
		"log:\n  level: debug\n  format: json\n  output: " + filepath.ToSlash(logPath) + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))

	prev := optConfigPath
	optConfigPath = cfgPath
	t.Cleanup(func() { optConfigPath = prev })

	cfg, err := loadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	log.Debug("m-from-config")
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "m-from-config")
	assert.Contains(t, string(data), `"level":"DEBUG"`)
}
