package errors

import (
	"fmt"
	"testing"

	stderrors "errors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Severity.String / ParseSeverity ----------------------------------------

func TestSeverity_String(t *testing.T) {
	cases := []struct {
		sev  Severity
		want string
	}{
		{Retryable, "retryable"},
		{ManualRetry, "manual_retry"},
		{Rollback, "rollback"},
		{Escalate, "escalate"},
		{Fatal, "fatal"},
		{Severity(99), "unknown(99)"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, c.sev.String())
		})
	}
}

func TestParseSeverity(t *testing.T) {
	cases := []struct {
		input string
		want  Severity
	}{
		{"retryable", Retryable},
		{"RETRYABLE", Retryable},
		{"  manual_retry  ", ManualRetry},
		{"manualretry", ManualRetry},
		{"rollback", Rollback},
		{"escalate", Escalate},
		{"fatal", Fatal},
		{"bogus", Fatal}, // unknown defaults to Fatal (conservative)
		{"", Fatal},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			assert.Equal(t, c.want, ParseSeverity(c.input))
		})
	}
}

// --- LEVEEError.Error / Unwrap / Is -----------------------------------------

func TestLEVEEError_Error_NoCause(t *testing.T) {
	e := New(LE001, "type mismatch", Retryable)
	assert.Equal(t, "LE001: retryable type mismatch", e.Error())
}

func TestLEVEEError_Error_WithCause(t *testing.T) {
	cause := fmt.Errorf("ssh: connection refused")
	e := WrapWith(cause, LE041, "action module unreachable", Retryable)
	assert.Contains(t, e.Error(), "LE041")
	assert.Contains(t, e.Error(), "retryable")
	assert.Contains(t, e.Error(), "action module unreachable")
	assert.Contains(t, e.Error(), "ssh: connection refused")
}

func TestLEVEEError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("root cause")
	e := Wrap(cause, LE002, Fatal)
	assert.Equal(t, cause, e.Unwrap())

	// errors.Unwrap should also work via the standard library.
	assert.Equal(t, cause, stderrors.Unwrap(e))
}

func TestLEVEEError_Is_ByCode(t *testing.T) {
	e1 := New(LE001, "type mismatch", Retryable)
	e2 := New(LE001, "different message", Fatal) // same code, different msg
	e3 := New(LE002, "other error", Retryable)   // different code

	// errors.Is matches by code regardless of message/severity.
	assert.True(t, stderrors.Is(e1, e2))
	assert.False(t, stderrors.Is(e1, e3))

	// Matching against a target with the same code via errors.Is.
	target := &LEVEEError{Code: LE001}
	assert.True(t, stderrors.Is(e1, target))
}

func TestLEVEEError_NilSafe(t *testing.T) {
	var e *LEVEEError
	assert.Equal(t, "<nil>", e.Error())
	assert.Nil(t, e.Unwrap())
	assert.False(t, e.Is(fmt.Errorf("x")))
}

// --- New / Wrap / WrapWith --------------------------------------------------

func TestNew(t *testing.T) {
	e := New(LE071, "DAG cycle", Rollback)
	require.NotNil(t, e)
	assert.Equal(t, LE071, e.Code)
	assert.Equal(t, "DAG cycle", e.Message)
	assert.Equal(t, Rollback, e.Severity)
	assert.Nil(t, e.Cause)
}

func TestWrap_NilError(t *testing.T) {
	assert.Nil(t, Wrap(nil, LE001, Retryable))
	assert.Nil(t, WrapWith(nil, LE001, "msg", Retryable))
}

func TestWrap_DerivesMessageFromCause(t *testing.T) {
	cause := fmt.Errorf("disk full")
	e := Wrap(cause, LE091, Fatal)
	assert.Equal(t, "disk full", e.Message)
	assert.Equal(t, cause, e.Cause)
}

func TestWrapWith_PreservesCustomMessage(t *testing.T) {
	cause := fmt.Errorf("disk full")
	e := WrapWith(cause, LE091, "rollback block missing", Fatal)
	assert.Equal(t, "rollback block missing", e.Message)
	assert.Equal(t, cause, e.Cause)
}

// --- Severity predicates ----------------------------------------------------

func TestSeverityPredicates(t *testing.T) {
	retry := New(LE041, "transient", Retryable)
	manual := New(LE041, "host down", ManualRetry)
	rollback := New(LE051, "gate failed", Rollback)
	escalate := New(LE081, "rollback failed", Escalate)
	fatal := New(LE091, "audit write failed", Fatal)

	assert.True(t, IsRetryable(retry))
	assert.False(t, IsRetryable(manual))

	assert.True(t, IsManualRetry(manual))
	assert.False(t, IsManualRetry(rollback))

	assert.True(t, IsRollback(rollback))
	assert.False(t, IsRollback(escalate))

	assert.True(t, IsEscalate(escalate))
	assert.False(t, IsEscalate(fatal))

	assert.True(t, IsFatal(fatal))
	assert.False(t, IsFatal(retry))
}

func TestSeverityPredicates_NonLEVEEError(t *testing.T) {
	plain := fmt.Errorf("plain error")
	assert.False(t, IsRetryable(plain))
	assert.False(t, IsFatal(plain))
	assert.Equal(t, Fatal, SeverityOf(plain)) // conservative default
	assert.Equal(t, "", CodeOf(plain))
}

func TestSeverityOf_And_CodeOf(t *testing.T) {
	e := New(LE031, "percentages not increasing", Rollback)
	assert.Equal(t, Rollback, SeverityOf(e))
	assert.Equal(t, LE031, CodeOf(e))
}

// --- Wrapped chain integration ---------------------------------------------

func TestWrappedChain_ErrorsAs(t *testing.T) {
	inner := fmt.Errorf("io error")
	mid := Wrap(inner, LE042, Retryable)
	outer := Wrap(mid, LE041, ManualRetry)

	// errors.As should find the LEVEEError in the chain.
	var target *LEVEEError
	assert.True(t, stderrors.As(outer, &target))
	assert.Equal(t, LE041, target.Code)

	// The inner cause is also retrievable.
	var innerTarget *LEVEEError
	assert.True(t, stderrors.As(stderrors.Unwrap(outer), &innerTarget))
	assert.Equal(t, LE042, innerTarget.Code)
}

// --- Error-code catalogue ---------------------------------------------------

func TestErrorCodes_Distinct(t *testing.T) {
	codes := []string{
		LE001, LE002, LE003,
		LE010, LE011, LE012,
		LE020, LE021,
		LE031, LE032, LE033, LE034,
		LE041, LE042,
		LE043, LE044,
		LE051, LE052,
		LE061,
		LE071,
		LE081, LE082, LE083,
		LE091, LE092, LE093, LE094, LE095, LE096,
	}
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		assert.False(t, seen[c], "duplicate error code: %s", c)
		seen[c] = true
	}
	assert.Equal(t, 29, len(codes), "expected 29 error codes")
}

func TestLookup_Registered(t *testing.T) {
	ci, ok := Lookup(LE001)
	require.True(t, ok)
	assert.Equal(t, "type", ci.Category)
	assert.Equal(t, "type mismatch", ci.Description)
	assert.Equal(t, CompileError, ci.Compile)
}

func TestLookup_WarningCode(t *testing.T) {
	ci, ok := Lookup(LE033)
	require.True(t, ok)
	assert.Equal(t, CompileWarning, ci.Compile)
	assert.Equal(t, "batch", ci.Category)
}

func TestLookup_Unknown(t *testing.T) {
	_, ok := Lookup("LE999")
	assert.False(t, ok)
}

func TestAllCodes_CountAndImmutable(t *testing.T) {
	all := AllCodes()
	assert.Equal(t, 29, len(all))

	// Mutating the returned slice must not affect the package-level catalogue.
	all[0] = CodeInfo{Code: "MUTATED"}
	again := AllCodes()
	assert.NotEqual(t, "MUTATED", again[0].Code)
}

func TestCompileSeverity_String(t *testing.T) {
	assert.Equal(t, "error", CompileError.String())
	assert.Equal(t, "warning", CompileWarning.String())
}

// --- Five-tier coverage -----------------------------------------------------

func TestAllFiveSeverities(t *testing.T) {
	// Ensure each tier produces a distinct, non-empty string and round-trips
	// through ParseSeverity.
	tiers := []Severity{Retryable, ManualRetry, Rollback, Escalate, Fatal}
	names := make(map[string]bool)
	for _, tier := range tiers {
		s := tier.String()
		assert.NotEmpty(t, s)
		assert.False(t, names[s], "duplicate tier name: %s", s)
		names[s] = true
		assert.Equal(t, tier, ParseSeverity(s))
	}
	assert.Equal(t, 5, len(names))
}
