// sources.go is the ingress-identity registry for alerts.
//
// Why it exists: AlertMessage carries no team, no environment and no target a
// policy could resolve, so for a long time the only thing guarding
// AlertService.ReceiveAlert was authentication — ANY valid token could post an
// alert, and GetAlertStatus/SubscribeAlerts exposed the whole stream to every
// caller (see the open item recorded on ReceiveAlert and the exemption table in
// cmd/levee/serve_policy_wiring_test.go). That is a fact about the payload, and
// the two tempting shortcuts both lie: judging an alert in
// permission.default_env grants or refuses a monitoring feed by whoever happens
// to own the default environment, and adding an `alert` action token fails open
// on every existing matrix.
//
// The missing input is ownership: which environment does this feed speak for?
// This file answers it from configuration rather than from the request. A
// source is a machine (a Prometheus/Alertmanager, a custom webhook) and owns an
// environment; the credential proves WHICH source is speaking, and the
// registration says what its alerts belong to. A caller cannot self-declare
// either: an unregistered source is refused, and the environment comes from the
// registry, never from the message.
//
// The file lives in the data directory (not the main config, which gets pasted
// into tickets and reviews) and is optional: no file means "no ingress identity
// configured", which keeps the historical authentication-only behaviour for
// deployments that have not opted in.
package alert

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SourcesFileName is the optional alert-source registry within the LEVEE data
// directory. Missing is normal (no ingress identity configured); malformed is
// not — a registry the operator wrote and LEVEE cannot parse must stop the load
// rather than silently downgrade to "no credential required".
const SourcesFileName = "alert-sources.yaml"

// HTTP header names the REST gateway copies into gRPC metadata before it calls
// ReceiveAlert. The gRPC surface carries the same facts as metadata keys; the
// REST gateway is the only place the two spellings have to be bridged.
const (
	SourceHeaderName = "X-Levee-Alert-Source"
	TokenHeaderName  = "X-Levee-Alert-Token" // #nosec G101 -- HTTP header NAME constant, holds no secret value

	// SourceMetadataKey and TokenMetadataKey are the lowercase gRPC metadata
	// keys. gRPC metadata keys are case-insensitive on the wire but are always
	// stored lowercase, so these are the spellings every reader uses.
	SourceMetadataKey = "x-levee-alert-source"
	TokenMetadataKey  = "x-levee-alert-token" // #nosec G101 -- gRPC metadata key NAME constant, holds no secret value
)

// Source is one registered alert source: the identity it authenticates as, the
// environment its alerts belong to, and the credential it presents.
type Source struct {
	// Name is the source's registered identity, presented as the
	// x-levee-alert-source metadata/header. It is not the same thing as
	// Alert.Source, which is a free-form descriptive label ("prometheus",
	// "custom") — the identity is what the credential is checked against.
	Name string `yaml:"name"`
	// Environment is the environment this source's alerts belong to. It is the
	// ownership input the matrix could never derive from the payload.
	Environment string `yaml:"environment"`
	// Secret is the per-source credential. It is compared in constant time.
	Secret string `yaml:"secret"`
}

// SourceRegistry is an immutable snapshot of alert-sources.yaml. A nil registry
// and an empty one mean the same thing: no ingress identity configured.
type SourceRegistry struct {
	sources map[string]Source
}

// Configured reports whether any source is registered. This is the switch
// between "check a per-source credential" and the historical
// authentication-only ingress.
func (r *SourceRegistry) Configured() bool {
	return r != nil && len(r.sources) > 0
}

// Lookup returns the registered source for name, if any.
func (r *SourceRegistry) Lookup(name string) (Source, bool) {
	if r == nil {
		return Source{}, false
	}
	s, ok := r.sources[name]
	return s, ok
}

// Names returns the registered source names in no particular order, for
// startup logging.
func (r *SourceRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.sources))
	for n := range r.sources {
		names = append(names, n)
	}
	return names
}

// alertSourcesFile is the on-disk shape. A top-level `sources:` list (rather
// than a bare list) leaves room for later keys without a breaking change.
type alertSourcesFile struct {
	Sources []Source `yaml:"sources"`
}

// LoadSources reads dataDir/alert-sources.yaml. A missing file yields an empty,
// usable registry and no error (the optional-file posture above). A file that
// exists but cannot be parsed, or that declares a source without a name,
// environment or secret, is an error: half a credential is worse than none.
func LoadSources(dataDir string) (*SourceRegistry, error) {
	reg := &SourceRegistry{sources: map[string]Source{}}
	if strings.TrimSpace(dataDir) == "" {
		return reg, nil
	}
	path := filepath.Join(dataDir, SourcesFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return nil, fmt.Errorf("read alert sources: %w", err)
	}
	var f alertSourcesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", SourcesFileName, err)
	}
	for i, s := range f.Sources {
		name := strings.TrimSpace(s.Name)
		env := strings.TrimSpace(s.Environment)
		switch {
		case name == "":
			return nil, fmt.Errorf("%s: sources[%d] has no name", SourcesFileName, i)
		case env == "":
			return nil, fmt.Errorf("%s: source %q has no environment (its alerts would be unattributable)", SourcesFileName, name)
		case s.Secret == "":
			return nil, fmt.Errorf("%s: source %q has no secret", SourcesFileName, name)
		}
		if _, dup := reg.sources[name]; dup {
			return nil, fmt.Errorf("%s: source %q is declared twice", SourcesFileName, name)
		}
		reg.sources[name] = Source{Name: name, Environment: env, Secret: s.Secret}
	}
	return reg, nil
}
