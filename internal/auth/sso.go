package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/repository"
	"github.com/google/uuid"
)

// UserInfo holds identity data returned by an identity provider.
type UserInfo struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Sub   string `json:"sub"`
}

// SSOProvider is the pluggable interface supporting modular OIDC providers.
type SSOProvider interface {
	Name() string
	GetAuthURL(state string) string
	ExchangeCode(ctx context.Context, code string) (*UserInfo, error)
}

// MockSSOProvider provides zero-friction local SSO login without needing real external OAuth credentials.
type MockSSOProvider struct{}

func (m *MockSSOProvider) Name() string {
	return "mock"
}

func (m *MockSSOProvider) GetAuthURL(state string) string {
	return fmt.Sprintf("/auth/callback/mock?code=mock_code_123&state=%s", url.QueryEscape(state))
}

func (m *MockSSOProvider) ExchangeCode(ctx context.Context, code string) (*UserInfo, error) {
	return &UserInfo{
		Email: "developer@example.com",
		Name:  "Test Developer",
		Sub:   "mock-user-123",
	}, nil
}

// GoogleSSOProvider implements standard Google OAuth 2.0 / OIDC flow.
type GoogleSSOProvider struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

func NewGoogleSSOProvider(clientID, clientSecret, redirectURL string) *GoogleSSOProvider {
	return &GoogleSSOProvider{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
	}
}

func (g *GoogleSSOProvider) Name() string {
	return "google"
}

func (g *GoogleSSOProvider) GetAuthURL(state string) string {
	params := url.Values{}
	params.Set("client_id", g.ClientID)
	params.Set("redirect_uri", g.RedirectURL)
	params.Set("response_type", "code")
	params.Set("scope", "openid email profile")
	params.Set("state", state)
	params.Set("access_type", "online")
	params.Set("prompt", "select_account")
	return "https://accounts.google.com/o/oauth2/v2/auth?" + params.Encode()
}

var ssoHTTPClient = &http.Client{Timeout: 10 * time.Second}

func (g *GoogleSSOProvider) ExchangeCode(ctx context.Context, code string) (*UserInfo, error) {
	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", g.ClientID)
	data.Set("client_secret", g.ClientSecret)
	data.Set("redirect_uri", g.RedirectURL)
	data.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, "POST", "https://oauth2.googleapis.com/token", strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := ssoHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange error with status: %d", resp.StatusCode)
	}

	var tokenRes struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenRes); err != nil {
		return nil, err
	}

	// Fetch user profile from userinfo endpoint
	userReq, err := http.NewRequestWithContext(ctx, "GET", "https://openidconnect.googleapis.com/v1/userinfo", nil)
	if err != nil {
		return nil, err
	}
	userReq.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)

	userResp, err := ssoHTTPClient.Do(userReq)
	if err != nil {
		return nil, fmt.Errorf("failed fetching userinfo: %w", err)
	}
	defer userResp.Body.Close()

	if userResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo endpoint returned status: %d", userResp.StatusCode)
	}

	var u UserInfo
	if err := json.NewDecoder(userResp.Body).Decode(&u); err != nil {
		return nil, err
	}
	if strings.TrimSpace(u.Email) == "" {
		return nil, errors.New("google userinfo did not return a valid email")
	}

	return &u, nil
}

// GenericOIDCProvider supports standard OpenID Connect providers (Okta, Keycloak, Auth0, Microsoft Entra).
type GenericOIDCProvider struct {
	ProviderName string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserInfoURL  string
	RedirectURL  string
	Scopes       []string
}

func NewGenericOIDCProvider(name, clientID, clientSecret, authURL, tokenURL, userInfoURL, redirectURL string, scopes []string) *GenericOIDCProvider {
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	return &GenericOIDCProvider{
		ProviderName: name,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthURL:      authURL,
		TokenURL:     tokenURL,
		UserInfoURL:  userInfoURL,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
	}
}

func (o *GenericOIDCProvider) Name() string {
	if o.ProviderName != "" {
		return o.ProviderName
	}
	return "oidc"
}

func (o *GenericOIDCProvider) GetAuthURL(state string) string {
	params := url.Values{}
	params.Set("client_id", o.ClientID)
	params.Set("redirect_uri", o.RedirectURL)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(o.Scopes, " "))
	params.Set("state", state)
	return o.AuthURL + "?" + params.Encode()
}

func (o *GenericOIDCProvider) ExchangeCode(ctx context.Context, code string) (*UserInfo, error) {
	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", o.ClientID)
	data.Set("client_secret", o.ClientSecret)
	data.Set("redirect_uri", o.RedirectURL)
	data.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, "POST", o.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := ssoHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange error with status: %d", resp.StatusCode)
	}

	var tokenRes struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenRes); err != nil {
		return nil, err
	}

	userReq, err := http.NewRequestWithContext(ctx, "GET", o.UserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	userReq.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)

	userResp, err := ssoHTTPClient.Do(userReq)
	if err != nil {
		return nil, fmt.Errorf("failed fetching userinfo: %w", err)
	}
	defer userResp.Body.Close()

	if userResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc userinfo returned status: %d", userResp.StatusCode)
	}

	var u UserInfo
	if err := json.NewDecoder(userResp.Body).Decode(&u); err != nil {
		return nil, err
	}
	if strings.TrimSpace(u.Email) == "" {
		return nil, errors.New("oidc userinfo did not return a valid email")
	}

	return &u, nil
}

// GitHubSSOProvider implements standard GitHub OAuth authentication.
type GitHubSSOProvider struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

func NewGitHubSSOProvider(clientID, clientSecret, redirectURL string) *GitHubSSOProvider {
	return &GitHubSSOProvider{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
	}
}

func (g *GitHubSSOProvider) Name() string {
	return "github"
}

func (g *GitHubSSOProvider) GetAuthURL(state string) string {
	params := url.Values{}
	params.Set("client_id", g.ClientID)
	params.Set("redirect_uri", g.RedirectURL)
	params.Set("scope", "user:email read:user")
	params.Set("state", state)
	return "https://github.com/login/oauth/authorize?" + params.Encode()
}

func (g *GitHubSSOProvider) ExchangeCode(ctx context.Context, code string) (*UserInfo, error) {
	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", g.ClientID)
	data.Set("client_secret", g.ClientSecret)
	data.Set("redirect_uri", g.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://github.com/login/oauth/access_token", strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := ssoHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github token exchange error with status: %d", resp.StatusCode)
	}

	var tokenRes struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenRes); err != nil {
		return nil, err
	}
	if tokenRes.AccessToken == "" {
		return nil, errors.New("github token exchange returned empty access_token")
	}

	// Fetch primary user info
	uReq, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user", nil)
	if err != nil {
		return nil, err
	}
	uReq.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)
	uReq.Header.Set("Accept", "application/json")

	uResp, err := ssoHTTPClient.Do(uReq)
	if err != nil {
		return nil, fmt.Errorf("failed fetching github user: %w", err)
	}
	defer uResp.Body.Close()

	if uResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github user API returned status: %d", uResp.StatusCode)
	}

	var ghUser struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(uResp.Body).Decode(&ghUser); err != nil {
		return nil, err
	}

	// Always prefer verified primary email from /user/emails to prevent unverified email spoofing
	email := ""
	eReq, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user/emails", nil)
	eReq.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)
	eReq.Header.Set("Accept", "application/json")
	if eResp, err := ssoHTTPClient.Do(eReq); err == nil {
		if eResp.StatusCode == http.StatusOK {
			var emails []struct {
				Email    string `json:"email"`
				Primary  bool   `json:"primary"`
				Verified bool   `json:"verified"`
			}
			if err := json.NewDecoder(eResp.Body).Decode(&emails); err == nil {
				for _, e := range emails {
					if e.Primary && e.Verified {
						email = e.Email
						break
					}
				}
				if email == "" {
					for _, e := range emails {
						if e.Verified {
							email = e.Email
							break
						}
					}
				}
			}
		}
		eResp.Body.Close()
	}
	if email == "" {
		email = ghUser.Email
	}
	if strings.TrimSpace(email) == "" {
		return nil, errors.New("no verified email address found on github account")
	}

	name := ghUser.Name
	if name == "" {
		name = ghUser.Login
	}

	return &UserInfo{
		Email: email,
		Name:  name,
		Sub:   fmt.Sprintf("github-%d", ghUser.ID),
	}, nil
}

// SSOManager coordinates provider registration and JIT user provisioning.
type SSOManager struct {
	providers map[string]SSOProvider
	repo      repository.Repository
}

// NewSSOManager creates an SSO manager.
func NewSSOManager(repo repository.Repository) *SSOManager {
	sm := &SSOManager{
		providers: make(map[string]SSOProvider),
		repo:      repo,
	}
	// Mock provider is disabled by default; only registered if ENABLE_MOCK_AUTH is explicitly "true"
	if os.Getenv("ENABLE_MOCK_AUTH") == "true" {
		sm.RegisterProvider(&MockSSOProvider{})
	}
	return sm
}

// RegisterProvider registers an SSO provider.
func (sm *SSOManager) RegisterProvider(p SSOProvider) {
	sm.providers[p.Name()] = p
}

// GetProvider retrieves an SSO provider by name.
func (sm *SSOManager) GetProvider(name string) (SSOProvider, error) {
	p, ok := sm.providers[name]
	if !ok {
		return nil, fmt.Errorf("unsupported SSO provider: %s", name)
	}
	return p, nil
}

// ProviderMetadata describes an active SSO provider for UI rendering.
type ProviderMetadata struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	IconURL string `json:"icon_url"`
}

// MetadataForProvider returns display label and official provider icon URL for a given SSOProvider.
func MetadataForProvider(p SSOProvider) ProviderMetadata {
	name := p.Name()
	switch name {
	case "google":
		return ProviderMetadata{
			Name:    "google",
			Label:   "Continue with Google",
			IconURL: "https://www.gstatic.com/firebasejs/ui/2.0.0/images/auth/google.svg",
		}
	case "github":
		return ProviderMetadata{
			Name:    "github",
			Label:   "Continue with GitHub",
			IconURL: "https://github.githubassets.com/favicons/favicon-dark.svg",
		}
	case "mock":
		return ProviderMetadata{
			Name:    "mock",
			Label:   "Continue as Local Dev (Mock)",
			IconURL: "/logo.svg",
		}
	default:
		iconURL := "/logo.svg"
		if oidc, ok := p.(*GenericOIDCProvider); ok && oidc.AuthURL != "" {
			if u, err := url.Parse(oidc.AuthURL); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
				iconURL = fmt.Sprintf("%s://%s/favicon.ico", u.Scheme, u.Host)
			}
		}
		return ProviderMetadata{
			Name:    name,
			Label:   "Continue with " + strings.ToUpper(name),
			IconURL: iconURL,
		}
	}
}

// ListProviderNames returns the sorted names of currently active SSO providers on this instance.
func (sm *SSOManager) ListProviderNames() []string {
	names := make([]string, 0, len(sm.providers))
	for name := range sm.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListProviderMetadata returns sorted metadata (name, label, icon_url) for active SSO providers.
func (sm *SSOManager) ListProviderMetadata() []ProviderMetadata {
	names := sm.ListProviderNames()
	out := make([]ProviderMetadata, 0, len(names))
	for _, name := range names {
		if p, ok := sm.providers[name]; ok {
			out = append(out, MetadataForProvider(p))
		}
	}
	return out
}

// acceptPendingInvites auto-accepts all active invites for the user across any tenant.
func (sm *SSOManager) acceptPendingInvites(ctx context.Context, u *domain.User) bool {
	invites, err := sm.repo.ListInvitesByEmail(ctx, u.Email)
	if err != nil || len(invites) == 0 {
		return false
	}

	userUpdated := false
	for _, inv := range invites {
		tenantID := inv.TenantID
		if tenantID == "" && inv.CreatedBy != "" {
			if creator, cErr := sm.repo.GetUserByID(ctx, inv.CreatedBy); cErr == nil && creator.TenantID != nil {
				tenantID = *creator.TenantID
			}
		}

		role := inv.Role
		if role == "" {
			role = domain.RoleMember
		}

		if tenantID != "" {
			_ = sm.repo.AddTenantMembership(ctx, &domain.TenantMembership{
				UserID:    u.ID,
				TenantID:  tenantID,
				Role:      role,
				ManagerID: inv.ManagerID,
				CreatedAt: time.Now().UTC(),
			})
		}

		// Only set global u.ManagerID if the invite is unscoped or matches the user's primary tenant
		if u.ManagerID == nil && inv.ManagerID != nil && (tenantID == "" || (u.TenantID != nil && *u.TenantID == tenantID)) {
			u.ManagerID = inv.ManagerID
			userUpdated = true
		}
		// Never promote a tenantless user to global RoleAdmin from a tenant-scoped invite
		// (their admin role for that specific tenant is stored in user_tenants above).
		if (u.Role == "" || u.Role == domain.RoleMember) && role != domain.RoleMember {
			if role == domain.RoleAdmin && tenantID != "" && (u.TenantID == nil || *u.TenantID == "") {
				// Keep global role as member; tenant admin rights are enforced via user_tenants
			} else if tenantID == "" || (u.TenantID != nil && *u.TenantID == tenantID) || role != domain.RoleAdmin {
				u.Role = role
				userUpdated = true
			}
		}

		_ = sm.repo.DeleteInvite(ctx, inv.ID)
	}

	if userUpdated {
		_ = sm.repo.UpdateUser(ctx, u)
	}
	return true
}

// ProvisionOrGetUser implements JIT (Just-In-Time) user provisioning:
// If the user already exists, auto-accepts any pending invites for any tenant and returns them.
// If isSignup is true, explicitly provisions a new user with their own Tenant (TenantID != nil, Role = admin).
// If the database has no users yet (0 users), the first user auto-bootstraps as Admin with their own Tenant.
// Otherwise (auto-created users via Invite, Mock SSO in dev, or collection allowed_domains),
// users are provisioned WITHOUT an owned tenant (TenantID = nil) and joined to any invited/domain-matched tenants.
func (sm *SSOManager) ProvisionOrGetUser(ctx context.Context, info *UserInfo, isSignup ...bool) (*domain.User, error) {
	if info == nil || strings.TrimSpace(info.Email) == "" {
		return nil, errors.New("invalid user info: missing email")
	}
	signup := false
	if len(isSignup) > 0 && isSignup[0] {
		signup = true
	}

	existing, err := sm.repo.GetUserByEmail(ctx, info.Email)
	if err == nil {
		// If user previously existed without an owned tenant and is now explicitly signing up, assign owned tenant
		if signup && (existing.TenantID == nil || *existing.TenantID == "") {
			tenantID := uuid.NewString()
			existing.TenantID = &tenantID
			existing.Role = domain.RoleAdmin
			_ = sm.repo.UpdateUser(ctx, existing)
		}
		// Auto-accept any pending invites for any tenant on login
		sm.acceptPendingInvites(ctx, existing)
		return existing, nil
	}
	if !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}

	// 1. Explicit Sign-up: Creates a new user with their own Tenant
	if signup {
		tenantID := uuid.NewString()
		newUser := &domain.User{
			ID:        uuid.NewString(),
			TenantID:  &tenantID,
			Email:     info.Email,
			Name:      info.Name,
			Role:      domain.RoleAdmin,
			CreatedAt: time.Now().UTC(),
		}
		if err := sm.repo.CreateUser(ctx, newUser); err != nil {
			return nil, fmt.Errorf("failed to provision tenant user: %w", err)
		}
		sm.acceptPendingInvites(ctx, newUser)
		return newUser, nil
	}

	// 2. Auto-bootstrap: If system has 0 users, the very first user is provisioned as Admin with a tenant
	allUsers, listErr := sm.repo.ListUsers(ctx)
	if listErr == nil && len(allUsers) == 0 {
		tenantID := uuid.NewString()
		adminUser := &domain.User{
			ID:        uuid.NewString(),
			TenantID:  &tenantID,
			Email:     info.Email,
			Name:      info.Name,
			Role:      domain.RoleAdmin,
			CreatedAt: time.Now().UTC(),
		}
		if err := sm.repo.CreateUser(ctx, adminUser); err != nil {
			return nil, fmt.Errorf("failed to auto-bootstrap initial admin user: %w", err)
		}
		sm.acceptPendingInvites(ctx, adminUser)
		return adminUser, nil
	}

	// 3. Auto-created user (No owned tenant, joined to invited / domain-matched tenants)
	invites, invErr := sm.repo.ListInvitesByEmail(ctx, info.Email)
	var role = domain.RoleMember
	var managerID *string
	var matchedDomainTenants []string

	if invErr == nil && len(invites) > 0 {
		for _, inv := range invites {
			invTenant := inv.TenantID
			if invTenant == "" && inv.CreatedBy != "" {
				if creator, cErr := sm.repo.GetUserByID(ctx, inv.CreatedBy); cErr == nil && creator.TenantID != nil {
					invTenant = *creator.TenantID
				}
			}
			if inv.Role != "" && role == domain.RoleMember {
				// Prevent tenant-scoped admin invites from granting global system-wide RoleAdmin (TenantID == nil + RoleAdmin)
				if inv.Role == domain.RoleAdmin && invTenant != "" {
					role = domain.RoleMember
				} else {
					role = inv.Role
				}
			}
			if inv.ManagerID != nil && managerID == nil && invTenant == "" {
				managerID = inv.ManagerID
			}
		}
	} else {
		// If no invite, check allowed_domains in collections to authorize JIT onboarding
		parts := strings.Split(info.Email, "@")
		if len(parts) != 2 {
			return nil, errors.New("invalid email address")
		}
		domainPart := parts[1]

		collections, colErr := sm.repo.ListCollections(ctx)
		if colErr != nil {
			return nil, colErr
		}

		domainAllowed := false
		// Mock SSO is only allowed if ENABLE_MOCK_AUTH is explicitly enabled
		if os.Getenv("ENABLE_MOCK_AUTH") == "true" && info.Sub == "mock-user-123" {
			domainAllowed = true
		}

		for _, c := range collections {
			for _, d := range c.AllowedDomains {
				if strings.EqualFold(d, domainPart) {
					domainAllowed = true
					if c.TenantID != "" {
						matchedDomainTenants = append(matchedDomainTenants, c.TenantID)
					}
					break
				}
			}
		}

		if !domainAllowed {
			return nil, errors.New("unauthorized: no active invite or allowed domain matches this user")
		}
	}

	newUser := &domain.User{
		ID:        uuid.NewString(),
		TenantID:  nil, // Auto-created users do not own a tenant initially
		Email:     info.Email,
		Name:      info.Name,
		Role:      role,
		ManagerID: managerID,
		CreatedAt: time.Now().UTC(),
	}

	if err := sm.repo.CreateUser(ctx, newUser); err != nil {
		return nil, fmt.Errorf("failed to JIT provision user: %w", err)
	}

	sm.acceptPendingInvites(ctx, newUser)
	for _, tid := range matchedDomainTenants {
		_ = sm.repo.AddTenantMembership(ctx, &domain.TenantMembership{
			UserID:    newUser.ID,
			TenantID:  tid,
			Role:      domain.RoleMember,
			CreatedAt: time.Now().UTC(),
		})
	}

	return newUser, nil
}
