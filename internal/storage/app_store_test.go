package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollectionSlugAndAppURL(t *testing.T) {
	colID := "550e8400-e29b-41d4-a716-446655440000"
	slug := CollectionSlug(colID)
	if slug != "550e8400e29b41d4a716446655440000" {
		t.Fatalf("unexpected slug: %s", slug)
	}

	prodURL := AppURL("https://vibebac.kr", colID)
	if prodURL != "https://550e8400e29b41d4a716446655440000.vibebac.kr" {
		t.Fatalf("unexpected prod app URL: %s", prodURL)
	}

	localURL := AppURL("http://localhost:8080", colID)
	if localURL != "http://localhost:8080/apps/550e8400e29b41d4a716446655440000" {
		t.Fatalf("unexpected local app URL: %s", localURL)
	}

	customSlug := EffectiveSlug(colID, "My-Cool-App-1")
	if customSlug != "my-cool-app-1" {
		t.Fatalf("expected custom slug my-cool-app-1, got %s", customSlug)
	}
	if got := AppURL("https://vibebac.kr", customSlug); got != "https://my-cool-app-1.vibebac.kr" {
		t.Fatalf("unexpected custom prod app URL: %s", got)
	}
	if got := AppURL("http://localhost:8080", customSlug); got != "http://localhost:8080/apps/my-cool-app-1" {
		t.Fatalf("unexpected custom local app URL: %s", got)
	}
	if fallback := EffectiveSlug(colID, ""); fallback != "550e8400e29b41d4a716446655440000" {
		t.Fatalf("expected fallback dashless UUID, got %s", fallback)
	}
}

func TestValidateAppHTML(t *testing.T) {
	if err := ValidateAppHTML("   "); err == nil {
		t.Fatal("expected error for empty HTML")
	}
	if err := ValidateAppHTML("<!DOCTYPE html><html><body>Hello</body></html>"); err != nil {
		t.Fatalf("unexpected error for valid HTML: %v", err)
	}
	tooLarge := strings.Repeat("a", MaxAppHTMLBytes+1)
	if err := ValidateAppHTML(tooLarge); err == nil {
		t.Fatal("expected error for oversized HTML")
	}
}

func TestLocalAppStore(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewLocalAppStore(tmpDir)
	ctx := context.Background()

	colID := "550e8400-e29b-41d4-a716-446655440000"
	html := "<!DOCTYPE html><html><body>Test App</body></html>"

	if err := store.PutApp(ctx, colID, html); err != nil {
		t.Fatalf("PutApp failed: %v", err)
	}

	got, err := store.GetApp(ctx, colID)
	if err != nil {
		t.Fatalf("GetApp failed: %v", err)
	}
	if got != html {
		t.Fatalf("expected %q, got %q", html, got)
	}

	if err := store.DeleteApp(ctx, colID); err != nil {
		t.Fatalf("DeleteApp failed: %v", err)
	}
	if _, err := store.GetApp(ctx, colID); err == nil {
		t.Fatal("expected error after DeleteApp")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestS3AppStoreSigV4(t *testing.T) {
	var receivedMethod, receivedPath, receivedHost, receivedAuth, receivedCacheControl string
	var receivedBody []byte

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_SESSION_TOKEN", "AQoDYXdzEJr1EXAMPLE")

	store := NewS3AppStore("my-test-bucket", "eu-west-2")
	store.fallback = NewLocalAppStore(t.TempDir())
	store.HTTPClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			receivedMethod = r.Method
			receivedPath = r.URL.Path
			receivedHost = r.Host
			receivedAuth = r.Header.Get("Authorization")
			receivedCacheControl = r.Header.Get("Cache-Control")
			if r.Body != nil {
				receivedBody, _ = io.ReadAll(r.Body)
			}
			rec := httptest.NewRecorder()
			rec.WriteHeader(http.StatusOK)
			return rec.Result(), nil
		}),
	}

	colID := "550e8400-e29b-41d4-a716-446655440000"
	html := "<!DOCTYPE html><html><body>S3 Hosted</body></html>"

	if err := store.PutApp(context.Background(), colID, html); err != nil {
		t.Fatalf("S3 PutApp failed: %v", err)
	}

	if receivedMethod != http.MethodPut {
		t.Fatalf("expected PUT, got %s", receivedMethod)
	}
	if receivedHost != "my-test-bucket.s3.eu-west-2.amazonaws.com" {
		t.Fatalf("unexpected S3 host: %s", receivedHost)
	}
	if receivedPath != "/apps/550e8400e29b41d4a716446655440000/index.html" {
		t.Fatalf("unexpected S3 path: %s", receivedPath)
	}
	if !strings.HasPrefix(receivedAuth, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/") {
		t.Fatalf("unexpected Authorization header: %s", receivedAuth)
	}
	if receivedCacheControl != "public, max-age=0, s-maxage=5, must-revalidate" {
		t.Fatalf("unexpected Cache-Control header: %s", receivedCacheControl)
	}
	if string(receivedBody) != html {
		t.Fatalf("unexpected body: %s", string(receivedBody))
	}

	if err := store.DeleteApp(context.Background(), colID); err != nil {
		t.Fatalf("S3 DeleteApp failed: %v", err)
	}
	if receivedMethod != http.MethodDelete {
		t.Fatalf("expected DELETE, got %s", receivedMethod)
	}
}
