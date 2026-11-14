package postgres

import (
	"context"
	"log"
	"strconv"
	"strings"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

// GetUser retrieves a user.
func (s *PostgresRepo) GetUser(ctx context.Context, username string) (user *model.User, err error) {
	const query = `SELECT id, username,	password, created, lastlogin, lastused FROM users WHERE username=? LIMIT 1`
	return s.loadUser(s.dbReadHandle.QueryRowContext(ctx, rebind(query), username))
}

// GetByID retrieves a user from the database by their ID.
func (s *PostgresRepo) GetUserByID(ctx context.Context, userID string) (*model.User, error) {
	query := `SELECT id, username, password, created, lastlogin, lastused FROM users WHERE id=? LIMIT 1`
	return s.loadUser(s.dbReadHandle.QueryRowContext(ctx, rebind(query), userID))
}

// GetAllUsers retrieves all users from the database.
func (s *PostgresRepo) GetAllUsers(ctx context.Context) ([]model.User, error) {
	const query = `SELECT id, username,	password, created, lastlogin, lastused FROM users`
	rows, err := s.dbReadHandle.QueryContext(ctx, rebind(query))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []model.User
	for rows.Next() {
		if user, err := s.loadUser(rows); err == nil {
			users = append(users, *user)
		} else {
			log.Printf("Error loading user from db: %s\n", err)
		}
	}
	return users, rows.Err()
}

type sqlScanner interface {
	Scan(dest ...any) error
}

func (s *PostgresRepo) loadUser(scanner sqlScanner) (*model.User, error) {
	var user model.User
	if err := scanner.Scan(
		&user.ID,
		&user.Username,
		&user.Password,
		&user.CreatedAt,
		&user.LastLogin,
		&user.LastUsed); err != nil {
		return nil, model.ErrNotFound
	}
	var err error
	user.Properties, err = s.loadUserProperties(context.Background(), user.ID)
	return &user, err
}

// UpsertUser upserts a user into the database.
func (s *PostgresRepo) UpsertUser(ctx context.Context, user *model.User) error {
	tx, err := s.dbWriteHandle.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	const query = `INSERT INTO users (id, username, password, created, lastlogin, lastused) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET username=EXCLUDED.username, password=EXCLUDED.password, created=EXCLUDED.created, lastlogin=EXCLUDED.lastlogin, lastused=EXCLUDED.lastused`
	_, err = tx.ExecContext(ctx, rebind(query),
		user.ID,
		user.Username,
		user.Password,
		user.CreatedAt,
		user.LastLogin,
		user.LastUsed)
	if err != nil {
		// log.Printf("Error upserting user to db for userID: %s: %s\n", user.ID, err)
		return err
	}
	err = tx.Commit()
	if err != nil {
		return err
	}
	return s.saveUserProperties(ctx, user.ID, user.Properties)
}

func (s *PostgresRepo) DeleteUser(ctx context.Context, userID string) error {
	tx, err := s.dbWriteHandle.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{
		`DELETE FROM playlist_items WHERE playlistid IN (SELECT id FROM playlists WHERE userid = ?)`,
		`DELETE FROM playlists WHERE userid = ?`,
		`DELETE FROM playstate WHERE userid = ?`,
		`DELETE FROM accesstokens WHERE userid = ?`,
		`DELETE FROM quickconnect WHERE userid = ?`,
		`DELETE FROM images WHERE itemid = ?`,
		`DELETE FROM user_properties WHERE userid = ?`,
		`DELETE FROM users WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, rebind(query), userID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	for token, accessToken := range s.accessTokenCache {
		if accessToken.UserID == userID {
			delete(s.accessTokenCache, token)
		}
	}
	for key := range s.userDataEntries {
		if key.userID == userID {
			delete(s.userDataEntries, key)
		}
	}
	s.mu.Unlock()
	return nil
}

// Database keys for user properties
const (
	propAdmin             = "admin"
	propDisabled          = "disabled"
	propEnableAllFolders  = "enableallfolders"
	propEnabledFolders    = "enabledfolders"
	propEnableDownloads   = "enabledownloads"
	propIsHidden          = "ishidden"
	propOrderedViews      = "orderedviews"
	propMyMediaExcludes   = "mymediaexcludes"
	propAllowTags         = "allowtags"
	propBlockTags         = "blocktags"
	propMaxParentalRating = "maxparentalrating"
	propBlockUnratedItems = "blockunrateditems"
	propMaxActiveSessions = "maxactivesessions"
)

func (s *PostgresRepo) loadUserProperties(ctx context.Context, userID string) (model.UserProperties, error) {
	const query = ` SELECT key, value FROM user_properties WHERE userid = ?`
	rows, err := s.dbReadHandle.QueryContext(ctx, rebind(query), userID)
	if err != nil {
		return model.UserProperties{}, err
	}
	defer rows.Close()
	// Set default values in case we do not have entries in db.
	props := model.UserPropertiesDefault()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return model.UserProperties{}, err
		}
		switch key {
		case propAdmin:
			props.Admin = value == "1"
		case propEnableDownloads:
			props.EnableDownloads = value == "1"
		case propDisabled:
			props.Disabled = value == "1"
		case propEnableAllFolders:
			props.EnableAllFolders = value == "1"
		case propEnabledFolders:
			props.EnabledFolders = splitComma(value)
		case propIsHidden:
			props.IsHidden = value == "1"
		case propOrderedViews:
			props.OrderedViews = splitComma(value)
		case propMyMediaExcludes:
			props.MyMediaExcludes = splitComma(value)
		case propAllowTags:
			props.AllowTags = splitComma(value)
		case propBlockTags:
			props.BlockTags = splitComma(value)
		case propMaxParentalRating:
			props.MaxParentalRating, _ = strconv.Atoi(value)
		case propBlockUnratedItems:
			props.BlockUnratedItems = splitComma(value)
		case propMaxActiveSessions:
			props.MaxActiveSessions, _ = strconv.Atoi(value)
		default:
			log.Printf("Unknown user property key: %s\n", key)
		}
	}
	return props, rows.Err()
}

func splitComma(value string) []string {
	if value == "" {
		return []string{}
	}
	return strings.Split(value, ",")
}

func (s *PostgresRepo) saveUserProperties(ctx context.Context, userID string, props model.UserProperties) error {
	const query = `INSERT INTO user_properties (userid, key, value) VALUES (?, ?, ?) ON CONFLICT(userid, key) DO UPDATE SET value = excluded.value`
	tx, err := s.dbWriteHandle.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, rebind(query))
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	// User properties to save, we convert boolean values to "1" or "0" strings
	// and slice values to comma-separated strings
	properties := []struct{ key, value string }{
		{propAdmin, boolToString(props.Admin)},
		{propIsHidden, boolToString(props.IsHidden)},
		{propDisabled, boolToString(props.Disabled)},
		{propEnableDownloads, boolToString(props.EnableDownloads)},
		{propEnableAllFolders, boolToString(props.EnableAllFolders)},
		{propEnabledFolders, strings.Join(props.EnabledFolders, ",")},
		{propOrderedViews, strings.Join(props.OrderedViews, ",")},
		{propMyMediaExcludes, strings.Join(props.MyMediaExcludes, ",")},
		{propAllowTags, strings.Join(props.AllowTags, ",")},
		{propBlockTags, strings.Join(props.BlockTags, ",")},
		{propMaxParentalRating, strconv.Itoa(props.MaxParentalRating)},
		{propBlockUnratedItems, strings.Join(props.BlockUnratedItems, ",")},
		{propMaxActiveSessions, strconv.Itoa(props.MaxActiveSessions)},
	}
	for _, item := range properties {
		// log.Printf("Saving user property for userID: %s, key: %s, value: %s\n", userID, item.key, item.value)
		if _, err := stmt.ExecContext(ctx, userID, item.key, item.value); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func boolToString(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
