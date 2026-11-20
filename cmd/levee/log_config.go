package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/log"
)

// loadConfig loads the process configuration and applies the log settings it
// carries.
//
// Every command that needs the configuration goes through here, so `log.*`
// means the same thing in every command: the logger is configured once, from
// the same file that carries the rest of the configuration, instead of being a
// knob that only some entry points honour. Before this wrapper existed the
// five log fields were validated at load time and then dropped — the process
// always ran on internal/log's built-in default (info / text / stderr), so
// `log.level: debug` changed nothing at all.
//
// The guard in cmd/levee/log_config_wiring_test.go fails if a command reaches
// for config.Load directly, because that is the only way to load a
// configuration without applying its log settings.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return nil, err
	}
	if err := applyLogConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// appliedLog remembers what the logger was last configured with.
//
// A command can load the configuration more than once (for example `levee
// new` loads it, then opens the store, which loads it again). Without this
// memo a file sink would be opened once per load: two append-mode handles on
// the same path, one of them never closed. The memo is keyed on the resolved
// level/format/output, so a genuinely different configuration still
// reconfigures the logger.
var appliedLog struct {
	mu     sync.Mutex
	key    string
	writer io.Writer
}

// applyLogConfig wires the resolved log settings into the process-wide
// logger.
//
// The level and format come from cfg.Log, which postProcess has already
// resolved against the legacy `server.log_*` alias: an explicitly set `log.*`
// wins, an unset `log.*` mirrors `server.*`. The output names the sink —
// "stdout", "stderr", or a file path.
//
// A file that cannot be opened is an error, not a silent fallback to stderr:
// the operator asked for their logs to land somewhere specific, and quietly
// writing them somewhere else is the same class of lie this change exists to
// remove.
func applyLogConfig(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	key := strings.Join([]string{cfg.Log.Level, cfg.Log.Format, cfg.Log.Output}, "\x00")

	appliedLog.mu.Lock()
	defer appliedLog.mu.Unlock()
	if appliedLog.key == key && appliedLog.writer != nil {
		// Already configured with exactly these settings — do not open the
		// sink a second time.
		return nil
	}
	w, err := resolveLogWriter(cfg.Log.Output)
	if err != nil {
		return err
	}
	// Publish the new sink before closing the old one: concurrent log writers
	// always have a live destination, and a failed resolve above leaves the
	// existing configuration untouched. On Windows an unclosed old file also
	// prevents its directory from being removed; on every platform it is a
	// descriptor leak when the level/format changes.
	previous := appliedLog.writer
	log.InitLoggerWithWriter(cfg.Log.Level, cfg.Log.Format, w)
	appliedLog.key = key
	appliedLog.writer = w
	closeLogWriter(previous)
	return nil
}

// closeLogWriter closes a replaced file sink. stdout/stderr belong to the
// process, not to this configuration, and must never be closed here.
func closeLogWriter(w io.Writer) {
	if w == nil || w == os.Stdout || w == os.Stderr {
		return
	}
	if closer, ok := w.(io.Closer); ok {
		_ = closer.Close()
	}
}

// resolveLogWriter maps the configured output onto the writer the logger
// writes to.
//
// "stdout" and "stderr" are the two sinks; anything else is a file path,
// opened in append mode — a process restart appends to the existing file.
// There is no rotation here: pair the file with the platform's log collector
// or logrotate. The returned file is left open for the lifetime of the
// process (the operating system closes it on exit); a caller that wants it
// closed earlier can type-assert to *os.File.
//
// The file is created if it does not exist, and its parent directory is
// created too — a log path is a destination the operator named, not a reason
// to fail the boot over a missing directory.
func resolveLogWriter(output string) (io.Writer, error) {
	switch strings.ToLower(strings.TrimSpace(output)) {
	case "", "stderr":
		return os.Stderr, nil
	case "stdout":
		return os.Stdout, nil
	}

	path := strings.TrimSpace(output)
	if path == "" {
		return os.Stderr, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create log output directory %q: %w", dir, err)
		}
	}
	// 0600: the log may carry request identifiers, host names and error
	// details; group/other read access buys nothing and gosec (G302) rightly
	// flags anything looser.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log output %q: %w", path, err)
	}
	return f, nil
}
