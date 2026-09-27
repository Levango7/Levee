// Package identity holds the registry that maps an authenticated caller to
// the team and role the permission model judges them by.
//
// It exists as a separate package for one reason: this data was previously
// defined inside package main (cmd/levee/cmd_user.go), so the CLI could read
// and write it but the serving process could not import it. Server-side
// authorisation therefore had no way to resolve "who is this caller", even
// though the answer had been sitting in <dataDir>/users.yaml the whole time.
//
// The join key. Registry.Name must equal the AUTHENTICATED subject: the
// subject a named token is bound to (grpc.TokenIdentity.Subject), the
// verified SSO session subject, or the OIDC subject. It is deliberately NOT
// the client-asserted x-actor / X-Acting-As label, which any token holder
// can set to any name — joining on that would let a caller pick which team
// it belongs to, which is exactly the failure the approval-identity work
// removed. Comparison is case-sensitive by design: folding case would make
// "Alice" and "alice" one principal with one membership while an IdP may
// treat them as two accounts.
//
// What this package does NOT do (yet): it has no reload/watch facility, so a
// Registry is a snapshot taken at load time and edits made by `levee user
// add` after the server started are invisible until restart. It also makes no
// allow/deny decision — mapping a subject onto a team is only half of
// authorisation; the other half (team × environment × action, and how that
// composes with the role tree) belongs to the permission package's checker.
package identity

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileName is the registry's file name within the LEVEE data directory.
const FileName = "users.yaml"

// User is a single registry entry. The yaml/json tags fix the on-disk shape,
// which predates this package: an existing <dataDir>/users.yaml must keep
// loading unchanged after the move.
type User struct {
	Name string `yaml:"name" json:"name"`
	Team string `yaml:"team" json:"team"`
	Role string `yaml:"role" json:"role"`
}

// Registry is the loaded user list. It is a plain data snapshot: concurrent
// readers are fine, mutation after publication is not.
type Registry struct {
	Users []User `yaml:"users"`
}

// FilePath returns <dataDir>/users.yaml.
func FilePath(dataDir string) string {
	return filepath.Join(dataDir, FileName)
}

// Load reads the registry from path. A missing file yields an empty registry
// and no error: "nobody registered yet" is a normal state, and it is the
// caller's job to decide what absence of membership data means for
// authorisation (see the posture discussion in docs/product-roadmap.md).
func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, fmt.Errorf("read user registry: %w", err)
	}
	var reg Registry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("unmarshal user registry: %w", err)
	}
	return &reg, nil
}

// LoadForDataDir loads the registry from the LEVEE data directory.
func LoadForDataDir(dataDir string) (*Registry, error) {
	return Load(FilePath(dataDir))
}

// Save writes the registry to path with 0600, creating the parent directory.
//
// The file is authorisation data — it states which team every caller
// belongs to — so it is never world-readable. 0600 applies only to creation:
// an existing file with looser permissions keeps them.
func Save(path string, reg *Registry) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create user registry dir: %w", err)
	}
	data, err := yaml.Marshal(reg)
	if err != nil {
		return fmt.Errorf("marshal user registry: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write user registry: %w", err)
	}
	return nil
}

// Lookup finds a user by name (the authenticated subject). Exact,
// case-sensitive match; the second result reports absence so callers can tell
// "not registered" apart from a registered-but-empty team.
func (r *Registry) Lookup(name string) (User, bool) {
	if r == nil {
		return User{}, false
	}
	for _, u := range r.Users {
		if u.Name == name {
			return u, true
		}
	}
	return User{}, false
}

// Names returns the registered subject names in file order. It feeds the
// startup reconciliation against configured credentials, which is what keeps a
// mis-registered operator from being silently denied (or silently allowed).
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Users))
	for _, u := range r.Users {
		out = append(out, u.Name)
	}
	return out
}
