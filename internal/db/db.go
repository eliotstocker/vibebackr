package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationsFS embed.FS

// DriverType distinguishes database engines.
type DriverType string

const (
	DriverSQLite   DriverType = "sqlite"
	DriverPostgres DriverType = "postgres"
)

// DB wraps an sqlx.DB connection and remembers the underlying driver type.
type DB struct {
	*sqlx.DB
	Driver DriverType
}

// Connect initializes and returns a DB instance based on driver and DSN.
func Connect(driverName, dsn string) (*DB, error) {
	var dt DriverType
	switch {
	case strings.HasPrefix(driverName, "sqlite"):
		dt = DriverSQLite
		driverName = "sqlite" //modernc driver name
	case strings.HasPrefix(driverName, "postgres"):
		dt = DriverPostgres
		driverName = "postgres"
		dsn = ensurePostgresBinaryParams(dsn)
	default:
		return nil, fmt.Errorf("unsupported driver: %s", driverName)
	}

	conn, err := sqlx.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	database := &DB{
		DB:     conn,
		Driver: dt,
	}

	if err := database.Migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	return database, nil
}

func ensurePostgresBinaryParams(dsn string) string {
	if strings.Contains(dsn, "binary_parameters=") {
		return dsn
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if strings.Contains(dsn, "?") {
			return dsn + "&binary_parameters=yes"
		}
		return dsn + "?binary_parameters=yes"
	}
	return strings.TrimSpace(dsn + " binary_parameters=yes")
}

// Migrate executes versioned goose migrations for the active database dialect.
func (db *DB) Migrate(ctx context.Context) error {
	var (
		dialect  goose.Dialect
		subDir   string
		legacyFn func(context.Context, *sql.DB) error
	)

	if db.Driver == DriverSQLite {
		dialect = goose.DialectSQLite3
		subDir = "migrations/sqlite"
		legacyFn = migrateLegacySQLite
	} else {
		dialect = goose.DialectPostgres
		subDir = "migrations/postgres"
		legacyFn = migrateLegacyPostgres
	}

	fsys, err := fs.Sub(migrationsFS, subDir)
	if err != nil {
		return fmt.Errorf("failed to load embedded migrations: %w", err)
	}

	legacyMigration := goose.NewGoMigration(
		2,
		&goose.GoFunc{RunDB: legacyFn},
		nil,
	)

	provider, err := goose.NewProvider(
		dialect,
		db.DB.DB,
		fsys,
		goose.WithGoMigrations(legacyMigration),
		goose.WithDisableGlobalRegistry(true),
		goose.WithLogger(goose.NopLogger()),
	)
	if err != nil {
		return fmt.Errorf("failed to initialize migration provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		return err
	}
	return nil
}

func migrateLegacySQLite(ctx context.Context, conn *sql.DB) error {
	_, _ = conn.ExecContext(ctx, "ALTER TABLE users ADD COLUMN tenant_id TEXT")
	_, _ = conn.ExecContext(ctx, "ALTER TABLE users ADD COLUMN token_version TEXT NOT NULL DEFAULT ''")
	_, _ = conn.ExecContext(ctx, "ALTER TABLE invites ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''")
	_, _ = conn.ExecContext(ctx, "ALTER TABLE refresh_tokens ADD COLUMN token_version TEXT NOT NULL DEFAULT ''")
	_, _ = conn.ExecContext(ctx, "ALTER TABLE collections ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''")
	_, _ = conn.ExecContext(ctx, "ALTER TABLE collections ADD COLUMN slug TEXT NOT NULL DEFAULT ''")
	_, _ = conn.ExecContext(ctx, "ALTER TABLE collections ADD COLUMN origin_urls TEXT NOT NULL DEFAULT '[]'")
	_, _ = conn.ExecContext(ctx, "ALTER TABLE collections ADD COLUMN auto_invite_shares BOOLEAN NOT NULL DEFAULT 0")

	//backfill invites.tenant_id from creator's primary tenant for legacy invites
	_, _ = conn.ExecContext(ctx, `UPDATE invites SET tenant_id = COALESCE((SELECT tenant_id FROM users WHERE users.id = invites.created_by), '') WHERE tenant_id = ''`)

	//if legacy sqlite invites table had unique on email alone, rebuild table to allow invites across multiple tenants
	var invitesSQL string
	_ = conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'invites'`).Scan(&invitesSQL)
	if strings.Contains(strings.ToUpper(invitesSQL), "UNIQUE") {
		rebuildInvites := `
		CREATE TABLE IF NOT EXISTS invites_new (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'member',
			manager_id TEXT,
			expires_at DATETIME NOT NULL,
			created_by TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (manager_id) REFERENCES users(id) ON DELETE SET NULL,
			FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE CASCADE
		);
		INSERT INTO invites_new (id, tenant_id, email, role, manager_id, expires_at, created_by, created_at)
			SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites;
		DROP TABLE invites;
		ALTER TABLE invites_new RENAME TO invites;
		`
		if _, err := conn.ExecContext(ctx, rebuildInvites); err != nil {
			return err
		}
	}

	_, _ = conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_invites_email_tenant ON invites(email, tenant_id)")
	_, _ = conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_collections_tenant ON collections(tenant_id)")
	_, _ = conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_collections_slug_unique ON collections(LOWER(slug)) WHERE slug != ''")
	_, _ = conn.ExecContext(ctx, `INSERT OR IGNORE INTO user_tenants (user_id, tenant_id, role, manager_id, created_at)
		SELECT id, tenant_id, role, manager_id, created_at FROM users WHERE tenant_id IS NOT NULL AND tenant_id != ''`)

	trialEnd := time.Now().UTC().Add(7 * 24 * time.Hour)
	_, _ = conn.ExecContext(ctx, `
		INSERT OR IGNORE INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at)
		SELECT DISTINCT tenant_id, 'starter', 'trialing', 10, ?
		FROM users WHERE tenant_id IS NOT NULL AND tenant_id != ''
	`, trialEnd)
	_, _ = conn.ExecContext(ctx, `
		INSERT OR IGNORE INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at)
		SELECT DISTINCT tenant_id, 'starter', 'trialing', 10, ?
		FROM collections WHERE tenant_id IS NOT NULL AND tenant_id != ''
	`, trialEnd)

	return nil
}

func migrateLegacyPostgres(ctx context.Context, conn *sql.DB) error {
	alterSchema := `
	ALTER TABLE users ADD COLUMN IF NOT EXISTS tenant_id TEXT;
	ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version TEXT NOT NULL DEFAULT '';
	ALTER TABLE invites ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE invites DROP CONSTRAINT IF EXISTS invites_email_key;
	UPDATE invites SET tenant_id = COALESCE((SELECT tenant_id FROM users WHERE users.id = invites.created_by), '') WHERE tenant_id = '';
	CREATE INDEX IF NOT EXISTS idx_invites_email_tenant ON invites(email, tenant_id);
	ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS token_version TEXT NOT NULL DEFAULT '';
	ALTER TABLE collections ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE collections ADD COLUMN IF NOT EXISTS slug TEXT NOT NULL DEFAULT '';
	ALTER TABLE collections ADD COLUMN IF NOT EXISTS origin_urls TEXT NOT NULL DEFAULT '[]';
	ALTER TABLE collections ADD COLUMN IF NOT EXISTS auto_invite_shares BOOLEAN NOT NULL DEFAULT FALSE;
	CREATE INDEX IF NOT EXISTS idx_collections_tenant ON collections(tenant_id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_collections_slug_unique ON collections(LOWER(slug)) WHERE slug != '';
	INSERT INTO user_tenants (user_id, tenant_id, role, manager_id, created_at)
		SELECT id, tenant_id, role, manager_id, created_at FROM users WHERE tenant_id IS NOT NULL AND tenant_id != ''
		ON CONFLICT (user_id, tenant_id) DO NOTHING;
	`
	if _, err := conn.ExecContext(ctx, alterSchema); err != nil {
		return err
	}

	trialEnd := time.Now().UTC().Add(7 * 24 * time.Hour)
	_, _ = conn.ExecContext(ctx, `
		INSERT INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at)
		SELECT DISTINCT tenant_id, 'starter', 'trialing', 10, $1
		FROM users WHERE tenant_id IS NOT NULL AND tenant_id != ''
		ON CONFLICT (tenant_id) DO NOTHING
	`, trialEnd)
	_, _ = conn.ExecContext(ctx, `
		INSERT INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at)
		SELECT DISTINCT tenant_id, 'starter', 'trialing', 10, $1
		FROM collections WHERE tenant_id IS NOT NULL AND tenant_id != ''
		ON CONFLICT (tenant_id) DO NOTHING
	`, trialEnd)

	//try enabling pgvector if available, but don't fail if extension isn't present
	_, _ = conn.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS vector;")
	var hasVector bool
	_ = conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'vector');").Scan(&hasVector)
	if hasVector {
		vecSchema := `
		CREATE TABLE IF NOT EXISTS record_embeddings (
			id TEXT PRIMARY KEY,
			record_id TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
			content_chunk TEXT NOT NULL,
			embedding vector(768) NOT NULL,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
		);
		`
		_, _ = conn.ExecContext(ctx, vecSchema)
	}

	return nil
}

