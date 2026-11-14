package postgres

import (
	"context"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

// GetQuickConnectCodeBySecret retrieves a quick connect code for a user by secret string.
func (s *PostgresRepo) GetQuickConnectCodeBySecret(ctx context.Context, secret string) (*model.QuickConnectCode, error) {
	query := `SELECT userid, deviceid, secret, authorized, code, created FROM quickconnect WHERE secret=? LIMIT 1`
	return s.loadQuickConnectCode(s.dbReadHandle.QueryRowContext(ctx, rebind(query), secret))
}

// GetQuickConnectCodeByCode retrieves a quick connect code for a user by code string.
func (s *PostgresRepo) GetQuickConnectCodeByCode(ctx context.Context, code string) (*model.QuickConnectCode, error) {
	query := `SELECT userid, deviceid, secret, authorized, code, created FROM quickconnect WHERE code=? LIMIT 1`
	return s.loadQuickConnectCode(s.dbReadHandle.QueryRowContext(ctx, rebind(query), code))
}

func (s *PostgresRepo) loadQuickConnectCode(scanner sqlScanner) (*model.QuickConnectCode, error) {
	var t model.QuickConnectCode
	if err := scanner.Scan(&t.UserID,
		&t.DeviceID,
		&t.Secret,
		&t.Authorized,
		&t.Code,
		&t.CreatedAt); err != nil {
		return nil, model.ErrNotFound
	}
	return &t, nil
}

// UpsertQuickConnectCode inserts or updates a quick connect code for a user.
func (s *PostgresRepo) UpsertQuickConnectCode(ctx context.Context, code model.QuickConnectCode) error {
	query := `INSERT INTO quickconnect (userid, deviceid, secret, authorized, code, created)
	VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(deviceid, secret) DO UPDATE SET userid=EXCLUDED.userid, authorized=EXCLUDED.authorized, code=EXCLUDED.code, created=EXCLUDED.created`

	_, err := s.dbWriteHandle.ExecContext(ctx, rebind(query),
		code.UserID,
		code.DeviceID,
		code.Secret,
		code.Authorized,
		code.Code,
		code.CreatedAt)
	return err
}
