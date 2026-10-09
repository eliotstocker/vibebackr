package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/repository"
)

func setupTestBilling(t *testing.T, cfg Config) (*db.DB, repository.Repository, *Module) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "billing_test.db")
	database, err := db.Connect("sqlite", dsn)
	if err != nil {
		t.Fatalf("failed to connect db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate db: %v", err)
	}

	mod, err := NewModule(database, cfg)
	if err != nil {
		t.Fatalf("failed to init billing module: %v", err)
	}

	repo := repository.New(database)
	repo.SetWriteGuard(mod.CheckWriteAccess)
	return database, repo, mod
}

func TestModuleDisabled_NoEnforcement(t *testing.T) {
	_, repo, mod := setupTestBilling(t, Config{
		Enabled: false,
	})
	ctx := context.Background()

	status, err := mod.GetStatus(ctx, "tenant-disabled")
	if err != nil {
		t.Fatalf("GetStatus error: %v", err)
	}
	if status.Enabled {
		t.Fatalf("expected Enabled=false when module disabled")
	}

	// Should allow creating more than 10 collections when disabled
	for i := 0; i < 12; i++ {
		col := &domain.Collection{
			ID:                "col-" + time.Now().Format("150405") + "-" + string(rune('a'+i)),
			TenantID:          "tenant-disabled",
			Name:              "Collection",
			DefaultVisibility: domain.VisibilityPrivate,
		}
		if err := repo.CreateCollection(ctx, col); err != nil {
			t.Fatalf("expected no quota limit when billing is disabled, failed at %d: %v", i, err)
		}
	}
}

func TestStarterTrial_QuotaAndReadOnlyLock(t *testing.T) {
	_, repo, mod := setupTestBilling(t, Config{
		Enabled: true,
	})
	ctx := context.Background()
	tenantID := "tenant-trial-1"

	user := &domain.User{
		ID:       "user-1",
		TenantID: &tenantID,
		Email:    "founder@example.com",
		Name:     "Founder",
		Role:     domain.RoleAdmin,
	}
	if err := repo.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	noTenantStatus, err := mod.GetStatus(ctx, "")
	if err != nil {
		t.Fatalf("GetStatus empty tenant failed: %v", err)
	}
	if noTenantStatus.BillingStatus != StatusNoTenant || noTenantStatus.TenantID != "" {
		t.Fatalf("expected StatusNoTenant for empty tenant, got: %+v", noTenantStatus)
	}

	status, err := mod.GetStatus(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if status.Plan != PlanStarter || status.MaxCollections != 10 || status.BillingStatus != StatusTrialing || status.ReadOnly {
		t.Fatalf("unexpected initial trial status: %+v", status)
	}
	if status.TrialDaysLeft < 6 || status.TrialDaysLeft > 7 {
		t.Fatalf("expected ~7 trial days left, got %d", status.TrialDaysLeft)
	}

	// Create 10 collections (the Starter limit)
	for i := 1; i <= 10; i++ {
		col := &domain.Collection{
			ID:                "col-" + string(rune('A'+i)),
			TenantID:          tenantID,
			Name:              "Tasks",
			DefaultVisibility: domain.VisibilityPrivate,
		}
		if err := repo.CreateCollection(ctx, col); err != nil {
			t.Fatalf("failed creating collection %d/10: %v", i, err)
		}
	}

	// 11th collection should be rejected with ErrCollectionQuotaExceeded
	extraCol := &domain.Collection{
		ID:                "col-11",
		TenantID:          tenantID,
		Name:              "Extra",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	err = repo.CreateCollection(ctx, extraCol)
	if !errors.Is(err, ErrCollectionQuotaExceeded) {
		t.Fatalf("expected ErrCollectionQuotaExceeded on 11th collection, got: %v", err)
	}

	// Creating a record inside an existing collection while at 10/10 should still succeed
	rec := &domain.Record{
		ID:           "rec-1",
		CollectionID: "col-B",
		CreatedBy:    user.ID,
		Title:        "Valid Record",
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
		Data:         `{"ok":true}`,
	}
	if err := repo.CreateRecord(ctx, rec); err != nil {
		t.Fatalf("expected record creation at 10/10 collections to succeed, got: %v", err)
	}

	// Now expire the 7-day trial
	tb, err := mod.GetTenantBilling(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenantBilling failed: %v", err)
	}
	tb.TrialEndsAt = time.Now().UTC().Add(-1 * time.Hour)
	if err := mod.UpdateTenantBilling(ctx, tb); err != nil {
		t.Fatalf("UpdateTenantBilling failed: %v", err)
	}

	statusAfter, err := mod.GetStatus(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if !statusAfter.ReadOnly {
		t.Fatalf("expected ReadOnly=true after trial expired")
	}

	// Writes (CreateRecord, UpdateRecord, DeleteRecord, UpdateCollection) must fail with ErrPaymentRequired
	if err := repo.CreateRecord(ctx, &domain.Record{
		ID:           "rec-blocked",
		CollectionID: "col-B",
		CreatedBy:    user.ID,
		Title:        "Blocked",
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
	}); !errors.Is(err, ErrPaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired on CreateRecord when trial expired, got: %v", err)
	}

	rec.Title = "Updated Title"
	if err := repo.UpdateRecord(ctx, rec, user); !errors.Is(err, ErrPaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired on UpdateRecord when trial expired, got: %v", err)
	}

	if err := repo.DeleteRecord(ctx, "rec-1", user); !errors.Is(err, ErrPaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired on DeleteRecord when trial expired, got: %v", err)
	}

	// Reads must still work while locked in read-only mode!
	fetched, err := repo.GetRecord(ctx, "rec-1", user)
	if err != nil || fetched.ID != "rec-1" {
		t.Fatalf("expected read (GetRecord) to succeed during read-only lock, got: %v", err)
	}

	// Deleting a collection must still be allowed so admins can clean up
	if err := repo.DeleteCollection(ctx, "col-B"); err != nil {
		t.Fatalf("expected DeleteCollection to remain allowed during read-only lock, got: %v", err)
	}
}

func TestVouchers_SeedAndRedeem(t *testing.T) {
	_, repo, mod := setupTestBilling(t, Config{
		Enabled:      true,
		SeedVouchers: "FOUNDER25:pro:2,STARTERFREE:starter:1",
	})
	ctx := context.Background()
	tenantID := "tenant-voucher-1"

	// Lock tenant by expiring its trial
	tb, err := mod.EnsureTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("EnsureTenant failed: %v", err)
	}
	tb.TrialEndsAt = time.Now().UTC().Add(-24 * time.Hour)
	if err := mod.UpdateTenantBilling(ctx, tb); err != nil {
		t.Fatalf("UpdateTenantBilling failed: %v", err)
	}

	// Verify write is blocked
	if err := mod.CheckWriteAccess(ctx, tenantID, true); !errors.Is(err, ErrPaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired before voucher redemption, got: %v", err)
	}

	// Redeem STARTERFREE (max_uses = 1)
	status, err := mod.RedeemVoucher(ctx, tenantID, "starterfree")
	if err != nil {
		t.Fatalf("RedeemVoucher failed: %v", err)
	}
	if status.BillingStatus != StatusFree || status.Plan != PlanStarter || status.MaxCollections != 10 || status.ReadOnly {
		t.Fatalf("unexpected status after starter voucher: %+v", status)
	}
	if status.VoucherCode != "ST****" {
		t.Fatalf("expected masked voucher code ST****, got %q", status.VoucherCode)
	}
	if status.PriceGBP != 0 {
		t.Fatalf("expected PriceGBP=0 for free voucher tenant, got %d", status.PriceGBP)
	}

	// Re-redeeming same voucher by same tenant is idempotent
	if _, err := mod.RedeemVoucher(ctx, tenantID, "STARTERFREE"); err != nil {
		t.Fatalf("expected idempotent voucher redemption for same tenant, got: %v", err)
	}

	// Another tenant trying to use exhausted STARTERFREE should fail
	if _, err := mod.RedeemVoucher(ctx, "tenant-voucher-2", "STARTERFREE"); !errors.Is(err, ErrInvalidVoucher) {
		t.Fatalf("expected ErrInvalidVoucher when max_uses exhausted, got: %v", err)
	}

	// Upgrade tenant-voucher-1 to Pro using FOUNDER25 voucher
	statusPro, err := mod.RedeemVoucher(ctx, tenantID, "founder25")
	if err != nil {
		t.Fatalf("RedeemVoucher FOUNDER25 failed: %v", err)
	}
	if statusPro.BillingStatus != StatusFree || statusPro.Plan != PlanPro || statusPro.MaxCollections != 25 || statusPro.VoucherCode != "FO****" {
		t.Fatalf("unexpected status after pro voucher: %+v", statusPro)
	}

	// Verify tenant can now create more than 10 collections
	for i := 0; i < 12; i++ {
		if err := repo.CreateCollection(ctx, &domain.Collection{
			ID:                "vcol-" + string(rune('a'+i)),
			TenantID:          tenantID,
			Name:              "Voucher Col",
			DefaultVisibility: domain.VisibilityPrivate,
		}); err != nil {
			t.Fatalf("failed creating collection %d with Pro voucher: %v", i, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestGoCardless_Checkout_ImmediateUpgrade_And_Webhooks(t *testing.T) {
	var createdSubAmount atomic.Int32
	var updatedSubAmount atomic.Int32
	var oneOffPaymentAmount atomic.Int32

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sandbox_test_token" {
			http.Error(w, `{"error":{"message":"Unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("GoCardless-Version") != goCardlessAPIVersion {
			http.Error(w, `{"error":{"message":"Bad version"}}`, http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/billing_requests":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"billing_requests": map[string]interface{}{
					"id": "BR123",
				},
			})

		case r.Method == http.MethodPost && r.URL.Path == "/billing_request_flows":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"billing_request_flows": map[string]interface{}{
					"authorisation_url": "https://pay-sandbox.gocardless.com/billing/BRF123",
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/billing_requests/BR123":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"billing_requests": map[string]interface{}{
					"id":     "BR123",
					"status": "fulfilled",
					"metadata": map[string]string{
						"tenant_id": "tenant-gc-1",
						"plan":      "starter",
					},
					"links": map[string]string{
						"customer":                "CU123",
						"mandate_request_mandate": "MD123",
					},
				},
			})

		case r.Method == http.MethodPost && r.URL.Path == "/subscriptions":
			var body struct {
				Subscriptions struct {
					Amount int `json:"amount"`
				} `json:"subscriptions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			createdSubAmount.Store(int32(body.Subscriptions.Amount))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"subscriptions": map[string]interface{}{
					"id": "SB123",
				},
			})

		case r.Method == http.MethodPost && r.URL.Path == "/payments":
			var body struct {
				Payments struct {
					Amount int `json:"amount"`
				} `json:"payments"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			oneOffPaymentAmount.Store(int32(body.Payments.Amount))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"payments": map[string]interface{}{
					"id": "PM_UPGRADE_200",
				},
			})

		case r.Method == http.MethodPut && r.URL.Path == "/subscriptions/SB123":
			var body struct {
				Subscriptions struct {
					Amount int `json:"amount"`
				} `json:"subscriptions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			updatedSubAmount.Store(int32(body.Subscriptions.Amount))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"subscriptions": map[string]interface{}{
					"id": "SB123",
				},
			})

		default:
			http.NotFound(w, r)
		}
	})

	webhookSecret := "whsec_test_secret_123"
	_, _, mod := setupTestBilling(t, Config{
		Enabled:         true,
		AccessToken:     "sandbox_test_token",
		WebhookSecret:   webhookSecret,
		Environment:     "sandbox",
		CustomGCBaseURL: "https://mock-gc.local",
	})
	mod.Client().SetHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			return rec.Result(), nil
		}),
	})

	ctx := context.Background()
	tenantID := "tenant-gc-1"

	// 1. Start checkout for Starter (£3/mo)
	authURL, err := mod.StartCheckout(ctx, tenantID, PlanStarter, "https://vibebac.kr/api/billing/callback", "https://vibebac.kr/login.html")
	if err != nil {
		t.Fatalf("StartCheckout failed: %v", err)
	}
	if authURL != "https://pay-sandbox.gocardless.com/billing/BRF123" {
		t.Fatalf("unexpected authURL: %s", authURL)
	}

	// 2. Complete checkout callback -> creates £3 (300 pence) monthly subscription
	status, err := mod.CompleteCheckout(ctx, tenantID, "BR123")
	if err != nil {
		t.Fatalf("CompleteCheckout failed: %v", err)
	}
	if status.BillingStatus != StatusActive || status.Plan != PlanStarter || status.MaxCollections != 10 || !status.HasActiveMandate {
		t.Fatalf("unexpected status after CompleteCheckout: %+v", status)
	}
	if createdSubAmount.Load() != 300 {
		t.Fatalf("expected initial subscription amount 300 pence (£3), got %d", createdSubAmount.Load())
	}

	// 3. Upgrade from active Starter (£3/mo) to Pro (£5/mo, 25 collections)
	// Must charge £2 (200 pence) immediately AND update subscription to 500 pence (£5/mo)
	upRes, err := mod.UpgradePlan(ctx, tenantID, "https://vibebac.kr/api/billing/callback", "https://vibebac.kr/login.html")
	if err != nil {
		t.Fatalf("UpgradePlan failed: %v", err)
	}
	if !upRes.UpgradedImmediately || upRes.PaymentID != "PM_UPGRADE_200" {
		t.Fatalf("expected immediate upgrade with PM_UPGRADE_200, got: %+v", upRes)
	}
	if oneOffPaymentAmount.Load() != 200 {
		t.Fatalf("expected immediate one-off payment of 200 pence (£2), got %d", oneOffPaymentAmount.Load())
	}
	if updatedSubAmount.Load() != 500 {
		t.Fatalf("expected updated subscription amount of 500 pence (£5), got %d", updatedSubAmount.Load())
	}
	if upRes.Status.Plan != PlanPro || upRes.Status.MaxCollections != 25 || upRes.Status.PriceGBP != 5 {
		t.Fatalf("unexpected status after Pro upgrade: %+v", upRes.Status)
	}

	// 4. Test GoCardless Webhook HMAC verification and mandate cancellation -> read-only lock
	webhookPayload := []byte(`{"events":[{"id":"EV1","resource_type":"mandates","action":"cancelled","links":{"mandate":"MD123"}}]}`)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(webhookPayload)
	sig := hex.EncodeToString(mac.Sum(nil))

	if err := mod.HandleWebhook(ctx, webhookPayload, "invalid_signature"); err == nil {
		t.Fatalf("expected HandleWebhook to reject invalid signature")
	}

	if err := mod.HandleWebhook(ctx, webhookPayload, sig); err != nil {
		t.Fatalf("HandleWebhook failed with valid signature: %v", err)
	}

	afterCancel, err := mod.GetStatus(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if afterCancel.BillingStatus != StatusCancelled || !afterCancel.ReadOnly {
		t.Fatalf("expected tenant to be cancelled and ReadOnly=true after mandate cancelled webhook, got: %+v", afterCancel)
	}
	if err := mod.CheckWriteAccess(ctx, tenantID, false); !errors.Is(err, ErrPaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired after mandate cancellation, got: %v", err)
	}
}
