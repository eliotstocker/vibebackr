package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/repository"
)

func setupTestDB(t *testing.T) repository.Repository {
	t.Helper()
	// Use isolated in-memory SQLite per test
	database, err := db.Connect("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed connecting test db: %v", err)
	}
	return repository.New(database)
}

func TestWaterfallAndACLPermissions(t *testing.T) {
	ctx := context.Background()
	repo := setupTestDB(t)

	// Setup Org Hierarchy:
	// CEO (admin) -> Director (manager) -> Manager (manager) -> Employee (member)
	// Other Employee (member, different team)
	ceo := &domain.User{ID: "u_ceo", Email: "ceo@corp.com", Name: "CEO", Role: domain.RoleAdmin}
	director := &domain.User{ID: "u_dir", Email: "dir@corp.com", Name: "Director", Role: domain.RoleManager, ManagerID: &ceo.ID}
	mgr := &domain.User{ID: "u_mgr", Email: "mgr@corp.com", Name: "Manager", Role: domain.RoleManager, ManagerID: &director.ID}
	emp := &domain.User{ID: "u_emp", Email: "emp@corp.com", Name: "Employee", Role: domain.RoleMember, ManagerID: &mgr.ID}
	otherEmp := &domain.User{ID: "u_other", Email: "other@corp.com", Name: "Other", Role: domain.RoleMember}

	for _, u := range []*domain.User{ceo, director, mgr, emp, otherEmp} {
		if err := repo.CreateUser(ctx, u); err != nil {
			t.Fatalf("failed to create user %s: %v", u.Email, err)
		}
	}

	// Create Collection
	col := &domain.Collection{
		ID:                "col_1",
		Name:              "Internal Docs",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	if err := repo.CreateCollection(ctx, col); err != nil {
		t.Fatalf("failed to create collection: %v", err)
	}

	// 1. Employee creates a private record
	recPrivate := &domain.Record{
		ID:           "rec_private",
		CollectionID: col.ID,
		Title:        "Private Draft",
		Content:      "Confidential",
		CreatedBy:    emp.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
	}
	if err := repo.CreateRecord(ctx, recPrivate); err != nil {
		t.Fatalf("failed creating private record: %v", err)
	}

	// Test 1: Creator can access
	if _, err := repo.GetRecord(ctx, recPrivate.ID, emp); err != nil {
		t.Errorf("creator should have access: %v", err)
	}

	// Test 2: Manager (direct report) can access via recursive CTE
	if _, err := repo.GetRecord(ctx, recPrivate.ID, mgr); err != nil {
		t.Errorf("manager should have access to employee record: %v", err)
	}

	// Test 3: Director (skip-level manager) can access via recursive CTE
	if _, err := repo.GetRecord(ctx, recPrivate.ID, director); err != nil {
		t.Errorf("director should have access via recursive CTE: %v", err)
	}

	// Test 4: CEO (Admin override) can access
	if _, err := repo.GetRecord(ctx, recPrivate.ID, ceo); err != nil {
		t.Errorf("admin should have access override: %v", err)
	}

	// Test 5: Unrelated employee CANNOT access (forbidden)
	if _, err := repo.GetRecord(ctx, recPrivate.ID, otherEmp); err != repository.ErrForbidden {
		t.Errorf("unrelated employee should be denied, got %v", err)
	}

	// 2. Employee shares a record explicitly with otherEmp
	recShared := &domain.Record{
		ID:           "rec_shared",
		CollectionID: col.ID,
		Title:        "Team Project",
		Content:      "Collab notes",
		CreatedBy:    emp.ID,
		AccessPolicy: domain.AccessPolicy{
			Visibility: domain.VisibilityShared,
			Shares:     []string{otherEmp.Email},
		},
	}
	if err := repo.CreateRecord(ctx, recShared); err != nil {
		t.Fatalf("failed creating shared record: %v", err)
	}

	// Test 6: otherEmp now has access via Email ACL Share
	if _, err := repo.GetRecord(ctx, recShared.ID, otherEmp); err != nil {
		t.Errorf("otherEmp should have access via explicit ACL share: %v", err)
	}

	// 3. Public collection or record
	recPublic := &domain.Record{
		ID:           "rec_public",
		CollectionID: col.ID,
		Title:        "Company Handbook",
		Content:      "Welcome",
		CreatedBy:    emp.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPublic},
	}
	if err := repo.CreateRecord(ctx, recPublic); err != nil {
		t.Fatalf("failed creating public record: %v", err)
	}

	// Test 7: otherEmp can access public record
	if _, err := repo.GetRecord(ctx, recPublic.ID, otherEmp); err != nil {
		t.Errorf("otherEmp should have access to public record: %v", err)
	}
}

func TestOrgDirectorySync(t *testing.T) {
	ctx := context.Background()
	repo := setupTestDB(t)

	users := []domain.User{
		{Email: "alice@test.com", Name: "Alice", Role: domain.RoleManager, CreatedAt: time.Now().UTC()},
		{Email: "bob@test.com", Name: "Bob", Role: domain.RoleMember, CreatedAt: time.Now().UTC()},
	}

	if err := repo.BatchUpsertUsers(ctx, users); err != nil {
		t.Fatalf("failed batch upsert: %v", err)
	}

	list, err := repo.ListUsers(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("expected 2 users, got %d, err: %v", len(list), err)
	}

	// Upsert update
	users[1].Name = "Bob Updated"
	if err := repo.BatchUpsertUsers(ctx, users); err != nil {
		t.Fatalf("failed batch update: %v", err)
	}

	u, err := repo.GetUserByEmail(ctx, "bob@test.com")
	if err != nil || u.Name != "Bob Updated" {
		t.Fatalf("expected name updated, got %s", u.Name)
	}
}

func TestRecordUpdateAndDeleteACL(t *testing.T) {
	ctx := context.Background()
	repo := setupTestDB(t)

	mgr := &domain.User{ID: "u_mgr", Email: "mgr@test.com", Role: domain.RoleManager}
	emp := &domain.User{ID: "u_emp", Email: "emp@test.com", Role: domain.RoleMember, ManagerID: &mgr.ID}
	other := &domain.User{ID: "u_other", Email: "other@test.com", Role: domain.RoleMember}
	_ = repo.CreateUser(ctx, mgr)
	_ = repo.CreateUser(ctx, emp)
	_ = repo.CreateUser(ctx, other)

	col := &domain.Collection{ID: "col_ops", Name: "Ops"}
	_ = repo.CreateCollection(ctx, col)

	rec := &domain.Record{
		ID:           "rec_1",
		CollectionID: col.ID,
		Title:        "Original",
		CreatedBy:    emp.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
	}
	_ = repo.CreateRecord(ctx, rec)

	// Unauthorized edit attempt by unrelated user
	rec.Title = "Hacked Title"
	if err := repo.UpdateRecord(ctx, rec, other); err != repository.ErrForbidden {
		t.Fatalf("expected ErrForbidden for unauthorized update, got %v", err)
	}

	// Authorized edit by creator
	rec.Title = "Creator Updated Title"
	if err := repo.UpdateRecord(ctx, rec, emp); err != nil {
		t.Fatalf("creator should be able to update: %v", err)
	}

	// Authorized edit by manager up the chain
	rec.Title = "Manager Updated Title"
	if err := repo.UpdateRecord(ctx, rec, mgr); err != nil {
		t.Fatalf("manager should be able to update subordinate's record: %v", err)
	}

	// Unauthorized delete attempt by unrelated user
	if err := repo.DeleteRecord(ctx, rec.ID, other); err != repository.ErrForbidden {
		t.Fatalf("expected ErrForbidden for unauthorized delete, got %v", err)
	}

	// Authorized delete by creator
	if err := repo.DeleteRecord(ctx, rec.ID, emp); err != nil {
		t.Fatalf("creator should be able to delete record: %v", err)
	}
}

func TestCollectionAndInviteOperations(t *testing.T) {
	ctx := context.Background()
	repo := setupTestDB(t)

	// 1. Collection ops
	col := &domain.Collection{ID: "c1", Name: "Col 1"}
	if err := repo.CreateCollection(ctx, col); err != nil {
		t.Fatalf("failed creating collection: %v", err)
	}
	fetchedCol, err := repo.GetCollection(ctx, col.ID)
	if err != nil || fetchedCol.Name != "Col 1" {
		t.Fatalf("failed getting collection: %v", err)
	}
	cols, err := repo.ListCollections(ctx)
	if err != nil || len(cols) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(cols))
	}

	// Test UpdateCollection
	fetchedCol.Name = "Col 1 Updated"
	fetchedCol.OriginURLs = domain.JSONStrings{"http://localhost:5173"}
	fetchedCol.VectorSearchEnabled = true
	fetchedCol.AutoInviteShares = true
	if err := repo.UpdateCollection(ctx, fetchedCol); err != nil {
		t.Fatalf("failed updating collection: %v", err)
	}
	updatedCol, err := repo.GetCollection(ctx, col.ID)
	if err != nil || updatedCol.Name != "Col 1 Updated" || len(updatedCol.OriginURLs) != 1 || !updatedCol.VectorSearchEnabled || !updatedCol.AutoInviteShares {
		t.Fatalf("unexpected updated collection state: %+v", updatedCol)
	}

	// 2. Invite ops
	admin := &domain.User{ID: "admin_u", Email: "admin@corp.com", Role: domain.RoleAdmin}
	_ = repo.CreateUser(ctx, admin)

	inv := &domain.Invite{
		Email:     "pending@corp.com",
		Role:      domain.RoleMember,
		CreatedBy: admin.ID,
	}
	if err := repo.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("failed creating invite: %v", err)
	}

	foundInv, err := repo.GetInviteByEmail(ctx, "pending@corp.com")
	if err != nil || foundInv.Email != "pending@corp.com" {
		t.Fatalf("failed getting invite: %v", err)
	}

	if err := repo.DeleteInvite(ctx, foundInv.ID); err != nil {
		t.Fatalf("failed deleting invite: %v", err)
	}

	if _, err := repo.GetInviteByEmail(ctx, "pending@corp.com"); err != repository.ErrNotFound {
		t.Fatalf("expected ErrNotFound after deleting invite")
	}
}

func TestAutoInviteRecordShares(t *testing.T) {
	ctx := context.Background()
	repo := setupTestDB(t)

	tenantID := "tenant_alpha"
	owner := &domain.User{ID: "u_owner", TenantID: &tenantID, Email: "owner@alpha.com", Name: "Owner", Role: domain.RoleAdmin}
	existingOutsider := &domain.User{ID: "u_outsider", Email: "outsider@beta.com", Name: "Outsider", Role: domain.RoleMember}
	_ = repo.CreateUser(ctx, owner)
	_ = repo.CreateUser(ctx, existingOutsider)

	// 1. Collection with AutoInviteShares = false (default)
	colDisabled := &domain.Collection{
		ID:                "col_disabled",
		TenantID:          tenantID,
		Name:              "Disabled Auto Invite",
		DefaultVisibility: domain.VisibilityPrivate,
		AutoInviteShares:  false,
	}
	_ = repo.CreateCollection(ctx, colDisabled)

	rec1 := &domain.Record{
		CollectionID: colDisabled.ID,
		Title:        "No Auto Invite",
		CreatedBy:    owner.ID,
		AccessPolicy: domain.AccessPolicy{
			Visibility: domain.VisibilityShared,
			Shares:     []string{"nonuser1@external.com"},
		},
	}
	if err := repo.CreateRecord(ctx, rec1); err != nil {
		t.Fatalf("CreateRecord failed: %v", err)
	}
	if _, err := repo.GetInviteByEmail(ctx, "nonuser1@external.com"); err != repository.ErrNotFound {
		t.Fatalf("expected no invite when AutoInviteShares is false, got %v", err)
	}

	// 2. Collection with AutoInviteShares = true
	colEnabled := &domain.Collection{
		ID:                "col_enabled",
		TenantID:          tenantID,
		Name:              "Enabled Auto Invite",
		DefaultVisibility: domain.VisibilityPrivate,
		AutoInviteShares:  true,
	}
	_ = repo.CreateCollection(ctx, colEnabled)

	rec2 := &domain.Record{
		ID:           "rec_2",
		CollectionID: colEnabled.ID,
		Title:        "Shared Doc",
		CreatedBy:    owner.ID,
		AccessPolicy: domain.AccessPolicy{
			Visibility: domain.VisibilityShared,
			Shares:     []string{"newguest@external.com", existingOutsider.Email},
		},
	}
	if err := repo.CreateRecord(ctx, rec2); err != nil {
		t.Fatalf("CreateRecord failed: %v", err)
	}

	// Non-user should have an active invite for tenant_alpha
	inv, err := repo.GetInviteByEmail(ctx, "newguest@external.com")
	if err != nil {
		t.Fatalf("expected auto-created invite for newguest@external.com, got err: %v", err)
	}
	if inv.TenantID != tenantID || inv.Role != domain.RoleMember {
		t.Fatalf("unexpected invite details: %+v", inv)
	}

	// Existing user not yet in tenant should be added to tenant memberships
	memberships, err := repo.ListUserTenants(ctx, existingOutsider.ID)
	if err != nil || len(memberships) != 1 || memberships[0].TenantID != tenantID {
		t.Fatalf("expected existingOutsider to be added to tenant %s, got %+v", tenantID, memberships)
	}

	// 3. Updating record with a new shared email should also auto-invite
	rec2.AccessPolicy.Shares = append(rec2.AccessPolicy.Shares, "secondguest@external.com")
	if err := repo.UpdateRecord(ctx, rec2, owner); err != nil {
		t.Fatalf("UpdateRecord failed: %v", err)
	}
	if _, err := repo.GetInviteByEmail(ctx, "secondguest@external.com"); err != nil {
		t.Fatalf("expected auto-created invite on UpdateRecord for secondguest@external.com, got err: %v", err)
	}

	// 4. Enabling auto_invite_shares on colDisabled should backfill invites for existing shared records
	colDisabled.AutoInviteShares = true
	if err := repo.UpdateCollection(ctx, colDisabled); err != nil {
		t.Fatalf("UpdateCollection failed: %v", err)
	}
	if _, err := repo.GetInviteByEmail(ctx, "nonuser1@external.com"); err != nil {
		t.Fatalf("expected auto-created invite when enabling AutoInviteShares on existing collection, got err: %v", err)
	}
}

func TestTenantlessAdminAndDefaultVisibilityGuardrails(t *testing.T) {
	ctx := context.Background()
	repo := setupTestDB(t)

	tenantID := "f2cc7ca5-4541-4431-8d9c-fac4f51a3ba9"
	owner := &domain.User{
		ID:       "u_owner",
		TenantID: &tenantID,
		Email:    "owner@gmail.com",
		Name:     "Owner",
		Role:     domain.RoleAdmin,
	}
	// Legacy user with users.tenant_id = nil and users.role = "admin", joined to tenantID as "member"
	legacyAdminMember := &domain.User{
		ID:       "u_legacy",
		TenantID: nil,
		Email:    "member@askattest.com",
		Name:     "Legacy User",
		Role:     domain.RoleAdmin,
	}
	_ = repo.CreateUser(ctx, owner)
	_ = repo.CreateUser(ctx, legacyAdminMember)
	_ = repo.AddTenantMembership(ctx, &domain.TenantMembership{
		UserID:   legacyAdminMember.ID,
		TenantID: tenantID,
		Role:     domain.RoleMember,
	})

	col := &domain.Collection{
		ID:                "col_todos",
		TenantID:          tenantID,
		Name:              "todos",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	_ = repo.CreateCollection(ctx, col)

	recPrivate := &domain.Record{
		ID:           "rec_priv",
		CollectionID: col.ID,
		Title:        "new thing",
		CreatedBy:    owner.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
	}
	recShared := &domain.Record{
		ID:           "rec_sh",
		CollectionID: col.ID,
		Title:        "testing stuff",
		CreatedBy:    owner.ID,
		AccessPolicy: domain.AccessPolicy{
			Visibility: domain.VisibilityShared,
			Shares:     []string{legacyAdminMember.Email},
		},
	}
	recPublic := &domain.Record{
		ID:           "rec_pub",
		CollectionID: col.ID,
		Title:        "Buy groceries",
		CreatedBy:    owner.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPublic},
	}
	_ = repo.CreateRecord(ctx, recPrivate)
	_ = repo.CreateRecord(ctx, recShared)
	_ = repo.CreateRecord(ctx, recPublic)

	// 1. legacyAdminMember must only see shared and public records, NOT owner's private record
	records, err := repo.ListRecords(ctx, col.ID, legacyAdminMember)
	if err != nil {
		t.Fatalf("ListRecords failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records (shared + public), got %d: %+v", len(records), records)
	}
	for _, r := range records {
		if r.ID == recPrivate.ID {
			t.Fatalf("tenantless admin member should NOT see private record in another user's tenant")
		}
	}

	if _, err := repo.GetRecord(ctx, recPrivate.ID, legacyAdminMember); err != repository.ErrForbidden {
		t.Fatalf("expected ErrForbidden on GetRecord for private record, got %v", err)
	}

	// 2. Public default_visibility collection: explicitly private record must stay private
	colPublic := &domain.Collection{
		ID:                "col_pub_default",
		TenantID:          tenantID,
		Name:              "public-board",
		DefaultVisibility: domain.VisibilityPublic,
	}
	_ = repo.CreateCollection(ctx, colPublic)

	recInheritedPub := &domain.Record{
		ID:           "rec_inherited_pub",
		CollectionID: colPublic.ID,
		Title:        "Default Public Item",
		CreatedBy:    owner.ID,
	}
	recExplicitPriv := &domain.Record{
		ID:           "rec_explicit_priv",
		CollectionID: colPublic.ID,
		Title:        "Secret Item in Public Collection",
		CreatedBy:    owner.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
	}
	_ = repo.CreateRecord(ctx, recInheritedPub)
	_ = repo.CreateRecord(ctx, recExplicitPriv)

	pubColRecords, err := repo.ListRecords(ctx, colPublic.ID, legacyAdminMember)
	if err != nil {
		t.Fatalf("ListRecords on public collection failed: %v", err)
	}
	if len(pubColRecords) != 1 || pubColRecords[0].ID != recInheritedPub.ID {
		t.Fatalf("expected only inherited public record, got %+v", pubColRecords)
	}
}

func TestCollectionProjectSlugs(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:slugtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	defer database.Close()

	repo := repository.New(database)
	tenantID := "tenant_slug_test"
	owner := &domain.User{
		ID:       "user_slug_owner",
		TenantID: &tenantID,
		Email:    "owner@slug.test",
		Name:     "Slug Owner",
		Role:     domain.RoleAdmin,
	}
	_ = repo.CreateUser(ctx, owner)

	// 1. Create collection with mixed-case slug -> normalized to lowercase
	col1 := &domain.Collection{
		ID:                "550e8400-e29b-41d4-a716-446655440099",
		TenantID:          tenantID,
		Name:              "CRM App",
		Slug:              "My-CRM-App-2026",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	if err := repo.CreateCollection(ctx, col1); err != nil {
		t.Fatalf("CreateCollection with valid slug failed: %v", err)
	}
	if col1.Slug != "my-crm-app-2026" {
		t.Fatalf("expected slug normalized to lowercase 'my-crm-app-2026', got %q", col1.Slug)
	}

	// 2. GetCollection works by UUID, exact slug, case-insensitive slug, and dashless UUID
	for _, ref := range []string{col1.ID, "my-crm-app-2026", "MY-CRM-APP-2026", "550e8400e29b41d4a716446655440099"} {
		fetched, err := repo.GetCollection(ctx, ref)
		if err != nil {
			t.Fatalf("GetCollection(%q) failed: %v", ref, err)
		}
		if fetched.ID != col1.ID || fetched.Slug != "my-crm-app-2026" {
			t.Fatalf("GetCollection(%q) returned unexpected collection: %+v", ref, fetched)
		}
	}

	// 3. Duplicate slug (even across different case) returns ErrSlugTaken
	colDup := &domain.Collection{
		ID:                "col_dup_slug",
		TenantID:          tenantID,
		Name:              "Duplicate Slug App",
		Slug:              "MY-CRM-APP-2026",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	if err := repo.CreateCollection(ctx, colDup); err != repository.ErrSlugTaken {
		t.Fatalf("expected ErrSlugTaken for duplicate slug, got %v", err)
	}

	// 4. Slug colliding with existing collection's dashless UUID returns ErrSlugTaken
	colCollision := &domain.Collection{
		ID:                "col_uuid_collision",
		TenantID:          tenantID,
		Name:              "UUID Collision App",
		Slug:              "550e8400e29b41d4a716446655440099",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	if err := repo.CreateCollection(ctx, colCollision); err != repository.ErrSlugTaken {
		t.Fatalf("expected ErrSlugTaken when slug collides with existing dashless UUID, got %v", err)
	}

	// 5. CreateRecord and ListRecords work when passed the project slug instead of UUID
	rec := &domain.Record{
		ID:           "rec_via_slug",
		CollectionID: "My-CRM-App-2026",
		Title:        "Deal #1",
		CreatedBy:    owner.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
	}
	if err := repo.CreateRecord(ctx, rec); err != nil {
		t.Fatalf("CreateRecord via slug failed: %v", err)
	}
	if rec.CollectionID != col1.ID {
		t.Fatalf("expected CreateRecord to canonicalize CollectionID to %q, got %q", col1.ID, rec.CollectionID)
	}

	records, err := repo.ListRecords(ctx, "MY-CRM-APP-2026", owner)
	if err != nil {
		t.Fatalf("ListRecords via slug failed: %v", err)
	}
	if len(records) != 1 || records[0].ID != rec.ID {
		t.Fatalf("expected 1 record from ListRecords via slug, got %+v", records)
	}
}

func TestCollectionLimits(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:collimitstest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	defer database.Close()

	repo := repository.New(database)
	owner := &domain.User{
		ID:    "user_limit_owner",
		Email: "owner@limits.test",
		Name:  "Limits Owner",
		Role:  domain.RoleAdmin,
	}
	_ = repo.CreateUser(ctx, owner)

	col := &domain.Collection{
		ID:                "col_limits_1",
		Name:              "Limited Collection",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	_ = repo.CreateCollection(ctx, col)

	// 1. Disabled by default: large record succeeds
	rec0 := &domain.Record{
		ID:           "rec_0",
		CollectionID: col.ID,
		Title:        "Initial Record",
		Content:      "123456789012345678901234567890",
		CreatedBy:    owner.ID,
	}
	if err := repo.CreateRecord(ctx, rec0); err != nil {
		t.Fatalf("expected CreateRecord to succeed when limits are disabled, got: %v", err)
	}

	// 2. Enable limits: max 2 records per collection, max 32 bytes per record
	repo.SetCollectionLimits(2, 32)

	// Record exceeding 32 bytes is rejected
	tooBig := &domain.Record{
		CollectionID: col.ID,
		Title:        "Too Big",
		Content:      "012345678901234567890123456789",
		CreatedBy:    owner.ID,
	}
	if err := repo.CreateRecord(ctx, tooBig); !errors.Is(err, repository.ErrRecordTooLarge) {
		t.Fatalf("expected ErrRecordTooLarge on CreateRecord, got: %v", err)
	}

	// 2nd record within byte limit succeeds
	rec1 := &domain.Record{
		ID:           "rec_1",
		CollectionID: col.ID,
		Title:        "Second",
		Content:      "ok",
		CreatedBy:    owner.ID,
	}
	if err := repo.CreateRecord(ctx, rec1); err != nil {
		t.Fatalf("expected 2nd record to succeed, got: %v", err)
	}

	// 3rd record exceeds maxRecords=2
	rec2 := &domain.Record{
		ID:           "rec_2",
		CollectionID: col.ID,
		Title:        "Third",
		Content:      "ok",
		CreatedBy:    owner.ID,
	}
	if err := repo.CreateRecord(ctx, rec2); !errors.Is(err, repository.ErrCollectionSizeExceeded) {
		t.Fatalf("expected ErrCollectionSizeExceeded on 3rd record, got: %v", err)
	}

	// Updating an existing record in a full collection still works if within byte limit
	rec1.Content = "updated"
	if err := repo.UpdateRecord(ctx, rec1, owner); err != nil {
		t.Fatalf("expected UpdateRecord within byte limit to succeed on full collection, got: %v", err)
	}

	// Updating an existing record beyond maxRecordBytes is rejected
	rec1.Content = "012345678901234567890123456789"
	if err := repo.UpdateRecord(ctx, rec1, owner); !errors.Is(err, repository.ErrRecordTooLarge) {
		t.Fatalf("expected ErrRecordTooLarge on UpdateRecord, got: %v", err)
	}

	// Deleting a record frees a slot
	if err := repo.DeleteRecord(ctx, rec1.ID, owner); err != nil {
		t.Fatalf("DeleteRecord failed: %v", err)
	}
	if err := repo.CreateRecord(ctx, rec2); err != nil {
		t.Fatalf("expected CreateRecord to succeed after deleting a record, got: %v", err)
	}
}

