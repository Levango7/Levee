package cluster

// Cluster node vocabulary names.
//
// The console's cluster page mirrors both enumerations in TypeScript, and until
// now the guard that kept them together parsed this package's constants AND
// web/src/utils/node.ts on every run. Parsing a transcription is what let #153
// pass: three readings on the cluster page stayed at their defaults while every
// check stayed green, because the contract test compared a hand-copied table with
// itself. These lists are the data internal/docgen writes into
// web/src/types/vocabulary.ts, so the mirror cannot disagree by construction.
//
// The constants stay in node.go's const block: they appear in switch cases, and a
// switch needs constants. What moves here is the order-bearing set, which now has
// exactly one home — and vocabulary_ownership_test.go turns "a new constant that
// never joined the list" into a red test instead of a wire value nobody documented.

// nodeStatusOrder lists the node lifecycle states in registry order: a healthy
// node, one asked to leave gracefully, one that failed health checks or
// deregistered.
//
// `leaving` is included because it is a declared state even though nothing in this
// package writes it today — the console colours it as a warning rather than a
// failure, which is the point of naming it here instead of letting the page fold
// three states into two colours with a ternary.
var nodeStatusOrder = []string{
	string(StatusActive),
	string(StatusLeaving),
	string(StatusOffline),
}

// nodeRoleOrder lists the roles in authority order: the node that owns scheduling
// first, then the nodes that execute batches.
var nodeRoleOrder = []string{
	string(RoleMaster),
	string(RoleWorker),
}

// NodeStatusNames returns the node lifecycle vocabulary. The result is a copy and
// may be modified by the caller.
func NodeStatusNames() []string {
	return append([]string(nil), nodeStatusOrder...)
}

// NodeRoleNames returns the cluster role vocabulary. The result is a copy and may
// be modified by the caller.
func NodeRoleNames() []string {
	return append([]string(nil), nodeRoleOrder...)
}
