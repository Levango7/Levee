package partition

import (
	"slices"
	"strings"
	"testing"
)

// The whitelist has two directions and both need pinning: the read-only
// commands that MUST be on it (an operator at a partition site depends on
// finding `show` and `audit verify`), and the execution commands that must NOT
// be (a single entry here that produces a remote side effect would make the
// whole list unsafe to hand to an operator).
//
// The negative list matters more than the positive one: a whitelist that
// quietly gains `apply` is a safety regression, while one that quietly loses
// `alert history` is a convenience regression. Only the first can get somebody
// hurt.

func TestReadonly_PositiveFamilies(t *testing.T) {
	for _, cmd := range []string{
		"show", "list", "version",
		"audit list", "audit show", "audit verify", "audit export", "audit report",
		"cluster status",
		"system version", "system status",
		"target list", "template show", "group list", "agent list",
		"sqlreview",
	} {
		if !Readonly(cmd) {
			t.Errorf("%q must be on the Q3 read-only whitelist", cmd)
		}
	}
}

func TestReadonly_RejectsSideEffectingCommands(t *testing.T) {
	// Every entry here produces a remote or persisted side effect during a
	// partition: executing, gating execution, minting work the next run would
	// execute, or pushing outbound.
	for _, cmd := range []string{
		"apply", "plan", "retry", "retry-host", "rollback",
		"approve", "reject", "cancel", "pause", "resume", "archive",
		"new", "clone", "import",
		"push", "chatops send", "alert silence add",
		"calendar create", "calendar update", "calendar delete",
		"drift baseline set", "serve", "agent start", "plugin install",
		"", "unknown command",
	} {
		if Readonly(cmd) {
			t.Errorf("%q produces a side effect and must NOT be whitelisted", cmd)
		}
	}
}

// A whitelist that shrinks without anybody noticing is how a later agent ends
// up telling an operator to run a command that no longer answers. The floor
// makes the drop visible; raising it is part of adding an entry.
func TestReadonlyCommandCountFloor(t *testing.T) {
	const floor = 25
	got := len(ReadonlyCommands())
	if got < floor {
		t.Fatalf("whitelist has %d entries, floor is %d — entries were dropped", got, floor)
	}
}

func TestReadonlyCommands_SortedAndCopied(t *testing.T) {
	first := ReadonlyCommands()
	second := ReadonlyCommands()
	if !slices.IsSorted(first) {
		t.Error("ReadonlyCommands must return the paths sorted")
	}
	if slices.Compare(first, second) != 0 {
		t.Error("two calls must agree")
	}
	// Mutating the returned slice must not touch the table.
	first[0] = "mutated"
	if Readonly("mutated") {
		t.Error("the returned slice aliases the table; callers must not be able to edit it")
	}
}

// The reason text is what an operator reads when deciding whether to trust the
// list. An entry with no reason means somebody added a command without asking
// the side-effect question.
func TestReadonly_EveryEntryHasAReason(t *testing.T) {
	for _, cmd := range ReadonlyCommands() {
		if strings.TrimSpace(Reason(cmd)) == "" {
			t.Errorf("%q is whitelisted without a reason", cmd)
		}
	}
}
