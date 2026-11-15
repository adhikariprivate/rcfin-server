package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

func (s *SqliteRepo) GetLibraryInventory(ctx context.Context, collectionID string) (*model.LibraryInventorySnapshot, error) {
	var payload string
	var generation int64
	var completedAt sql.NullTime
	err := s.dbReadHandle.QueryRowxContext(ctx, `SELECT generation,completed_at,payload FROM library_inventory_snapshots WHERE collection_id=?`, collectionID).Scan(&generation, &completedAt, &payload)
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

func (s *SqliteRepo) SaveLibraryInventory(ctx context.Context, snapshot *model.LibraryInventorySnapshot) error {
	if snapshot == nil || snapshot.CollectionID == "" {
		return fmt.Errorf("library inventory collection ID is required")
	}
	tx, err := s.dbWriteHandle.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var generation int64
	err = tx.GetContext(ctx, &generation, `SELECT generation FROM library_inventory_snapshots WHERE collection_id=?`, snapshot.CollectionID)
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
	_, err = tx.ExecContext(ctx, `INSERT INTO library_inventory_snapshots(collection_id,generation,completed_at,payload) VALUES(?,?,?,?) ON CONFLICT(collection_id) DO UPDATE SET generation=excluded.generation,completed_at=excluded.completed_at,payload=excluded.payload`, snapshot.CollectionID, generation, snapshot.CompletedAt, string(payload))
	if err != nil {
		return err
	}
	return tx.Commit()
}
