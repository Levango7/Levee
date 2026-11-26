package config

// D-9, operator-facing half.
//
// cmd/levee/channel_config_wiring_test.go polices the CODE side: every field
// of the channel config sections is either forwarded to a transport or
// registered in knownUnwiredChannelConfig with a reason. That guard is
// textual and package-local to cmd/levee, so it cannot speak to the operator
// who is currently writing a config file.
//
// This file does. It reports the inert fields the operator actually changed,
// each with a sentence naming what governs the behaviour instead. It changes
// nothing about how the program behaves — the shipped defaults stay shipped,
// and a deployment that never mentioned these keys is never nagged.
//
// The rule is "differs from the shipped default", not "is set": after
// setDefaults every field holds a value, so "was it in the file?" is not
// answerable here without reaching back into viper. The residual false
// positive is an operator who explicitly typed the exact default, and telling
// them the field has no effect is still true in that case.

import (
	"fmt"
	"os"
	"time"
)

// shipped inert defaults, mirrored from setDefaults. Kept as constants because
// a warning that compares against a value that can drift is worse than no
// warning at all — it teaches the operator the wrong thing.
//
// key_path is the awkward one: it ships as "~/.ssh/id_rsa" and postProcess runs
// expandHome on it, so by the time a config reaches a caller it is already
// "<home>/.ssh/id_rsa". Comparing against the literal would warn on every
// deployment that never touched the key — measured, not theorised: that false
// positive is exactly how this file's first version was caught.
const (
	inertSSHKeyPathDefault    = "~/.ssh/id_rsa"
	inertSSHPoolSizeDefault   = 10
	inertWinRMPortDefault     = 5985
	inertWinRMTransportDeflt  = "negotiate"
	inertWinRMTimeoutDeflt    = 10 * time.Second
	inertWinRMPoolSizeDefault = 5
)

// expectedKeyPath resolves the shipped key_path default the same way
// postProcess does, so the comparison is apples to apples. It reports false
// when the home directory cannot be resolved, and the caller must stay silent
// in that case: a false warning on every untouched deployment is a worse
// outcome than a missed one on a deployment that changed the key.
func expectedKeyPath() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return expandHome(inertSSHKeyPathDefault, home), true
}

// InertChannelFields names the channel-config fields that are validated at
// load time but do not reach a transport today. It is the operator-facing
// mirror of knownUnwiredChannelConfig in
// cmd/levee/channel_config_wiring_test.go; the guard test and this list must
// move together, and the test in channel_inert_warnings_test.go fails when
// they drift.
func InertChannelFields() []string {
	return []string{
		"channel.ssh.key_path",
		"channel.ssh.pool_size",
		"channel.winrm.port",
		"channel.winrm.transport",
		"channel.winrm.connect_timeout",
		"channel.winrm.pool_size",
	}
}

// InertChannelWarnings returns one note per inert channel field whose value
// differs from the shipped default, i.e. per field the operator explicitly
// configured. Each note names the field as written in the config file and
// states what actually governs the behaviour, because "this does nothing"
// without that sentence just reads as a bug report.
//
// Empty means "nothing here was changed", which is the normal case and must
// stay quiet: an operator who never touched these keys gains nothing from
// learning about them.
func InertChannelWarnings(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var notes []string

	if expected, ok := expectedKeyPath(); ok && cfg.Channel.SSH.KeyPath != expected {
		notes = append(notes, fmt.Sprintf(
			"channel.ssh.key_path=%q has no effect: the private key path comes from the resolved "+
				"credential (channel.CredentialRef.KeyPath), not from this file. Remove the key or "+
				"set it on the credential.", cfg.Channel.SSH.KeyPath))
	}
	if cfg.Channel.SSH.PoolSize != inertSSHPoolSizeDefault {
		notes = append(notes, fmt.Sprintf(
			"channel.ssh.pool_size=%d has no effect: the SSH connection pool carries its own "+
				"defaults. Remove the key; pool sizing is not configurable per deployment.", cfg.Channel.SSH.PoolSize))
	}

	if cfg.Channel.WinRM.Port != inertWinRMPortDefault {
		notes = append(notes, fmt.Sprintf(
			"channel.winrm.port=%d has no effect: the WinRM factory is registered with an empty "+
				"Config, so the whole winrm section is inert and the port comes from the target or "+
				"the channel's DefaultPort.", cfg.Channel.WinRM.Port))
	}
	if cfg.Channel.WinRM.Transport != inertWinRMTransportDeflt {
		notes = append(notes, fmt.Sprintf(
			"channel.winrm.transport=%q has no effect: the whole winrm section is inert (the "+
				"factory is registered with an empty Config), so the transport is always the channel "+
				"default. The vocabulary itself is correct (negotiate|ntlm); the section just does "+
				"not reach a transport yet.", cfg.Channel.WinRM.Transport))
	}
	if cfg.Channel.WinRM.ConnectTimeout != inertWinRMTimeoutDeflt {
		notes = append(notes, fmt.Sprintf(
			"channel.winrm.connect_timeout=%s has no effect: the WinRM channel uses its own "+
				"DefaultTimeout.", cfg.Channel.WinRM.ConnectTimeout))
	}
	if cfg.Channel.WinRM.PoolSize != inertWinRMPoolSizeDefault {
		notes = append(notes, fmt.Sprintf(
			"channel.winrm.pool_size=%d has no effect: the WinRM pool carries its own defaults "+
				"and the whole winrm section is inert.", cfg.Channel.WinRM.PoolSize))
	}

	return notes
}
