package postgres

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
)

func applyMigrations(ctx context.Context, db *sqlx.DB) error {
	const version = 1
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize migration checks so concurrent application replicas cannot
	// both decide that the same migration is pending.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7410911)`); err != nil {
		return err
	}
	var applied bool
	if err := tx.GetContext(ctx, &applied, rebind(`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ?)`), version); err != nil {
		return err
	}
	if applied {
		return tx.Commit()
	}
	const regularUsers = `users.id NOT IN (SELECT userid FROM user_properties WHERE key = 'admin' AND value = '1')`
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_properties(userid, key, value) SELECT users.id, 'enableallfolders', '0' FROM users WHERE `+regularUsers+` ON CONFLICT(userid, key) DO UPDATE SET value = '0'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_properties WHERE key = 'enabledfolders' AND userid IN (SELECT users.id FROM users WHERE `+regularUsers+`)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, rebind(`INSERT INTO schema_migrations(version, applied) VALUES (?, ?)`), version, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
