package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrNotFound               = errors.New("resource not found")
	ErrForbidden              = errors.New("access denied by ACL / hierarchy policy")
	ErrSlugTaken              = errors.New("project slug is already in use")
	ErrCollectionSizeExceeded = errors.New("collection record limit exceeded")
	ErrRecordTooLarge         = errors.New("record payload exceeds maximum allowed size")
)

const (
	repoCacheTTL                = 5 * time.Second
	DefaultMaxCollectionRecords = 500
	DefaultMaxRecordBytes       = 64 * 1024
)

type cachedUserEntry struct {
	user      domain.User
	expiresAt time.Time
}

type cachedCollectionEntry struct {
	col       domain.Collection
	expiresAt time.Time
}

// WriteGuard is an optional hook (e.g. from the billing module) that validates whether
// a tenant is permitted to perform write operations or create a new collection.
type WriteGuard func(ctx context.Context, tenantID string, isCreatingCollection bool) error

// RecordIndexHook is an optional callback invoked after a record is created or updated.
type RecordIndexHook func(ctx context.Context, rec *domain.Record)

// Repository defines data access operations.
type Repository interface {
	SetWriteGuard(guard WriteGuard)
	SetRecordIndexHook(hook RecordIndexHook)
	SetCollectionLimits(maxRecords int, maxRecordBytes int)

	// User operations
	CreateUser(ctx context.Context, u *domain.User) error
	GetUserByID(ctx context.Context, id string) (*domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	ListUsers(ctx context.Context) ([]domain.User, error)
	BatchUpsertUsers(ctx context.Context, users []domain.User) error
	UpdateUser(ctx context.Context, u *domain.User) error

	// Tenant membership operations
	AddTenantMembership(ctx context.Context, m *domain.TenantMembership) error
	RemoveTenantMembership(ctx context.Context, userID, tenantID string) error
	ListUserTenants(ctx context.Context, userID string) ([]domain.TenantMembership, error)

	// Invite operations
	CreateInvite(ctx context.Context, inv *domain.Invite) error
	GetInviteByID(ctx context.Context, id string) (*domain.Invite, error)
	GetInviteByEmail(ctx context.Context, email string) (*domain.Invite, error)
	ListInvitesByEmail(ctx context.Context, email string) ([]domain.Invite, error)
	ListInvitesByTenant(ctx context.Context, tenantID string) ([]domain.Invite, error)
	BatchCreateInvites(ctx context.Context, invites []domain.Invite) error
	DeleteInvite(ctx context.Context, id string) error

	// Collection operations
	CreateCollection(ctx context.Context, col *domain.Collection) error
	GetCollection(ctx context.Context, id string) (*domain.Collection, error)
	ListCollections(ctx context.Context) ([]domain.Collection, error)
	ListCollectionsForUser(ctx context.Context, user *domain.User) ([]domain.Collection, error)
	UpdateCollection(ctx context.Context, col *domain.Collection) error
	DeleteCollection(ctx context.Context, id string) error

	// Record operations with ACL enforcement
	CreateRecord(ctx context.Context, rec *domain.Record) error
	GetRecord(ctx context.Context, id string, user *domain.User) (*domain.Record, error)
	ListRecords(ctx context.Context, collectionID string, user *domain.User) ([]domain.Record, error)
	UpdateRecord(ctx context.Context, rec *domain.Record, user *domain.User) error
	DeleteRecord(ctx context.Context, id string, user *domain.User) error

	// OAuth & PKCE & Refresh Tokens
	CreateOAuthClient(ctx context.Context, client *domain.OAuthClient) error
	GetOAuthClient(ctx context.Context, id string) (*domain.OAuthClient, error)
	CreateAuthCode(ctx context.Context, code *domain.AuthCode) error
	ConsumeAuthCode(ctx context.Context, code string) (*domain.AuthCode, error)
	CreateRefreshToken(ctx context.Context, token *domain.RefreshToken) error
	GetRefreshTokenByHash(ctx context.Context, tokenHash string) (*domain.RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, id string) error
	RevokeAllUserRefreshTokens(ctx context.Context, userID string) error
	UpdateUserTokenVersion(ctx context.Context, userID string, tokenVersion string) error
}

type sqlRepo struct {
	database                *db.DB
	writeGuard              WriteGuard
	recordIndexHook         RecordIndexHook
	maxRecordsPerCollection int
	maxRecordBytes          int
	cacheMu                 sync.RWMutex
	userCache               map[string]cachedUserEntry
	colCache                map[string]cachedCollectionEntry
}

// New creates a new Repository implementation backed by sqlx.
func New(database *db.DB) Repository {
	return &sqlRepo{
		database:  database,
		userCache: make(map[string]cachedUserEntry),
		colCache:  make(map[string]cachedCollectionEntry),
	}
}

func cloneUser(u domain.User) *domain.User {
	cp := u
	if u.TenantID != nil {
		tid := *u.TenantID
		cp.TenantID = &tid
	}
	if u.ManagerID != nil {
		mid := *u.ManagerID
		cp.ManagerID = &mid
	}
	return &cp
}

func cloneCollection(c domain.Collection) *domain.Collection {
	cp := c
	if c.AllowedDomains != nil {
		cp.AllowedDomains = append(domain.JSONStrings(nil), c.AllowedDomains...)
	}
	if c.OriginURLs != nil {
		cp.OriginURLs = append(domain.JSONStrings(nil), c.OriginURLs...)
	}
	return &cp
}

func (r *sqlRepo) invalidateUserCache() {
	r.cacheMu.Lock()
	r.userCache = make(map[string]cachedUserEntry)
	r.cacheMu.Unlock()
}

func (r *sqlRepo) invalidateColCache() {
	r.cacheMu.Lock()
	r.colCache = make(map[string]cachedCollectionEntry)
	r.cacheMu.Unlock()
}

func (r *sqlRepo) SetWriteGuard(guard WriteGuard) {
	r.writeGuard = guard
}

func (r *sqlRepo) SetRecordIndexHook(hook RecordIndexHook) {
	r.recordIndexHook = hook
}

func (r *sqlRepo) SetCollectionLimits(maxRecords int, maxRecordBytes int) {
	r.maxRecordsPerCollection = maxRecords
	r.maxRecordBytes = maxRecordBytes
}

func (r *sqlRepo) checkRecordByteSize(rec *domain.Record) error {
	if r.maxRecordBytes <= 0 || rec == nil {
		return nil
	}
	size := len(rec.Title) + len(rec.Content) + len(rec.Data)
	if size > r.maxRecordBytes {
		return fmt.Errorf("%w: %d/%d bytes", ErrRecordTooLarge, size, r.maxRecordBytes)
	}
	return nil
}

func (r *sqlRepo) checkWrite(ctx context.Context, tenantID string, isCreatingCollection bool) error {
	if r.writeGuard == nil || tenantID == "" {
		return nil
	}
	return r.writeGuard(ctx, tenantID, isCreatingCollection)
}

func (r *sqlRepo) CreateUser(ctx context.Context, u *domain.User) error {
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	if u.TokenVersion == "" {
		u.TokenVersion = uuid.NewString()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	query := `
	INSERT INTO users (id, tenant_id, email, name, role, manager_id, token_version, created_at)
	VALUES (:id, :tenant_id, :email, :name, :role, :manager_id, :token_version, :created_at)
	`
	if _, err := r.database.NamedExecContext(ctx, query, u); err != nil {
		return err
	}
	r.invalidateUserCache()
	if u.TenantID != nil && *u.TenantID != "" {
		role := u.Role
		if role == "" {
			role = domain.RoleMember
		}
		_ = r.AddTenantMembership(ctx, &domain.TenantMembership{
			UserID:    u.ID,
			TenantID:  *u.TenantID,
			Role:      role,
			ManagerID: u.ManagerID,
			CreatedAt: u.CreatedAt,
		})
	}
	return nil
}

func (r *sqlRepo) GetUserByID(ctx context.Context, id string) (*domain.User, error) {
	now := time.Now()
	r.cacheMu.RLock()
	if entry, ok := r.userCache[id]; ok && now.Before(entry.expiresAt) {
		r.cacheMu.RUnlock()
		return cloneUser(entry.user), nil
	}
	r.cacheMu.RUnlock()

	var u domain.User
	query := "SELECT id, tenant_id, email, name, role, manager_id, token_version, created_at FROM users WHERE id = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, tenant_id, email, name, role, manager_id, token_version, created_at FROM users WHERE id = $1"
	}
	if err := r.database.GetContext(ctx, &u, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	r.cacheMu.Lock()
	r.userCache[id] = cachedUserEntry{user: *cloneUser(u), expiresAt: now.Add(repoCacheTTL)}
	r.cacheMu.Unlock()

	return &u, nil
}

func (r *sqlRepo) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	var u domain.User
	query := "SELECT id, tenant_id, email, name, role, manager_id, token_version, created_at FROM users WHERE email = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, tenant_id, email, name, role, manager_id, token_version, created_at FROM users WHERE email = $1"
	}
	if err := r.database.GetContext(ctx, &u, query, email); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (r *sqlRepo) ListUsers(ctx context.Context) ([]domain.User, error) {
	var users []domain.User
	err := r.database.SelectContext(ctx, &users, "SELECT id, tenant_id, email, name, role, manager_id, token_version, created_at FROM users ORDER BY name ASC")
	return users, err
}

func (r *sqlRepo) BatchUpsertUsers(ctx context.Context, users []domain.User) error {
	tx, err := r.database.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, u := range users {
		if u.ID == "" {
			u.ID = uuid.NewString()
		}
		if u.TokenVersion == "" {
			u.TokenVersion = uuid.NewString()
		}
		if u.CreatedAt.IsZero() {
			u.CreatedAt = now
		}

		var query string
		if r.database.Driver == db.DriverSQLite {
			query = `
			INSERT INTO users (id, tenant_id, email, name, role, manager_id, token_version, created_at)
			VALUES (:id, :tenant_id, :email, :name, :role, :manager_id, :token_version, :created_at)
			ON CONFLICT(email) DO UPDATE SET
				name = CASE WHEN COALESCE(users.tenant_id, '') = COALESCE(excluded.tenant_id, '') THEN excluded.name ELSE users.name END,
				role = CASE WHEN COALESCE(users.tenant_id, '') = COALESCE(excluded.tenant_id, '') THEN excluded.role ELSE users.role END,
				manager_id = CASE WHEN COALESCE(users.tenant_id, '') = COALESCE(excluded.tenant_id, '') THEN excluded.manager_id ELSE users.manager_id END
			`
		} else {
			query = `
			INSERT INTO users (id, tenant_id, email, name, role, manager_id, token_version, created_at)
			VALUES (:id, :tenant_id, :email, :name, :role, :manager_id, :token_version, :created_at)
			ON CONFLICT(email) DO UPDATE SET
				name = CASE WHEN COALESCE(users.tenant_id, '') = COALESCE(EXCLUDED.tenant_id, '') THEN EXCLUDED.name ELSE users.name END,
				role = CASE WHEN COALESCE(users.tenant_id, '') = COALESCE(EXCLUDED.tenant_id, '') THEN EXCLUDED.role ELSE users.role END,
				manager_id = CASE WHEN COALESCE(users.tenant_id, '') = COALESCE(EXCLUDED.tenant_id, '') THEN EXCLUDED.manager_id ELSE users.manager_id END
			`
		}

		if _, err := tx.NamedExecContext(ctx, query, u); err != nil {
			return fmt.Errorf("failed upserting user %s: %w", u.Email, err)
		}

		if u.TenantID != nil && *u.TenantID != "" {
			var resolvedID string
			idQuery := "SELECT id FROM users WHERE email = ?"
			if r.database.Driver == db.DriverPostgres {
				idQuery = "SELECT id FROM users WHERE email = $1"
			}
			if err := tx.GetContext(ctx, &resolvedID, idQuery, u.Email); err == nil && resolvedID != "" {
				m := domain.TenantMembership{
					UserID:    resolvedID,
					TenantID:  *u.TenantID,
					Role:      u.Role,
					ManagerID: u.ManagerID,
					CreatedAt: now,
				}
				var mQuery string
				if r.database.Driver == db.DriverSQLite {
					mQuery = `
					INSERT INTO user_tenants (user_id, tenant_id, role, manager_id, created_at)
					VALUES (:user_id, :tenant_id, :role, :manager_id, :created_at)
					ON CONFLICT(user_id, tenant_id) DO UPDATE SET
						role = excluded.role,
						manager_id = COALESCE(excluded.manager_id, user_tenants.manager_id)
					`
				} else {
					mQuery = `
					INSERT INTO user_tenants (user_id, tenant_id, role, manager_id, created_at)
					VALUES (:user_id, :tenant_id, :role, :manager_id, :created_at)
					ON CONFLICT(user_id, tenant_id) DO UPDATE SET
						role = EXCLUDED.role,
						manager_id = COALESCE(EXCLUDED.manager_id, user_tenants.manager_id)
					`
				}
				_, _ = tx.NamedExecContext(ctx, mQuery, m)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	r.invalidateUserCache()
	return nil
}

func (r *sqlRepo) UpdateUser(ctx context.Context, u *domain.User) error {
	query := `
	UPDATE users
	SET tenant_id = :tenant_id,
	    email = :email,
	    name = :name,
	    role = :role,
	    manager_id = :manager_id,
	    token_version = :token_version
	WHERE id = :id
	`
	res, err := r.database.NamedExecContext(ctx, query, u)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	r.invalidateUserCache()
	if u.TenantID != nil && *u.TenantID != "" {
		role := u.Role
		if role == "" {
			role = domain.RoleAdmin
		}
		_ = r.AddTenantMembership(ctx, &domain.TenantMembership{
			UserID:    u.ID,
			TenantID:  *u.TenantID,
			Role:      role,
			ManagerID: u.ManagerID,
			CreatedAt: time.Now().UTC(),
		})
	}
	return nil
}

func (r *sqlRepo) AddTenantMembership(ctx context.Context, m *domain.TenantMembership) error {
	if m.UserID == "" || m.TenantID == "" {
		return nil
	}
	if m.Role == "" {
		m.Role = domain.RoleMember
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	var query string
	if r.database.Driver == db.DriverSQLite {
		query = `
		INSERT INTO user_tenants (user_id, tenant_id, role, manager_id, created_at)
		VALUES (:user_id, :tenant_id, :role, :manager_id, :created_at)
		ON CONFLICT(user_id, tenant_id) DO UPDATE SET
			role = excluded.role,
			manager_id = COALESCE(excluded.manager_id, user_tenants.manager_id)
		`
	} else {
		query = `
		INSERT INTO user_tenants (user_id, tenant_id, role, manager_id, created_at)
		VALUES (:user_id, :tenant_id, :role, :manager_id, :created_at)
		ON CONFLICT(user_id, tenant_id) DO UPDATE SET
			role = EXCLUDED.role,
			manager_id = COALESCE(EXCLUDED.manager_id, user_tenants.manager_id)
		`
	}
	_, err := r.database.NamedExecContext(ctx, query, m)
	return err
}

func (r *sqlRepo) RemoveTenantMembership(ctx context.Context, userID, tenantID string) error {
	query := "DELETE FROM user_tenants WHERE user_id = ? AND tenant_id = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "DELETE FROM user_tenants WHERE user_id = $1 AND tenant_id = $2"
	}
	res, err := r.database.ExecContext(ctx, query, userID, tenantID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqlRepo) ListUserTenants(ctx context.Context, userID string) ([]domain.TenantMembership, error) {
	memberships := []domain.TenantMembership{}
	query := "SELECT user_id, tenant_id, role, manager_id, created_at FROM user_tenants WHERE user_id = ? ORDER BY created_at ASC"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT user_id, tenant_id, role, manager_id, created_at FROM user_tenants WHERE user_id = $1 ORDER BY created_at ASC"
	}
	err := r.database.SelectContext(ctx, &memberships, query, userID)
	return memberships, err
}

func (r *sqlRepo) CreateInvite(ctx context.Context, inv *domain.Invite) error {
	if inv.ID == "" {
		inv.ID = uuid.NewString()
	}
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now().UTC()
	}
	if inv.ExpiresAt.IsZero() {
		inv.ExpiresAt = inv.CreatedAt.Add(7 * 24 * time.Hour)
	}
	if inv.Role == "" {
		inv.Role = domain.RoleMember
	}
	if inv.TenantID == "" && inv.CreatedBy != "" {
		if creator, err := r.GetUserByID(ctx, inv.CreatedBy); err == nil && creator.TenantID != nil {
			inv.TenantID = *creator.TenantID
		}
	}
	query := `
	INSERT INTO invites (id, tenant_id, email, role, manager_id, expires_at, created_by, created_at)
	VALUES (:id, :tenant_id, :email, :role, :manager_id, :expires_at, :created_by, :created_at)
	`
	if _, err := r.database.NamedExecContext(ctx, query, inv); err != nil {
		return err
	}
	// If the invited user already exists, immediately bind their tenant membership
	if inv.TenantID != "" && inv.Email != "" {
		if existingUser, uErr := r.GetUserByEmail(ctx, inv.Email); uErr == nil && existingUser != nil {
			_ = r.AddTenantMembership(ctx, &domain.TenantMembership{
				UserID:    existingUser.ID,
				TenantID:  inv.TenantID,
				Role:      inv.Role,
				ManagerID: inv.ManagerID,
				CreatedAt: inv.CreatedAt,
			})
		}
	}
	return nil
}

func (r *sqlRepo) GetInviteByID(ctx context.Context, id string) (*domain.Invite, error) {
	var inv domain.Invite
	query := "SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites WHERE id = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites WHERE id = $1"
	}
	if err := r.database.GetContext(ctx, &inv, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &inv, nil
}

func (r *sqlRepo) GetInviteByEmail(ctx context.Context, email string) (*domain.Invite, error) {
	var inv domain.Invite
	query := "SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites WHERE LOWER(email) = LOWER(?) AND expires_at > ? ORDER BY created_at DESC LIMIT 1"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites WHERE LOWER(email) = LOWER($1) AND expires_at > $2 ORDER BY created_at DESC LIMIT 1"
	}
	if err := r.database.GetContext(ctx, &inv, query, email, time.Now().UTC()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &inv, nil
}

func (r *sqlRepo) ListInvitesByEmail(ctx context.Context, email string) ([]domain.Invite, error) {
	invites := []domain.Invite{}
	query := "SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites WHERE LOWER(email) = LOWER(?) AND expires_at > ? ORDER BY created_at ASC"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites WHERE LOWER(email) = LOWER($1) AND expires_at > $2 ORDER BY created_at ASC"
	}
	err := r.database.SelectContext(ctx, &invites, query, email, time.Now().UTC())
	return invites, err
}

func (r *sqlRepo) ListInvitesByTenant(ctx context.Context, tenantID string) ([]domain.Invite, error) {
	invites := []domain.Invite{}
	query := "SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites WHERE tenant_id = ? AND expires_at > ? ORDER BY created_at DESC"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, tenant_id, email, role, manager_id, expires_at, created_by, created_at FROM invites WHERE tenant_id = $1 AND expires_at > $2 ORDER BY created_at DESC"
	}
	err := r.database.SelectContext(ctx, &invites, query, tenantID, time.Now().UTC())
	return invites, err
}

func (r *sqlRepo) BatchCreateInvites(ctx context.Context, invites []domain.Invite) error {
	tx, err := r.database.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for i := range invites {
		inv := &invites[i]
		if inv.ID == "" {
			inv.ID = uuid.NewString()
		}
		if inv.CreatedAt.IsZero() {
			inv.CreatedAt = now
		}
		if inv.ExpiresAt.IsZero() {
			inv.ExpiresAt = now.Add(7 * 24 * time.Hour)
		}
		if inv.Role == "" {
			inv.Role = domain.RoleMember
		}
		query := `
		INSERT INTO invites (id, tenant_id, email, role, manager_id, expires_at, created_by, created_at)
		VALUES (:id, :tenant_id, :email, :role, :manager_id, :expires_at, :created_by, :created_at)
		`
		if _, err := tx.NamedExecContext(ctx, query, inv); err != nil {
			return fmt.Errorf("failed creating invite for %s: %w", inv.Email, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	for _, inv := range invites {
		if inv.TenantID != "" && inv.Email != "" {
			if existingUser, uErr := r.GetUserByEmail(ctx, inv.Email); uErr == nil && existingUser != nil {
				_ = r.AddTenantMembership(ctx, &domain.TenantMembership{
					UserID:    existingUser.ID,
					TenantID:  inv.TenantID,
					Role:      inv.Role,
					ManagerID: inv.ManagerID,
					CreatedAt: inv.CreatedAt,
				})
			}
		}
	}
	return nil
}

func (r *sqlRepo) DeleteInvite(ctx context.Context, id string) error {
	query := "DELETE FROM invites WHERE id = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "DELETE FROM invites WHERE id = $1"
	}
	_, err := r.database.ExecContext(ctx, query, id)
	return err
}

func (r *sqlRepo) checkSlugAvailable(ctx context.Context, slug, excludeID string) error {
	if slug == "" {
		return nil
	}
	var exists bool
	query := `SELECT EXISTS(
		SELECT 1 FROM collections
		WHERE id != ?
		  AND ((slug != '' AND LOWER(slug) = LOWER(?)) OR LOWER(id) = LOWER(?) OR REPLACE(LOWER(id), '-', '') = LOWER(?))
	)`
	if r.database.Driver == db.DriverPostgres {
		query = `SELECT EXISTS(
			SELECT 1 FROM collections
			WHERE id != $1
			  AND ((slug != '' AND LOWER(slug) = LOWER($2)) OR LOWER(id) = LOWER($3) OR REPLACE(LOWER(id), '-', '') = LOWER($4))
		)`
	}
	if err := r.database.GetContext(ctx, &exists, query, excludeID, slug, slug, slug); err != nil {
		return err
	}
	if exists {
		return ErrSlugTaken
	}
	return nil
}

func (r *sqlRepo) CreateCollection(ctx context.Context, col *domain.Collection) error {
	if err := r.checkWrite(ctx, col.TenantID, true); err != nil {
		return err
	}
	normSlug, err := domain.ValidateSlug(col.Slug)
	if err != nil {
		return err
	}
	col.Slug = normSlug

	if col.ID == "" {
		col.ID = uuid.NewString()
	}
	if err := r.checkSlugAvailable(ctx, col.Slug, col.ID); err != nil {
		return err
	}
	if col.CreatedAt.IsZero() {
		col.CreatedAt = time.Now().UTC()
	}
	if col.DefaultVisibility == "" {
		col.DefaultVisibility = domain.VisibilityPrivate
	}
	query := `
	INSERT INTO collections (id, tenant_id, name, slug, default_visibility, allowed_domains, origin_urls, vector_search_enabled, auto_invite_shares, created_at)
	VALUES (:id, :tenant_id, :name, :slug, :default_visibility, :allowed_domains, :origin_urls, :vector_search_enabled, :auto_invite_shares, :created_at)
	`
	_, err = r.database.NamedExecContext(ctx, query, col)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return ErrSlugTaken
	}
	if err == nil {
		r.invalidateColCache()
	}
	return err
}

func (r *sqlRepo) GetCollection(ctx context.Context, idOrSlug string) (*domain.Collection, error) {
	lookup := strings.TrimSpace(idOrSlug)
	if lookup == "" {
		return nil, ErrNotFound
	}
	normKey := strings.ToLower(lookup)
	now := time.Now()

	r.cacheMu.RLock()
	if entry, ok := r.colCache[normKey]; ok && now.Before(entry.expiresAt) {
		r.cacheMu.RUnlock()
		return cloneCollection(entry.col), nil
	}
	r.cacheMu.RUnlock()

	var col domain.Collection
	query := `SELECT id, tenant_id, name, slug, default_visibility, allowed_domains, origin_urls, vector_search_enabled, auto_invite_shares, created_at
	FROM collections
	WHERE id = ? OR (slug != '' AND LOWER(slug) = LOWER(?)) OR REPLACE(LOWER(id), '-', '') = LOWER(?)
	LIMIT 1`
	if r.database.Driver == db.DriverPostgres {
		query = `SELECT id, tenant_id, name, slug, default_visibility, allowed_domains, origin_urls, vector_search_enabled, auto_invite_shares, created_at
		FROM collections
		WHERE id = $1 OR (slug != '' AND LOWER(slug) = LOWER($2)) OR REPLACE(LOWER(id), '-', '') = LOWER($3)
		LIMIT 1`
	}
	if err := r.database.GetContext(ctx, &col, query, lookup, lookup, lookup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	entry := cachedCollectionEntry{col: *cloneCollection(col), expiresAt: now.Add(repoCacheTTL)}
	r.cacheMu.Lock()
	r.colCache[normKey] = entry
	r.colCache[strings.ToLower(col.ID)] = entry
	if col.Slug != "" {
		r.colCache[strings.ToLower(col.Slug)] = entry
	}
	r.cacheMu.Unlock()

	return &col, nil
}

func (r *sqlRepo) ListCollections(ctx context.Context) ([]domain.Collection, error) {
	var collections []domain.Collection
	err := r.database.SelectContext(ctx, &collections, "SELECT id, tenant_id, name, slug, default_visibility, allowed_domains, origin_urls, vector_search_enabled, auto_invite_shares, created_at FROM collections ORDER BY name ASC")
	return collections, err
}

func (r *sqlRepo) ListCollectionsForUser(ctx context.Context, user *domain.User) ([]domain.Collection, error) {
	collections := []domain.Collection{}

	if user == nil {
		return collections, nil
	}

	// System admin without a tenant restriction sees only unscoped collections
	if user.TenantID == nil || *user.TenantID == "" {
		if user.Role == domain.RoleAdmin {
			err := r.database.SelectContext(ctx, &collections, "SELECT id, tenant_id, name, slug, default_visibility, allowed_domains, origin_urls, vector_search_enabled, auto_invite_shares, created_at FROM collections WHERE tenant_id = '' ORDER BY name ASC")
			return collections, err
		}
		return collections, nil
	}

	query := "SELECT id, tenant_id, name, slug, default_visibility, allowed_domains, origin_urls, vector_search_enabled, auto_invite_shares, created_at FROM collections WHERE tenant_id = ? ORDER BY name ASC"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, tenant_id, name, slug, default_visibility, allowed_domains, origin_urls, vector_search_enabled, auto_invite_shares, created_at FROM collections WHERE tenant_id = $1 ORDER BY name ASC"
	}

	if err := r.database.SelectContext(ctx, &collections, query, *user.TenantID); err != nil {
		return nil, err
	}
	return collections, nil
}

func (r *sqlRepo) UpdateCollection(ctx context.Context, col *domain.Collection) error {
	if err := r.checkWrite(ctx, col.TenantID, false); err != nil {
		return err
	}
	normSlug, err := domain.ValidateSlug(col.Slug)
	if err != nil {
		return err
	}
	col.Slug = normSlug
	if err := r.checkSlugAvailable(ctx, col.Slug, col.ID); err != nil {
		return err
	}

	query := `
	UPDATE collections
	SET name = :name,
	    slug = :slug,
	    tenant_id = :tenant_id,
	    default_visibility = :default_visibility,
	    allowed_domains = :allowed_domains,
	    origin_urls = :origin_urls,
	    vector_search_enabled = :vector_search_enabled,
	    auto_invite_shares = :auto_invite_shares
	WHERE id = :id
	`
	res, err := r.database.NamedExecContext(ctx, query, col)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrSlugTaken
		}
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	r.invalidateColCache()
	if col.AutoInviteShares {
		var existingRecords []domain.Record
		recQuery := "SELECT id, collection_id, created_by, access_policy FROM records WHERE collection_id = ?"
		if r.database.Driver == db.DriverPostgres {
			recQuery = "SELECT id, collection_id, created_by, access_policy FROM records WHERE collection_id = $1"
		}
		if err := r.database.SelectContext(ctx, &existingRecords, recQuery, col.ID); err == nil {
			for _, rec := range existingRecords {
				if len(rec.AccessPolicy.Shares) > 0 {
					r.autoInviteRecordShares(ctx, col.ID, rec.AccessPolicy.Shares, rec.CreatedBy)
				}
			}
		}
	}
	return nil
}

func (r *sqlRepo) DeleteCollection(ctx context.Context, idOrSlug string) error {
	query := `DELETE FROM collections WHERE id = ? OR (slug != '' AND LOWER(slug) = LOWER(?))`
	if r.database.DriverName() == "postgres" {
		query = `DELETE FROM collections WHERE id = $1 OR (slug != '' AND LOWER(slug) = LOWER($2))`
	}
	res, err := r.database.ExecContext(ctx, query, idOrSlug, idOrSlug)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	r.invalidateColCache()
	return nil
}

// autoInviteRecordShares checks if the collection has auto_invite_shares enabled, and if so,
// automatically creates pending Invites (for non-users) or adds TenantMembership (for existing users not yet in the tenant).
func (r *sqlRepo) autoInviteRecordShares(ctx context.Context, collectionID string, shares []string, actorID string) {
	if len(shares) == 0 {
		return
	}
	col, err := r.GetCollection(ctx, collectionID)
	if err != nil || col == nil || !col.AutoInviteShares {
		return
	}

	tenantID := col.TenantID
	if tenantID == "" && actorID != "" {
		if actor, aErr := r.GetUserByID(ctx, actorID); aErr == nil && actor != nil && actor.TenantID != nil {
			tenantID = *actor.TenantID
		}
	}

	seen := make(map[string]bool, len(shares))
	for _, rawEmail := range shares {
		email := strings.TrimSpace(rawEmail)
		if email == "" || seen[strings.ToLower(email)] {
			continue
		}
		seen[strings.ToLower(email)] = true

		// Check if user already exists
		existingUser, uErr := r.GetUserByEmail(ctx, email)
		if uErr == nil && existingUser != nil {
			if tenantID != "" {
				inTenant := existingUser.TenantID != nil && *existingUser.TenantID == tenantID
				if !inTenant {
					if memberships, mErr := r.ListUserTenants(ctx, existingUser.ID); mErr == nil {
						for _, m := range memberships {
							if m.TenantID == tenantID {
								inTenant = true
								break
							}
						}
					}
				}
				if !inTenant {
					_ = r.AddTenantMembership(ctx, &domain.TenantMembership{
						UserID:    existingUser.ID,
						TenantID:  tenantID,
						Role:      domain.RoleMember,
						CreatedAt: time.Now().UTC(),
					})
				}
			}
			continue
		}

		// User does not exist yet: check if an active invite already exists for this email and tenant
		hasInvite := false
		if invites, iErr := r.ListInvitesByEmail(ctx, email); iErr == nil {
			for _, inv := range invites {
				if inv.TenantID == tenantID {
					hasInvite = true
					break
				}
			}
		}
		if !hasInvite {
			_ = r.CreateInvite(ctx, &domain.Invite{
				TenantID:  tenantID,
				Email:     email,
				Role:      domain.RoleMember,
				CreatedBy: actorID,
			})
		}
	}
}

func (r *sqlRepo) CreateRecord(ctx context.Context, rec *domain.Record) error {
	if err := r.checkRecordByteSize(rec); err != nil {
		return err
	}
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = now
	}
	if col, err := r.GetCollection(ctx, rec.CollectionID); err == nil && col != nil {
		if err := r.checkWrite(ctx, col.TenantID, false); err != nil {
			return err
		}
		rec.CollectionID = col.ID
		if rec.AccessPolicy.Visibility == "" && col.DefaultVisibility != "" {
			rec.AccessPolicy.Visibility = col.DefaultVisibility
		}
	}
	if r.maxRecordsPerCollection > 0 {
		var count int
		countQuery := "SELECT COUNT(*) FROM records WHERE collection_id = ?"
		if r.database.Driver == db.DriverPostgres {
			countQuery = "SELECT COUNT(*) FROM records WHERE collection_id = $1"
		}
		if err := r.database.GetContext(ctx, &count, countQuery, rec.CollectionID); err != nil {
			return err
		}
		if count >= r.maxRecordsPerCollection {
			return fmt.Errorf("%w: %d/%d records in collection", ErrCollectionSizeExceeded, count, r.maxRecordsPerCollection)
		}
	}
	if rec.AccessPolicy.Visibility == "" {
		rec.AccessPolicy.Visibility = domain.VisibilityPrivate
	}

	query := `
	INSERT INTO records (id, collection_id, title, content, data, created_by, access_policy, created_at, updated_at)
	VALUES (:id, :collection_id, :title, :content, :data, :created_by, :access_policy, :created_at, :updated_at)
	`
	if _, err := r.database.NamedExecContext(ctx, query, rec); err != nil {
		return err
	}
	r.autoInviteRecordShares(ctx, rec.CollectionID, rec.AccessPolicy.Shares, rec.CreatedBy)
	if r.recordIndexHook != nil {
		r.recordIndexHook(ctx, rec)
	}
	return nil
}

func (r *sqlRepo) buildRecordACLFilter(user *domain.User) (string, map[string]interface{}) {
	tenantID := ""
	if user.TenantID != nil {
		tenantID = *user.TenantID
	}
	userDomain := ""
	if parts := strings.Split(user.Email, "@"); len(parts) == 2 {
		userDomain = parts[1]
	}

	params := map[string]interface{}{
		"user_id":        user.ID,
		"user_email":     user.Email,
		"user_domain":    userDomain,
		"user_role":      user.Role,
		"user_tenant_id": tenantID,
	}

	var jsonShareCondition string
	var jsonVisCondition string

	if r.database.Driver == db.DriverSQLite {
		jsonVisCondition = `(
			json_extract(r.access_policy, '$.visibility') = 'public'
			AND (
				c.tenant_id = ''
				OR c.tenant_id = :user_tenant_id
				OR EXISTS (SELECT 1 FROM user_tenants ut WHERE ut.user_id = :user_id AND ut.tenant_id = c.tenant_id)
				OR (:user_domain != '' AND EXISTS (SELECT 1 FROM json_each(CASE WHEN c.allowed_domains IS NOT NULL AND c.allowed_domains != '' THEN c.allowed_domains ELSE '[]' END) WHERE LOWER(value) = LOWER(:user_domain)))
			)
		)`
		jsonShareCondition = "EXISTS (SELECT 1 FROM json_each(json_extract(r.access_policy, '$.shares')) WHERE LOWER(value) = LOWER(:user_email))"
	} else {
		jsonVisCondition = `(
			(CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' THEN CAST(r.access_policy AS jsonb)->>'visibility' ELSE 'private' END = 'public')
			AND (
				c.tenant_id = ''
				OR c.tenant_id = :user_tenant_id
				OR EXISTS (SELECT 1 FROM user_tenants ut WHERE ut.user_id = :user_id AND ut.tenant_id = c.tenant_id)
				OR (:user_domain != '' AND CASE WHEN c.allowed_domains IS NOT NULL AND c.allowed_domains != '' AND jsonb_typeof(CAST(c.allowed_domains AS jsonb)) = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(CAST(c.allowed_domains AS jsonb)) WHERE LOWER(value) = LOWER(:user_domain)) ELSE false END)
			)
		)`
		jsonShareCondition = "CASE WHEN r.access_policy IS NOT NULL AND r.access_policy != '' AND jsonb_typeof(CAST(r.access_policy AS jsonb)->'shares') = 'array' THEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(CAST(r.access_policy AS jsonb)->'shares') WHERE LOWER(value) = LOWER(:user_email)) ELSE false END"
	}

	// Recursive CTE finds all reports down the management tree of user_id across users and user_tenants,
	// scoped by tenant_id so manager relationships in one tenant never grant access to another tenant's collections.
	// Using a unified org_edges CTE ensures a single non-recursive term and single recursive term,
	// which is required by PostgreSQL's WITH RECURSIVE parser.
	cte := fmt.Sprintf(`
	WITH RECURSIVE org_edges AS (
		SELECT id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM users
		UNION ALL
		SELECT user_id AS id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM user_tenants
	),
	subordinates AS (
		SELECT id, tenant_id FROM org_edges WHERE manager_id = :user_id
		UNION
		SELECT e.id, e.tenant_id FROM org_edges e JOIN subordinates s ON e.manager_id = s.id AND (e.tenant_id = '' OR s.tenant_id = '' OR e.tenant_id = s.tenant_id)
	)
	SELECT r.id, r.collection_id, r.title, r.content, r.data, r.created_by, r.access_policy, r.created_at, r.updated_at
	FROM records r
	JOIN collections c ON r.collection_id = c.id
	WHERE (
		(:user_role = 'admin' AND c.tenant_id = :user_tenant_id)
		OR (c.tenant_id != '' AND EXISTS (SELECT 1 FROM user_tenants ut WHERE ut.user_id = :user_id AND ut.tenant_id = c.tenant_id AND ut.role = 'admin'))
		OR r.created_by = :user_id
		OR EXISTS (SELECT 1 FROM subordinates sub WHERE sub.id = r.created_by AND (c.tenant_id = '' OR sub.tenant_id = c.tenant_id))
		OR %s
		OR %s
	)
	`, jsonVisCondition, jsonShareCondition)

	return cte, params
}

func (r *sqlRepo) GetRecord(ctx context.Context, id string, user *domain.User) (*domain.Record, error) {
	baseCTE, params := r.buildRecordACLFilter(user)
	query := baseCTE + " AND r.id = :record_id"
	params["record_id"] = id

	namedQuery, args, err := sqlx.Named(query, params)
	if err != nil {
		return nil, fmt.Errorf("failed binding ACL statement: %w", err)
	}
	namedQuery = r.database.Rebind(namedQuery)

	var rec domain.Record
	if err := r.database.GetContext(ctx, &rec, namedQuery, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Check if it exists at all to differentiate 404 from 403
			var exists bool
			checkQuery := "SELECT EXISTS(SELECT 1 FROM records WHERE id = ?)"
			if r.database.Driver == db.DriverPostgres {
				checkQuery = "SELECT EXISTS(SELECT 1 FROM records WHERE id = $1)"
			}
			_ = r.database.GetContext(ctx, &exists, checkQuery, id)
			if exists {
				return nil, ErrForbidden
			}
			return nil, ErrNotFound
		}
		return nil, err
	}

	return &rec, nil
}

func (r *sqlRepo) ListRecords(ctx context.Context, collectionID string, user *domain.User) ([]domain.Record, error) {
	baseCTE, params := r.buildRecordACLFilter(user)
	query := baseCTE + " AND (r.collection_id = :collection_id OR (c.slug != '' AND LOWER(c.slug) = LOWER(:collection_id)) OR REPLACE(LOWER(c.id), '-', '') = LOWER(:collection_id)) ORDER BY r.created_at DESC"
	params["collection_id"] = strings.TrimSpace(collectionID)

	namedQuery, args, err := sqlx.Named(query, params)
	if err != nil {
		return nil, fmt.Errorf("failed binding list ACL query: %w", err)
	}
	namedQuery = r.database.Rebind(namedQuery)

	var records []domain.Record
	if err := r.database.SelectContext(ctx, &records, namedQuery, args...); err != nil {
		return nil, err
	}
	return records, nil
}

func (r *sqlRepo) isTenantAdminOrGlobalAdmin(ctx context.Context, user *domain.User, collectionID string) bool {
	if user == nil {
		return false
	}
	userTenantID := ""
	if user.TenantID != nil {
		userTenantID = *user.TenantID
	}
	col, err := r.GetCollection(ctx, collectionID)
	if err != nil {
		return user.Role == domain.RoleAdmin && userTenantID == ""
	}
	if user.Role == domain.RoleAdmin && userTenantID == col.TenantID {
		return true
	}
	if col.TenantID != "" {
		memberships, _ := r.ListUserTenants(ctx, user.ID)
		for _, m := range memberships {
			if m.TenantID == col.TenantID && m.Role == domain.RoleAdmin {
				return true
			}
		}
	}
	return false
}

func (r *sqlRepo) UpdateRecord(ctx context.Context, rec *domain.Record, user *domain.User) error {
	// First verify user has access to update this record
	existing, err := r.GetRecord(ctx, rec.ID, user)
	if err != nil {
		return err
	}

	// User can update if: admin, owner, or manager above creator within the collection's tenant
	if !r.isTenantAdminOrGlobalAdmin(ctx, user, existing.CollectionID) && existing.CreatedBy != user.ID {
		colTenantID := ""
		if col, err := r.GetCollection(ctx, existing.CollectionID); err == nil && col != nil {
			colTenantID = col.TenantID
		}
		// Verify tenant-scoped manager relationship via CTE
		var isManager bool
		mgrQuery := `
		WITH RECURSIVE org_edges AS (
			SELECT id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM users
			UNION ALL
			SELECT user_id AS id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM user_tenants
		),
		subordinates AS (
			SELECT id, tenant_id FROM org_edges WHERE manager_id = ?
			UNION
			SELECT e.id, e.tenant_id FROM org_edges e JOIN subordinates s ON e.manager_id = s.id AND (e.tenant_id = '' OR s.tenant_id = '' OR e.tenant_id = s.tenant_id)
		)
		SELECT EXISTS(SELECT 1 FROM subordinates WHERE id = ? AND (? = '' OR tenant_id = ?))
		`
		if r.database.Driver == db.DriverPostgres {
			mgrQuery = `
			WITH RECURSIVE org_edges AS (
				SELECT id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM users
				UNION ALL
				SELECT user_id AS id, manager_id, COALESCE(tenant_id, '') AS tenant_id FROM user_tenants
			),
			subordinates AS (
				SELECT id, tenant_id FROM org_edges WHERE manager_id = $1
				UNION
				SELECT e.id, e.tenant_id FROM org_edges e JOIN subordinates s ON e.manager_id = s.id AND (e.tenant_id = '' OR s.tenant_id = '' OR e.tenant_id = s.tenant_id)
			)
			SELECT EXISTS(SELECT 1 FROM subordinates WHERE id = $2 AND ($3 = '' OR tenant_id = $4))
			`
		}
		if err := r.database.GetContext(ctx, &isManager, mgrQuery, user.ID, existing.CreatedBy, colTenantID, colTenantID); err != nil || !isManager {
			return ErrForbidden
		}
	}

	if col, err := r.GetCollection(ctx, existing.CollectionID); err == nil && col != nil {
		if err := r.checkWrite(ctx, col.TenantID, false); err != nil {
			return err
		}
	}

	if rec.Title == "" {
		rec.Title = existing.Title
	}
	if rec.Content == "" {
		rec.Content = existing.Content
	}
	if rec.Data == "" {
		rec.Data = existing.Data
	}
	if rec.AccessPolicy.Visibility == "" {
		rec.AccessPolicy.Visibility = existing.AccessPolicy.Visibility
	}
	if rec.AccessPolicy.Shares == nil {
		rec.AccessPolicy.Shares = existing.AccessPolicy.Shares
	}

	if rec.CollectionID == "" {
		rec.CollectionID = existing.CollectionID
	}
	if err := r.checkRecordByteSize(rec); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now().UTC()
	query := `
	UPDATE records
	SET title = :title, content = :content, data = :data, access_policy = :access_policy, updated_at = :updated_at
	WHERE id = :id
	`
	if _, err = r.database.NamedExecContext(ctx, query, rec); err != nil {
		return err
	}
	actorID := existing.CreatedBy
	if user != nil && user.ID != "" {
		actorID = user.ID
	}
	r.autoInviteRecordShares(ctx, existing.CollectionID, rec.AccessPolicy.Shares, actorID)
	if r.recordIndexHook != nil {
		r.recordIndexHook(ctx, rec)
	}
	return nil
}

func (r *sqlRepo) DeleteRecord(ctx context.Context, id string, user *domain.User) error {
	existing, err := r.GetRecord(ctx, id, user)
	if err != nil {
		return err
	}

	if !r.isTenantAdminOrGlobalAdmin(ctx, user, existing.CollectionID) && existing.CreatedBy != user.ID {
		return ErrForbidden
	}

	if col, err := r.GetCollection(ctx, existing.CollectionID); err == nil && col != nil {
		if err := r.checkWrite(ctx, col.TenantID, false); err != nil {
			return err
		}
	}

	query := "DELETE FROM records WHERE id = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "DELETE FROM records WHERE id = $1"
	}
	_, err = r.database.ExecContext(ctx, query, id)
	return err
}

func (r *sqlRepo) CreateOAuthClient(ctx context.Context, client *domain.OAuthClient) error {
	if client.ID == "" {
		client.ID = uuid.NewString()
	}
	if client.CreatedAt.IsZero() {
		client.CreatedAt = time.Now().UTC()
	}
	query := `
	INSERT INTO oauth_clients (id, client_secret, client_name, redirect_uris, grant_types, created_at)
	VALUES (:id, :client_secret, :client_name, :redirect_uris, :grant_types, :created_at)
	`
	_, err := r.database.NamedExecContext(ctx, query, client)
	return err
}

func (r *sqlRepo) GetOAuthClient(ctx context.Context, id string) (*domain.OAuthClient, error) {
	var c domain.OAuthClient
	query := "SELECT id, client_secret, client_name, redirect_uris, grant_types, created_at FROM oauth_clients WHERE id = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, client_secret, client_name, redirect_uris, grant_types, created_at FROM oauth_clients WHERE id = $1"
	}
	if err := r.database.GetContext(ctx, &c, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

func (r *sqlRepo) CreateAuthCode(ctx context.Context, code *domain.AuthCode) error {
	query := `
	INSERT INTO auth_codes (code, client_id, user_id, code_challenge, code_challenge_method, expires_at)
	VALUES (:code, :client_id, :user_id, :code_challenge, :code_challenge_method, :expires_at)
	`
	_, err := r.database.NamedExecContext(ctx, query, code)
	return err
}

func (r *sqlRepo) ConsumeAuthCode(ctx context.Context, code string) (*domain.AuthCode, error) {
	var ac domain.AuthCode
	query := "SELECT code, client_id, user_id, code_challenge, code_challenge_method, expires_at FROM auth_codes WHERE code = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT code, client_id, user_id, code_challenge, code_challenge_method, expires_at FROM auth_codes WHERE code = $1"
	}
	if err := r.database.GetContext(ctx, &ac, query, code); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	now := time.Now().UTC()
	// Delete the consumed auth code and clean up expired auth codes (single-use enforcement)
	delQuery := "DELETE FROM auth_codes WHERE code = ? OR expires_at < ?"
	if r.database.Driver == db.DriverPostgres {
		delQuery = "DELETE FROM auth_codes WHERE code = $1 OR expires_at < $2"
	}
	_, _ = r.database.ExecContext(ctx, delQuery, code, now)

	if now.After(ac.ExpiresAt) {
		return nil, errors.New("auth code expired")
	}

	return &ac, nil
}

func (r *sqlRepo) CreateRefreshToken(ctx context.Context, rt *domain.RefreshToken) error {
	query := `
	INSERT INTO refresh_tokens (id, user_id, client_id, token_hash, token_version, expires_at, revoked_at, created_at)
	VALUES (:id, :user_id, :client_id, :token_hash, :token_version, :expires_at, :revoked_at, :created_at)
	`
	_, err := r.database.NamedExecContext(ctx, query, rt)
	return err
}

func (r *sqlRepo) GetRefreshTokenByHash(ctx context.Context, tokenHash string) (*domain.RefreshToken, error) {
	var rt domain.RefreshToken
	query := "SELECT id, user_id, client_id, token_hash, token_version, expires_at, revoked_at, created_at FROM refresh_tokens WHERE token_hash = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "SELECT id, user_id, client_id, token_hash, token_version, expires_at, revoked_at, created_at FROM refresh_tokens WHERE token_hash = $1"
	}
	if err := r.database.GetContext(ctx, &rt, query, tokenHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &rt, nil
}

func (r *sqlRepo) RevokeRefreshToken(ctx context.Context, id string) error {
	now := time.Now().UTC()
	query := "UPDATE refresh_tokens SET revoked_at = ? WHERE id = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "UPDATE refresh_tokens SET revoked_at = $1 WHERE id = $2"
	}
	_, err := r.database.ExecContext(ctx, query, now, id)
	return err
}

func (r *sqlRepo) RevokeAllUserRefreshTokens(ctx context.Context, userID string) error {
	now := time.Now().UTC()
	query := "UPDATE refresh_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL"
	if r.database.Driver == db.DriverPostgres {
		query = "UPDATE refresh_tokens SET revoked_at = $1 WHERE user_id = $2 AND revoked_at IS NULL"
	}
	_, err := r.database.ExecContext(ctx, query, now, userID)
	return err
}

func (r *sqlRepo) UpdateUserTokenVersion(ctx context.Context, userID string, tokenVersion string) error {
	query := "UPDATE users SET token_version = ? WHERE id = ?"
	if r.database.Driver == db.DriverPostgres {
		query = "UPDATE users SET token_version = $1 WHERE id = $2"
	}
	_, err := r.database.ExecContext(ctx, query, tokenVersion, userID)
	if err == nil {
		r.invalidateUserCache()
	}
	return err
}

