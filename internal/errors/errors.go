// Package errors defines LEVEE's structured error model. Every error
// produced inside the engine is a *LEVEEError carrying a stable machine-readable
// code (LE001-LE096, see LEVEELang spec appendix C), a human-readable message,
// a runtime failure-severity tier (the five-tier model from design doc 4.4.9)
// and an optional wrapped cause.
//
// The five failure-severity tiers drive the closed-loop control flow:
//
//	Retryable   — transient failure (e.g. network jitter); auto-retry.
//	ManualRetry — non-transient but recoverable (e.g. host temporarily down);
//	              do not auto-retry, wait for human retry.
//	Rollback    — failure requires rollback (e.g. SLO gate failed); trigger
//	              automatic rollback.
//	Escalate    — failure requires human intervention (e.g. rollback itself
//	              failed); escalate alert, stop automatic actions.
//	Fatal       — unrecoverable (e.g. snapshot corruption, audit write failed);
//	              emergency alert, stop all automatic actions.
//
// The package also exposes the full catalogue of compile-time error codes as
// typed constants so that callers never pass magic strings.
package errors

import (
	"errors"
	"fmt"
	"strings"
)

// Severity is the runtime failure-semantics tier. It uses the five-tier model
// from design document section 4.4.9.
type Severity int

const (
	// Retryable indicates a transient failure that the engine may automatically
	// retry (default 3 attempts). Example: SSH connection timed out.
	Retryable Severity = iota

	// ManualRetry indicates a non-transient but recoverable failure. The engine
	// must not auto-retry; it waits for a human to issue an explicit retry.
	// Example: target host temporarily unreachable.
	ManualRetry

	// Rollback indicates a failure that requires rolling back already-applied
	// changes. Example: SLO verification gate failed post-batch.
	Rollback

	// Escalate indicates a failure that requires human intervention before any
	// further automatic action. Example: rollback itself failed.
	Escalate

	// Fatal indicates an unrecoverable failure. The engine must stop all
	// automatic actions and raise an emergency alert. Example: audit log write
	// failed, snapshot corrupted.
	Fatal
)

// String returns the lower-case identifier of the severity tier, matching the
// vocabulary used in the design document (e.g. "retryable", "fatal").
func (s Severity) String() string {
	switch s {
	case Retryable:
		return "retryable"
	case ManualRetry:
		return "manual_retry"
	case Rollback:
		return "rollback"
	case Escalate:
		return "escalate"
	case Fatal:
		return "fatal"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// ParseSeverity converts a string tier name to a Severity. Unknown values map
// to Fatal so that an unrecognised tier is treated as the safest (most
// conservative) option.
func ParseSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "retryable":
		return Retryable
	case "manual_retry", "manualretry":
		return ManualRetry
	case "rollback":
		return Rollback
	case "escalate":
		return Escalate
	case "fatal":
		return Fatal
	default:
		return Fatal
	}
}

// LEVEEError is the structured error type used throughout LEVEE. It carries a
// stable error code, a human-readable message, a runtime severity tier and an
// optional wrapped cause. It implements the error interface and supports
// errors.Is / errors.As / errors.Unwrap.
type LEVEEError struct {
	// Code is the stable machine-readable error code (e.g. "LE001").
	Code string

	// Message is a human-readable description of the failure.
	Message string

	// Severity is the runtime failure-semantics tier.
	Severity Severity

	// Cause is the underlying error that triggered this one, or nil when this
	// error is a root cause.
	Cause error
}

// Error returns a single-line representation suitable for logging and CLI
// output. The format is: "LE###: <severity> <message>: <cause>".
func (e *LEVEEError) Error() string {
	if e == nil {
		return "<nil>"
	}
	base := fmt.Sprintf("%s: %s %s", e.Code, e.Severity, e.Message)
	if e.Cause != nil {
		return base + ": " + e.Cause.Error()
	}
	return base
}

// Unwrap returns the wrapped cause, enabling errors.Is and errors.As to
// traverse the error chain.
func (e *LEVEEError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is reports whether this error matches the target. Two LEVEEErrors match when
// their Code fields are equal, allowing callers to use errors.Is(err,
// &errors.LEVEEError{Code: errors.LE001}) regardless of the message or cause.
func (e *LEVEEError) Is(target error) bool {
	if e == nil {
		return false
	}
	var t *LEVEEError
	if errors.As(target, &t) {
		return e.Code == t.Code
	}
	return false
}

// New creates a root LEVEEError with no wrapped cause.
func New(code, message string, severity Severity) *LEVEEError {
	return &LEVEEError{
		Code:     code,
		Message:  message,
		Severity: severity,
	}
}

// Wrap creates a LEVEEError that wraps an existing error as its cause. The
// message is derived from the cause when the provided message is empty.
func Wrap(err error, code string, severity Severity) *LEVEEError {
	if err == nil {
		return nil
	}
	msg := err.Error()
	return &LEVEEError{
		Code:     code,
		Message:  msg,
		Severity: severity,
		Cause:    err,
	}
}

// WrapWith is like Wrap but allows the caller to supply a custom message that
// describes the wrapping context, in addition to preserving the original error
// as the cause.
func WrapWith(err error, code, message string, severity Severity) *LEVEEError {
	if err == nil {
		return nil
	}
	return &LEVEEError{
		Code:     code,
		Message:  message,
		Severity: severity,
		Cause:    err,
	}
}

// --- Severity predicates ----------------------------------------------------

// IsRetryable reports whether err is a LEVEEError with Retryable severity.
func IsRetryable(err error) bool {
	var le *LEVEEError
	if errors.As(err, &le) {
		return le.Severity == Retryable
	}
	return false
}

// IsManualRetry reports whether err is a LEVEEError with ManualRetry severity.
func IsManualRetry(err error) bool {
	var le *LEVEEError
	if errors.As(err, &le) {
		return le.Severity == ManualRetry
	}
	return false
}

// IsRollback reports whether err is a LEVEEError with Rollback severity.
func IsRollback(err error) bool {
	var le *LEVEEError
	if errors.As(err, &le) {
		return le.Severity == Rollback
	}
	return false
}

// IsEscalate reports whether err is a LEVEEError with Escalate severity.
func IsEscalate(err error) bool {
	var le *LEVEEError
	if errors.As(err, &le) {
		return le.Severity == Escalate
	}
	return false
}

// IsFatal reports whether err is a LEVEEError with Fatal severity.
func IsFatal(err error) bool {
	var le *LEVEEError
	if errors.As(err, &le) {
		return le.Severity == Fatal
	}
	return false
}

// SeverityOf extracts the Severity from a LEVEEError. For non-LEVEE errors it
// returns Fatal (conservative default).
func SeverityOf(err error) Severity {
	var le *LEVEEError
	if errors.As(err, &le) {
		return le.Severity
	}
	return Fatal
}

// CodeOf extracts the error code from a LEVEEError. For non-LEVEE errors it
// returns an empty string.
func CodeOf(err error) string {
	var le *LEVEEError
	if errors.As(err, &le) {
		return le.Code
	}
	return ""
}

// --- Compile-time error codes (LE001-LE096) ---------------------------------
//
// These constants are the stable identifiers defined in LEVEELang spec
// appendix C. They are grouped by category for readability.

const (
	// Type errors (LE001-LE003)
	LE001 = "LE001" // type mismatch
	LE002 = "LE002" // required field missing or name duplicated
	LE003 = "LE003" // enum value illegal

	// Label errors (LE010-LE012)
	LE010 = "LE010" // label expression syntax error
	LE011 = "LE011" // label key name violates naming convention
	LE012 = "LE012" // asset type not in whitelist

	// Window errors (LE020-LE021)
	LE020 = "LE020" // time format illegal or start >= end
	LE021 = "LE021" // timezone not a valid IANA name

	// Batch errors (LE031-LE034)
	LE031 = "LE031" // percentage array not non-decreasing
	LE032 = "LE032" // percentage array does not end at 100
	LE033 = "LE033" // first batch exceeds 5% (canary principle) [warning]
	LE034 = "LE034" // strategy / steps type mismatch

	// Action errors (LE041-LE042)
	LE041 = "LE041" // action module or action does not exist
	LE042 = "LE042" // action parameters violate contract

	// Approval errors (LE043-LE044)
	LE043 = "LE043" // high level does not exclude initiator
	LE044 = "LE044" // approval level illegal (not one of three tiers)

	// Gate errors (LE051-LE052)
	LE051 = "LE051" // verify / gate expression syntax error
	LE052 = "LE052" // batches missing post_batch gate [warning]

	// Reference errors (LE061)
	LE061 = "LE061" // referenced step output does not exist or type mismatch

	// Dependency errors (LE071)
	LE071 = "LE071" // DAG contains a cycle

	// Rollback errors (LE081-LE083)
	LE081 = "LE081" // rollback action not in whitelist
	LE082 = "LE082" // irreversible action not in allow_irreversible
	LE083 = "LE083" // irreversible action present but approval level < high

	// Structure errors (LE091-LE096)
	LE091 = "LE091" // missing rollback block
	LE092 = "LE092" // missing target block
	LE093 = "LE093" // missing step block
	LE094 = "LE094" // missing approval block (uses default standard) [warning]
	LE095 = "LE095" // missing window block (no window constraint) [warning]
	LE096 = "LE096" // missing batches block (single batch full) [warning]
)

// CompileSeverity is the compile-time severity of an error code: either "error"
// (blocks plan) or "warning" (passes through with a notice).
type CompileSeverity int

const (
	// CompileError blocks entry into the plan phase.
	CompileError CompileSeverity = iota
	// CompileWarning passes compilation but emits a notice for the approver.
	CompileWarning
)

func (cs CompileSeverity) String() string {
	if cs == CompileWarning {
		return "warning"
	}
	return "error"
}

// CodeInfo describes a single error-code entry in the catalogue.
type CodeInfo struct {
	Code        string
	Category    string
	Description string
	Compile     CompileSeverity
}

// codeCatalogue is the authoritative lookup table for all error codes.
var codeCatalogue = []CodeInfo{
	{LE001, "type", "type mismatch", CompileError},
	{LE002, "structure", "required field missing or name duplicated", CompileError},
	{LE003, "type", "enum value illegal", CompileError},
	{LE010, "label", "label expression syntax error", CompileError},
	{LE011, "label", "label key name violates naming convention", CompileError},
	{LE012, "label", "asset type not in whitelist", CompileError},
	{LE020, "window", "time format illegal or start >= end", CompileError},
	{LE021, "window", "timezone not a valid IANA name", CompileError},
	{LE031, "batch", "percentage array not non-decreasing", CompileError},
	{LE032, "batch", "percentage array does not end at 100", CompileError},
	{LE033, "batch", "first batch exceeds 5% (canary principle)", CompileWarning},
	{LE034, "batch", "strategy / steps type mismatch", CompileError},
	{LE041, "action", "action module or action does not exist", CompileError},
	{LE042, "action", "action parameters violate contract", CompileError},
	{LE043, "approval", "high level does not exclude initiator", CompileError},
	{LE044, "approval", "approval level illegal (not one of three tiers)", CompileError},
	{LE051, "gate", "verify / gate expression syntax error", CompileError},
	{LE052, "gate", "batches missing post_batch gate", CompileWarning},
	{LE061, "reference", "referenced step output does not exist or type mismatch", CompileError},
	{LE071, "dependency", "DAG contains a cycle", CompileError},
	{LE081, "rollback", "rollback action not in whitelist", CompileError},
	{LE082, "rollback", "irreversible action not in allow_irreversible", CompileError},
	{LE083, "rollback", "irreversible action present but approval level < high", CompileError},
	{LE091, "structure", "missing rollback block", CompileError},
	{LE092, "structure", "missing target block", CompileError},
	{LE093, "structure", "missing step block", CompileError},
	{LE094, "structure", "missing approval block (uses default standard)", CompileWarning},
	{LE095, "structure", "missing window block (no window constraint)", CompileWarning},
	{LE096, "structure", "missing batches block (single batch full)", CompileWarning},
}

// Lookup returns the CodeInfo for the given code, or false if the code is not
// registered in the catalogue.
func Lookup(code string) (CodeInfo, bool) {
	for _, ci := range codeCatalogue {
		if ci.Code == code {
			return ci, true
		}
	}
	return CodeInfo{}, false
}

// AllCodes returns the full catalogue slice. The returned slice is a copy and
// may be safely modified by the caller.
func AllCodes() []CodeInfo {
	out := make([]CodeInfo, len(codeCatalogue))
	copy(out, codeCatalogue)
	return out
}
