package state

import "github.com/nexus/levee/internal/runstatus"

// auditOutcomeTokens is every OUTCOME token audit.result may carry. A list
// rather than a switch inside AuditResultKnown so the guard next door can
// assert it set-equal to the AuditResult* constants — a constant added without
// a line here (or the reverse) fails a test instead of a query.
var auditOutcomeTokens = []string{
	AuditResultSuccess,
	AuditResultFailed,
	AuditResultPassed,
	AuditResultDenied,
	AuditResultTriggered,
	AuditResultQuorumPending,
}

// AuditResultKnown reports whether v is a value the audit.result column may
// carry: an outcome token, or a run status (a transition records the status it
// moved the run to — the constants block in store.go says why the run status
// belongs in this column at all).
//
// The audit writer calls this and warns when it is false; it does NOT refuse
// the row. A row recorded with an out-of-vocabulary result is mislabelled, a
// row not recorded at all is an audit loss. There is no exception list: the
// prose values this column used to carry were removed rather than given a
// detail column (the chain hashes a fixed field set), so what the two families
// below accept is the whole contract.
func AuditResultKnown(v string) bool {
	if runstatus.IsValid(v) {
		return true
	}
	for _, tok := range auditOutcomeTokens {
		if v == tok {
			return true
		}
	}
	return false
}
