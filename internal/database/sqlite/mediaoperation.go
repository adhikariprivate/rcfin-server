package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

func (s *SqliteRepo) CreateMediaOperation(ctx context.Context, op *model.MediaOperation) (bool, error) {
	if op == nil || op.ID == "" || op.CollectionID == "" || op.ItemKey == "" || op.Action == "" {
		return false, fmt.Errorf("media operation ID, collection, item key, and action are required")
	}
	result, err := s.dbWriteHandle.ExecContext(ctx, `INSERT INTO media_operations (id,collection_id,item_key,source_path,destination_path,action,status,provider_id,confidence,evidence,attempts,next_attempt,lease_owner,lease_until,bytes,checksum,error,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		op.ID, op.CollectionID, op.ItemKey, op.SourcePath, op.DestinationPath, op.Action, op.Status, op.ProviderID, op.Confidence, op.Evidence, op.Attempts, op.NextAttempt, op.LeaseOwner, op.LeaseUntil, op.Bytes, op.Checksum, op.Error, op.CreatedAt, op.UpdatedAt)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *SqliteRepo) UpsertMediaOperation(ctx context.Context, op *model.MediaOperation) error {
	if op == nil || op.ID == "" || op.CollectionID == "" || op.ItemKey == "" || op.Action == "" {
		return fmt.Errorf("media operation ID, collection, item key, and action are required")
	}
	// A concurrent inventory/refresh pass may have read a stale row before a
	// worker claimed it. Do not let its planned upsert erase a live lease.
	_, err := s.dbWriteHandle.ExecContext(ctx, `INSERT INTO media_operations (id,collection_id,item_key,source_path,destination_path,action,status,provider_id,confidence,evidence,attempts,next_attempt,lease_owner,lease_until,bytes,checksum,error,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET source_path=excluded.source_path,destination_path=excluded.destination_path,status=excluded.status,provider_id=excluded.provider_id,confidence=excluded.confidence,evidence=excluded.evidence,attempts=excluded.attempts,next_attempt=excluded.next_attempt,lease_owner=excluded.lease_owner,lease_until=excluded.lease_until,bytes=excluded.bytes,checksum=excluded.checksum,error=excluded.error,updated_at=excluded.updated_at
WHERE NOT (media_operations.status='running' AND (media_operations.lease_owner<>excluded.lease_owner OR (julianday(media_operations.lease_until)>julianday(excluded.updated_at) AND excluded.status IN ('planned','queued'))))`,
		op.ID, op.CollectionID, op.ItemKey, op.SourcePath, op.DestinationPath, op.Action, op.Status, op.ProviderID, op.Confidence, op.Evidence, op.Attempts, op.NextAttempt, op.LeaseOwner, op.LeaseUntil, op.Bytes, op.Checksum, op.Error, op.CreatedAt, op.UpdatedAt)
	return err
}

func (s *SqliteRepo) GetMediaOperation(ctx context.Context, collectionID, itemKey, action string) (*model.MediaOperation, error) {
	var op model.MediaOperation
	err := s.dbReadHandle.GetContext(ctx, &op, `SELECT * FROM media_operations WHERE collection_id=? AND item_key=? AND action=?`, collectionID, itemKey, action)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &op, err
}

// RelinkMediaOperation carries a journal row to a confirmed new path while
// preserving its state, provider evidence, attempts, and creation timestamp.
func (s *SqliteRepo) RelinkMediaOperation(ctx context.Context, collectionID, oldID, oldItemKey string, operation *model.MediaOperation) (bool, error) {
	if operation == nil || operation.ID == "" || operation.ItemKey == "" || operation.Action == "" {
		return false, fmt.Errorf("relinked media operation ID, item key, and action are required")
	}
	result, err := s.dbWriteHandle.ExecContext(ctx, `UPDATE media_operations SET id=?,item_key=?,source_path=?,destination_path=?,evidence=?,updated_at=? WHERE id=? AND collection_id=? AND item_key=? AND status<>'running'`, operation.ID, operation.ItemKey, operation.SourcePath, operation.DestinationPath, operation.Evidence, operation.UpdatedAt, oldID, collectionID, oldItemKey)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *SqliteRepo) ListMediaOperations(ctx context.Context, collectionID string, limit int) ([]model.MediaOperation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ops []model.MediaOperation
	err := s.dbReadHandle.SelectContext(ctx, &ops, `SELECT * FROM media_operations WHERE collection_id=? ORDER BY updated_at DESC LIMIT ?`, collectionID, limit)
	return ops, err
}

func (s *SqliteRepo) ListMediaOperationsPage(ctx context.Context, collectionID string, limit int, beforeUpdatedAt time.Time, beforeID string) ([]model.MediaOperation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ops []model.MediaOperation
	var err error
	if beforeUpdatedAt.IsZero() {
		err = s.dbReadHandle.SelectContext(ctx, &ops, `SELECT * FROM media_operations WHERE collection_id=? ORDER BY updated_at DESC,id DESC LIMIT ?`, collectionID, limit)
	} else {
		err = s.dbReadHandle.SelectContext(ctx, &ops, `SELECT * FROM media_operations WHERE collection_id=? AND (updated_at<? OR (updated_at=? AND id<?)) ORDER BY updated_at DESC,id DESC LIMIT ?`, collectionID, beforeUpdatedAt, beforeUpdatedAt, beforeID, limit)
	}
	return ops, err
}

func (s *SqliteRepo) ClaimMediaOperation(ctx context.Context, id, worker string, now, leaseUntil time.Time) (bool, error) {
	result, err := s.dbWriteHandle.ExecContext(ctx, `UPDATE media_operations SET status='running',lease_owner=?,lease_until=?,attempts=attempts+1,updated_at=? WHERE id=? AND status IN ('planned','retryable','failed_retryable','running') AND next_attempt<=? AND lease_until<=?`, worker, leaseUntil, now, id, now, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *SqliteRepo) RenewMediaOperationLease(ctx context.Context, id, worker string, now, leaseUntil time.Time) (bool, error) {
	if !leaseUntil.After(now) {
		return false, fmt.Errorf("media operation lease renewal must end in the future")
	}
	result, err := s.dbWriteHandle.ExecContext(ctx, `UPDATE media_operations SET lease_until=? WHERE id=? AND status='running' AND lease_owner=?`, leaseUntil, id, worker)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}
