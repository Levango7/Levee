package approval

// The approval record's status vocabulary, as data.
//
// This list exists so the set has one home that the generator
// (internal/docgen → web/src/types/vocabulary.ts) and the guards can read. The
// prose that used to describe it got the set wrong: schema.sql, pgschema.sql and
// proto/levee.proto all advertise `timeout` (and runs.approval_status's comment
// additionally advertises `skipped`), while the constants here name `expired` —
// two spellings for one idea across two columns, which is the failure shape this
// repo has already paid for twice (#153's cluster readings, the runs.status
// comment that documented four values no writer produced).
//
// What the four members are worth to a reader is not equal, and that is recorded
// rather than hidden: StatusApproved / StatusRejected are written by the decision
// path, StatusPending is the initial state, and StatusExpired is produced only by
// CheckExpiry — which today has NO production caller (repo-wide sweep: the only
// call sites are in service_test.go). So an approval that times out keeps reading
// `pending` until something drives expiry, which is a behaviour gap, not a
// vocabulary gap; naming the member keeps the wire honest if that caller appears.

var statusOrder = []string{
	string(StatusPending),
	string(StatusApproved),
	string(StatusRejected),
	string(StatusExpired),
}

// StatusNames returns the approval-record statuses in lifecycle order. The result
// is a copy and may be modified by the caller.
func StatusNames() []string {
	return append([]string(nil), statusOrder...)
}
