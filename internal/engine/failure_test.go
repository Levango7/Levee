package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	leveeerr "github.com/nexus/levee/internal/errors"
)

// --- FailureCategory.String -------------------------------------------------

func TestFailureCategory_String(t *testing.T) {
	cases := []struct {
		cat  FailureCategory
		want string
	}{
		{CategoryRetryable, "retryable"},
		{CategoryManualRetry, "manual_retry"},
		{CategoryRollback, "rollback"},
		{CategoryEscalate, "escalate"},
		{CategoryFatal, "fatal"},
		{FailureCategory(99), "unknown(99)"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, c.cat.String())
		})
	}
}

// --- severityToCategory ----------------------------------------------------

func TestSeverityToCategory(t *testing.T) {
	cases := []struct {
		sev  leveeerr.Severity
		want FailureCategory
	}{
		{leveeerr.Retryable, CategoryRetryable},
		{leveeerr.ManualRetry, CategoryManualRetry},
		{leveeerr.Rollback, CategoryRollback},
		{leveeerr.Escalate, CategoryEscalate},
		{leveeerr.Fatal, CategoryFatal},
		{leveeerr.Severity(99), CategoryFatal}, // unknown -> Fatal
	}
	for _, c := range cases {
		t.Run(c.sev.String(), func(t *testing.T) {
			assert.Equal(t, c.want, severityToCategory(c.sev))
		})
	}
}

// --- FailureClassifier defaults --------------------------------------------

func TestNewFailureClassifier_Defaults(t *testing.T) {
	c := NewFailureClassifier()

	// Fatal codes (compile-time structural errors).
	fatalCodes := []string{
		leveeerr.LE001, leveeerr.LE002, leveeerr.LE003,
		leveeerr.LE010, leveeerr.LE011, leveeerr.LE012,
		leveeerr.LE020, leveeerr.LE021,
		leveeerr.LE031, leveeerr.LE032, leveeerr.LE033, leveeerr.LE034,
		leveeerr.LE051, leveeerr.LE052,
		leveeerr.LE061,
		leveeerr.LE071,
		leveeerr.LE091, leveeerr.LE092, leveeerr.LE093,
		leveeerr.LE094, leveeerr.LE095, leveeerr.LE096,
	}
	for _, code := range fatalCodes {
		err := leveeerr.New(code, "test", leveeerr.Fatal)
		assert.Equal(t, CategoryFatal, c.Classify(err),
			"code %s should classify as Fatal", code)
	}

	// ManualRetry codes (action / approval errors).
	manualRetryCodes := []string{
		leveeerr.LE041, leveeerr.LE042,
		leveeerr.LE043, leveeerr.LE044,
	}
	for _, code := range manualRetryCodes {
		err := leveeerr.New(code, "test", leveeerr.Fatal)
		assert.Equal(t, CategoryManualRetry, c.Classify(err),
			"code %s should classify as ManualRetry", code)
	}

	// Escalate codes (rollback errors).
	escalateCodes := []string{
		leveeerr.LE081, leveeerr.LE082, leveeerr.LE083,
	}
	for _, code := range escalateCodes {
		err := leveeerr.New(code, "test", leveeerr.Fatal)
		assert.Equal(t, CategoryEscalate, c.Classify(err),
			"code %s should classify as Escalate", code)
	}
}

func TestFailureClassifier_Fallback(t *testing.T) {
	c := NewFailureClassifier()

	// Default fallback is Fatal.
	assert.Equal(t, CategoryFatal, c.Fallback())

	// A plain non-LEVEE error uses the fallback.
	assert.Equal(t, CategoryFatal, c.Classify(errors.New("plain error")))

	// Change the fallback and verify.
	c.SetFallback(CategoryRetryable)
	assert.Equal(t, CategoryRetryable, c.Fallback())
	assert.Equal(t, CategoryRetryable, c.Classify(errors.New("plain error")))
}

func TestFailureClassifier_NilError(t *testing.T) {
	c := NewFailureClassifier()
	// A nil error is a no-op success -> Retryable.
	assert.Equal(t, CategoryRetryable, c.Classify(nil))
}

// --- FailureClassifier override -------------------------------------------

func TestFailureClassifier_RegisterOverride(t *testing.T) {
	c := NewFailureClassifier()

	// LE081 defaults to Escalate; override it to Fatal.
	err := leveeerr.New(leveeerr.LE081, "rollback not in whitelist", leveeerr.Fatal)
	require.Equal(t, CategoryEscalate, c.Classify(err))

	c.Register(leveeerr.LE081, CategoryFatal)
	assert.Equal(t, CategoryFatal, c.Classify(err),
		"registered override should take precedence over default")
}

func TestFailureClassifier_SeverityFallback(t *testing.T) {
	c := NewFailureClassifier()

	// A LEVEEError whose code is not in the override table falls back to
	// its own Severity. We use a synthetic code "LE999" that is not
	// registered.
	err := leveeerr.New("LE999", "synthetic", leveeerr.Retryable)
	assert.Equal(t, CategoryRetryable, c.Classify(err))

	err = leveeerr.New("LE999", "synthetic", leveeerr.Rollback)
	assert.Equal(t, CategoryRollback, c.Classify(err))

	err = leveeerr.New("LE999", "synthetic", leveeerr.Fatal)
	assert.Equal(t, CategoryFatal, c.Classify(err))
}

func TestFailureClassifier_WrappedError(t *testing.T) {
	c := NewFailureClassifier()

	// A wrapped LEVEEError should still be classified by its code.
	inner := leveeerr.New(leveeerr.LE041, "action missing", leveeerr.Fatal)
	wrapped := &wrappedErr{msg: "dispatch failed", cause: inner}
	assert.Equal(t, CategoryManualRetry, c.Classify(wrapped))
}

// wrappedErr is a test helper that wraps an error, exercising the
// errors.As traversal in Classify.
type wrappedErr struct {
	msg   string
	cause error
}

func (w *wrappedErr) Error() string { return w.msg + ": " + w.cause.Error() }
func (w *wrappedErr) Unwrap() error { return w.cause }

// --- ClassifyFailure -------------------------------------------------------

func TestFailureClassifier_ClassifyFailure(t *testing.T) {
	c := NewFailureClassifier()

	t.Run("nil error", func(t *testing.T) {
		f := c.ClassifyFailure(nil, 0)
		assert.Equal(t, CategoryRetryable, f.Category)
		assert.Empty(t, f.Code)
		assert.Nil(t, f.Err)
		assert.Equal(t, 0, f.Attempts)
	})

	t.Run("LEVEE error", func(t *testing.T) {
		err := leveeerr.New(leveeerr.LE041, "action missing", leveeerr.Fatal)
		f := c.ClassifyFailure(err, 2)
		assert.Equal(t, CategoryManualRetry, f.Category)
		assert.Equal(t, leveeerr.LE041, f.Code)
		assert.Equal(t, err, f.Err)
		assert.Equal(t, 2, f.Attempts)
	})

	t.Run("plain error", func(t *testing.T) {
		err := errors.New("network timeout")
		f := c.ClassifyFailure(err, 1)
		assert.Equal(t, CategoryFatal, f.Category) // default fallback
		assert.Empty(t, f.Code)
		assert.Equal(t, 1, f.Attempts)
	})
}

// --- FailureHandler.Handle: five categories --------------------------------

func TestFailureHandler_Retryable(t *testing.T) {
	h := NewFailureHandler()
	ctx := context.Background()

	t.Run("first attempt", func(t *testing.T) {
		f := Failure{Category: CategoryRetryable, Attempts: 0}
		a := h.Handle(ctx, f)
		assert.Equal(t, CategoryRetryable, a.Category)
		assert.True(t, a.ShouldRetry)
		assert.False(t, a.ShouldRollback)
		assert.False(t, a.ShouldNotify)
		assert.False(t, a.ShouldEscalate)
		assert.False(t, a.ShouldPause)
		assert.False(t, a.ShouldAbort)
		assert.Equal(t, 3, a.MaxRetries)
		// backoff(0) = baseDelay * 2^0 = 100ms.
		assert.Equal(t, 100*time.Millisecond, a.RetryDelay)
	})

	t.Run("second attempt", func(t *testing.T) {
		f := Failure{Category: CategoryRetryable, Attempts: 1}
		a := h.Handle(ctx, f)
		assert.True(t, a.ShouldRetry)
		// backoff(1) = 100ms * 2^1 = 200ms.
		assert.Equal(t, 200*time.Millisecond, a.RetryDelay)
	})

	t.Run("third attempt", func(t *testing.T) {
		f := Failure{Category: CategoryRetryable, Attempts: 2}
		a := h.Handle(ctx, f)
		assert.True(t, a.ShouldRetry)
		// backoff(2) = 100ms * 2^2 = 400ms.
		assert.Equal(t, 400*time.Millisecond, a.RetryDelay)
	})

	t.Run("retries exhausted", func(t *testing.T) {
		f := Failure{Category: CategoryRetryable, Attempts: 3}
		a := h.Handle(ctx, f)
		assert.False(t, a.ShouldRetry, "should stop retrying at MaxRetries")
		assert.True(t, a.ShouldNotify, "should notify when retries exhausted")
		assert.Equal(t, 0, int(a.RetryDelay))
	})

	t.Run("beyond max retries", func(t *testing.T) {
		f := Failure{Category: CategoryRetryable, Attempts: 10}
		a := h.Handle(ctx, f)
		assert.False(t, a.ShouldRetry)
		assert.True(t, a.ShouldNotify)
	})
}

func TestFailureHandler_ManualRetry(t *testing.T) {
	h := NewFailureHandler()
	ctx := context.Background()

	f := Failure{Category: CategoryManualRetry, Attempts: 0}
	a := h.Handle(ctx, f)

	assert.Equal(t, CategoryManualRetry, a.Category)
	assert.False(t, a.ShouldRetry, "ManualRetry must not auto-retry")
	assert.False(t, a.ShouldRollback)
	assert.True(t, a.ShouldNotify, "ManualRetry must notify for human confirmation")
	assert.False(t, a.ShouldEscalate)
	assert.True(t, a.ShouldPause, "ManualRetry must pause the run")
	assert.False(t, a.ShouldAbort)
	assert.Equal(t, 0, a.MaxRetries)
	assert.Equal(t, time.Duration(0), a.RetryDelay)
}

func TestFailureHandler_Rollback(t *testing.T) {
	h := NewFailureHandler()
	ctx := context.Background()

	f := Failure{Category: CategoryRollback, Attempts: 0}
	a := h.Handle(ctx, f)

	assert.Equal(t, CategoryRollback, a.Category)
	assert.False(t, a.ShouldRetry)
	assert.True(t, a.ShouldRollback, "Rollback must trigger rollback flow")
	assert.True(t, a.ShouldNotify, "Rollback must notify operations")
	assert.False(t, a.ShouldEscalate)
	assert.False(t, a.ShouldPause, "Rollback is automatic, no pause")
	assert.False(t, a.ShouldAbort)
}

func TestFailureHandler_Escalate(t *testing.T) {
	h := NewFailureHandler()
	ctx := context.Background()

	f := Failure{Category: CategoryEscalate, Attempts: 0}
	a := h.Handle(ctx, f)

	assert.Equal(t, CategoryEscalate, a.Category)
	assert.False(t, a.ShouldRetry)
	assert.False(t, a.ShouldRollback)
	assert.True(t, a.ShouldNotify, "Escalate must notify ops + dev")
	assert.True(t, a.ShouldEscalate, "Escalate must escalate to higher authority")
	assert.True(t, a.ShouldPause, "Escalate must pause the run")
	assert.False(t, a.ShouldAbort)
}

func TestFailureHandler_Fatal(t *testing.T) {
	h := NewFailureHandler()
	ctx := context.Background()

	f := Failure{Category: CategoryFatal, Attempts: 0}
	a := h.Handle(ctx, f)

	assert.Equal(t, CategoryFatal, a.Category)
	assert.False(t, a.ShouldRetry)
	assert.False(t, a.ShouldRollback)
	assert.True(t, a.ShouldNotify, "Fatal must notify ops + dev + management")
	assert.True(t, a.ShouldEscalate, "Fatal must escalate")
	assert.False(t, a.ShouldPause, "Fatal aborts, does not pause")
	assert.True(t, a.ShouldAbort, "Fatal must abort the run permanently")
}

func TestFailureHandler_UnknownCategory(t *testing.T) {
	h := NewFailureHandler()
	ctx := context.Background()

	// An unknown category should be treated as Fatal (conservative).
	f := Failure{Category: FailureCategory(99), Attempts: 0}
	a := h.Handle(ctx, f)

	assert.Equal(t, CategoryFatal, a.Category)
	assert.True(t, a.ShouldAbort)
	assert.True(t, a.ShouldNotify)
}

// --- backoff ---------------------------------------------------------------

func TestFailureHandler_Backoff(t *testing.T) {
	h := NewFailureHandler() // base=100ms, max=10s

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},  // 100ms * 2^0
		{1, 200 * time.Millisecond},  // 100ms * 2^1
		{2, 400 * time.Millisecond},  // 100ms * 2^2
		{3, 800 * time.Millisecond},  // 100ms * 2^3
		{4, 1600 * time.Millisecond}, // 100ms * 2^4
		{5, 3200 * time.Millisecond}, // 100ms * 2^5
		{6, 6400 * time.Millisecond}, // 100ms * 2^6
		{7, 10 * time.Second},        // 100ms * 2^7 = 12.8s -> capped at 10s
		{30, 10 * time.Second},       // still capped
		{31, 10 * time.Second},       // > 30, returns maxDelay directly
		{100, 10 * time.Second},      // very large, returns maxDelay
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			assert.Equal(t, c.want, h.backoff(c.attempt))
		})
	}
}

func TestFailureHandler_Backoff_NegativeAttempt(t *testing.T) {
	h := NewFailureHandler()
	// Negative attempt is treated as 0.
	assert.Equal(t, 100*time.Millisecond, h.backoff(-1))
	assert.Equal(t, 100*time.Millisecond, h.backoff(-100))
}

func TestFailureHandler_Backoff_CustomPolicy(t *testing.T) {
	h := &FailureHandler{
		MaxRetries: 5,
		BaseDelay:  1 * time.Second,
		MaxDelay:   30 * time.Second,
	}
	assert.Equal(t, 1*time.Second, h.backoff(0))
	assert.Equal(t, 2*time.Second, h.backoff(1))
	assert.Equal(t, 4*time.Second, h.backoff(2))
	assert.Equal(t, 8*time.Second, h.backoff(3))
	assert.Equal(t, 16*time.Second, h.backoff(4))
	assert.Equal(t, 30*time.Second, h.backoff(5)) // 32s -> capped
}

// --- End-to-end: Classify + Handle -----------------------------------------

func TestClassifyAndHandle_EndToEnd(t *testing.T) {
	c := NewFailureClassifier()
	h := NewFailureHandler()
	ctx := context.Background()

	t.Run("retryable transient error", func(t *testing.T) {
		err := leveeerr.New("LE999", "ssh timeout", leveeerr.Retryable)
		f := c.ClassifyFailure(err, 0)
		a := h.Handle(ctx, f)
		assert.True(t, a.ShouldRetry)
		assert.Equal(t, CategoryRetryable, a.Category)
	})

	t.Run("action error -> manual retry", func(t *testing.T) {
		err := leveeerr.New(leveeerr.LE041, "action missing", leveeerr.Fatal)
		f := c.ClassifyFailure(err, 0)
		a := h.Handle(ctx, f)
		assert.Equal(t, CategoryManualRetry, a.Category)
		assert.True(t, a.ShouldPause)
		assert.True(t, a.ShouldNotify)
	})

	t.Run("rollback error -> escalate", func(t *testing.T) {
		err := leveeerr.New(leveeerr.LE081, "rollback not whitelisted", leveeerr.Fatal)
		f := c.ClassifyFailure(err, 0)
		a := h.Handle(ctx, f)
		assert.Equal(t, CategoryEscalate, a.Category)
		assert.True(t, a.ShouldEscalate)
		assert.True(t, a.ShouldPause)
	})

	t.Run("structural error -> fatal", func(t *testing.T) {
		err := leveeerr.New(leveeerr.LE091, "missing rollback block", leveeerr.Fatal)
		f := c.ClassifyFailure(err, 0)
		a := h.Handle(ctx, f)
		assert.Equal(t, CategoryFatal, a.Category)
		assert.True(t, a.ShouldAbort)
	})

	t.Run("plain error -> fatal fallback", func(t *testing.T) {
		err := errors.New("disk full")
		f := c.ClassifyFailure(err, 0)
		a := h.Handle(ctx, f)
		assert.Equal(t, CategoryFatal, a.Category)
		assert.True(t, a.ShouldAbort)
	})
}

// --- Concurrency -----------------------------------------------------------

func TestFailureClassifier_ConcurrentRegister(t *testing.T) {
	c := NewFailureClassifier()
	err := leveeerr.New("LE999", "test", leveeerr.Retryable)

	// Concurrently register overrides and classify; should not race.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.Register("LE999", CategoryRetryable)
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_ = c.Classify(err)
	}
	<-done
}
