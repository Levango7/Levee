package executor

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- construction ----------------------------------------------------------

func TestNewIrreversibleCheckerEmpty(t *testing.T) {
	c := NewIrreversibleChecker()
	assert.Empty(t, c.Whitelist(), "new checker should have an empty whitelist")
}

func TestIrreversibleCheckerWhitelistSorted(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	c.RegisterWhitelist("file", "delete")
	c.RegisterWhitelist("user", "remove")
	assert.Equal(t,
		[]string{"file.delete", "pkg.remove", "user.remove"},
		c.Whitelist(),
		"Whitelist should return sorted entries")
}

func TestIrreversibleCheckerRegisterIdempotent(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	c.RegisterWhitelist("pkg", "remove")
	assert.Equal(t, []string{"pkg.remove"}, c.Whitelist(),
		"registering the same pair twice should not duplicate")
}

func TestIrreversibleCheckerUnregister(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	c.UnregisterWhitelist("pkg", "remove")
	assert.Empty(t, c.Whitelist())

	// Unregistering a missing pair is a no-op.
	c.UnregisterWhitelist("nope", "nope")
	assert.Empty(t, c.Whitelist())
}

// --- explicit declaration --------------------------------------------------

func TestCheckExplicitIrreversible(t *testing.T) {
	c := NewIrreversibleChecker()
	res := c.Check(Step{Module: "custom", Action: "wipe", Irreversible: true})
	assert.True(t, res.Irreversible)
	assert.Equal(t, "high", res.SuggestLevel)
	assert.Contains(t, res.Reason, "explicitly marked as irreversible")
	assert.Contains(t, res.Reason, "custom.wipe")
}

func TestCheckExplicitIrreversibleOverridesWhitelistAbsence(t *testing.T) {
	// A custom module not in the whitelist is still irreversible when the
	// author says so.
	c := NewIrreversibleChecker()
	res := c.Check(Step{Module: "myapp", Action: "nuke", Irreversible: true})
	assert.True(t, res.Irreversible)
	assert.Equal(t, "high", res.SuggestLevel)
}

func TestCheckExplicitIrreversibleWinsOverWhitelist(t *testing.T) {
	// Both signals set: explicit declaration should be the one cited in
	// the reason, since it takes priority.
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	res := c.Check(Step{Module: "pkg", Action: "remove", Irreversible: true})
	assert.True(t, res.Irreversible)
	assert.Equal(t, "high", res.SuggestLevel)
	assert.Contains(t, res.Reason, "explicitly marked")
	assert.NotContains(t, res.Reason, "whitelist")
}

// --- whitelist match -------------------------------------------------------

func TestCheckWhitelistMatch(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	c.RegisterWhitelist("file", "delete")
	c.RegisterWhitelist("user", "remove")

	for _, tc := range []struct {
		module, action string
	}{
		{"pkg", "remove"},
		{"file", "delete"},
		{"user", "remove"},
	} {
		res := c.Check(Step{Module: tc.module, Action: tc.action})
		assert.Truef(t, res.Irreversible, "%s.%s should be irreversible", tc.module, tc.action)
		assert.Equal(t, "high", res.SuggestLevel)
		assert.Contains(t, res.Reason, "irreversible whitelist")
		assert.Contains(t, res.Reason, tc.module+"."+tc.action)
	}
}

func TestCheckWhitelistMissDifferentAction(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	// pkg.install is not in the whitelist (only pkg.remove is).
	res := c.Check(Step{Module: "pkg", Action: "install"})
	assert.False(t, res.Irreversible)
	assert.Empty(t, res.SuggestLevel)
}

func TestCheckWhitelistMissDifferentModule(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	res := c.Check(Step{Module: "file", Action: "remove"})
	assert.False(t, res.Irreversible)
	assert.Empty(t, res.SuggestLevel)
}

// --- reversible default ----------------------------------------------------

func TestCheckReversibleDefault(t *testing.T) {
	c := NewIrreversibleChecker()
	res := c.Check(Step{Module: "shell", Action: "exec"})
	assert.False(t, res.Irreversible)
	assert.Empty(t, res.SuggestLevel, "reversible steps need no escalation")
	assert.Contains(t, res.Reason, "reversible")
	assert.Contains(t, res.Reason, "shell.exec")
}

func TestCheckReversibleWithNonDestructiveWhitelist(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	// pkg.install is not whitelisted, so it is reversible.
	res := c.Check(Step{Module: "pkg", Action: "install"})
	assert.False(t, res.Irreversible)
	assert.Empty(t, res.SuggestLevel)
}

// --- suggest level ---------------------------------------------------------

func TestSuggestLevelHighForIrreversible(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")

	// Whitelist path.
	res := c.Check(Step{Module: "pkg", Action: "remove"})
	assert.Equal(t, ApprovalLevelHigh, res.SuggestLevel)

	// Explicit path.
	res = c.Check(Step{Module: "x", Action: "y", Irreversible: true})
	assert.Equal(t, ApprovalLevelHigh, res.SuggestLevel)
}

func TestSuggestLevelEmptyForReversible(t *testing.T) {
	c := NewIrreversibleChecker()
	res := c.Check(Step{Module: "shell", Action: "exec"})
	assert.Empty(t, res.SuggestLevel)
}

// --- concurrency -----------------------------------------------------------

func TestCheckConcurrent(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterWhitelist("pkg", "remove")
	c.RegisterWhitelist("file", "delete")

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = c.Check(Step{Module: "pkg", Action: "remove"})
			_ = c.Check(Step{Module: "shell", Action: "exec"})
		}()
	}
	wg.Wait()
	// No data race detected by -race; the test just exercises the lock.
}

func TestRegisterConcurrent(t *testing.T) {
	c := NewIrreversibleChecker()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			c.RegisterWhitelist("mod", "act")
		}(i)
	}
	wg.Wait()
	assert.Equal(t, []string{"mod.act"}, c.Whitelist())
}

// --- Step struct -----------------------------------------------------------

func TestStepZeroValue(t *testing.T) {
	var s Step
	assert.Empty(t, s.Module)
	assert.Empty(t, s.Action)
	assert.False(t, s.Irreversible)
}

func TestIrreversibleResultZeroValue(t *testing.T) {
	var r IrreversibleResult
	assert.False(t, r.Irreversible)
	assert.Empty(t, r.Reason)
	assert.Empty(t, r.SuggestLevel)
}
