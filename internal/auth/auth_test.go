package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/eliotstocker/vibebackr/internal/auth"
	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/repository"
)

func TestTokenManager(t *testing.T) {
	tm := auth.NewTokenManager("my-secret-key", 2*time.Second)

	user := &domain.User{
		ID:    "user_test_jwt",
		Email: "jwt@corp.com",
		Role:  domain.RoleManager,
	}

	tokenStr, err := tm.GenerateToken(user)
	if err != nil {
		t.Fatalf("failed generating token: %v", err)
	}

	claims, err := tm.ValidateToken(tokenStr)
	if err != nil {
		t.Fatalf("failed validating token: %v", err)
	}

	if claims.UserID != user.ID || claims.Role != user.Role || claims.Email != user.Email {
		t.Fatalf("unexpected claims payload: %+v", claims)
	}

	// Validate bad secret fails
	badTm := auth.NewTokenManager("wrong-secret-key", 2*time.Second)
	if _, err := badTm.ValidateToken(tokenStr); err == nil {
		t.Fatalf("expected validation failure with bad secret")
	}
}

func TestVerifyPKCE(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challengeS256 := base64.RawURLEncoding.EncodeToString(sum[:])

	// S256 valid
	if !auth.VerifyPKCE(verifier, challengeS256, "S256") {
		t.Errorf("expected S256 PKCE to verify successfully")
	}

	// S256 invalid
	if auth.VerifyPKCE("wrong_verifier", challengeS256, "S256") {
		t.Errorf("expected S256 PKCE to fail on wrong verifier")
	}

	// Plain valid
	if !auth.VerifyPKCE("my-plain-verifier", "my-plain-verifier", "plain") {
		t.Errorf("expected plain PKCE to verify successfully")
	}

	// Empty verifier fails
	if auth.VerifyPKCE("", "", "S256") {
		t.Errorf("empty verifier should fail")
	}
}

func TestDCRAndRedirectURI(t *testing.T) {
	req := auth.DCRRegistrationRequest{
		ClientName:   "Test Agent",
		RedirectURIs: []string{"https://agent.local/callback"},
		GrantTypes:   []string{"authorization_code"},
	}

	client := auth.RegisterClient(req)
	if client.ClientName != "Test Agent" || len(client.RedirectURIs) != 1 {
		t.Fatalf("unexpected client registration: %+v", client)
	}

	if err := auth.ValidateRedirectURI(client, "https://agent.local/callback"); err != nil {
		t.Errorf("expected valid redirect uri: %v", err)
	}

	if err := auth.ValidateRedirectURI(client, "https://evil.com"); err == nil {
		t.Errorf("expected unauthorized redirect uri to fail")
	}
}

func TestSSOManagerAndJITProvisioning(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:ssotest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db error: %v", err)
	}
	repo := repository.New(database)
	sm := auth.NewSSOManager(repo)

	// Verify Mock provider is disabled by default
	if _, err := sm.GetProvider("mock"); err == nil {
		t.Fatalf("expected mock SSO provider to be disabled by default")
	}

	// Enable mock provider for testing
	t.Setenv("ENABLE_MOCK_AUTH", "true")
	sm.RegisterProvider(&auth.MockSSOProvider{})
	p, err := sm.GetProvider("mock")
	if err != nil || p.Name() != "mock" {
		t.Fatalf("failed getting mock SSO provider: %v", err)
	}

	// 0. Auto-bootstrap initial admin when system has 0 users
	bootstrapAdminInfo := &auth.UserInfo{
		Email: "founder@startup.com",
		Name:  "Founder",
	}
	bootstrappedAdmin, err := sm.ProvisionOrGetUser(ctx, bootstrapAdminInfo)
	if err != nil {
		t.Fatalf("failed auto-bootstrapping initial admin: %v", err)
	}
	if bootstrappedAdmin.Role != domain.RoleAdmin {
		t.Fatalf("expected initial bootstrapped user to have role 'admin', got '%s'", bootstrappedAdmin.Role)
	}

	// 1. Now that admin exists, uninvited & unallowed domain user -> should fail
	unauthorizedInfo := &auth.UserInfo{
		Email: "intruder@unknown.com",
		Name:  "Intruder",
	}
	if _, err := sm.ProvisionOrGetUser(ctx, unauthorizedInfo); err == nil {
		t.Fatalf("expected unauthorized user to fail JIT provisioning")
	}

	// 2. Setup allowed domain in a collection
	col := &domain.Collection{
		ID:             "col_allowed",
		Name:           "Public Docs",
		AllowedDomains: domain.JSONStrings{"acme.com"},
	}
	if err := repo.CreateCollection(ctx, col); err != nil {
		t.Fatalf("failed creating collection: %v", err)
	}

	// Now acme.com user should successfully JIT provision
	acmeUser := &auth.UserInfo{
		Email: "alice@acme.com",
		Name:  "Alice Acme",
	}
	u1, err := sm.ProvisionOrGetUser(ctx, acmeUser)
	if err != nil {
		t.Fatalf("failed JIT provisioning allowed domain user: %v", err)
	}
	if u1.Email != "alice@acme.com" || u1.Role != domain.RoleMember {
		t.Fatalf("unexpected user: %+v", u1)
	}

	// Calling again should return existing user
	u2, err := sm.ProvisionOrGetUser(ctx, acmeUser)
	if err != nil || u2.ID != u1.ID {
		t.Fatalf("expected existing user returned")
	}

	// 3. JIT provisioning via Invite
	inv := &domain.Invite{
		Email:     "bob@special.org",
		Role:      domain.RoleManager,
		CreatedBy: u1.ID,
	}
	if err := repo.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("failed creating invite: %v", err)
	}

	bobInfo := &auth.UserInfo{
		Email: "bob@special.org",
		Name:  "Bob Special",
	}
	uBob, err := sm.ProvisionOrGetUser(ctx, bobInfo)
	if err != nil {
		t.Fatalf("failed JIT provisioning invited user: %v", err)
	}
	if uBob.Role != domain.RoleManager {
		t.Fatalf("expected role manager from invite, got %s", uBob.Role)
	}

	// 4. Mock SSO developer provisioning
	mockDevInfo := &auth.UserInfo{
		Email: "developer@example.com",
		Name:  "Test Developer",
		Sub:   "mock-user-123",
	}
	uMock, err := sm.ProvisionOrGetUser(ctx, mockDevInfo)
	if err != nil {
		t.Fatalf("failed JIT provisioning mock developer: %v", err)
	}
	if uMock.Email != "developer@example.com" {
		t.Fatalf("unexpected mock user email: %s", uMock.Email)
	}
}

func TestGenericOIDCAndGitHubProviders(t *testing.T) {
	// Generic OIDC Provider
	oidc := auth.NewGenericOIDCProvider(
		"okta",
		"okta-client-123",
		"okta-sec-456",
		"https://company.okta.com/oauth2/v1/authorize",
		"https://company.okta.com/oauth2/v1/token",
		"https://company.okta.com/oauth2/v1/userinfo",
		"https://vibebackr.local/auth/callback/okta",
		[]string{"openid", "email", "profile"},
	)

	if oidc.Name() != "okta" {
		t.Errorf("expected provider name okta, got %s", oidc.Name())
	}
	authURL := oidc.GetAuthURL("random_state_123")
	if !strings.Contains(authURL, "company.okta.com") || !strings.Contains(authURL, "okta-client-123") {
		t.Errorf("malformed OIDC auth URL: %s", authURL)
	}

	// Google Provider
	google := auth.NewGoogleSSOProvider(
		"google-client-123",
		"google-sec-456",
		"https://vibebackr.local/auth/callback/google",
	)
	gURL := google.GetAuthURL("state_google")
	if !strings.Contains(gURL, "prompt=select_account") {
		t.Errorf("expected Google auth URL to include prompt=select_account, got %s", gURL)
	}

	// GitHub Provider
	gh := auth.NewGitHubSSOProvider(
		"github-client-123",
		"github-sec-456",
		"https://vibebackr.local/auth/callback/github",
	)

	if gh.Name() != "github" {
		t.Errorf("expected provider name github, got %s", gh.Name())
	}
	ghURL := gh.GetAuthURL("state_gh")
	if !strings.Contains(ghURL, "github.com/login/oauth/authorize") || !strings.Contains(ghURL, "github-client-123") {
		t.Errorf("malformed GitHub auth URL: %s", ghURL)
	}

	// Test Registration in SSOManager
	repo := repository.New(nil)
	sm := auth.NewSSOManager(repo)
	sm.RegisterProvider(oidc)
	sm.RegisterProvider(gh)

	if p, err := sm.GetProvider("okta"); err != nil || p == nil {
		t.Errorf("expected to retrieve registered okta provider")
	}
	if p, err := sm.GetProvider("github"); err != nil || p == nil {
		t.Errorf("expected to retrieve registered github provider")
	}
}

func TestRefreshTokenGenerationAndHashing(t *testing.T) {
	raw, hash, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("failed generating refresh token: %v", err)
	}
	if !strings.HasPrefix(raw, "rt_") {
		t.Errorf("expected refresh token to start with rt_, got %s", raw)
	}
	if len(raw) < 64 {
		t.Errorf("expected raw token length >= 64, got %d", len(raw))
	}

	computedHash := auth.HashRefreshToken(raw)
	if computedHash != hash {
		t.Errorf("expected hash %s, got %s", hash, computedHash)
	}

	tm := auth.NewTokenManager("test", 1*time.Hour)
	if tm.TTL() != 1*time.Hour {
		t.Errorf("expected access TTL 1h, got %v", tm.TTL())
	}
	if tm.RefreshTTL() != 30*24*time.Hour {
		t.Errorf("expected refresh TTL 720h, got %v", tm.RefreshTTL())
	}
	tm.SetRefreshTTL(7 * 24 * time.Hour)
	if tm.RefreshTTL() != 7*24*time.Hour {
		t.Errorf("expected updated refresh TTL 168h, got %v", tm.RefreshTTL())
	}
}
