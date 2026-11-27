package state

import (
	"context"
	"database/sql"
	"time"
)

// ChangeIdempotency records that one asynchronous StartChange request was
// accepted. The change's current status is read from runs on replay, so this
// row never becomes a second execution state machine.
type ChangeIdempotency struct {
	ChangeID           string
	IdempotencyKey     string
	RequestFingerprint string
	CreatedAt          time.Time
}

// ChangeIdempotencyStore is the optional durable StartChange capability.
// Production SQLite/PostgreSQL stores implement it; reduced stores can reject
// keyed StartChange rather than pretending restart-safe idempotency.
type ChangeIdempotencyStore interface {
	GetChangeIdempotency(ctx context.Context, changeID, key string) (*ChangeIdempotency, error)
	// ClaimChangeExecution atomically inserts the key and performs the run
	// status CAS. already=true means the key existed and no status was changed.
	// transitioned=false with already=false means the status CAS lost; the new
	// key is rolled back so it cannot poison a later, valid retry.
	ClaimChangeExecution(ctx context.Context, record *ChangeIdempotency, fromStatus, toStatus string, updatedAt time.Time) (already, transitioned bool, err error)
}

// GetChangeIdempotency returns nil, nil when the key has not been accepted.
func (s *SQLiteStore) GetChangeIdempotency(ctx context.Context, changeID, key string) (*ChangeIdempotency, error) {
	var record ChangeIdempotency
	err := s.db.QueryRowContext(ctx,
		`SELECT change_id, idempotency_key, request_fingerprint, created_at FROM change_idempotency WHERE change_id = ? AND idempotency_key = ?`,
		changeID, key).Scan(&record.ChangeID, &record.IdempotencyKey, &record.RequestFingerprint, &record.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// PutChangeIdempotency inserts the key exactly once across concurrent callers.
func (s *SQLiteStore) PutChangeIdempotency(ctx context.Context, record *ChangeIdempotency) (bool, error) {
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO change_idempotency (change_id, idempotency_key, request_fingerprint, created_at) VALUES (?, ?, ?, ?)`,
		record.ChangeID, record.IdempotencyKey, record.RequestFingerprint, createdAt)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 0, nil
}

// ClaimChangeExecution atomically claims the key and transitions the run. A
// status conflict rolls back the inserted key in the same transaction.
func (s *SQLiteStore) ClaimChangeExecution(ctx context.Context, record *ChangeIdempotency, fromStatus, toStatus string, updatedAt time.Time) (bool, bool, error) {
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO change_idempotency (change_id, idempotency_key, request_fingerprint, created_at) VALUES (?, ?, ?, ?)`,
		record.ChangeID, record.IdempotencyKey, record.RequestFingerprint, createdAt)
	if err != nil {
		return false, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, false, err
	}
	if rows == 0 {
		if err := tx.Commit(); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	result, err = tx.ExecContext(ctx,
		`UPDATE runs SET status=?, updated_at=? WHERE id=? AND status=?`,
		toStatus, updatedAt, record.ChangeID, fromStatus)
	if err != nil {
		return false, false, err
	}
	rows, err = result.RowsAffected()
	if err != nil {
		return false, false, err
	}
	if rows == 0 {
		return false, false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return false, true, nil
}

// GetChangeIdempotency returns nil, nil when the key has not been accepted.
func (s *PGStore) GetChangeIdempotency(ctx context.Context, changeID, key string) (*ChangeIdempotency, error) {
	var record ChangeIdempotency
	err := s.db.QueryRowContext(ctx,
		`SELECT change_id, idempotency_key, request_fingerprint, created_at FROM change_idempotency WHERE change_id = $1 AND idempotency_key = $2`,
		changeID, key).Scan(&record.ChangeID, &record.IdempotencyKey, &record.RequestFingerprint, &record.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// PutChangeIdempotency inserts the key exactly once across concurrent callers.
func (s *PGStore) PutChangeIdempotency(ctx context.Context, record *ChangeIdempotency) (bool, error) {
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO change_idempotency (change_id, idempotency_key, request_fingerprint, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT (change_id, idempotency_key) DO NOTHING`,
		record.ChangeID, record.IdempotencyKey, record.RequestFingerprint, createdAt)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 0, nil
}

// ClaimChangeExecution atomically claims the key and transitions the run. A
// status conflict rolls back the inserted key in the same transaction.
func (s *PGStore) ClaimChangeExecution(ctx context.Context, record *ChangeIdempotency, fromStatus, toStatus string, updatedAt time.Time) (bool, bool, error) {
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx,
		`INSERT INTO change_idempotency (change_id, idempotency_key, request_fingerprint, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT (change_id, idempotency_key) DO NOTHING`,
		record.ChangeID, record.IdempotencyKey, record.RequestFingerprint, createdAt)
	if err != nil {
		return false, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, false, err
	}
	if rows == 0 {
		if err := tx.Commit(); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	result, err = tx.ExecContext(ctx,
		`UPDATE runs SET status=$1, updated_at=$2 WHERE id=$3 AND status=$4`,
		toStatus, updatedAt, record.ChangeID, fromStatus)
	if err != nil {
		return false, false, err
	}
	rows, err = result.RowsAffected()
	if err != nil {
		return false, false, err
	}
	if rows == 0 {
		return false, false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return false, true, nil
}
