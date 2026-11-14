package postgres

import (
	"context"
	"fmt"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

type PostgresRepo struct {
	// Read db handle
	dbReadHandle *sqlx.DB
	// Handle specfically for writes
	dbWriteHandle *sqlx.DB
	// in-memory access token store, entries written to the database every 3 seconds.
	accessTokenCache map[string]*model.AccessToken
	// last time the access token cache was synced to the database
	accessTokenCacheSyncTime time.Time
	// in-memory user data store, entries are written to the database every 3 seconds.
	userDataEntries map[userDataKey]model.UserData
	// last time the user data entries were synced to the database
	userDataEntriesCacheSyncTime time.Time
	// mutex to protect access to in-memory stores
	mu sync.Mutex
}

// Config holds PostgreSQL connection settings.
type Config struct {
	URL string `mapstructure:"url" yaml:"url"`
}

func init() { sqlx.BindDriver("pgx", sqlx.DOLLAR) }

func rebind(query string) string { return sqlx.Rebind(sqlx.DOLLAR, query) }

// New connects to PostgreSQL and initializes the application schema.
func New(o Config) (*PostgresRepo, error) {
	if o.URL == "" {
		return nil, fmt.Errorf("database.postgres.url is required")
	}

	dbHandle, err := sqlx.Connect("pgx", o.URL)
	if err != nil {
		return nil, err
	}
	dbHandle.SetMaxOpenConns(10)
	dbHandle.SetMaxIdleConns(5)

	if err := dbInitSchema(dbHandle); err != nil {
		dbHandle.Close()
		return nil, err
	}
	if err := applyMigrations(context.Background(), dbHandle); err != nil {
		dbHandle.Close()
		return nil, err
	}

	d := &PostgresRepo{
		dbReadHandle:     dbHandle,
		dbWriteHandle:    dbHandle,
		userDataEntries:  make(map[userDataKey]model.UserData),
		accessTokenCache: make(map[string]*model.AccessToken),
	}

	if err := d.loadUserDataFromDB(); err != nil {
		d.Close()
		return nil, err
	}

	return d, nil
}

// Close releases the database connections opened by New.
func (s *PostgresRepo) Close() error {
	var readErr, writeErr error
	if s.dbReadHandle != nil {
		readErr = s.dbReadHandle.Close()
	}
	if s.dbWriteHandle != nil && s.dbWriteHandle != s.dbReadHandle {
		writeErr = s.dbWriteHandle.Close()
	}
	if readErr != nil {
		return readErr
	}
	return writeErr
}

// StartBackgroundJobs starts background jobs for the database repository.
// these jobs handle periodic syncing of in-memory caches to the database.
func (s *PostgresRepo) StartBackgroundJobs(ctx context.Context) {
	syncInterval := 10 * time.Second

	go s.accessTokenBackgroundJob(ctx, syncInterval)
	go s.userDataBackgroundJob(ctx, syncInterval)
}
