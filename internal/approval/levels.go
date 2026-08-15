package approval

// Package approval implements the approval service for LEVEE's change
// pipeline. The levels.go file (T032) defines the three-tier approval
// vocabulary — standard, high, emergency — together with a LevelManager
// that decides which tier a workflow step requires based on its
// attributes (irreversible / emergency markers).
//
// The three tiers map to the operational requirements in the design
// document (section 4.4.6):
//
//	standard  — reversible changes, 1 approver,  24h timeout
//	high      — irreversible changes, 2 approvers, 4h timeout
//	emergency — emergency channel,   1 approver,  30min timeout
//
// The LevelManager is intentionally decoupled from the executor package:
// it consumes a local Step struct so that the approval package can be
// unit-tested in isolation and so that callers can convert from
// executor.Step or dsl.Step at the boundary.

import (
	"fmt"
	"sync"
	"time"
)

// --- Level constants --------------------------------------------------------

// The three approval tiers. These constants are exported so that callers
// can compare against stable identifiers rather than magic strings. The
// values match the LEVEELang spec (LE083) and the dsl.ApprovalSpec
// vocabulary.
const (
	// LevelStandard is the default tier for reversible operations.
	LevelStandard = "standard"

	// LevelHigh is the tier for irreversible or destructive operations.
	LevelHigh = "high"

	// LevelEmergency is the fast-track tier for emergency changes.
	LevelEmergency = "emergency"
)

// --- Escalation policy ------------------------------------------------------

// EscalationActionOnTimeout constants. They describe what the system
// does when an approval at a given level times out.
const (
	// EscalateNotify instructs the caller to notify the approvers but
	// leave the approval pending (the human must still decide).
	EscalateNotify = "notify"

	// EscalateEscalate instructs the caller to re-create the approval
	// at the level named in EscalateTo.
	EscalateEscalate = "escalate"

	// EscalateAutoReject instructs the caller to transition the
	// approval to StatusExpired / rejected automatically.
	EscalateAutoReject = "auto-reject"
)

// EscalationPolicy describes what happens when an approval at a given
// level times out. It is part of LevelConfig and is consumed by the
// expiry/escalation loop in the planner (T035).
type EscalationPolicy struct {
	// OnTimeout is one of EscalateNotify / EscalateEscalate /
	// EscalateAutoReject. It is the action to take when the approval
	// times out before a decision is reached.
	OnTimeout string `json:"on_timeout"`

	// EscalateTo is the target level when OnTimeout == EscalateEscalate.
	// It is ignored for the other actions. It must be one of the three
	// legal tiers when set.
	EscalateTo string `json:"escalate_to,omitempty"`

	// NotifyApprovers is the optional list of additional approvers to
	// notify when OnTimeout == EscalateNotify. The original approvers
	// are always notified; this list extends the notification set
	// (e.g. with the change author's manager).
	NotifyApprovers []string `json:"notify_approvers,omitempty"`
}

// --- LevelConfig ------------------------------------------------------------

// LevelConfig is the configuration for a single approval tier. A
// LevelManager holds one LevelConfig per tier.
type LevelConfig struct {
	// Level is the tier name: LevelStandard / LevelHigh / LevelEmergency.
	Level string `json:"level"`

	// TriggerCondition is a human-readable description of when this
	// tier triggers. It is purely informational (used in `levee plan
	// --show-levels` output and the audit log); the actual trigger
	// logic lives in LevelManager.DetermineLevel.
	TriggerCondition string `json:"trigger_condition"`

	// RequiredApprovers is the total number of approvers expected to
	// be configured on an approval at this level. It is the upper bound
	// for MinApprovers and is used by the planner to validate that the
	// approval spec supplies enough approvers.
	RequiredApprovers int `json:"required_approvers"`

	// MinApprovers is the minimum number of distinct "approve" decisions
	// needed to transition the approval to StatusApproved. It mirrors
	// Approval.MinApprovers and is copied into CreateRequest when the
	// planner builds an approval from a level config.
	MinApprovers int `json:"min_approvers"`

	// Timeout is the maximum wall-clock duration an approval at this
	// level may remain pending before the escalation policy kicks in.
	// A zero value means no timeout (the approval never expires).
	Timeout time.Duration `json:"timeout"`

	// EscalationPolicy is the action to take when Timeout elapses.
	EscalationPolicy EscalationPolicy `json:"escalation_policy"`
}

// --- Step -------------------------------------------------------------------

// Step is the subset of a workflow step that the LevelManager needs to
// decide the approval tier. It is a local struct rather than an alias
// for executor.Step or dsl.Step so that the approval package stays free
// of executor/dsl dependencies and can be unit-tested in isolation.
//
// Callers convert from executor.Step (or dsl.Step) at the boundary:
//
//	approval.Step{
//	    Module:       execStep.Module,
//	    Action:       execStep.Action,
//	    Target:       execStep.Target, // or dslStep.Target
//	    Irreversible: result.Irreversible, // from IrreversibleChecker
//	    Emergency:    dslStep.Emergency,
//	}
type Step struct {
	// Module is the module name, e.g. "pkg", "svc", "shell".
	Module string

	// Action is the action verb, e.g. "remove", "restart", "exec".
	Action string

	// Target is the target identifier or argument string, e.g.
	// "mysql", "redis", "iptables -F". It is used by the template
	// library (T033) to match high-risk patterns; the level manager
	// itself does not inspect it.
	Target string

	// Irreversible is the explicit author declaration or the verdict
	// of the IrreversibleChecker. When true the step is routed to the
	// high tier (unless Emergency is also true, which takes priority).
	Irreversible bool

	// Emergency is the explicit emergency-channel marker. When true
	// the step is routed to the emergency tier regardless of the
	// irreversible flag — emergency is the highest-priority tier.
	Emergency bool
}

// --- LevelManager -----------------------------------------------------------

// LevelManager manages the configuration of the three approval tiers
// and provides DetermineLevel to route a step to the right tier. It is
// safe for concurrent use: the configs map is guarded by an RWMutex so
// that Get / All / DetermineLevel (readers) can run in parallel with
// SetConfig (writer).
type LevelManager struct {
	mu      sync.RWMutex
	configs map[string]LevelConfig
}

// NewLevelManager returns a LevelManager populated with the default
// three-tier configuration:
//
//	standard  — 1 approver,  24h timeout, notify on timeout
//	high      — 2 approvers,  4h timeout, escalate to emergency on timeout
//	emergency — 1 approver, 30min timeout, auto-reject on timeout
//
// The defaults match the operational requirements in the design document
// (section 4.4.6). Callers may override individual tiers with SetConfig.
func NewLevelManager() *LevelManager {
	m := &LevelManager{configs: make(map[string]LevelConfig, 3)}
	m.configs[LevelStandard] = LevelConfig{
		Level:             LevelStandard,
		TriggerCondition:  "default tier for reversible operations",
		RequiredApprovers: 1,
		MinApprovers:      1,
		Timeout:           24 * time.Hour,
		EscalationPolicy:  EscalationPolicy{OnTimeout: EscalateNotify},
	}
	m.configs[LevelHigh] = LevelConfig{
		Level:             LevelHigh,
		TriggerCondition:  "irreversible or destructive operations (explicit mark or whitelist match)",
		RequiredApprovers: 2,
		MinApprovers:      2,
		Timeout:           4 * time.Hour,
		EscalationPolicy:  EscalationPolicy{OnTimeout: EscalateEscalate, EscalateTo: LevelEmergency},
	}
	m.configs[LevelEmergency] = LevelConfig{
		Level:             LevelEmergency,
		TriggerCondition:  "emergency change channel, fast turnaround, single approver",
		RequiredApprovers: 1,
		MinApprovers:      1,
		Timeout:           30 * time.Minute,
		EscalationPolicy:  EscalationPolicy{OnTimeout: EscalateAutoReject},
	}
	return m
}

// Get returns the LevelConfig for the given tier name. It returns an
// error wrapping ErrInvalidLevel when the name is not one of the three
// legal tiers, so callers can use errors.Is to detect the failure.
func (m *LevelManager) Get(level string) (LevelConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg, ok := m.configs[level]
	if !ok {
		return LevelConfig{}, fmt.Errorf("%w: %q (allowed: standard, high, emergency)", ErrInvalidLevel, level)
	}
	return cfg, nil
}

// All returns the three level configs in a stable order: standard, high,
// emergency. The returned slice is a copy and may be safely modified.
func (m *LevelManager) All() []LevelConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return []LevelConfig{m.configs[LevelStandard], m.configs[LevelHigh], m.configs[LevelEmergency]}
}

// SetConfig replaces the config for a tier. The Level field of cfg must
// be one of the three legal tiers; otherwise SetConfig returns an error
// wrapping ErrInvalidLevel. This is the supported way to override the
// defaults (e.g. to tighten the high-tier timeout in a regulated
// environment).
func (m *LevelManager) SetConfig(cfg LevelConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.configs[cfg.Level]; !ok {
		return fmt.Errorf("%w: %q (allowed: standard, high, emergency)", ErrInvalidLevel, cfg.Level)
	}
	m.configs[cfg.Level] = cfg
	return nil
}

// DetermineLevel decides which approval tier a step requires and
// returns the corresponding LevelConfig. The decision follows this
// priority order:
//
//  1. step.Emergency == true    -> emergency (highest priority)
//  2. step.Irreversible == true -> high
//  3. otherwise                 -> standard
//
// Emergency takes priority over irreversible because the emergency
// channel is meant for break-glass changes that must go through fast
// even when they are destructive — the operator has explicitly opted
// into the fast track.
//
// DetermineLevel never returns an error: the three tiers are always
// present in the manager (constructed by NewLevelManager), so the map
// lookup always succeeds. Callers can branch on a single field
// (cfg.Level) without a second error-handling path.
func (m *LevelManager) DetermineLevel(step Step) LevelConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	switch {
	case step.Emergency:
		return m.configs[LevelEmergency]
	case step.Irreversible:
		return m.configs[LevelHigh]
	default:
		return m.configs[LevelStandard]
	}
}

// DetermineLevelName is a convenience wrapper that returns just the
// tier name. It is useful when the caller only needs the level string
// (e.g. to set CreateRequest.Level) and not the full config.
func (m *LevelManager) DetermineLevelName(step Step) string {
	return m.DetermineLevel(step).Level
}
