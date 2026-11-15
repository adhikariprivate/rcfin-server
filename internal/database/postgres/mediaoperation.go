package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

func (p *PostgresRepo) CreateMediaOperation(ctx context.Context, op *model.MediaOperation) (bool, error) {
	if op == nil || op.ID == "" || op.CollectionID == "" || op.ItemKey == "" || op.Action == "" {
		return false, fmt.Errorf("media operation ID, collection, item key, and action are required")
	}
	query := `INSERT INTO media_operations (id,collection_id,item_key,source_path,destination_path,action,status,provider_id,confidence,evidence,attempts,next_attempt,lease_owner,lease_until,bytes,checksum,error,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`
	result, err := p.dbWriteHandle.ExecContext(ctx, rebind(query), op.ID, op.CollectionID, op.ItemKey, op.SourcePath, op.DestinationPath, op.Action, op.Status, op.ProviderID, op.Confidence, op.Evidence, op.Attempts, op.NextAttempt, op.LeaseOwner, op.LeaseUntil, op.Bytes, op.Checksum, op.Error, op.CreatedAt, op.UpdatedAt)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (p *PostgresRepo) UpsertMediaOperation(ctx context.Context, op *model.MediaOperation) error {
	if op == nil || op.ID == "" || op.CollectionID == "" || op.ItemKey == "" || op.Action == "" {
		return fmt.Errorf("media operation ID, collection, item key, and action are required")
	}
	// Preserve a live claim if a concurrent refresh pass tries to reset a stale
	// in-memory copy of the operation back to planned.
	query := `INSERT INTO media_operations (id,collection_id,item_key,source_path,destination_path,action,status,provider_id,confidence,evidence,attempts,next_attempt,lease_owner,lease_until,bytes,checksum,error,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET source_path=EXCLUDED.source_path,destination_path=EXCLUDED.destination_path,status=EXCLUDED.status,provider_id=EXCLUDED.provider_id,confidence=EXCLUDED.confidence,evidence=EXCLUDED.evidence,attempts=EXCLUDED.attempts,next_attempt=EXCLUDED.next_attempt,lease_owner=EXCLUDED.lease_owner,lease_until=EXCLUDED.lease_until,bytes=EXCLUDED.bytes,checksum=EXCLUDED.checksum,error=EXCLUDED.error,updated_at=EXCLUDED.updated_at
WHERE NOT (media_operations.status='running' AND (media_operations.lease_owner<>EXCLUDED.lease_owner OR (media_operations.lease_until>EXCLUDED.updated_at AND EXCLUDED.status IN ('planned','queued'))))`
	_, err := p.dbWriteHandle.ExecContext(ctx, rebind(query), op.ID, op.CollectionID, op.ItemKey, op.SourcePath, op.DestinationPath, op.Action, op.Status, op.ProviderID, op.Confidence, op.Evidence, op.Attempts, op.NextAttempt, op.LeaseOwner, op.LeaseUntil, op.Bytes, op.Checksum, op.Error, op.CreatedAt, op.UpdatedAt)
	return err
}

func (p *PostgresRepo) GetMediaOperation(ctx context.Context, collectionID, itemKey, action string) (*model.MediaOperation, error) {
	var op model.MediaOperation
	err := p.dbReadHandle.GetContext(ctx, &op, `SELECT * FROM media_operations WHERE collection_id=$1 AND item_key=$2 AND action=$3`, collectionID, itemKey, action)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &op, err
}

func (p *PostgresRepo) ListMediaOperations(ctx context.Context, collectionID string, limit int) ([]model.MediaOperation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ops []model.MediaOperation
	err := p.dbReadHandle.SelectContext(ctx, &ops, `SELECT * FROM media_operations WHERE collection_id=$1 ORDER BY updated_at DESC LIMIT $2`, collectionID, limit)
	return ops, err
}

func (p *PostgresRepo) ListMediaOperationsPage(ctx context.Context, collectionID string, limit int, beforeUpdatedAt time.Time, beforeID string) ([]model.MediaOperation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ops []model.MediaOperation
	var err error
	if beforeUpdatedAt.IsZero() {
		err = p.dbReadHandle.SelectContext(ctx, &ops, `SELECT * FROM media_operations WHERE collection_id=$1 ORDER BY updated_at DESC,id DESC LIMIT $2`, collectionID, limit)
	} else {
		err = p.dbReadHandle.SelectContext(ctx, &ops, `SELECT * FROM media_operations WHERE collection_id=$1 AND (updated_at<$2 OR (updated_at=$2 AND id<$3)) ORDER BY updated_at DESC,id DESC LIMIT $4`, collectionID, beforeUpdatedAt, beforeID, limit)
	}
	return ops, err
}

func (p *PostgresRepo) ClaimMediaOperation(ctx context.Context, id, worker string, now, leaseUntil time.Time) (bool, error) {
	result, err := p.dbWriteHandle.ExecContext(ctx, `UPDATE media_operations SET status='running',lease_owner=$1,lease_until=$2,attempts=attempts+1,updated_at=$3 WHERE id=$4 AND status IN ('planned','retryable','failed_retryable','running') AND next_attempt<=$3 AND lease_until<=$3`, worker, leaseUntil, now, id)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (p *PostgresRepo) RenewMediaOperationLease(ctx context.Context, id, worker string, now, leaseUntil time.Time) (bool, error) {
	if !leaseUntil.After(now) {
		return false, fmt.Errorf("media operation lease renewal must end in the future")
	}
	result, err := p.dbWriteHandle.ExecContext(ctx, rebind(`UPDATE media_operations SET lease_until=? WHERE id=? AND status='running' AND lease_owner=?`), leaseUntil, id, worker)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}
