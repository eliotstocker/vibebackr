package db_test

import (
	"context"
	"testing"

	"github.com/eliotstocker/vibebackr/internal/db"
)

func TestConnectSQLite(t *testing.T) {
	database, err := db.Connect("sqlite", "file:dbtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed connecting to SQLite: %v", err)
	}
	defer database.Close()

	if database.Driver != db.DriverSQLite {
		t.Errorf("expected sqlite driver, got %s", database.Driver)
	}

	// Verify tables were created (including billing tables)
	var tableCount int
	err = database.GetContext(context.Background(), &tableCount, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('users', 'user_tenants', 'records', 'collections', 'invites', 'oauth_clients', 'tenant_billing', 'vouchers', 'goose_db_version')")
	if err != nil || tableCount < 9 {
		t.Fatalf("expected at least 9 tables created, got %d, err: %v", tableCount, err)
	}
}

func TestLegacySchemaMigration(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:legacydbtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed connecting to SQLite: %v", err)
	}
	defer database.Close()

	// Simulate pre-goose legacy schema before user_tenants and invites.tenant_id existed
	legacySetup := `
	DROP TABLE IF EXISTS goose_db_version;
	DROP TABLE IF EXISTS user_tenants;
	DROP TABLE IF EXISTS invites;
	CREATE TABLE invites (
		id TEXT PRIMARY KEY,
		email TEXT UNIQUE NOT NULL,
		role TEXT NOT NULL DEFAULT 'member',
		manager_id TEXT,
		expires_at DATETIME NOT NULL,
		created_by TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	INSERT INTO users (id, tenant_id, email, name, role) VALUES ('u1', 't1', 'owner1@test.com', 'Owner 1', 'admin');
	INSERT INTO users (id, tenant_id, email, name, role) VALUES ('u2', 't2', 'owner2@test.com', 'Owner 2', 'admin');
	INSERT INTO invites (id, email, role, expires_at, created_by) VALUES ('inv1', 'shared@test.com', 'member', '2030-01-01T00:00:00Z', 'u1');
	`
	if _, err := database.ExecContext(ctx, legacySetup); err != nil {
		t.Fatalf("failed setting up legacy schema: %v", err)
	}

	// Re-run Migrate on the legacy database
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Migrate on legacy DB failed: %v", err)
	}

	// 1. Verify user_tenants was backfilled from users
	var utCount int
	if err := database.GetContext(ctx, &utCount, "SELECT count(*) FROM user_tenants"); err != nil || utCount != 2 {
		t.Fatalf("expected 2 backfilled user_tenants rows, got %d (err: %v)", utCount, err)
	}

	// 2. Verify legacy invite had tenant_id backfilled from creator u1 ('t1')
	var invTenant string
	if err := database.GetContext(ctx, &invTenant, "SELECT tenant_id FROM invites WHERE id = 'inv1'"); err != nil || invTenant != "t1" {
		t.Fatalf("expected backfilled invite tenant_id 't1', got %q (err: %v)", invTenant, err)
	}

	// 3. Verify another tenant ('t2') can now also invite 'shared@test.com' without UNIQUE(email) failing
	if _, err := database.ExecContext(ctx, `INSERT INTO invites (id, tenant_id, email, role, expires_at, created_by) VALUES ('inv2', 't2', 'shared@test.com', 'manager', '2030-01-01T00:00:00Z', 'u2')`); err != nil {
		t.Fatalf("expected second tenant invite for same email to succeed after migration, got err: %v", err)
	}
}

func TestConnectUnsupportedDriver(t *testing.T) {
	_, err := db.Connect("oracle", "some-dsn")
	if err == nil {
		t.Errorf("expected error for unsupported driver")
	}
}
