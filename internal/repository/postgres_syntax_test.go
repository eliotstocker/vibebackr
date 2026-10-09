package repository

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/jmoiron/sqlx"
)

func TestPostgresQueryNoMangledColons(t *testing.T) {
	r := &sqlRepo{
		database: &db.DB{
			Driver: db.DriverPostgres,
		},
	}
	user := &domain.User{
		ID:    "user-1",
		Email: "test@example.com",
		Role:  domain.RoleMember,
	}

	// 1. Test buildRecordACLFilter for ListRecords / GetRecord
	cte, params := r.buildRecordACLFilter(user)
	query := cte + " AND r.collection_id = :collection_id ORDER BY r.created_at DESC"
	params["collection_id"] = "col-1"

	boundQuery, _, err := sqlx.Named(query, params)
	if err != nil {
		t.Fatalf("Named query failed to compile: %v", err)
	}

	if strings.Contains(boundQuery, ":jsonb") || strings.Contains(boundQuery, ":vector") {
		t.Fatalf("boundQuery contains mangled sqlx colon artifact: %s", boundQuery)
	}

	rebound := sqlx.Rebind(sqlx.DOLLAR, boundQuery)
	if strings.Contains(rebound, ":jsonb") {
		t.Fatalf("rebound contains :jsonb: %s", rebound)
	}

	shareCheck := "(CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' AND jsonb_typeof(CAST(r.access_policy AS jsonb)->'shares') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(CAST(r.access_policy AS jsonb)->'shares') WHERE value = :user_email) ELSE false END) OR (CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' THEN CAST(r.access_policy AS jsonb)->>'visibility' ELSE 'private' END = 'public')"
	colQuery := fmt.Sprintf(`
	SELECT DISTINCT c.id, c.tenant_id, c.name, c.default_visibility, c.allowed_domains, c.origin_urls, c.vector_search_enabled, c.created_at
	FROM collections c
	WHERE (:user_tenant_id != '' AND c.tenant_id = :user_tenant_id)
	   OR c.default_visibility = 'public'
	   OR EXISTS (
	       SELECT 1 FROM records r
	       WHERE r.collection_id = c.id
	         AND (r.created_by = :user_id OR %s)
	   )
	ORDER BY c.name ASC
	`, shareCheck)

	colParams := map[string]interface{}{
		"user_id":        user.ID,
		"user_email":     user.Email,
		"user_tenant_id": "tenant-1",
	}

	colBound, _, err := sqlx.Named(colQuery, colParams)
	if err != nil {
		t.Fatalf("ListCollections named query failed to compile: %v", err)
	}
	if strings.Contains(colBound, ":jsonb") {
		t.Fatalf("colBound contains :jsonb: %s", colBound)
	}
}

func TestPostgresLiveListRecords(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping live Postgres test; set TEST_DATABASE_URL to run")
	}

	database, err := db.Connect("postgres", dsn)
	if err != nil {
		t.Fatalf("failed connecting to Postgres: %v", err)
	}
	defer database.Close()

	repo := New(database)
	user := &domain.User{
		ID:    "2ef5d964-82df-4c5e-823f-fbc9bb1d26ae",
		Email: "eliot.stocker@gmail.com",
		Role:  domain.RoleAdmin,
	}

	records, err := repo.ListRecords(context.Background(), "d0615940-b483-4e0a-9409-737eb9e0b2dc", user)
	if err != nil {
		t.Fatalf("ListRecords failed on live Postgres: %v", err)
	}
	t.Logf("Found %d records", len(records))
}

