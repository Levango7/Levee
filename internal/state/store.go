package state

import (
	"context"
	"errors"
	"time"
)

// Run is a top-level change execution unit. One Run owns multiple Batches
// executed serially; each Batch owns multiple Steps executed concurrently
// across hosts.
type Run struct {
	ID           string `json:"id"`
	WorkflowName string `json:"workflow_name"`
	TemplateName string `json:"template_name"`
	Params       string `json:"params"` // JSON encoded
	PlanHash     string `json:"plan_hash"`
	// PlanJSON is the canonical JSON encoding of the generated plan.Plan
	// ('' when no plan has been persisted). Apply refuses to execute
	// without it and re-verifies PlanHash against it to detect drift.
	PlanJSON       string    `json:"plan_json"`
	Status         string    `json:"status"`
	ApprovalStatus string    `json:"approval_status"`
	ApprovalLevel  string    `json:"approval_level"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Creator        string    `json:"creator"`
	IncidentID     string    `json:"incident_id"`
}

// Batch is a single batch within a run. Batches are numbered sequentially
// starting at 1 and execute serially.
type Batch struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	BatchNo     int        `json:"batch_no"`
	Status      string     `json:"status"`
	TotalHosts  int        `json:"total_hosts"`
	Succeeded   int        `json:"succeeded"`
	Failed      int        `json:"failed"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Step is a per-host step execution record. One Step corresponds to one
// action executed on one host within one batch.
type Step struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	BatchID     string     `json:"batch_id"`
	Host        string     `json:"host"`
	StepName    string     `json:"step_name"`
	Action      string     `json:"action"`
	Status      string     `json:"status"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	Stdout      string     `json:"stdout"`
	Stderr      string     `json:"stderr"`
	DurationMs  int        `json:"duration_ms"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Trace is an audit trace record. Traces form a hash chain per run: each
// record's CurrHash depends on PrevHash, making tampering detectable.
type Trace struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Event     string    `json:"event"`
	Actor     string    `json:"actor"`
	Detail    string    `json:"detail"` // JSON encoded
	PrevHash  string    `json:"prev_hash"`
	CurrHash  string    `json:"curr_hash"`
	Timestamp time.Time `json:"timestamp"`
}

// Approval is a single approval record within a multi-level approval chain.
type Approval struct {
	ID        string     `json:"id"`
	RunID     string     `json:"run_id"`
	Level     string     `json:"level"`
	Approver  string     `json:"approver"`
	Status    string     `json:"status"`
	Comment   string     `json:"comment"`
	TimeoutAt *time.Time `json:"timeout_at,omitempty"`
	ActedAt   *time.Time `json:"acted_at,omitempty"`
}

// Lock is a mutex lock with a TTL. Locks are scoped (e.g. host:<name>) and
// enforce mutual exclusion across concurrent runs.
type Lock struct {
	ID         string    `json:"id"`
	Scope      string    `json:"scope"`
	Owner      string    `json:"owner"`
	TTLSeconds int       `json:"ttl_seconds"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Credential is an encrypted credential reference. The plaintext never enters
// the database, logs or trace; only AES-GCM ciphertext is stored.
//
// Tags carries operator metadata (e.g. target/env) as the JSON encoding of a
// map[string]string, or "" when the credential has no tags. The state layer
// stores it verbatim; the credential package owns encode/decode.
type Credential struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	EncryptedData []byte     `json:"encrypted_data"`
	CreatedAt     time.Time  `json:"created_at"`
	RotatedAt     *time.Time `json:"rotated_at,omitempty"`
	Tags          string     `json:"tags,omitempty"`
}

// Audit is a high-level audit log entry. Audit entries describe who did what
// to which target and when; they complement the fine-grained Trace chain.
type Audit struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Target    string    `json:"target"`
	Result    string    `json:"result"`
	Timestamp time.Time `json:"timestamp"`
}

// RunFilter narrows ListRuns results. Empty fields are ignored; non-empty
// fields are combined with AND semantics.
type RunFilter struct {
	Status       string
	WorkflowName string
	TemplateName string
	Creator      string
	IncidentID   string
	// Limit caps the number of returned runs. <= 0 means no cap (callers
	// should set a sane limit to avoid loading the whole table).
	Limit int
	// Offset skips the first Offset matching rows (for keyset-free
	// pagination). Negative values are treated as 0.
	Offset int
}

// BatchFilter narrows ListBatches results within a run.
type BatchFilter struct {
	RunID  string
	Status string
	Limit  int
}

// StepFilter narrows ListSteps results.
type StepFilter struct {
	RunID   string
	BatchID string
	Host    string
	Status  string
	Limit   int
}

// TraceFilter narrows ListTraces results.
type TraceFilter struct {
	RunID string
	Event string
	Limit int
}

// ApprovalFilter narrows ListApprovals results.
type ApprovalFilter struct {
	RunID  string
	Level  string
	Status string
	Limit  int
}

// AuditFilter narrows ListAudits results.
type AuditFilter struct {
	RunID  string
	Action string
	Actor  string
	Limit  int
	// Offset skips the first Offset matching rows (timestamp DESC order).
	// Negative values are treated as 0. Use with Limit for pagination.
	Offset int
}

// Target is a managed inventory host. It persists across daemon restarts,
// unlike the earlier in-memory TargetService registry.
type Target struct {
	ID            string            `json:"id"`
	Hostname      string            `json:"hostname"` // address: IP or DNS name
	Port          int               `json:"port"`
	ChannelType   string            `json:"channel_type"` // ssh|winrm
	CredentialRef string            `json:"credential_ref,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	GroupID       string            `json:"group_id,omitempty"`
	Status        string            `json:"status"` // active|frozen|retired
	Reachable     bool              `json:"reachable"`
	LastCheckedAt *time.Time        `json:"last_checked_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

// InventoryGroup is a hierarchical grouping of targets. Names are unique
// and path-style ("prod/db"); ParentID allows tree structures.
type InventoryGroup struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ParentID  string    `json:"parent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrDuplicateTarget is wrapped by UpsertTarget when the (hostname, port)
// pair is already owned by a different target ID.
var ErrDuplicateTarget = errors.New("state: target address already in use")

// Target lifecycle statuses. active targets may be selected for changes;
// frozen targets are rejected at change creation AND apply time; retired
// targets are kept for history queries only.
const (
	StatusActive  = "active"
	StatusFrozen  = "frozen"
	StatusRetired = "retired"
)

// TargetFilter narrows ListTargets results.
type TargetFilter struct {
	GroupID string
	Status  string
	// Labels requires EVERY listed label to match exactly (AND semantics).
	Labels map[string]string
	Limit  int
	Offset int
}

// WORMStore is a restricted subset of Store that only allows append-only
// operations on trace records, consistent with Write-Once-Read-Many semantics.
// Use this interface in audit/WORM contexts to prevent accidental or malicious
// modification of trace data. SQLiteStore implements WORMStore implicitly.
type WORMStore interface {
	// Trace append-only operations.
	CreateTrace(ctx context.Context, trace *Trace) error
	GetTrace(ctx context.Context, id string) (*Trace, error)
	ListTraces(ctx context.Context, filter TraceFilter) ([]*Trace, error)

	// Run operations needed for trace context (FK constraint).
	GetRun(ctx context.Context, id string) (*Run, error)
	CreateRun(ctx context.Context, run *Run) error

	// Close releases resources.
	Close() error
}

// Store is the persistence abstraction used by every LEVEE subsystem.
// Implementations must be safe for concurrent use; the SQLite implementation
// achieves this by relying on database/sql's connection pool and serialising
// writes through a single writer connection (WAL mode).
//
// Methods follow a consistent convention:
//   - Create* inserts a new row; the ID must be set by the caller.
//   - Get* returns (nil, nil) when the row does not exist.
//   - Update* overwrites all mutable columns; the ID is used as the key.
//   - List* applies the given filter and returns a slice (possibly empty).
//   - Delete* removes a row by ID and returns nil if it did not exist.
type Store interface {
	// Run CRUD.
	CreateRun(ctx context.Context, run *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	UpdateRun(ctx context.Context, run *Run) error
	// UpdateRunStatusIf atomically transitions a run's status from `from`
	// to `to` (WHERE id=? AND status=?), stamping updated_at with the given
	// time. It returns (true, nil) when the transition was applied and
	// (false, nil) when the run does not exist or its current status is not
	// `from` — the row is left untouched in that case. Callers use it to
	// guard state-machine transitions (e.g. only "approved" runs may
	// become "running") against concurrent racers.
	UpdateRunStatusIf(ctx context.Context, id string, from string, to string, updatedAt time.Time) (bool, error)
	// MarkNonTerminalSteps flips every step row of the run whose status
	// is a non-terminal vocabulary (running/pending) to the given
	// terminal marker, and returns the number of rows flipped. It is the
	// defensive terminal-marking pass of the failover takeover: with the
	// current persist-once evidence model it matches 0 rows by
	// construction (step rows only land with terminal statuses), but a
	// future incremental-persistence model cannot silently reintroduce
	// the "step stuck in running" failure mode while this exists.
	MarkNonTerminalSteps(ctx context.Context, runID string, marker string) (int64, error)
	ListRuns(ctx context.Context, filter RunFilter) ([]*Run, error)
	DeleteRun(ctx context.Context, id string) error

	// Batch CRUD.
	CreateBatch(ctx context.Context, batch *Batch) error
	GetBatch(ctx context.Context, id string) (*Batch, error)
	UpdateBatch(ctx context.Context, batch *Batch) error
	ListBatches(ctx context.Context, filter BatchFilter) ([]*Batch, error)
	DeleteBatch(ctx context.Context, id string) error

	// Step CRUD.
	CreateStep(ctx context.Context, step *Step) error
	GetStep(ctx context.Context, id string) (*Step, error)
	UpdateStep(ctx context.Context, step *Step) error
	ListSteps(ctx context.Context, filter StepFilter) ([]*Step, error)
	DeleteStep(ctx context.Context, id string) error

	// Trace CRUD.
	CreateTrace(ctx context.Context, trace *Trace) error
	GetTrace(ctx context.Context, id string) (*Trace, error)
	UpdateTrace(ctx context.Context, trace *Trace) error
	// UpdateTraceChecksum writes checksum into the curr_hash column of the
	// trace identified by id, but only when curr_hash is still empty
	// (WHERE id=? AND curr_hash=''). It exists so the archiver can stamp a
	// WORM content checksum onto pre-existing, unchecksummed traces
	// without ever overwriting an already-computed hash (checksum or chain
	// hash). It returns an error when no row was updated.
	UpdateTraceChecksum(ctx context.Context, id string, checksum string) error
	ListTraces(ctx context.Context, filter TraceFilter) ([]*Trace, error)
	DeleteTrace(ctx context.Context, id string) error

	// Approval CRUD.
	CreateApproval(ctx context.Context, approval *Approval) error
	GetApproval(ctx context.Context, id string) (*Approval, error)
	UpdateApproval(ctx context.Context, approval *Approval) error
	// UpdateApprovalIfPending is a compare-and-set variant of
	// UpdateApproval: it applies the update only when the stored row is
	// still in status "pending" (WHERE id=? AND status='pending'). It
	// returns true when the update was applied and false when the row was
	// concurrently decided by another actor (or does not exist), so the
	// caller can retry or report a conflict without ever overwriting a
	// terminal decision.
	UpdateApprovalIfPending(ctx context.Context, approval *Approval) (bool, error)
	ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*Approval, error)
	DeleteApproval(ctx context.Context, id string) error

	// Lock CRUD.
	CreateLock(ctx context.Context, lock *Lock) error
	GetLock(ctx context.Context, id string) (*Lock, error)
	GetLockByScope(ctx context.Context, scope string) (*Lock, error)
	UpdateLock(ctx context.Context, lock *Lock) error
	// UpdateLockOwnedBy atomically updates the owner (and TTL/acquired/
	// expires) of a lock identified by id, but only when the lock has
	// expired (expires_at <= now). It returns the number of rows
	// affected so callers can detect concurrent races: 0 means another
	// actor won the update and the caller should retry.
	UpdateLockOwnedBy(ctx context.Context, id string, owner string, ttlSeconds int, now time.Time) (int64, error)
	ListLocks(ctx context.Context) ([]*Lock, error)
	DeleteLock(ctx context.Context, id string) error
	// DeleteLockByIDAndOwner deletes the lock identified by id but only
	// when it is still owned by owner. The ownership check and the delete
	// happen inside a single statement so a concurrent ForceAcquire
	// takeover (which reuses the same row id) cannot be deleted by the
	// stale owner: rowsAffected==0 means the lock was taken over by
	// another owner or is already gone.
	DeleteLockByIDAndOwner(ctx context.Context, id string, owner string) (bool, error)
	DeleteExpiredLocks(ctx context.Context, now time.Time) (int64, error)

	// Credential CRUD.
	CreateCredential(ctx context.Context, cred *Credential) error
	GetCredential(ctx context.Context, id string) (*Credential, error)
	GetCredentialByName(ctx context.Context, name string) (*Credential, error)
	UpdateCredential(ctx context.Context, cred *Credential) error
	ListCredentials(ctx context.Context) ([]*Credential, error)
	DeleteCredential(ctx context.Context, id string) error

	// Audit CRUD.
	CreateAudit(ctx context.Context, audit *Audit) error
	GetAudit(ctx context.Context, id string) (*Audit, error)
	ListAudits(ctx context.Context, filter AuditFilter) ([]*Audit, error)

	// Dispatch assignment CRUD (design-cluster-dispatch.md).
	CreateAssignment(ctx context.Context, a *Assignment) error
	GetAssignment(ctx context.Context, runID string) (*Assignment, error)
	// UpdateAssignmentStateIf is a compare-and-set on (run_id, epoch, state):
	// it applies expected→next only when the row's current state is expected.
	// It reports (true, nil) when the transition was applied and (false, nil)
	// when the row does not exist or its state/epoch no longer matches (a
	// concurrent actor won the race). Callers use it to serialise assignment
	// state transitions without a distributed lock.
	UpdateAssignmentStateIf(ctx context.Context, runID string, epoch int64, expected, next string) (bool, error)
	// Reassign bumps the epoch and resets state to pending for a run, used by
	// the dispatch loop when a worker dies and the run must be given to a
	// fresh node. It returns (true, nil) when the existing row matched
	// (runID, prevEpoch) and was bumped.
	Reassign(ctx context.Context, runID string, prevEpoch int64, newNode string) (bool, error)
	// SetAssignmentResult writes the terminal result onto an assignment row
	// identified by (runID, epoch). Best-evidence: a store failure is logged
	// by the caller but never aborts the run's terminal transition.
	SetAssignmentResult(ctx context.Context, runID string, epoch int64, result string) error
	ListAssignments(ctx context.Context, filter AssignmentFilter) ([]*Assignment, error)
	// DeleteAssignment removes the assignment for a run. Idempotent.
	DeleteAssignment(ctx context.Context, runID string) error

	// Inventory: managed target hosts and hierarchical groups.
	UpsertInventoryGroup(ctx context.Context, group *InventoryGroup) error
	GetInventoryGroup(ctx context.Context, id string) (*InventoryGroup, error)
	GetInventoryGroupByName(ctx context.Context, name string) (*InventoryGroup, error)
	ListInventoryGroups(ctx context.Context) ([]*InventoryGroup, error)
	DeleteInventoryGroup(ctx context.Context, id string) error

	// UpsertTarget inserts or updates by ID. The (hostname, port) pair is
	// UNIQUE across the table: inserting a different ID with an address
	// already owned by another row fails with a wrapped ErrDuplicateTarget.
	UpsertTarget(ctx context.Context, target *Target) error
	GetTarget(ctx context.Context, id string) (*Target, error)
	FindTargetByAddress(ctx context.Context, hostname string, port int) (*Target, error)
	ListTargets(ctx context.Context, filter TargetFilter) ([]*Target, error)
	UpdateTargetStatus(ctx context.Context, id string, status string) error
	SetTargetReachability(ctx context.Context, id string, reachable bool, at time.Time) error
	DeleteTarget(ctx context.Context, id string) error
	CountTargetsInGroup(ctx context.Context, groupID string) (int, error)

	// Close releases all underlying resources.
	Close() error
}

// Assignment is the cross-node dispatch assignment for a run
// (design-cluster-dispatch.md). One active assignment per run (run_id PK).
// The epoch increases on every reassignment so a stale scheduler can detect
// that it no longer owns the assignment it is about to write.
type Assignment struct {
	RunID      string
	OwnerNode  string
	Epoch      int64
	State      string // pending | executing | done | interrupted
	Result     string // completed | failed | rolled_back | '' (while not done)
	AssignedAt time.Time
	UpdatedAt  time.Time
}

// AssignmentFilter narrows ListAssignments results. Empty fields are ignored;
// non-empty fields combine with AND.
type AssignmentFilter struct {
	RunID     string
	OwnerNode string
	State     string
	// States is the inverse of State: list assignments whose state is NOT in
	// this set. Used to find "active" assignments (state NOT IN (done,
	// interrupted)). Ignored when empty.
	ExcludeStates []string
	Limit         int
}

const (
	// Assignment states.
	AssignStatePending      = "pending"
	AssignmentStateExecuting = "executing"
	AssignmentStateDone      = "done"
	AssignmentStateInterrupted = "interrupted"

	// Assignment results (mirrors the run status vocabulary).
	AssignResultCompleted   = "completed"
	AssignResultFailed      = "failed"
	AssignResultRolledBack  = "rolled_back"
)
