package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

func (p *PostgresRepo) GetLibraryInventory(ctx context.Context, collectionID string) (*model.LibraryInventorySnapshot, error) {
	var payload string
	var generation int64
	var completedAt sql.NullTime
	err := p.dbReadHandle.QueryRowxContext(ctx, `SELECT generation,completed_at,payload FROM library_inventory_snapshots WHERE collection_id=$1`, collectionID).Scan(&generation, &completedAt, &payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot model.LibraryInventorySnapshot
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return nil, fmt.Errorf("decode library inventory snapshot: %w", err)
	}
	snapshot.CollectionID = collectionID
	snapshot.Generation = generation
	if completedAt.Valid {
		snapshot.CompletedAt = completedAt.Time
	}
	return &snapshot, nil
}

func (p *PostgresRepo) SaveLibraryInventory(ctx context.Context, snapshot *model.LibraryInventorySnapshot) error {
	if snapshot == nil || snapshot.CollectionID == "" {
		return fmt.Errorf("library inventory collection ID is required")
	}
	tx, err := p.dbWriteHandle.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, snapshot.CollectionID); err != nil {
		return err
	}
	var generation int64
	err = tx.GetContext(ctx, &generation, `SELECT generation FROM library_inventory_snapshots WHERE collection_id=$1`, snapshot.CollectionID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows {
		generation = 1
	} else {
		generation++
	}
	snapshot.Generation = generation
	if snapshot.CompletedAt.IsZero() {
		return fmt.Errorf("library inventory completion time is required")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO library_inventory_snapshots(collection_id,generation,completed_at,payload) VALUES($1,$2,$3,$4) ON CONFLICT(collection_id) DO UPDATE SET generation=EXCLUDED.generation,completed_at=EXCLUDED.completed_at,payload=EXCLUDED.payload`, snapshot.CollectionID, generation, snapshot.CompletedAt, string(payload))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// MarkLibraryInventoryMovesApplied clears durable move-relink work without
// creating a new scan generation.
func (p *PostgresRepo) MarkLibraryInventoryMovesApplied(ctx context.Context, collectionID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := p.dbWriteHandle.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, collectionID); err != nil {
		return err
	}
	var payload string
	if err := tx.GetContext(ctx, &payload, `SELECT payload FROM library_inventory_snapshots WHERE collection_id=$1 FOR UPDATE`, collectionID); err != nil {
		return err
	}
	var snapshot model.LibraryInventorySnapshot
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return fmt.Errorf("decode library inventory snapshot: %w", err)
	}
	selected := make(map[string]bool, len(paths))
	for _, path := range paths {
		selected[path] = true
	}
	for i := range snapshot.Entries {
		if selected[snapshot.Entries[i].Path] {
			snapshot.Entries[i].MoveRelinkPending = false
		}
	}
	updated, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE library_inventory_snapshots SET payload=$1 WHERE collection_id=$2`, string(updated), collectionID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateLibraryInventorySnapshot persists an administrative catalog decision
// without claiming that another remote scan generation completed.
func (p *PostgresRepo) UpdateLibraryInventorySnapshot(ctx context.Context, snapshot *model.LibraryInventorySnapshot) error {
	if snapshot == nil || snapshot.CollectionID == "" || snapshot.Generation <= 0 {
		return fmt.Errorf("existing library inventory snapshot is required")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	result, err := p.dbWriteHandle.ExecContext(ctx, `UPDATE library_inventory_snapshots SET payload=$1 WHERE collection_id=$2 AND generation=$3`, string(payload), snapshot.CollectionID, snapshot.Generation)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("library inventory generation changed during administrative update")
	}
	return nil
}
