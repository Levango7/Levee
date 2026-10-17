package conversation

// Session state vocabulary.
//
// The console's chat view maps these names to Chinese labels
// (web/src/utils/session.ts). The wire value is not a constant set — SessionState
// is an int enum and REST/JSON carries whatever String() returns — so the mirror
// could go wrong in two ways: a new state nobody mapped, and the fallback spelling
// being mistaken for a state the machine can actually be in.
// internal/conversation/session_state_vocabulary_test.go used to read String()'s
// own return values out of the source on every run, alongside a hand-copied
// TypeScript list; this file is the data internal/docgen turns into
// web/src/types/vocabulary.ts, so the copy is gone and the two remaining risks are
// pinned here instead (vocabulary_ownership_test.go).
//
// The names are DERIVED from String() rather than restated: a second list of eight
// spellings would be the same mistake this file exists to remove.

// SessionStateUnknownName is what String() reports for a value outside the
// declared states. It belongs in the vocabulary the console must be able to name —
// a stored row from before a state was removed, or a corrupt value, reaches the UI
// as this string — but it is not a state the machine transitions into.
const SessionStateUnknownName = "unknown"

// sessionStateLast is the top of the declared enumeration. vocabulary_ownership_
// test.go recomputes it from the const block, so declaring a state after this one
// without widening the list is a red test rather than a name the console never
// learns.
const sessionStateLast = StateFailed

// SessionStateNames returns one name per declared state in declaration order,
// followed by the out-of-range fallback. The result is a copy and may be modified
// by the caller.
func SessionStateNames() []string {
	names := make([]string, 0, int(sessionStateLast)+2)
	for s := SessionState(0); s <= sessionStateLast; s++ {
		names = append(names, s.String())
	}
	return append(names, SessionStateUnknownName)
}
