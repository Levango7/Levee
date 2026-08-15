// Package engine implements LEVEE's runtime engine core, including the
// five-tier failure-semantics model (design doc section 4.4.9). It classifies
// failures into one of five categories — Retryable, ManualRetry, Rollback,
// Escalate, Fatal — and translates each category into a concrete FailureAction
// that drives the closed-loop control flow (auto-retry, manual retry,
// rollback, escalation, emergency stop).
//
// The five tiers and their default control-flow decisions are:
//
//	Retryable   — transient failure (network jitter, timeout); auto-retry
//	              with exponential back-off, up to MaxRetries (default 3).
//	ManualRetry — non-transient but recoverable (permission denied,
//	              configuration error); pause run, notify human, wait for
//	              explicit retry.
//	Rollback    — failure requires rollback (step succeeded but verification
//	              gate failed, data inconsistency); trigger rollback flow,
//	              notify operations.
//	Escalate    — failure requires human intervention (partial rollback,
//	              lock conflict); pause run, escalate to higher authority,
//	              notify operations + development.
//	Fatal       — unrecoverable (data corruption, disk full); abort run
//	              permanently, notify operations + development + management.
package engine

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/errors"
)

// --- FailureCategory --------------------------------------------------------

// FailureCategory is the five-tier failure-semantics type. It mirrors
// errors.Severity but lives in the engine package so that the engine can
// extend it with action semantics without coupling the errors package to
// control-flow concerns.
type FailureCategory int

const (
	// CategoryRetryable indicates a transient failure that the engine may
	// automatically retry with exponential back-off. Example: network jitter,
	// SSH connection timed out.
	CategoryRetryable FailureCategory = iota

	// CategoryManualRetry indicates a non-transient but recoverable failure.
	// The engine must not auto-retry; it pauses the run and waits for a human
	// to issue an explicit retry. Example: permission denied, configuration
	// error.
	CategoryManualRetry

	// CategoryRollback indicates a failure that requires rolling back
	// already-applied changes. Example: step executed successfully but
	// verification gate failed, data inconsistency detected.
	CategoryRollback

	// CategoryEscalate indicates a failure that requires human intervention
	// before any further automatic action. Example: partial rollback, lock
	// conflict.
	CategoryEscalate

	// CategoryFatal indicates an unrecoverable failure. The engine must stop
	// all automatic actions and raise an emergency alert. Example: data
	// corruption, disk full.
	CategoryFatal
)

// String returns the lower-case identifier of the category, matching the
// vocabulary used in the design document (e.g. "retryable", "fatal").
func (c FailureCategory) String() string {
	switch c {
	case CategoryRetryable:
		return "retryable"
	case CategoryManualRetry:
		return "manual_retry"
	case CategoryRollback:
		return "rollback"
	case CategoryEscalate:
		return "escalate"
	case CategoryFatal:
		return "fatal"
	default:
		return fmt.Sprintf("unknown(%d)", int(c))
	}
}

// severityToCategory converts an errors.Severity to a FailureCategory.
// Unknown severity values map to CategoryFatal (conservative default).
func severityToCategory(s errors.Severity) FailureCategory {
	switch s {
	case errors.Retryable:
		return CategoryRetryable
	case errors.ManualRetry:
		return CategoryManualRetry
	case errors.Rollback:
		return CategoryRollback
	case errors.Escalate:
		return CategoryEscalate
	case errors.Fatal:
		return CategoryFatal
	default:
		return CategoryFatal
	}
}

// --- Failure ---------------------------------------------------------------

// Failure represents a classified failure ready for handling. It bundles the
// original error, its derived category, the stable error code (when available)
// and the number of attempts made so far (used by the handler to compute
// back-off delays and enforce the MaxRetries cap).
type Failure struct {
	// Err is the original error that triggered the failure. It may be nil
	// only when the Failure is constructed synthetically in tests.
	Err error

	// Category is the classified failure-semantics tier.
	Category FailureCategory

	// Code is the stable LEVEE error code (LE001-LE096) when Err is a
	// *errors.LEVEEError, or empty otherwise.
	Code string

	// Attempts is the number of retries already performed for this failure.
	// The handler uses it to compute the next back-off delay and to enforce
	// the MaxRetries cap. Zero means this is the first attempt.
	Attempts int
}

// --- FailureClassifier -----------------------------------------------------

// FailureClassifier maps errors to FailureCategory values. It maintains a
// code -> category override table that takes precedence over the severity
// carried by a *errors.LEVEEError, allowing operators to re-classify a code
// without changing the errors package.
//
// A zero-value FailureClassifier is not usable; create one with
// NewFailureClassifier.
type FailureClassifier struct {
	mu       sync.RWMutex
	rules    map[string]FailureCategory
	fallback FailureCategory
}

// NewFailureClassifier creates a classifier populated with the default
// code -> category mappings for the LE001-LE096 catalogue. The default
// fallback for non-LEVEE errors is CategoryFatal (conservative).
func NewFailureClassifier() *FailureClassifier {
	c := &FailureClassifier{
		rules:    make(map[string]FailureCategory),
		fallback: CategoryFatal,
	}
	c.registerDefaults()
	return c
}

// registerDefaults installs the default code -> category mappings. The
// mappings follow the semantics documented in the errors package:
//
//   - Compile-time structural / type / label / window / batch / gate /
//     reference / dependency errors are Fatal at runtime: they indicate a
//     malformed plan that should not be retried.
//   - Action errors (LE041-LE042) are ManualRetry: the action module or
//     parameters are wrong and a human must fix the invocation.
//   - Approval errors (LE043-LE044) are ManualRetry: the approval
//     configuration must be corrected by a human.
//   - Rollback errors (LE081-LE083) are Escalate: rollback itself failed
//     and human intervention is required.
func (c *FailureClassifier) registerDefaults() {
	// Compile-time errors -> Fatal at runtime.
	fatalCodes := []string{
		errors.LE001, errors.LE002, errors.LE003,
		errors.LE010, errors.LE011, errors.LE012,
		errors.LE020, errors.LE021,
		errors.LE031, errors.LE032, errors.LE033, errors.LE034,
		errors.LE051, errors.LE052,
		errors.LE061,
		errors.LE071,
		errors.LE091, errors.LE092, errors.LE093, errors.LE094, errors.LE095, errors.LE096,
	}
	for _, code := range fatalCodes {
		c.rules[code] = CategoryFatal
	}

	// Action / approval errors -> ManualRetry.
	manualRetryCodes := []string{
		errors.LE041, errors.LE042,
		errors.LE043, errors.LE044,
	}
	for _, code := range manualRetryCodes {
		c.rules[code] = CategoryManualRetry
	}

	// Rollback errors -> Escalate.
	escalateCodes := []string{
		errors.LE081, errors.LE082, errors.LE083,
	}
	for _, code := range escalateCodes {
		c.rules[code] = CategoryEscalate
	}
}

// Register adds or overrides a code -> category mapping. It is safe for
// concurrent use.
func (c *FailureClassifier) Register(code string, cat FailureCategory) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rules[code] = cat
}

// SetFallback sets the category returned for errors that are not
// *errors.LEVEEError and whose code is not registered. The default is
// CategoryFatal.
func (c *FailureClassifier) SetFallback(cat FailureCategory) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fallback = cat
}

// Fallback returns the currently configured fallback category.
func (c *FailureClassifier) Fallback() FailureCategory {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fallback
}

// Classify determines the FailureCategory for the given error. A nil error
// returns CategoryRetryable (treated as a no-op success).
//
// Classification order:
//  1. If err is a *errors.LEVEEError and its Code has a registered override,
//     the override wins.
//  2. Otherwise, if err is a *errors.LEVEEError, its own Severity is mapped
//     to a FailureCategory.
//  3. Otherwise, the fallback category is returned.
func (c *FailureClassifier) Classify(err error) FailureCategory {
	if err == nil {
		return CategoryRetryable
	}

	var le *errors.LEVEEError
	if stderrors.As(err, &le) {
		c.mu.RLock()
		cat, ok := c.rules[le.Code]
		c.mu.RUnlock()
		if ok {
			return cat
		}
		return severityToCategory(le.Severity)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fallback
}

// ClassifyFailure builds a Failure from the given error, classifying it and
// extracting the stable code when available. attempts is the number of
// retries already performed (0 for the first attempt).
func (c *FailureClassifier) ClassifyFailure(err error, attempts int) Failure {
	f := Failure{Err: err, Attempts: attempts}
	if err == nil {
		f.Category = CategoryRetryable
		return f
	}
	f.Category = c.Classify(err)
	f.Code = errors.CodeOf(err)
	return f
}

// --- FailureAction ---------------------------------------------------------

// FailureAction describes what the engine should do in response to a failure.
// It is the output of FailureHandler.Handle and drives the closed-loop
// control flow.
type FailureAction struct {
	// Category is the failure-semantics tier this action corresponds to.
	Category FailureCategory

	// ShouldRetry reports whether the engine should retry the failed
	// operation. True only for CategoryRetryable.
	ShouldRetry bool

	// ShouldRollback reports whether the engine should trigger the rollback
	// flow. True for CategoryRollback.
	ShouldRollback bool

	// ShouldNotify reports whether the engine should send a notification.
	// True for all categories except Retryable (which is handled silently
	// until retries are exhausted).
	ShouldNotify bool

	// ShouldEscalate reports whether the engine should escalate the failure
	// to a higher authority. True for CategoryEscalate and CategoryFatal.
	ShouldEscalate bool

	// ShouldPause reports whether the engine should pause the run and wait
	// for external input. True for CategoryManualRetry and CategoryEscalate.
	ShouldPause bool

	// ShouldAbort reports whether the engine should terminate the run
	// permanently. True only for CategoryFatal.
	ShouldAbort bool

	// MaxRetries is the maximum number of retries the engine should attempt.
	// Non-zero only for CategoryRetryable.
	MaxRetries int

	// RetryDelay is the delay before the next retry. Computed using
	// exponential back-off: baseDelay * 2^attempts, capped at maxDelay.
	// Zero when ShouldRetry is false.
	RetryDelay time.Duration
}

// --- FailureHandler --------------------------------------------------------

// FailureHandler translates a classified Failure into a concrete
// FailureAction. It owns the retry policy (max retries, back-off parameters)
// and the notification / escalation / abort decisions for each category.
//
// A zero-value FailureHandler is not usable; create one with
// NewFailureHandler.
type FailureHandler struct {
	// MaxRetries is the maximum number of automatic retries for
	// CategoryRetryable failures. Default 3.
	MaxRetries int

	// BaseDelay is the initial back-off delay for the first retry. Default
	// 100ms.
	BaseDelay time.Duration

	// MaxDelay is the upper bound on the back-off delay. Default 10s.
	MaxDelay time.Duration
}

// NewFailureHandler creates a handler with the default retry policy:
// 3 retries, 100ms base delay, 10s max delay.
func NewFailureHandler() *FailureHandler {
	return &FailureHandler{
		MaxRetries: 3,
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   10 * time.Second,
	}
}

// Handle returns the FailureAction to take for the given failure. The
// context is accepted for future cancellation hooks but is not currently
// used to block the decision (the decision is synchronous and fast).
//
// For CategoryRetryable, the action's RetryDelay is computed using
// exponential back-off based on failure.Attempts. When failure.Attempts
// reaches MaxRetries, ShouldRetry is set to false and ShouldNotify is set
// to true so that the caller can alert that retries are exhausted.
func (h *FailureHandler) Handle(ctx context.Context, failure Failure) FailureAction {
	_ = ctx // reserved for future cancellation / deadline hooks

	switch failure.Category {
	case CategoryRetryable:
		return h.handleRetryable(failure)
	case CategoryManualRetry:
		return h.handleManualRetry(failure)
	case CategoryRollback:
		return h.handleRollback(failure)
	case CategoryEscalate:
		return h.handleEscalate(failure)
	case CategoryFatal:
		return h.handleFatal(failure)
	default:
		// Unknown category: treat as Fatal (conservative).
		return h.handleFatal(failure)
	}
}

// handleRetryable builds the action for a transient failure: retry with
// exponential back-off up to MaxRetries, no notification while retries
// remain. Once attempts reach MaxRetries, the action flips to "stop retrying
// and notify" so the caller can escalate the exhausted-retry situation.
func (h *FailureHandler) handleRetryable(f Failure) FailureAction {
	action := FailureAction{
		Category:   CategoryRetryable,
		MaxRetries: h.MaxRetries,
	}
	if f.Attempts < h.MaxRetries {
		action.ShouldRetry = true
		action.RetryDelay = h.backoff(f.Attempts)
	} else {
		// Retries exhausted: stop retrying, notify so the caller can decide
		// whether to escalate.
		action.ShouldRetry = false
		action.ShouldNotify = true
	}
	return action
}

// handleManualRetry builds the action for a non-transient recoverable
// failure: pause the run, notify for human confirmation, no auto-retry.
func (h *FailureHandler) handleManualRetry(f Failure) FailureAction {
	return FailureAction{
		Category:     CategoryManualRetry,
		ShouldNotify: true,
		ShouldPause:  true,
	}
}

// handleRollback builds the action for a failure requiring rollback:
// trigger rollback flow, notify operations. No pause (rollback is automatic).
func (h *FailureHandler) handleRollback(f Failure) FailureAction {
	return FailureAction{
		Category:       CategoryRollback,
		ShouldRollback: true,
		ShouldNotify:   true,
	}
}

// handleEscalate builds the action for a failure requiring escalation:
// pause the run, escalate to a higher authority, notify ops + dev.
func (h *FailureHandler) handleEscalate(f Failure) FailureAction {
	return FailureAction{
		Category:       CategoryEscalate,
		ShouldNotify:   true,
		ShouldEscalate: true,
		ShouldPause:    true,
	}
}

// handleFatal builds the action for an unrecoverable failure: abort the
// run permanently, escalate, notify ops + dev + management.
func (h *FailureHandler) handleFatal(f Failure) FailureAction {
	return FailureAction{
		Category:       CategoryFatal,
		ShouldNotify:   true,
		ShouldEscalate: true,
		ShouldAbort:    true,
	}
}

// backoff computes the exponential back-off delay for the given attempt
// number (0-based). The formula is baseDelay * 2^attempt, capped at maxDelay.
// A negative attempt is treated as 0. For very large attempt values the
// function returns maxDelay directly to avoid overflow.
func (h *FailureHandler) backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// Guard against overflow: for attempt > 30 the shift would overflow a
	// 32-bit int on 32-bit platforms; return maxDelay directly.
	if attempt > 30 {
		return h.MaxDelay
	}
	d := h.BaseDelay * time.Duration(1<<uint(attempt))
	if d > h.MaxDelay || d < 0 {
		return h.MaxDelay
	}
	return d
}
