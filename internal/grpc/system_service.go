// SystemService implementation for the LEVEE gRPC API.
//
// SystemService exposes system-level introspection: version information,
// daemon health status, runtime configuration, and diagnostic checks
// (doctor). It is read-only and does not mutate any state.
package grpc

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// SystemService implements pb.SystemServiceServer. It provides version,
// status, config, and doctor diagnostics. All methods are read-only.
type SystemService struct {
	pb.UnimplementedSystemServiceServer

	store     state.Store
	cfg       *config.Config
	cfgPath   string
	startTime time.Time

	// Build info injected from cmd/levee (ldflags).
	version   string
	gitCommit string
	buildDate string
	goVersion string
}

// NewSystemService returns a SystemService backed by the given store and
// config. startTime records when the daemon was launched (used for uptime).
// Build info fields are injected from cmd/levee ldflags. If any build field
// is empty, a sensible default is used.
func NewSystemService(store state.Store, cfg *config.Config, cfgPath string,
	version, gitCommit, buildDate, goVersion string, startTime time.Time,
) *SystemService {
	if version == "" {
		version = "dev"
	}
	if gitCommit == "" {
		gitCommit = "unknown"
	}
	if buildDate == "" {
		buildDate = "unknown"
	}
	if goVersion == "" {
		goVersion = runtime.Version()
	}
	if startTime.IsZero() {
		startTime = time.Now()
	}
	return &SystemService{
		store:     store,
		cfg:       cfg,
		cfgPath:   cfgPath,
		startTime: startTime,
		version:   version,
		gitCommit: gitCommit,
		buildDate: buildDate,
		goVersion: goVersion,
	}
}

// GetVersion returns build-time version information.
func (s *SystemService) GetVersion(ctx context.Context, _ *emptypb.Empty) (*pb.VersionInfo, error) {
	return &pb.VersionInfo{
		Version:   s.version,
		GitCommit: s.gitCommit,
		BuildDate: s.buildDate,
		GoVersion: s.goVersion,
	}, nil
}

// GetStatus returns a snapshot of daemon health: active/paused run counts,
// uptime, store type, and any warnings.
func (s *SystemService) GetStatus(ctx context.Context, _ *emptypb.Empty) (*pb.SystemStatus, error) {
	resp := &pb.SystemStatus{
		Status:        "healthy",
		ActiveRuns:    0,
		PausedRuns:    0,
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
		StoreType:     "sqlite",
		Warnings:      nil,
	}

	// Count active and paused runs if a store is configured.
	if s.store != nil {
		activeRuns, err := s.store.ListRuns(ctx, state.RunFilter{Status: "running", Limit: 10000})
		if err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("count active runs: %v", err))
		} else {
			resp.ActiveRuns = int32(len(activeRuns))
		}

		pausedRuns, err := s.store.ListRuns(ctx, state.RunFilter{Status: "paused", Limit: 10000})
		if err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("count paused runs: %v", err))
		} else {
			resp.PausedRuns = int32(len(pausedRuns))
		}
	} else {
		resp.Warnings = append(resp.Warnings, "store not configured")
	}

	// Determine store type from config.
	if s.cfg != nil && s.cfg.Database.Driver != "" {
		resp.StoreType = s.cfg.Database.Driver
	}

	// Derive overall status from warnings.
	if len(resp.Warnings) > 0 {
		resp.Status = "degraded"
	}

	return resp, nil
}

// GetConfig returns the current configuration, optionally redacting secret
// fields and filtering to a specific section.
func (s *SystemService) GetConfig(ctx context.Context, req *pb.GetConfigRequest) (*pb.Config, error) {
	if s.cfg == nil {
		return nil, status.Error(codes.FailedPrecondition, "config not loaded")
	}

	// Marshal config to YAML.
	content, err := yaml.Marshal(s.cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal config: %v", err)
	}

	// Apply section filter if requested.
	if req != nil && req.Section != "" {
		sectionContent, found := extractConfigSection(string(content), req.Section)
		if !found {
			return nil, status.Errorf(codes.NotFound, "config section %q not found", req.Section)
		}
		content = []byte(sectionContent)
	}

	// Redact secrets if requested.
	if req != nil && req.RedactSecrets {
		content = []byte(redactSecrets(string(content)))
	}

	return &pb.Config{
		Format:     "yaml",
		Content:    content,
		SourcePath: s.cfgPath,
		LoadedAt:   s.startTime.Unix(),
	}, nil
}

// RunDoctor executes a suite of diagnostic checks and returns a report.
// Checks include: config loadability, store reachability, data directory
// writability, and configuration completeness.
func (s *SystemService) RunDoctor(ctx context.Context, _ *emptypb.Empty) (*pb.DoctorReport, error) {
	var checks []*pb.DoctorCheck

	// Check 1: Configuration.
	checks = append(checks, s.doctorCheckConfig())

	// Check 2: Store reachability.
	checks = append(checks, s.doctorCheckStore(ctx))

	// Check 3: Data directory writable.
	checks = append(checks, s.doctorCheckDataDir())

	// Check 4: Database path configured.
	checks = append(checks, s.doctorCheckDBPath())

	// Determine overall status.
	overall := "pass"
	for _, c := range checks {
		if c.Status == "fail" {
			overall = "fail"
			break
		}
		if c.Status == "warn" && overall != "fail" {
			overall = "warn"
		}
	}

	return &pb.DoctorReport{
		Status:    overall,
		Checks:    checks,
		CheckedAt: time.Now().Unix(),
	}, nil
}

// --- Doctor sub-checks --------------------------------------------------------

// doctorCheckConfig verifies that a configuration is loaded.
func (s *SystemService) doctorCheckConfig() *pb.DoctorCheck {
	c := &pb.DoctorCheck{Name: "config"}
	if s.cfg == nil {
		c.Status = "fail"
		c.Message = "configuration not loaded"
		c.Remediation = "provide a valid config file via --config"
		return c
	}
	c.Status = "pass"
	c.Message = "configuration loaded successfully"
	return c
}

// doctorCheckStore verifies that the store is reachable and responsive.
func (s *SystemService) doctorCheckStore(ctx context.Context) *pb.DoctorCheck {
	c := &pb.DoctorCheck{Name: "store"}
	if s.store == nil {
		c.Status = "fail"
		c.Message = "store not configured"
		c.Remediation = "ensure database.path is set and accessible"
		return c
	}
	// Probe the store with a lightweight query.
	_, err := s.store.ListRuns(ctx, state.RunFilter{Limit: 1})
	if err != nil {
		c.Status = "fail"
		c.Message = fmt.Sprintf("store query failed: %v", err)
		c.Remediation = "check database file permissions and integrity"
		return c
	}
	c.Status = "pass"
	c.Message = "store reachable"
	return c
}

// doctorCheckDataDir verifies that the data directory is writable.
func (s *SystemService) doctorCheckDataDir() *pb.DoctorCheck {
	c := &pb.DoctorCheck{Name: "data_dir"}
	if s.cfg == nil || s.cfg.Server.DataDir == "" {
		c.Status = "warn"
		c.Message = "data_dir not configured"
		c.Remediation = "set server.data_dir in config"
		return c
	}
	// Check directory exists and is writable.
	info, err := os.Stat(s.cfg.Server.DataDir)
	if err != nil {
		if os.IsNotExist(err) {
			c.Status = "warn"
			c.Message = fmt.Sprintf("data_dir %q does not exist", s.cfg.Server.DataDir)
			c.Remediation = "create the directory or update server.data_dir"
			return c
		}
		c.Status = "fail"
		c.Message = fmt.Sprintf("stat data_dir: %v", err)
		return c
	}
	if !info.IsDir() {
		c.Status = "fail"
		c.Message = fmt.Sprintf("data_dir %q is not a directory", s.cfg.Server.DataDir)
		return c
	}
	// Test writability by creating a temp file.
	tmpFile := s.cfg.Server.DataDir + string(os.PathSeparator) + ".levee-doctor-probe"
	if err := os.WriteFile(tmpFile, []byte("probe"), 0o644); err != nil {
		c.Status = "fail"
		c.Message = fmt.Sprintf("data_dir not writable: %v", err)
		c.Remediation = "check directory permissions"
		return c
	}
	_ = os.Remove(tmpFile)
	c.Status = "pass"
	c.Message = fmt.Sprintf("data_dir %q writable", s.cfg.Server.DataDir)
	return c
}

// doctorCheckDBPath verifies that the database path is configured and the
// file is accessible.
func (s *SystemService) doctorCheckDBPath() *pb.DoctorCheck {
	c := &pb.DoctorCheck{Name: "database"}
	if s.cfg == nil {
		c.Status = "skip"
		c.Message = "config not loaded"
		return c
	}
	if s.cfg.Database.Path == "" {
		c.Status = "fail"
		c.Message = "database.path is empty"
		c.Remediation = "set database.path in config"
		return c
	}
	// For in-memory databases, we can't stat the file.
	if s.cfg.Database.Path == ":memory:" {
		c.Status = "pass"
		c.Message = "using in-memory database"
		return c
	}
	if _, err := os.Stat(s.cfg.Database.Path); err != nil {
		if os.IsNotExist(err) {
			c.Status = "warn"
			c.Message = fmt.Sprintf("database file %q not found (will be created on first use)", s.cfg.Database.Path)
			return c
		}
		c.Status = "fail"
		c.Message = fmt.Sprintf("stat database: %v", err)
		return c
	}
	c.Status = "pass"
	c.Message = fmt.Sprintf("database file %q accessible", s.cfg.Database.Path)
	return c
}

// --- Config helpers -----------------------------------------------------------

// extractConfigSection extracts a top-level YAML section (e.g. "server",
// "database") from a YAML string. Returns the section content and true if
// found, or empty string and false if not.
func extractConfigSection(yamlContent, section string) (string, bool) {
	lines := strings.Split(yamlContent, "\n")
	var sectionLines []string
	inSection := false
	sectionPrefix := section + ":"

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Detect the start of the requested section.
		if !inSection {
			if trimmed == sectionPrefix {
				inSection = true
				sectionLines = append(sectionLines, line)
				continue
			}
			continue
		}
		// We're inside the section. A top-level key (no leading whitespace)
		// that isn't a comment marks the end of the section.
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "#") {
			break
		}
		sectionLines = append(sectionLines, line)
	}

	if !inSection {
		return "", false
	}
	return strings.Join(sectionLines, "\n"), true
}

// redactSecrets replaces values of known secret keys with "***REDACTED***".
// It scans YAML output line-by-line for sensitive keys.
func redactSecrets(yamlContent string) string {
	secretKeys := []string{
		"password", "token", "secret", "key_path", "api_key",
		"credential", "passphrase", "private_key",
	}
	lines := strings.Split(yamlContent, "\n")
	for i, line := range lines {
		for _, sk := range secretKeys {
			if isYAMLKeyValue(line, sk) {
				lines[i] = redactYAMLValue(line)
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

// isYAMLKeyValue checks if a YAML line defines a key matching secretKey.
func isYAMLKeyValue(line, secretKey string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	// A YAML key-value line looks like "key: value" or "key:".
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) < 2 {
		return false
	}
	key := strings.TrimSpace(parts[0])
	// Match exact key or key ending with the secret keyword.
	return key == secretKey || strings.Contains(key, secretKey)
}

// redactYAMLValue replaces the value portion of a "key: value" YAML line.
func redactYAMLValue(line string) string {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return line
	}
	// Preserve indentation and key, replace value.
	indent := line[:idx]
	return indent + ": \"***REDACTED***\""
}
