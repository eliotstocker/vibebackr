package ratelimit

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultStarterRPM = 60
	DefaultProRPM     = 180
	DefaultCacheTTL   = 60 * time.Second
)

// ErrRateLimitExceeded is returned when a collection exceeds its per-minute token bucket.
var ErrRateLimitExceeded = errors.New("rate limit exceeded for collection")

// CollectionResolver resolves a collection ID or slug to its canonical ID and owning tenant ID.
type CollectionResolver func(ctx context.Context, idOrSlug string) (canonicalID string, tenantID string, err error)

// PlanResolver returns the active billing plan ("starter" or "pro") for a given tenant ID.
type PlanResolver func(ctx context.Context, tenantID string) string

// Config configures the pluggable per-collection rate-limiting module.
type Config struct {
	Enabled    bool
	StarterRPM int
	ProRPM     int
	CacheTTL   time.Duration
}

// Decision contains rate-limit metadata for HTTP headers and error reporting.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	ResetAfter time.Duration
	RetryAfter time.Duration
}

type bucket struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
	rpm        int
	burst      int
}

type cachedCollection struct {
	canonicalID string
	tenantID    string
	rpm         int
	burst       int
	expiresAt   time.Time
}

// Module implements a zero-dependency, in-memory per-collection token-bucket rate limiter.
type Module struct {
	mu          sync.RWMutex
	enabled     bool
	starterRPM  int
	proRPM      int
	cacheTTL    time.Duration
	resolveCol  CollectionResolver
	resolvePlan PlanResolver
	cache       map[string]cachedCollection
	buckets     map[string]*bucket
	nowFunc     func() time.Time
}

// NewModule creates a new per-collection rate limiter module.
func NewModule(cfg Config, colResolver CollectionResolver, planResolver PlanResolver) *Module {
	starter := cfg.StarterRPM
	if starter <= 0 {
		starter = DefaultStarterRPM
	}
	pro := cfg.ProRPM
	if pro <= 0 {
		pro = DefaultProRPM
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Module{
		enabled:     cfg.Enabled,
		starterRPM:  starter,
		proRPM:      pro,
		cacheTTL:    ttl,
		resolveCol:  colResolver,
		resolvePlan: planResolver,
		cache:       make(map[string]cachedCollection),
		buckets:     make(map[string]*bucket),
		nowFunc:     time.Now,
	}
}

// Enabled reports whether per-collection rate limiting is active.
func (m *Module) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled
}

// SetEnabled dynamically toggles rate limiting.
func (m *Module) SetEnabled(enabled bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.enabled = enabled
	m.mu.Unlock()
}

// SetPlanResolver sets or updates the tenant plan lookup callback.
func (m *Module) SetPlanResolver(pr PlanResolver) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.resolvePlan = pr
	m.cache = make(map[string]cachedCollection)
	m.mu.Unlock()
}

// InvalidateTenant clears cached plan limits for a tenant (e.g. immediately after an upgrade or voucher redemption).
func (m *Module) InvalidateTenant(tenantID string) {
	if m == nil || tenantID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.cache {
		if v.tenantID == tenantID {
			delete(m.cache, k)
		}
	}
}

func burstForRPM(rpm int) int {
	if rpm == 60 {
		return 20
	}
	if rpm == 180 {
		return 45
	}
	b := rpm / 3
	if b < 5 {
		b = rpm
	}
	if b < 1 {
		b = 1
	}
	return b
}

func (m *Module) resolveCollectionConfig(ctx context.Context, colKey string) cachedCollection {
	now := m.nowFunc()
	normKey := strings.ToLower(strings.TrimSpace(colKey))

	m.mu.RLock()
	if cc, ok := m.cache[normKey]; ok && now.Before(cc.expiresAt) {
		m.mu.RUnlock()
		return cc
	}
	colRes := m.resolveCol
	planRes := m.resolvePlan
	starterRPM := m.starterRPM
	proRPM := m.proRPM
	ttl := m.cacheTTL
	m.mu.RUnlock()

	canonicalID := normKey
	tenantID := ""
	if colRes != nil {
		if cid, tid, err := colRes(ctx, colKey); err == nil && cid != "" {
			canonicalID = cid
			tenantID = tid
		}
	}

	rpm := starterRPM
	if planRes != nil && tenantID != "" {
		if strings.EqualFold(planRes(ctx, tenantID), "pro") {
			rpm = proRPM
		}
	}
	burst := burstForRPM(rpm)

	cc := cachedCollection{
		canonicalID: canonicalID,
		tenantID:    tenantID,
		rpm:         rpm,
		burst:       burst,
		expiresAt:   now.Add(ttl),
	}

	m.mu.Lock()
	if len(m.cache) > 4096 {
		for k, v := range m.cache {
			if now.After(v.expiresAt) {
				delete(m.cache, k)
			}
		}
	}
	m.cache[normKey] = cc
	m.cache[strings.ToLower(canonicalID)] = cc
	m.mu.Unlock()

	return cc
}

// Allow checks and consumes 1 token from the collection's rate-limit bucket.
func (m *Module) Allow(ctx context.Context, colIDOrSlug string) (Decision, error) {
	if !m.Enabled() || strings.TrimSpace(colIDOrSlug) == "" {
		return Decision{Allowed: true}, nil
	}

	cc := m.resolveCollectionConfig(ctx, colIDOrSlug)
	now := m.nowFunc()

	m.mu.Lock()
	b, exists := m.buckets[cc.canonicalID]
	if !exists {
		if len(m.buckets) > 8192 {
			// Evict stale buckets idle for > 5 minutes
			cutoff := now.Add(-5 * time.Minute)
			for k, ob := range m.buckets {
				ob.mu.Lock()
				idle := ob.lastRefill.Before(cutoff)
				ob.mu.Unlock()
				if idle {
					delete(m.buckets, k)
				}
			}
		}
		b = &bucket{
			tokens:     float64(cc.burst),
			lastRefill: now,
			rpm:        cc.rpm,
			burst:      cc.burst,
		}
		m.buckets[cc.canonicalID] = b
	}
	m.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()

	// If tenant upgraded/downgraded plan, adjust bucket capacity dynamically
	if b.rpm != cc.rpm || b.burst != cc.burst {
		if cc.burst > b.burst {
			b.tokens += float64(cc.burst - b.burst)
		}
		b.rpm = cc.rpm
		b.burst = cc.burst
		if b.tokens > float64(b.burst) {
			b.tokens = float64(b.burst)
		}
	}

	ratePerSec := float64(b.rpm) / 60.0
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(float64(b.burst), b.tokens+elapsed*ratePerSec)
		b.lastRefill = now
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		rem := int(math.Floor(b.tokens))
		deficit := float64(b.burst) - b.tokens
		resetSec := deficit / ratePerSec
		return Decision{
			Allowed:    true,
			Limit:      b.rpm,
			Remaining:  rem,
			ResetAfter: time.Duration(resetSec * float64(time.Second)),
		}, nil
	}

	needed := 1.0 - b.tokens
	retrySec := math.Ceil(needed / ratePerSec)
	if retrySec < 1 {
		retrySec = 1
	}
	retryDur := time.Duration(retrySec) * time.Second
	return Decision{
		Allowed:    false,
		Limit:      b.rpm,
		Remaining:  0,
		ResetAfter: retryDur,
		RetryAfter: retryDur,
	}, ErrRateLimitExceeded
}

// WriteHeaders attaches standard X-RateLimit-* and Retry-After headers to an HTTP response.
func WriteHeaders(w http.ResponseWriter, d Decision) {
	if w == nil || d.Limit <= 0 {
		return
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
	resetSecs := int(math.Ceil(d.ResetAfter.Seconds()))
	if resetSecs < 1 {
		resetSecs = 1
	}
	w.Header().Set("X-RateLimit-Reset", strconv.Itoa(resetSecs))
	if !d.Allowed {
		retrySecs := int(math.Ceil(d.RetryAfter.Seconds()))
		if retrySecs < 1 {
			retrySecs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retrySecs))
	}
}
