package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eliotstocker/vibebackr/internal/db"
)

// Plan and billing status constants.
const (
	PlanStarter = "starter"
	PlanPro     = "pro"

	StarterCollectionsLimit = 10
	ProCollectionsLimit     = 25

	StarterPricePence  = 300 // £3.00 / month
	ProPricePence      = 500 // £5.00 / month
	ProUpgradeFeePence = 200 // £2.00 immediate one-off upgrade payment

	StatusNoTenant  = "no_tenant"
	StatusTrialing  = "trialing"
	StatusActive    = "active"
	StatusFree      = "free"
	StatusPastDue   = "past_due"
	StatusCancelled = "cancelled"

	DefaultTrialDuration = 7 * 24 * time.Hour
)

var (
	ErrPaymentRequired         = errors.New("payment required: tenant trial expired or subscription inactive (read-only mode)")
	ErrCollectionQuotaExceeded = errors.New("collection quota exceeded for current plan")
	ErrInvalidVoucher          = errors.New("invalid, expired, or exhausted voucher code")
	ErrBillingNotConfigured    = errors.New("gocardless billing is not configured on this instance")
)

// Config holds configuration for the pluggable billing module.
type Config struct {
	Enabled         bool
	AccessToken     string
	WebhookSecret   string
	Environment     string
	CustomGCBaseURL string
	SeedVouchers    string
}

// TenantBilling stores per-tenant subscription, trial, quota, and GoCardless state.
type TenantBilling struct {
	TenantID           string    `json:"tenant_id" db:"tenant_id"`
	Plan               string    `json:"plan" db:"plan"`
	BillingStatus      string    `json:"billing_status" db:"billing_status"`
	MaxCollections     int       `json:"max_collections" db:"max_collections"`
	TrialEndsAt        time.Time `json:"trial_ends_at" db:"trial_ends_at"`
	VoucherCode        string    `json:"-" db:"voucher_code"`
	GCCustomerID       string    `json:"-" db:"gc_customer_id"`
	GCMandateID        string    `json:"-" db:"gc_mandate_id"`
	GCSubscriptionID   string    `json:"-" db:"gc_subscription_id"`
	GCBillingRequestID string    `json:"-" db:"gc_billing_request_id"`
	PendingPlan        string    `json:"pending_plan,omitempty" db:"pending_plan"`
	CreatedAt          time.Time `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time `json:"updated_at" db:"updated_at"`
}

// IsWriteAllowed returns true if the tenant's billing status or active trial permits writes.
func (tb *TenantBilling) IsWriteAllowed(now time.Time) bool {
	if tb == nil {
		return true
	}
	switch tb.BillingStatus {
	case StatusActive, StatusFree:
		return true
	case StatusTrialing:
		return now.Before(tb.TrialEndsAt)
	default:
		return false
	}
}

// Voucher represents a redeemable promotional/complimentary code.
type Voucher struct {
	Code      string     `json:"code" db:"code"`
	Plan      string     `json:"plan" db:"plan"`
	MaxUses   int        `json:"max_uses" db:"max_uses"`
	UsedCount int        `json:"used_count" db:"used_count"`
	ExpiresAt *time.Time `json:"expires_at,omitempty" db:"expires_at"`
	CreatedAt time.Time  `json:"created_at" db:"created_at"`
}

// StatusResponse is the public billing summary returned by REST and MCP endpoints.
type StatusResponse struct {
	Enabled              bool       `json:"enabled"`
	TenantID             string     `json:"tenant_id,omitempty"`
	Plan                 string     `json:"plan,omitempty"`
	PriceGBP             int        `json:"price_gbp,omitempty"`
	BillingStatus        string     `json:"billing_status,omitempty"`
	VoucherCode          string     `json:"voucher_code,omitempty"`
	CollectionsUsed      int        `json:"collections_used"`
	MaxCollections       int        `json:"max_collections,omitempty"`
	TrialEndsAt          *time.Time `json:"trial_ends_at,omitempty"`
	TrialDaysLeft        int        `json:"trial_days_left,omitempty"`
	ReadOnly             bool       `json:"read_only"`
	HasActiveMandate     bool       `json:"has_active_mandate"`
	CanUpgrade           bool       `json:"can_upgrade"`
	GoCardlessConfigured bool       `json:"gocardless_configured"`
}

// UpgradeResult describes the outcome of a plan upgrade request.
type UpgradeResult struct {
	UpgradedImmediately bool            `json:"upgraded_immediately"`
	AuthorisationURL    string          `json:"authorisation_url,omitempty"`
	PaymentID           string          `json:"payment_id,omitempty"`
	Message             string          `json:"message"`
	Status              *StatusResponse `json:"status,omitempty"`
}

const billingCacheTTL = 15 * time.Second

type cachedTenantBilling struct {
	tb        TenantBilling
	colCount  int
	expiresAt time.Time
}

// Module encapsulates all per-tenant billing, quota enforcement, and voucher logic.
type Module struct {
	db          *db.DB
	gc          *GoCardlessClient
	enabled     bool
	cacheMu     sync.RWMutex
	tenantCache map[string]cachedTenantBilling
}

// PlanQuota returns the collection limit for a given plan name.
func PlanQuota(plan string) int {
	if strings.EqualFold(strings.TrimSpace(plan), PlanPro) {
		return ProCollectionsLimit
	}
	return StarterCollectionsLimit
}

// PlanPricePence returns the monthly price in GBP pence for a given plan name.
func PlanPricePence(plan string) int {
	if strings.EqualFold(strings.TrimSpace(plan), PlanPro) {
		return ProPricePence
	}
	return StarterPricePence
}

// NormalizePlan normalizes a plan string to "starter" or "pro".
func NormalizePlan(plan string) string {
	if strings.EqualFold(strings.TrimSpace(plan), PlanPro) {
		return PlanPro
	}
	return PlanStarter
}

// NewModule initializes the billing module, runs its schema migrations, and seeds configured vouchers.
func NewModule(database *db.DB, cfg Config) (*Module, error) {
	gc := NewGoCardlessClient(cfg.AccessToken, cfg.WebhookSecret, cfg.Environment, cfg.CustomGCBaseURL)
	m := &Module{
		db:          database,
		gc:          gc,
		enabled:     cfg.Enabled,
		tenantCache: make(map[string]cachedTenantBilling),
	}
	if err := m.Migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("billing migration failed: %w", err)
	}
	if strings.TrimSpace(cfg.SeedVouchers) != "" {
		if err := m.SeedVouchersFromEnv(context.Background(), cfg.SeedVouchers); err != nil {
			return nil, fmt.Errorf("failed seeding vouchers: %w", err)
		}
	}
	return m, nil
}

// Enabled reports whether billing & quota enforcement is active.
func (m *Module) Enabled() bool {
	return m != nil && m.enabled
}

// SetEnabled dynamically toggles the billing module (useful for tests).
func (m *Module) SetEnabled(enabled bool) {
	if m != nil {
		m.enabled = enabled
		m.invalidateTenant("")
	}
}

func (m *Module) invalidateTenant(tenantID string) {
	if m == nil {
		return
	}
	m.cacheMu.Lock()
	if tenantID == "" || m.tenantCache == nil {
		m.tenantCache = make(map[string]cachedTenantBilling)
	} else {
		delete(m.tenantCache, tenantID)
	}
	m.cacheMu.Unlock()
}

// Client returns the underlying GoCardless client.
func (m *Module) Client() *GoCardlessClient {
	if m == nil {
		return nil
	}
	return m.gc
}

// Migrate backfills any existing tenants into tenant_billing (schema DDL is managed in internal/db/migrations).
func (m *Module) Migrate(ctx context.Context) error {
	if m == nil || m.db == nil {
		return nil
	}

	trialEnd := time.Now().UTC().Add(DefaultTrialDuration)
	if m.db.Driver == db.DriverSQLite {
		_, _ = m.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at)
			SELECT DISTINCT tenant_id, 'starter', 'trialing', 10, ?
			FROM users WHERE tenant_id IS NOT NULL AND tenant_id != ''
		`, trialEnd)
		_, _ = m.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at)
			SELECT DISTINCT tenant_id, 'starter', 'trialing', 10, ?
			FROM collections WHERE tenant_id IS NOT NULL AND tenant_id != ''
		`, trialEnd)
		return nil
	}

	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at)
		SELECT DISTINCT tenant_id, 'starter', 'trialing', 10, $1
		FROM users WHERE tenant_id IS NOT NULL AND tenant_id != ''
		ON CONFLICT (tenant_id) DO NOTHING
	`, trialEnd)
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at)
		SELECT DISTINCT tenant_id, 'starter', 'trialing', 10, $1
		FROM collections WHERE tenant_id IS NOT NULL AND tenant_id != ''
		ON CONFLICT (tenant_id) DO NOTHING
	`, trialEnd)
	return nil
}

// SeedVouchersFromEnv parses comma-separated voucher specs ("CODE[:plan[:max_uses]]") and upserts them.
func (m *Module) SeedVouchersFromEnv(ctx context.Context, raw string) error {
	entries := strings.Split(raw, ",")
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		code := strings.ToUpper(strings.TrimSpace(parts[0]))
		if code == "" {
			continue
		}
		plan := PlanPro
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			plan = NormalizePlan(parts[1])
		}
		maxUses := 1
		if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
			if parsed, err := strconv.Atoi(strings.TrimSpace(parts[2])); err == nil && parsed >= 0 {
				maxUses = parsed
			}
		}
		if err := m.UpsertVoucher(ctx, &Voucher{
			Code:    code,
			Plan:    plan,
			MaxUses: maxUses,
		}); err != nil {
			return err
		}
	}
	return nil
}

// UpsertVoucher creates or updates a voucher code without resetting its existing used_count.
func (m *Module) UpsertVoucher(ctx context.Context, v *Voucher) error {
	code := strings.ToUpper(strings.TrimSpace(v.Code))
	if code == "" {
		return errors.New("voucher code is required")
	}
	v.Code = code
	v.Plan = NormalizePlan(v.Plan)
	if v.MaxUses < 0 {
		v.MaxUses = 0
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}

	var query string
	if m.db.Driver == db.DriverSQLite {
		query = `
		INSERT INTO vouchers (code, plan, max_uses, used_count, expires_at, created_at)
		VALUES (:code, :plan, :max_uses, :used_count, :expires_at, :created_at)
		ON CONFLICT(code) DO UPDATE SET
			plan = excluded.plan,
			max_uses = excluded.max_uses,
			expires_at = excluded.expires_at
		`
	} else {
		query = `
		INSERT INTO vouchers (code, plan, max_uses, used_count, expires_at, created_at)
		VALUES (:code, :plan, :max_uses, :used_count, :expires_at, :created_at)
		ON CONFLICT(code) DO UPDATE SET
			plan = EXCLUDED.plan,
			max_uses = EXCLUDED.max_uses,
			expires_at = EXCLUDED.expires_at
		`
	}
	_, err := m.db.NamedExecContext(ctx, query, v)
	return err
}

// GetVoucher retrieves a voucher by code.
func (m *Module) GetVoucher(ctx context.Context, code string) (*Voucher, error) {
	norm := strings.ToUpper(strings.TrimSpace(code))
	if norm == "" {
		return nil, ErrInvalidVoucher
	}
	var v Voucher
	query := "SELECT code, plan, max_uses, used_count, expires_at, created_at FROM vouchers WHERE code = ?"
	if m.db.Driver == db.DriverPostgres {
		query = "SELECT code, plan, max_uses, used_count, expires_at, created_at FROM vouchers WHERE code = $1"
	}
	if err := m.db.GetContext(ctx, &v, query, norm); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInvalidVoucher
		}
		return nil, err
	}
	return &v, nil
}

// EnsureTenant returns the TenantBilling record for tenantID, provisioning a 7-day Starter trial if none exists.
func (m *Module) EnsureTenant(ctx context.Context, tenantID string) (*TenantBilling, error) {
	if tb, err := m.GetTenantBilling(ctx, tenantID); err == nil {
		return tb, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	now := time.Now().UTC()
	defaultTB := &TenantBilling{
		TenantID:       tenantID,
		Plan:           PlanStarter,
		BillingStatus:  StatusTrialing,
		MaxCollections: StarterCollectionsLimit,
		TrialEndsAt:    now.Add(DefaultTrialDuration),
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	var insertQuery string
	if m.db.Driver == db.DriverSQLite {
		insertQuery = `
		INSERT OR IGNORE INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at, created_at, updated_at)
		VALUES (:tenant_id, :plan, :billing_status, :max_collections, :trial_ends_at, :created_at, :updated_at)
		`
	} else {
		insertQuery = `
		INSERT INTO tenant_billing (tenant_id, plan, billing_status, max_collections, trial_ends_at, created_at, updated_at)
		VALUES (:tenant_id, :plan, :billing_status, :max_collections, :trial_ends_at, :created_at, :updated_at)
		ON CONFLICT (tenant_id) DO NOTHING
		`
	}
	if _, err := m.db.NamedExecContext(ctx, insertQuery, defaultTB); err != nil {
		return nil, err
	}

	return m.GetTenantBilling(ctx, tenantID)
}

// GetTenantBilling fetches an existing TenantBilling row.
func (m *Module) GetTenantBilling(ctx context.Context, tenantID string) (*TenantBilling, error) {
	now := time.Now()
	m.cacheMu.RLock()
	if entry, ok := m.tenantCache[tenantID]; ok && now.Before(entry.expiresAt) {
		cp := entry.tb
		m.cacheMu.RUnlock()
		return &cp, nil
	}
	m.cacheMu.RUnlock()

	var tb TenantBilling
	query := `SELECT tenant_id, plan, billing_status, max_collections, trial_ends_at, voucher_code,
		gc_customer_id, gc_mandate_id, gc_subscription_id, gc_billing_request_id, pending_plan, created_at, updated_at
		FROM tenant_billing WHERE tenant_id = ?`
	if m.db.Driver == db.DriverPostgres {
		query = `SELECT tenant_id, plan, billing_status, max_collections, trial_ends_at, voucher_code,
			gc_customer_id, gc_mandate_id, gc_subscription_id, gc_billing_request_id, pending_plan, created_at, updated_at
			FROM tenant_billing WHERE tenant_id = $1`
	}
	if err := m.db.GetContext(ctx, &tb, query, tenantID); err != nil {
		return nil, err
	}

	m.cacheMu.Lock()
	if m.tenantCache == nil {
		m.tenantCache = make(map[string]cachedTenantBilling)
	}
	prevCount := -1
	if existing, ok := m.tenantCache[tenantID]; ok {
		prevCount = existing.colCount
	}
	m.tenantCache[tenantID] = cachedTenantBilling{
		tb:        tb,
		colCount:  prevCount,
		expiresAt: now.Add(billingCacheTTL),
	}
	m.cacheMu.Unlock()

	return &tb, nil
}

// UpdateTenantBilling persists changes to a TenantBilling record.
func (m *Module) UpdateTenantBilling(ctx context.Context, tb *TenantBilling) error {
	tb.UpdatedAt = time.Now().UTC()
	query := `
	UPDATE tenant_billing
	SET plan = :plan,
	    billing_status = :billing_status,
	    max_collections = :max_collections,
	    trial_ends_at = :trial_ends_at,
	    voucher_code = :voucher_code,
	    gc_customer_id = :gc_customer_id,
	    gc_mandate_id = :gc_mandate_id,
	    gc_subscription_id = :gc_subscription_id,
	    gc_billing_request_id = :gc_billing_request_id,
	    pending_plan = :pending_plan,
	    updated_at = :updated_at
	WHERE tenant_id = :tenant_id
	`
	_, err := m.db.NamedExecContext(ctx, query, tb)
	if err == nil {
		m.invalidateTenant(tb.TenantID)
	}
	return err
}

// CountTenantCollections returns the current number of collections owned by tenantID.
func (m *Module) CountTenantCollections(ctx context.Context, tenantID string) (int, error) {
	if tenantID == "" {
		return 0, nil
	}
	var count int
	query := "SELECT COUNT(*) FROM collections WHERE tenant_id = ?"
	if m.db.Driver == db.DriverPostgres {
		query = "SELECT COUNT(*) FROM collections WHERE tenant_id = $1"
	}
	if err := m.db.GetContext(ctx, &count, query, tenantID); err != nil {
		return 0, err
	}
	m.cacheMu.Lock()
	if entry, ok := m.tenantCache[tenantID]; ok {
		entry.colCount = count
		m.tenantCache[tenantID] = entry
	}
	m.cacheMu.Unlock()
	return count, nil
}

// CheckWriteAccess enforces the read-only lock and collection quota when the billing module is enabled.
func (m *Module) CheckWriteAccess(ctx context.Context, tenantID string, isCreatingCollection bool) error {
	if !m.Enabled() || tenantID == "" {
		return nil
	}
	tb, err := m.EnsureTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	if !tb.IsWriteAllowed(time.Now().UTC()) {
		return ErrPaymentRequired
	}
	if !isCreatingCollection {
		m.cacheMu.RLock()
		entry, ok := m.tenantCache[tenantID]
		m.cacheMu.RUnlock()
		if ok && entry.colCount >= 0 && entry.colCount <= tb.MaxCollections {
			return nil
		}
	}
	count, err := m.CountTenantCollections(ctx, tenantID)
	if err != nil {
		return err
	}
	if isCreatingCollection {
		if count >= tb.MaxCollections {
			return fmt.Errorf("%w: %d/%d collections used on %s plan", ErrCollectionQuotaExceeded, count, tb.MaxCollections, tb.Plan)
		}
		// Optimistically increment cached count for the collection being created
		m.cacheMu.Lock()
		if entry, ok := m.tenantCache[tenantID]; ok {
			entry.colCount = count + 1
			m.tenantCache[tenantID] = entry
		}
		m.cacheMu.Unlock()
		return nil
	}
	if count > tb.MaxCollections {
		return fmt.Errorf("%w: %d/%d collections used on %s plan (read-only until under quota or upgraded)", ErrCollectionQuotaExceeded, count, tb.MaxCollections, tb.Plan)
	}
	return nil
}

// GetStatus returns the comprehensive billing and quota status for a tenant.
func (m *Module) GetStatus(ctx context.Context, tenantID string) (*StatusResponse, error) {
	if !m.Enabled() {
		return &StatusResponse{Enabled: false}, nil
	}
	if tenantID == "" {
		return &StatusResponse{
			Enabled:              true,
			Plan:                 PlanStarter,
			PriceGBP:             StarterPricePence / 100,
			BillingStatus:        StatusNoTenant,
			MaxCollections:       StarterCollectionsLimit,
			CanUpgrade:           true,
			GoCardlessConfigured: m.gc.IsConfigured(),
		}, nil
	}

	tb, err := m.EnsureTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	count, err := m.CountTenantCollections(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	writeAllowed := tb.IsWriteAllowed(now)
	readOnly := !writeAllowed || count > tb.MaxCollections

	var trialEnds *time.Time
	trialDaysLeft := 0
	if tb.BillingStatus == StatusTrialing {
		t := tb.TrialEndsAt
		trialEnds = &t
		if now.Before(tb.TrialEndsAt) {
			rem := tb.TrialEndsAt.Sub(now)
			trialDaysLeft = int((rem + 24*time.Hour - 1) / (24 * time.Hour))
		}
	}

	priceGBP := PlanPricePence(tb.Plan) / 100
	if tb.BillingStatus == StatusFree {
		priceGBP = 0
	}

	canUpgrade := tb.Plan != PlanPro || (!writeAllowed && tb.BillingStatus != StatusFree)

	return &StatusResponse{
		Enabled:              true,
		TenantID:             tb.TenantID,
		Plan:                 tb.Plan,
		PriceGBP:             priceGBP,
		BillingStatus:        tb.BillingStatus,
		VoucherCode:          MaskVoucherCode(tb.VoucherCode),
		CollectionsUsed:      count,
		MaxCollections:       tb.MaxCollections,
		TrialEndsAt:          trialEnds,
		TrialDaysLeft:        trialDaysLeft,
		ReadOnly:             readOnly,
		HasActiveMandate:     tb.GCMandateID != "" && tb.GCSubscriptionID != "" && tb.BillingStatus == StatusActive,
		CanUpgrade:           canUpgrade,
		GoCardlessConfigured: m.gc.IsConfigured(),
	}, nil
}

// MaskVoucherCode redacts a voucher code so the full code cannot be extracted and shared from status responses.
func MaskVoucherCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	if len(code) <= 4 {
		return "****"
	}
	return code[:2] + "****"
}

// RedeemVoucher validates and applies a voucher code to a tenant, unlocking free Starter or Pro access.
func (m *Module) RedeemVoucher(ctx context.Context, tenantID, rawCode string) (*StatusResponse, error) {
	if tenantID == "" {
		return nil, errors.New("tenant_id is required")
	}
	code := strings.ToUpper(strings.TrimSpace(rawCode))
	if code == "" {
		return nil, ErrInvalidVoucher
	}

	tb, err := m.EnsureTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	// Idempotent if tenant already redeemed this exact voucher
	if strings.EqualFold(tb.VoucherCode, code) && tb.BillingStatus == StatusFree {
		return m.GetStatus(ctx, tenantID)
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var v Voucher
	vQuery := "SELECT code, plan, max_uses, used_count, expires_at, created_at FROM vouchers WHERE code = ?"
	if m.db.Driver == db.DriverPostgres {
		vQuery = "SELECT code, plan, max_uses, used_count, expires_at, created_at FROM vouchers WHERE code = $1 FOR UPDATE"
	}
	if err := tx.GetContext(ctx, &v, vQuery, code); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInvalidVoucher
		}
		return nil, err
	}

	now := time.Now().UTC()
	if v.ExpiresAt != nil && now.After(*v.ExpiresAt) {
		return nil, ErrInvalidVoucher
	}
	if v.MaxUses > 0 && v.UsedCount >= v.MaxUses {
		return nil, ErrInvalidVoucher
	}

	incQuery := "UPDATE vouchers SET used_count = used_count + 1 WHERE code = ?"
	if m.db.Driver == db.DriverPostgres {
		incQuery = "UPDATE vouchers SET used_count = used_count + 1 WHERE code = $1"
	}
	if _, err := tx.ExecContext(ctx, incQuery, code); err != nil {
		return nil, err
	}

	oldSubID := tb.GCSubscriptionID
	plan := NormalizePlan(v.Plan)
	maxCols := PlanQuota(plan)

	updQuery := `
	UPDATE tenant_billing
	SET plan = ?, billing_status = ?, max_collections = ?, voucher_code = ?, gc_subscription_id = '', pending_plan = '', updated_at = ?
	WHERE tenant_id = ?
	`
	if m.db.Driver == db.DriverPostgres {
		updQuery = `
		UPDATE tenant_billing
		SET plan = $1, billing_status = $2, max_collections = $3, voucher_code = $4, gc_subscription_id = '', pending_plan = '', updated_at = $5
		WHERE tenant_id = $6
		`
	}
	if _, err := tx.ExecContext(ctx, updQuery, plan, StatusFree, maxCols, code, now, tenantID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.invalidateTenant(tenantID)

	if oldSubID != "" && m.gc.IsConfigured() {
		_ = m.gc.CancelSubscription(ctx, oldSubID)
	}

	return m.GetStatus(ctx, tenantID)
}

// StartCheckout initiates a GoCardless Billing Request Flow for the requested plan ("starter" or "pro").
func (m *Module) StartCheckout(ctx context.Context, tenantID, rawPlan, redirectURI, exitURI string) (string, error) {
	if !m.gc.IsConfigured() {
		return "", ErrBillingNotConfigured
	}
	plan := NormalizePlan(rawPlan)
	tb, err := m.EnsureTenant(ctx, tenantID)
	if err != nil {
		return "", err
	}

	brID, err := m.gc.CreateBillingRequest(ctx, tenantID, plan)
	if err != nil {
		return "", err
	}
	authURL, err := m.gc.CreateBillingRequestFlow(ctx, brID, redirectURI, exitURI)
	if err != nil {
		return "", err
	}

	tb.GCBillingRequestID = brID
	tb.PendingPlan = plan
	if err := m.UpdateTenantBilling(ctx, tb); err != nil {
		return "", err
	}
	return authURL, nil
}

// CompleteCheckout finalizes a GoCardless Billing Request, creates the monthly subscription, and activates the tenant.
func (m *Module) CompleteCheckout(ctx context.Context, tenantID, billingReqID string) (*StatusResponse, error) {
	if !m.gc.IsConfigured() {
		return nil, ErrBillingNotConfigured
	}

	var tb *TenantBilling
	var err error
	if tenantID != "" {
		tb, err = m.EnsureTenant(ctx, tenantID)
		if err != nil {
			return nil, err
		}
	} else if billingReqID != "" {
		tb, err = m.findTenantByGC(ctx, billingReqID, "", "")
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("missing tenant_id and billing_request_id")
	}

	if billingReqID == "" {
		billingReqID = tb.GCBillingRequestID
	}
	if billingReqID == "" {
		return nil, errors.New("no pending billing request found for tenant")
	}

	brInfo, err := m.gc.GetBillingRequest(ctx, billingReqID)
	if err != nil {
		return nil, err
	}
	if brInfo.MandateID == "" {
		return nil, fmt.Errorf("direct debit mandate not yet authorized (billing request status: %s)", brInfo.Status)
	}

	plan := tb.PendingPlan
	if plan == "" {
		plan = brInfo.Plan
	}
	plan = NormalizePlan(plan)

	subID := tb.GCSubscriptionID
	if subID == "" {
		subID, err = m.gc.CreateSubscription(ctx, brInfo.MandateID, PlanPricePence(plan), plan, tb.TenantID)
		if err != nil {
			return nil, err
		}
	} else if tb.Plan != plan {
		if err := m.gc.UpdateSubscriptionAmount(ctx, subID, PlanPricePence(plan), plan); err != nil {
			return nil, err
		}
	}

	tb.Plan = plan
	tb.BillingStatus = StatusActive
	tb.MaxCollections = PlanQuota(plan)
	if brInfo.CustomerID != "" {
		tb.GCCustomerID = brInfo.CustomerID
	}
	tb.GCMandateID = brInfo.MandateID
	tb.GCSubscriptionID = subID
	tb.PendingPlan = ""
	tb.VoucherCode = ""

	if err := m.UpdateTenantBilling(ctx, tb); err != nil {
		return nil, err
	}
	return m.GetStatus(ctx, tb.TenantID)
}

// UpgradePlan upgrades a tenant from Starter (10 collections, £3/mo) to Pro (25 collections, £5/mo).
// If the tenant already has an active GoCardless mandate & subscription, it charges £2 immediately and updates the subscription to £5/mo.
// Otherwise, it starts a GoCardless checkout flow for the £5/mo Pro plan.
func (m *Module) UpgradePlan(ctx context.Context, tenantID, redirectURI, exitURI string) (*UpgradeResult, error) {
	if !m.gc.IsConfigured() {
		return nil, ErrBillingNotConfigured
	}
	tb, err := m.EnsureTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	if tb.Plan == PlanPro && (tb.BillingStatus == StatusActive || tb.BillingStatus == StatusFree) {
		status, _ := m.GetStatus(ctx, tenantID)
		return &UpgradeResult{
			UpgradedImmediately: true,
			Message:             "Tenant is already on the Pro plan (25 collections)",
			Status:              status,
		}, nil
	}

	// Active GoCardless mandate + subscription: charge £2 one-off payment and update subscription to £5/mo
	if tb.BillingStatus == StatusActive && tb.GCMandateID != "" && tb.GCSubscriptionID != "" {
		idempotencyKey := fmt.Sprintf("upgrade-%s-%s", tenantID, tb.GCSubscriptionID)
		paymentID, err := m.gc.ChargeOneOffPayment(ctx, tb.GCMandateID, ProUpgradeFeePence, "VibeBackr Pro Plan Upgrade (£2.00)", idempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("failed charging £2 upgrade payment: %w", err)
		}
		if err := m.gc.UpdateSubscriptionAmount(ctx, tb.GCSubscriptionID, ProPricePence, PlanPro); err != nil {
			return nil, fmt.Errorf("failed updating subscription to £5/month: %w", err)
		}

		tb.Plan = PlanPro
		tb.MaxCollections = ProCollectionsLimit
		tb.PendingPlan = ""
		if err := m.UpdateTenantBilling(ctx, tb); err != nil {
			return nil, err
		}
		status, _ := m.GetStatus(ctx, tenantID)
		return &UpgradeResult{
			UpgradedImmediately: true,
			PaymentID:           paymentID,
			Message:             "Upgraded to Pro plan (25 collections). Charged £2.00 immediate upgrade fee and updated subscription to £5.00/month.",
			Status:              status,
		}, nil
	}

	// No active mandate yet (e.g. trialing, expired, or free starter voucher): start checkout for Pro (£5/mo)
	authURL, err := m.StartCheckout(ctx, tenantID, PlanPro, redirectURI, exitURI)
	if err != nil {
		return nil, err
	}
	status, _ := m.GetStatus(ctx, tenantID)
	return &UpgradeResult{
		UpgradedImmediately: false,
		AuthorisationURL:    authURL,
		Message:             "Complete GoCardless Direct Debit authorization to activate the £5/month Pro plan (25 collections).",
		Status:              status,
	}, nil
}

func (m *Module) findTenantByGC(ctx context.Context, billingReqID, mandateID, subscriptionID string) (*TenantBilling, error) {
	var tb TenantBilling
	if billingReqID != "" {
		q := `SELECT tenant_id, plan, billing_status, max_collections, trial_ends_at, voucher_code,
			gc_customer_id, gc_mandate_id, gc_subscription_id, gc_billing_request_id, pending_plan, created_at, updated_at
			FROM tenant_billing WHERE gc_billing_request_id = ? LIMIT 1`
		if m.db.Driver == db.DriverPostgres {
			q = `SELECT tenant_id, plan, billing_status, max_collections, trial_ends_at, voucher_code,
				gc_customer_id, gc_mandate_id, gc_subscription_id, gc_billing_request_id, pending_plan, created_at, updated_at
				FROM tenant_billing WHERE gc_billing_request_id = $1 LIMIT 1`
		}
		if err := m.db.GetContext(ctx, &tb, q, billingReqID); err == nil {
			return &tb, nil
		}
	}
	if subscriptionID != "" {
		q := `SELECT tenant_id, plan, billing_status, max_collections, trial_ends_at, voucher_code,
			gc_customer_id, gc_mandate_id, gc_subscription_id, gc_billing_request_id, pending_plan, created_at, updated_at
			FROM tenant_billing WHERE gc_subscription_id = ? LIMIT 1`
		if m.db.Driver == db.DriverPostgres {
			q = `SELECT tenant_id, plan, billing_status, max_collections, trial_ends_at, voucher_code,
				gc_customer_id, gc_mandate_id, gc_subscription_id, gc_billing_request_id, pending_plan, created_at, updated_at
				FROM tenant_billing WHERE gc_subscription_id = $1 LIMIT 1`
		}
		if err := m.db.GetContext(ctx, &tb, q, subscriptionID); err == nil {
			return &tb, nil
		}
	}
	if mandateID != "" {
		q := `SELECT tenant_id, plan, billing_status, max_collections, trial_ends_at, voucher_code,
			gc_customer_id, gc_mandate_id, gc_subscription_id, gc_billing_request_id, pending_plan, created_at, updated_at
			FROM tenant_billing WHERE gc_mandate_id = ? LIMIT 1`
		if m.db.Driver == db.DriverPostgres {
			q = `SELECT tenant_id, plan, billing_status, max_collections, trial_ends_at, voucher_code,
				gc_customer_id, gc_mandate_id, gc_subscription_id, gc_billing_request_id, pending_plan, created_at, updated_at
				FROM tenant_billing WHERE gc_mandate_id = $1 LIMIT 1`
		}
		if err := m.db.GetContext(ctx, &tb, q, mandateID); err == nil {
			return &tb, nil
		}
	}
	return nil, sql.ErrNoRows
}

// HandleWebhook verifies and processes incoming GoCardless webhook events.
func (m *Module) HandleWebhook(ctx context.Context, payload []byte, signatureHeader string) error {
	if !m.gc.VerifyWebhookSignature(payload, signatureHeader) {
		return errors.New("invalid gocardless webhook signature")
	}

	var envelope struct {
		Events []WebhookEvent `json:"events"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("invalid webhook payload: %w", err)
	}

	for _, ev := range envelope.Events {
		switch ev.ResourceType {
		case "billing_requests":
			if ev.Action == "fulfilled" && ev.Links.BillingRequest != "" {
				_, _ = m.CompleteCheckout(ctx, "", ev.Links.BillingRequest)
			}

		case "mandates":
			if ev.Action == "cancelled" || ev.Action == "failed" || ev.Action == "expired" {
				if tb, err := m.findTenantByGC(ctx, "", ev.Links.Mandate, ""); err == nil && tb.BillingStatus != StatusFree {
					tb.BillingStatus = StatusCancelled
					_ = m.UpdateTenantBilling(ctx, tb)
				}
			}

		case "subscriptions":
			if ev.Action == "cancelled" || ev.Action == "finished" {
				if tb, err := m.findTenantByGC(ctx, "", ev.Links.Mandate, ev.Links.Subscription); err == nil && tb.BillingStatus != StatusFree {
					tb.BillingStatus = StatusCancelled
					_ = m.UpdateTenantBilling(ctx, tb)
				}
			}

		case "payments":
			if ev.Action == "failed" {
				if tb, err := m.findTenantByGC(ctx, "", ev.Links.Mandate, ev.Links.Subscription); err == nil && tb.BillingStatus != StatusFree {
					tb.BillingStatus = StatusPastDue
					_ = m.UpdateTenantBilling(ctx, tb)
				}
			} else if ev.Action == "confirmed" || ev.Action == "paid_out" {
				if tb, err := m.findTenantByGC(ctx, "", ev.Links.Mandate, ev.Links.Subscription); err == nil && tb.BillingStatus == StatusPastDue {
					tb.BillingStatus = StatusActive
					_ = m.UpdateTenantBilling(ctx, tb)
				}
			}
		}
	}

	return nil
}
