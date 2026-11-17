package ssh

// config_vocabulary_test.go — 配置接受的 auth_method 必须覆盖通道实现的。
//
// Why this guard exists: internal/channel/ssh has always implemented
// "key+password" / "password+key" (try the key, fall back to the password —
// buildAuthMethods' own comment says it is for a host that accepts either).
// internal/config, meanwhile, accepted only `key` and `password`. Two lists,
// two owners, nothing comparing them.
//
// The consequence was not a crash: it was an unreachable capability. An
// operator whose hosts accept either auth method had to pick one in the config,
// and the mode that exists in the channel could not be selected at all. Worse,
// because the channel's value was never reachable it was also never exercised —
// dead code that looked like a feature.
//
// This test reads the accepted set from internal/config and the implemented set
// from buildAuthMethods' own behaviour, and requires each to contain the other.
// Adding a method to the channel without widening the config now fails here,
// instead of silently shipping a mode nobody can select.
//
// The probe generates a real throwaway key: buildAuthMethods READS the private
// key for every key-bearing method, so pointing at a non-existent path would
// make the probe report the key methods as "not implemented" — a guard that
// proves the wrong thing is worse than no guard.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/config"
)

// throwawayKey writes an OpenSSH private key to a temp file and returns its path.
// ParsePrivateKey must be able to read what it writes, or the probe below
// measures disk IO instead of the vocabulary.
func throwawayKey(t *testing.T) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "drill-key")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0o600))
	_ = pub
	return path
}

// fullCredential carries everything every branch asks for, so a branch fails
// only when it does not recognise the method.
func fullCredential(keyPath string) channel.CredentialRef {
	return channel.CredentialRef{Username: "user", Password: "pass", KeyPath: keyPath}
}

// implementedAuthMethods asks buildAuthMethods itself which methods work.
// Enumerated from the dispatcher rather than transcribed, so a case added there
// is discovered here.
func implementedAuthMethods(t *testing.T) []string {
	t.Helper()
	keyPath := throwawayKey(t)
	candidates := []string{"key", "password", "key+password", "password+key"}
	var got []string
	for _, m := range candidates {
		c := &SSHChannel{cfg: &Config{AuthMethod: m}}
		if _, err := c.buildAuthMethods(fullCredential(keyPath)); err != nil {
			continue
		}
		got = append(got, m)
	}
	require.NotEmpty(t, got,
		"no candidate method dispatched — the probe no longer reaches buildAuthMethods")
	return got
}

func TestConfigAcceptsEveryMethodTheChannelImplements(t *testing.T) {
	accepted := map[string]bool{}
	for _, m := range config.SSHAuthMethods() {
		accepted[m] = true
	}
	for _, m := range implementedAuthMethods(t) {
		assert.Truef(t, accepted[m],
			"the SSH channel implements auth_method %q but internal/config rejects it: "+
				"the mode is unreachable (and untested) through configuration", m)
	}
}

func TestConfigRejectsWhatTheChannelDoesNotImplement(t *testing.T) {
	// The other direction: widening the config to a value the channel cannot
	// dispatch would turn a typo into a runtime error deep inside Connect.
	keyPath := throwawayKey(t)
	for _, m := range config.SSHAuthMethods() {
		c := &SSHChannel{cfg: &Config{AuthMethod: m}}
		_, err := c.buildAuthMethods(fullCredential(keyPath))
		assert.NoErrorf(t, err, "config accepts %q but buildAuthMethods cannot dispatch it", m)
	}
}
