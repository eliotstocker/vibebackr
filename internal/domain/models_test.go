package domain_test

import (
	"testing"

	"github.com/eliotstocker/vibebackr/internal/domain"
)

func TestJSONStringsScanAndValue(t *testing.T) {
	var js domain.JSONStrings = []string{"admin.corp", "dev.corp"}

	val, err := js.Value()
	if err != nil {
		t.Fatalf("JSONStrings.Value failed: %v", err)
	}

	var scanned domain.JSONStrings
	if err := scanned.Scan(val); err != nil {
		t.Fatalf("JSONStrings.Scan failed: %v", err)
	}

	if len(scanned) != 2 || scanned[0] != "admin.corp" || scanned[1] != "dev.corp" {
		t.Fatalf("unexpected scanned JSONStrings: %v", scanned)
	}

	// Test nil scan
	var nilScanned domain.JSONStrings
	if err := nilScanned.Scan(nil); err != nil || len(nilScanned) != 0 {
		t.Fatalf("expected empty slice on nil scan, got %v", nilScanned)
	}
}

func TestAccessPolicyScanAndValue(t *testing.T) {
	policy := domain.AccessPolicy{
		Visibility: domain.VisibilityShared,
		Shares:     []string{"user1@test.com", "user2@test.com"},
	}

	val, err := policy.Value()
	if err != nil {
		t.Fatalf("AccessPolicy.Value failed: %v", err)
	}

	var scanned domain.AccessPolicy
	if err := scanned.Scan(val); err != nil {
		t.Fatalf("AccessPolicy.Scan failed: %v", err)
	}

	if scanned.Visibility != domain.VisibilityShared || len(scanned.Shares) != 2 {
		t.Fatalf("unexpected scanned AccessPolicy: %+v", scanned)
	}

	// Test nil scan fallback
	var defaultPolicy domain.AccessPolicy
	if err := defaultPolicy.Scan(nil); err != nil || defaultPolicy.Visibility != domain.VisibilityPrivate {
		t.Fatalf("expected private visibility on nil scan, got %+v", defaultPolicy)
	}
}

func TestValidateSlug(t *testing.T) {
	validCases := map[string]string{
		"":                  "",
		"   ":               "",
		"my-project":        "my-project",
		"My-Project-123":    "my-project-123",
		"  ALPHA-99  ":      "alpha-99",
		"a":                 "a",
		"9":                 "9",
		"vibe--app":         "vibe--app",
		"a1-b2-c3-d4-e5-f6": "a1-b2-c3-d4-e5-f6",
	}
	for input, expected := range validCases {
		got, err := domain.ValidateSlug(input)
		if err != nil {
			t.Fatalf("expected %q to be valid, got error: %v", input, err)
		}
		if got != expected {
			t.Fatalf("expected %q -> %q, got %q", input, expected, got)
		}
	}

	invalidCases := []string{
		"-leading-dash",
		"trailing-dash-",
		"-",
		"has_underscore",
		"has space",
		"has.dot",
		"special!char",
		"api",
		"WWW",
		"MCP",
		"login",
		"apps",
		"a234567890123456789012345678901234567890123456789012345678901234", // 64 chars
	}
	for _, input := range invalidCases {
		if got, err := domain.ValidateSlug(input); err == nil {
			t.Fatalf("expected %q to fail validation, but got %q", input, got)
		}
	}
}

