package partition

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakePinger is a Pinger whose answer the test controls. delay is applied
// before the answer so the timeout case has something to interrupt.
type fakePinger struct {
	err   error
	delay time.Duration
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func TestCheck_NilCheckerIsNoOp(t *testing.T) {
	var c *Checker
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("nil checker should report reachable, got %v", err)
	}
	if !c.Available(context.Background()) {
		t.Fatal("nil checker should be available")
	}
}

func TestCheck_NilPingerIsNoOp(t *testing.T) {
	c := NewChecker(nil, 0)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("checker without a pinger should report reachable, got %v", err)
	}
}

func TestCheck_Reachable(t *testing.T) {
	c := NewChecker(fakePinger{}, 0)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("healthy pinger should report reachable, got %v", err)
	}
	if !c.Available(context.Background()) {
		t.Fatal("healthy pinger should be available")
	}
}

func TestCheck_UnreachableIsReported(t *testing.T) {
	sentinel := errors.New("connection refused")
	c := NewChecker(fakePinger{err: sentinel}, 0)
	err := c.Check(context.Background())
	if err == nil {
		t.Fatal("unreachable pinger must be reported")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("the probe's own error should survive wrapping, got %v", err)
	}
	if c.Available(context.Background()) {
		t.Fatal("unreachable pinger must not be available")
	}
}

// The timeout is the whole point of a bounded probe: a database that stops
// answering must cost a bounded wait, not an unbounded one. The fake blocks
// far longer than the budget and returns the context's own error, which is
// what a real driver does when its deadline fires.
func TestCheck_TimeoutBoundsTheProbe(t *testing.T) {
	budget := 50 * time.Millisecond
	c := NewChecker(fakePinger{delay: 10 * time.Second}, budget)

	start := time.Now()
	err := c.Check(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a pinger that never answers must be reported as unreachable")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("probe ran %v, far past its %v budget", elapsed, budget)
	}
	// The message must let an operator tell a deadline from a refusal; the
	// context's DeadlineExceeded is the marker for that.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline to surface, got %v", err)
	}
}

// A non-positive timeout must not become a no-deadline probe — the default
// keeps the construction safe rather than turning a typo into a hang.
func TestNewChecker_NonPositiveTimeoutFallsBackToDefault(t *testing.T) {
	c := NewChecker(fakePinger{}, 0)
	if c.timeout != DefaultTimeout {
		t.Fatalf("timeout = %v, want the default %v", c.timeout, DefaultTimeout)
	}
}
