package ratelimit

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterStarterAndProPlans(t *testing.T) {
	ctx := context.Background()
	tenantPlans := map[string]string{
		"tenant-starter": "starter",
		"tenant-pro":     "pro",
	}

	mod := NewModule(
		Config{
			Enabled:    true,
			StarterRPM: 60,
			ProRPM:     180,
			CacheTTL:   60 * time.Second,
		},
		func(ctx context.Context, idOrSlug string) (string, string, error) {
			if idOrSlug == "tasks" || idOrSlug == "col-starter" {
				return "col-starter", "tenant-starter", nil
			}
			if idOrSlug == "col-pro" {
				return "col-pro", "tenant-pro", nil
			}
			return idOrSlug, "tenant-starter", nil
		},
		func(ctx context.Context, tenantID string) string {
			return tenantPlans[tenantID]
		},
	)

	now := time.Now()
	mod.nowFunc = func() time.Time { return now }

	// Starter plan: 60 RPM -> burst 20
	for i := 0; i < 20; i++ {
		slugOrID := "col-starter"
		if i%2 == 1 {
			slugOrID = "tasks" // Verify slug and canonical ID share the same bucket
		}
		d, err := mod.Allow(ctx, slugOrID)
		if err != nil || !d.Allowed {
			t.Fatalf("expected starter request %d to be allowed, got err=%v", i+1, err)
		}
		if d.Limit != 60 {
			t.Fatalf("expected limit 60, got %d", d.Limit)
		}
	}

	// 21st immediate request should be rejected
	d, err := mod.Allow(ctx, "col-starter")
	if err != ErrRateLimitExceeded || d.Allowed {
		t.Fatalf("expected 21st burst request on starter collection to fail, got allowed=%v err=%v", d.Allowed, err)
	}
	if d.RetryAfter < time.Second {
		t.Fatalf("expected RetryAfter >= 1s, got %v", d.RetryAfter)
	}

	// Advance clock by 2 seconds -> refills 2 tokens (at 60 RPM = 1 token/sec)
	now = now.Add(2 * time.Second)
	for i := 0; i < 2; i++ {
		d, err := mod.Allow(ctx, "col-starter")
		if err != nil || !d.Allowed {
			t.Fatalf("expected refilled request %d to succeed, got err=%v", i+1, err)
		}
	}
	if _, err := mod.Allow(ctx, "col-starter"); err != ErrRateLimitExceeded {
		t.Fatalf("expected 3rd request after 2s refill to be rate limited")
	}

	// Pro plan: 180 RPM -> burst 45
	for i := 0; i < 45; i++ {
		d, err := mod.Allow(ctx, "col-pro")
		if err != nil || !d.Allowed {
			t.Fatalf("expected pro request %d to be allowed, got err=%v", i+1, err)
		}
		if d.Limit != 180 {
			t.Fatalf("expected pro limit 180, got %d", d.Limit)
		}
	}
	if _, err := mod.Allow(ctx, "col-pro"); err != ErrRateLimitExceeded {
		t.Fatalf("expected 46th immediate request on pro collection to be rate limited")
	}

	// Upgrade tenant-starter to pro and invalidate cache
	tenantPlans["tenant-starter"] = "pro"
	mod.InvalidateTenant("tenant-starter")
	dUp, err := mod.Allow(ctx, "col-starter")
	if err != nil || !dUp.Allowed || dUp.Limit != 180 {
		t.Fatalf("expected upgraded tenant collection to immediately get pro capacity (180 RPM), got allowed=%v limit=%d err=%v", dUp.Allowed, dUp.Limit, err)
	}
}

func TestRateLimiterDisabledAndHeaders(t *testing.T) {
	ctx := context.Background()
	var nilMod *Module
	if nilMod.Enabled() {
		t.Fatalf("expected nil module to be disabled")
	}
	if d, err := nilMod.Allow(ctx, "col-1"); err != nil || !d.Allowed {
		t.Fatalf("expected nil module to allow all requests")
	}

	mod := NewModule(Config{Enabled: false}, nil, nil)
	for i := 0; i < 100; i++ {
		if d, err := mod.Allow(ctx, "col-1"); err != nil || !d.Allowed {
			t.Fatalf("expected disabled module to allow request %d", i)
		}
	}

	mod.SetEnabled(true)
	if !mod.Enabled() {
		t.Fatalf("expected module to be enabled after SetEnabled(true)")
	}

	rec := httptest.NewRecorder()
	WriteHeaders(rec, Decision{
		Allowed:    false,
		Limit:      60,
		Remaining:  0,
		ResetAfter: 15 * time.Second,
		RetryAfter: 2 * time.Second,
	})
	if rec.Header().Get("X-RateLimit-Limit") != "60" {
		t.Fatalf("expected X-RateLimit-Limit=60, got %q", rec.Header().Get("X-RateLimit-Limit"))
	}
	if rec.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("expected X-RateLimit-Remaining=0, got %q", rec.Header().Get("X-RateLimit-Remaining"))
	}
	if rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("expected Retry-After=2, got %q", rec.Header().Get("Retry-After"))
	}
}
