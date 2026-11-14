package partition

import "sort"

// readonly lists the CLI command paths that produce NO remote side effect, each
// with the reason it earned its place on the list. This is the Q3 whitelist
// (docs/design-disconnect-policy.md §2.3): during a partition an operator needs
// to know which commands are still safe to run, and the answer must be curated
// rather than inferred from a name — every entry below was asked the same
// question: "does running this against a partitioned platform cause anything to
// happen on a host?"
//
// Deliberately NOT here, because each has a remote side effect:
//   - apply / plan / retry / retry-host / rollback  (execution);
//   - approve / reject / cancel / pause / resume / archive  (lifecycle writes
//     that gate execution);
//   - new / clone / import  (they mint rows the next run will execute);
//   - push / chatops send / alert silence add / calendar create|update|delete
//     / drift baseline set  (outbound or persisted state).
//
// A read command whose data source is unreachable must say so (see Checker):
// the whitelist tells the operator what is SAFE, not what is TRUE — those are
// different questions and the policy is explicit that a stale-looking answer
// with an honest marker beats a fresh-looking one without it.
var readonly = map[string]string{
	// The families the policy names first.
	"show":           "reads one change and its evidence",
	"list":           "lists changes",
	"version":        "prints build information",
	"audit list":     "reads the audit chain",
	"audit show":     "reads one trace record",
	"audit verify":   "verifies hashes over stored rows",
	"audit export":   "renders stored rows to a document",
	"audit report":   "composes governance evidence for runs",
	"cluster status": "reads cluster membership and assignments",

	// System facts about the deployment itself.
	"system version":         "reports the server version",
	"system status":          "probes config, store and cluster state",
	"system batch-status":    "reads batch outcomes",
	"system change-trend":    "aggregates run outcomes",
	"system takeover-events": "reads takeover history",

	// Registry reads.
	"target list":    "lists inventory targets",
	"target history": "reads one target's change history",
	"template list":  "lists workflow templates",
	"template show":  "renders one template",
	"group list":     "lists inventory groups",
	"agent list":     "lists registered agents",
	"agent show":     "reads one agent",
	"alert list":     "lists alerts in the ring",
	"alert show":     "reads one alert",
	"alert history":  "reads an alert's history",

	// Authorisation diagnostics (read the policy, never enforce a change).
	"authz status":  "renders the effective permission matrix",
	"authz explain": "explains one decision the policy engine would make",

	// Calendar reads.
	"calendar list":  "lists change windows",
	"calendar show":  "reads one change window",
	"calendar check": "reports whether a freeze covers a moment",

	// Local-only surfaces: they do not touch the control plane at all, so they
	// remain available no matter what the database is doing.
	"sqlreview": "scores SQL against the rule engine, entirely in-process",
}

// Readonly reports whether a CLI command path is on the Q3 whitelist. The path
// is the command line minus the binary and arguments, e.g. "audit list" or
// "dbchange show". An empty or unknown path is false: an unrecognised command
// is not evidence that it is safe.
func Readonly(command string) bool {
	_, ok := readonly[command]
	return ok
}

// Reason returns why a whitelisted command earned its place, or "" when the
// command is not on the list. A surface that prints the whitelist should print
// this next to it: the entry without a reason is the one nobody asked the
// side-effect question for.
func Reason(command string) string { return readonly[command] }

// ReadonlyCommands returns the whitelist sorted, for surfaces that list them
// (an operator looking at a partition needs the whole set, not a membership
// probe). The returned slice is a copy; callers cannot mutate the table.
func ReadonlyCommands() []string {
	out := make([]string, 0, len(readonly))
	for k := range readonly {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
