package sqlite

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
)

// applyMigrations revokes the legacy all-library default from existing
// non-administrator accounts once. This is the access-control major upgrade.
func applyMigrations(ctx context.Context, db *sqlx.DB) error {
	const migrationVersion = 1
	var applied bool
	if err := db.GetContext(ctx, &applied, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ?)`, migrationVersion); err != nil {
		return err
	}
	if applied {
		return nil
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const regularUsers = `users.id NOT IN (
		SELECT userid FROM user_properties WHERE key = 'admin' AND value = '1'
	)`
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_properties(userid, key, value)
		SELECT users.id, 'enableallfolders', '0' FROM users
		WHERE `+regularUsers+`
		ON CONFLICT(userid, key) DO UPDATE SET value = '0'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM user_properties WHERE key = 'enabledfolders' AND userid IN (
			SELECT users.id FROM users WHERE `+regularUsers+`
		)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied) VALUES (?, ?)`, migrationVersion, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
