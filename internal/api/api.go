package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/eliotstocker/vibebackr/internal/auth"
	"github.com/eliotstocker/vibebackr/internal/billing"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/mcp"
	"github.com/eliotstocker/vibebackr/internal/ratelimit"
	"github.com/eliotstocker/vibebackr/internal/repository"
	"github.com/eliotstocker/vibebackr/internal/search"
	"github.com/eliotstocker/vibebackr/internal/storage"
	"github.com/google/uuid"
)

type contextKey string

const userContextKey contextKey = "currentUser"

// Server configures and mounts the REST API and streamable MCP.
type Server struct {
	repo         repository.Repository
	tokenManager *auth.TokenManager
	ssoManager   *auth.SSOManager
	searchEngine *search.SearchEngine
	mcpServer    *mcp.Server
	billing      *billing.Module
	rateLimiter  *ratelimit.Module
	instanceName string
}

// NewServer initializes the API server.
func NewServer(
	repo repository.Repository,
	tm *auth.TokenManager,
	sm *auth.SSOManager,
	se *search.SearchEngine,
	mcpServer *mcp.Server,
) *Server {
	if mcpServer != nil && sm != nil {
		mcpServer.SetProviderLister(sm.ListProviderNames)
	}
	return &Server{
		repo:         repo,
		tokenManager: tm,
		ssoManager:   sm,
		searchEngine: se,
		mcpServer:    mcpServer,
	}
}

// SetInstanceName configures a custom company/organization name for the self-hosted root landing page.
func (s *Server) SetInstanceName(name string) {
	s.instanceName = strings.TrimSpace(name)
}

// SetBilling attaches the optional billing module to the API server, repository write guard, and MCP server.
func (s *Server) SetBilling(b *billing.Module) {
	s.billing = b
	if s.repo != nil {
		if b != nil {
			s.repo.SetWriteGuard(b.CheckWriteAccess)
		} else {
			s.repo.SetWriteGuard(nil)
		}
	}
	if s.mcpServer != nil {
		s.mcpServer.SetBilling(b)
	}
}

// SetRateLimiter attaches the optional per-collection rate limiter to the API server and MCP server.
func (s *Server) SetRateLimiter(rl *ratelimit.Module) {
	s.rateLimiter = rl
	if s.mcpServer != nil {
		s.mcpServer.SetRateLimiter(rl)
	}
}

func (s *Server) rateLimitCollection(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimiter != nil && s.rateLimiter.Enabled() {
			colID := r.PathValue("collection_id")
			if colID != "" {
				dec, err := s.rateLimiter.Allow(r.Context(), colID)
				ratelimit.WriteHeaders(w, dec)
				if err != nil {
					s.jsonError(w, fmt.Sprintf("rate limit exceeded for collection (%d requests/minute allowed)", dec.Limit), http.StatusTooManyRequests)
					return
				}
			}
		}
		next(w, r)
	}
}

// Routes builds and returns the http.Handler using Go 1.22+ method-based pattern routing.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// MCP streamable endpoint
	mux.HandleFunc("/mcp", s.handleMCP)

	// OAuth & DCR endpoints
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleOAuthProtectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.handleOAuthProtectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleOAuthMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOAuthMetadata)
	mux.HandleFunc("POST /oauth/register", s.handleOAuthRegister)
	mux.HandleFunc("POST /oauth/token", s.handleOAuthToken)
	mux.HandleFunc("GET /oauth/authorize", s.handleOAuthAuthorize)
	mux.HandleFunc("POST /oauth/authorize", s.handleOAuthAuthorizeSubmit)
	mux.HandleFunc("GET /oauth/client-info", s.handleOAuthClientInfo)

	// SSO & Auth
	mux.HandleFunc("GET /auth/login/{provider}", s.handleSSOLogin)
	mux.HandleFunc("GET /auth/callback/{provider}", s.handleSSOCallback)
	mux.HandleFunc("GET /api/auth/providers", s.handleListProviders)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/auth/revoke-all", s.requireAuth(s.handleRevokeAllSessions))

	// Client Integration Guide & Claude Desktop MCP Bundle (.mcpb)
	mux.HandleFunc("GET /api/client-guide", s.handleClientGuide)
	mux.HandleFunc("GET /vibebackr.mcpb", s.handleMCPBundle)

	// Billing & Voucher module endpoints
	mux.HandleFunc("GET /api/billing/status", s.requireAuth(s.handleBillingStatus))
	mux.HandleFunc("POST /api/billing/voucher", s.requireAuth(s.handleRedeemVoucher))
	mux.HandleFunc("POST /api/billing/checkout", s.requireAuth(s.handleBillingCheckout))
	mux.HandleFunc("GET /api/billing/callback", s.handleBillingCallback)
	mux.HandleFunc("POST /api/billing/upgrade", s.requireAuth(s.handleBillingUpgrade))
	mux.HandleFunc("POST /api/billing/webhook", s.handleBillingWebhook)

	// Runtime REST endpoints
	mux.HandleFunc("GET /api/tenants", s.requireAuth(s.handleListTenants))
	mux.HandleFunc("POST /api/tenants", s.requireAuth(s.handleCreateTenant))
	mux.HandleFunc("DELETE /api/tenants/{tenant_id}", s.requireAuth(s.handleLeaveTenant))
	mux.HandleFunc("GET /api/invites", s.requireAuth(s.handleListInvites))
	mux.HandleFunc("POST /api/invites", s.requireAuth(s.handleCreateInvite))
	mux.HandleFunc("DELETE /api/invites/{id}", s.requireAuth(s.handleDeleteInvite))
	mux.HandleFunc("GET /api/collections", s.requireAuth(s.handleListCollections))
	mux.HandleFunc("POST /api/collections", s.requireAuth(s.handleCreateCollection))
	mux.HandleFunc("GET /api/collections/{collection_id}", s.requireAuth(s.rateLimitCollection(s.handleGetCollection)))
	mux.HandleFunc("PUT /api/collections/{collection_id}", s.requireAuth(s.rateLimitCollection(s.handleUpdateCollection)))
	mux.HandleFunc("PUT /api/collections/{collection_id}/app", s.requireAuth(s.rateLimitCollection(s.handlePublishApp)))
	mux.HandleFunc("DELETE /api/collections/{collection_id}", s.requireAuth(s.rateLimitCollection(s.handleDeleteCollection)))
	mux.HandleFunc("GET /api/users/{id}", s.requireAuth(s.handleGetUser))

	// Local/fallback static app routes
	mux.HandleFunc("GET /apps/{slug}", s.handleGetApp)
	mux.HandleFunc("GET /apps/{slug}/index.html", s.handleGetApp)

	mux.HandleFunc("GET /api/collections/{collection_id}/records", s.requireAuth(s.rateLimitCollection(s.handleListRecords)))
	mux.HandleFunc("POST /api/collections/{collection_id}/records", s.requireAuth(s.rateLimitCollection(s.handleCreateRecord)))

	mux.HandleFunc("GET /api/collections/{collection_id}/records/{record_id}", s.requireAuth(s.rateLimitCollection(s.handleGetRecord)))
	mux.HandleFunc("PUT /api/collections/{collection_id}/records/{record_id}", s.requireAuth(s.rateLimitCollection(s.handleUpdateRecord)))
	mux.HandleFunc("DELETE /api/collections/{collection_id}/records/{record_id}", s.requireAuth(s.rateLimitCollection(s.handleDeleteRecord)))

	mux.HandleFunc("POST /api/collections/{collection_id}/search", s.requireAuth(s.rateLimitCollection(s.handleSearchRecords)))

	// Self-hosted root landing page (VibeBackr OS) and canonical SVG logo
	mux.HandleFunc("GET /{$}", s.handleSelfHostedRoot)
	mux.HandleFunc("GET /index.html", s.handleSelfHostedRoot)
	mux.HandleFunc("GET /logo.svg", s.handleLogoSVG)
	mux.HandleFunc("GET /favicon.svg", s.handleLogoSVG)
	mux.HandleFunc("GET /favicon.ico", s.handleLogoSVG)

	// Static frontend routes for local development or direct server hosting
	webDir := ""
	for _, candidate := range []string{"web/dist", "../../web/dist", "web", "../../web"} {
		if _, err := os.Stat(candidate); err == nil {
			webDir = candidate
			break
		}
	}

	if webDir != "" {
		mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
			s.serveLoginFile(w, webDir+"/login.html")
		})
		mux.HandleFunc("GET /guide", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/guide.html", "text/html; charset=utf-8")
		})
		mux.HandleFunc("GET /nerds", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/nerds.html", "text/html; charset=utf-8")
		})
		mux.HandleFunc("GET /nerds.html", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/nerds.html", "text/html; charset=utf-8")
		})
		mux.HandleFunc("GET /relay", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/relay.html", "text/html; charset=utf-8")
		})
		mux.HandleFunc("GET /relay.html", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/relay.html", "text/html; charset=utf-8")
		})
		mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/style.css", "text/css; charset=utf-8")
		})
		mux.HandleFunc("GET /vibebackr.js", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/vibebackr.js", "application/javascript; charset=utf-8")
		})
		mux.HandleFunc("GET /vibe.js", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/vibe.js", "application/javascript; charset=utf-8")
		})
		mux.HandleFunc("GET /vibebackr.d.ts", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/vibebackr.d.ts", "application/typescript; charset=utf-8")
		})
		mux.HandleFunc("GET /vibe.d.ts", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/vibe.d.ts", "application/typescript; charset=utf-8")
		})
		mux.HandleFunc("GET /logo.png", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/logo.png", "image/png")
		})
		mux.HandleFunc("GET /logo-dark.png", func(w http.ResponseWriter, r *http.Request) {
			s.serveStaticFile(w, webDir+"/logo-dark.png", "image/png")
		})
	}

	return s.corsMiddleware(mux)
}

func (s *Server) serveLoginFile(w http.ResponseWriter, filePath string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	content := string(data)
	if start := strings.Index(content, `<header class="nav">`); start > -1 {
		if end := strings.Index(content[start:], "</header>"); end > -1 {
			content = content[:start] + content[start+end+len("</header>"):]
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

func (s *Server) serveStaticFile(w http.ResponseWriter, filePath, contentType string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("--> %s %s [Origin: %s, Auth: %t]", r.Method, r.URL.Path, r.Header.Get("Origin"), r.Header.Get("Authorization") != "")
		origin := r.Header.Get("Origin")
		allowOrigin := "*"
		if origin != "" && origin != "null" {
			allowOrigin = origin
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, MCP-Protocol-Version, Last-Event-ID")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, MCP-Protocol-Version")
		w.Header().Set("Vary", "Origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticateToken(ctx context.Context, tokenStr string) (*domain.User, error) {
	claims, err := s.tokenManager.ValidateToken(tokenStr)
	if err != nil {
		return nil, errors.New("invalid or expired token")
	}
	u, err := s.repo.GetUserByID(ctx, claims.UserID)
	if err != nil {
		return nil, errors.New("user not found")
	}
	if claims.TokenVersion != "" && u.TokenVersion != "" && claims.TokenVersion != u.TokenVersion {
		return nil, errors.New("token revoked: session version mismatch")
	}
	return u, nil
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			s.jsonError(w, "missing or malformed authorization header", http.StatusUnauthorized)
			return
		}

		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		u, err := s.authenticateToken(r.Context(), tokenStr)
		if err != nil {
			s.jsonError(w, err.Error(), http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

func currentUser(r *http.Request) *domain.User {
	u, _ := r.Context().Value(userContextKey).(*domain.User)
	return u
}

func (s *Server) jsonError(w http.ResponseWriter, message string, code int) {
	log.Printf("[HTTP %d ERROR] %s", code, message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (s *Server) jsonResponse(w http.ResponseWriter, data interface{}, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// If bearer token present or passed as query param (e.g. for EventSource), authenticate caller
	var caller *domain.User
	tokenStr := ""
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		tokenStr = strings.TrimPrefix(authHeader, "Bearer ")
	} else if qToken := r.URL.Query().Get("token"); qToken != "" {
		tokenStr = qToken
	} else if qToken := r.URL.Query().Get("access_token"); qToken != "" {
		tokenStr = qToken
	}

	if tokenStr != "" {
		caller, _ = s.authenticateToken(r.Context(), tokenStr)
	}
	baseURL := s.getBaseURL(r)
	if caller == nil {
		realm := r.Host
		if canonical := os.Getenv("CANONICAL_DOMAIN"); canonical != "" {
			realm = canonical
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s", resource_metadata="%s/.well-known/oauth-protected-resource", error="invalid_token"`, realm, baseURL))
		s.jsonError(w, "authentication required: provide valid Bearer token", http.StatusUnauthorized)
		return
	}
	ctx := context.WithValue(r.Context(), mcp.BaseURLContextKey, baseURL)
	s.mcpServer.ServeHTTP(w, r.WithContext(ctx), caller)
}

func (s *Server) getBaseURL(r *http.Request) string {
	return s.mcpServer.GetBaseURLFromRequest(r)
}

// GET /.well-known/oauth-protected-resource and /.well-known/oauth-protected-resource/mcp (RFC 9728)
func (s *Server) handleOAuthProtectedResource(w http.ResponseWriter, r *http.Request) {
	baseURL := s.getBaseURL(r)

	s.jsonResponse(w, map[string]interface{}{
		"resource":                 baseURL + "/mcp",
		"authorization_servers":    []string{baseURL},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"openid", "profile", "email", "mcp"},
		"resource_documentation":   baseURL + "/guide",
	}, http.StatusOK)
}

// GET /.well-known/oauth-authorization-server and /.well-known/openid-configuration (RFC 8414)
func (s *Server) handleOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	baseURL := s.getBaseURL(r)

	s.jsonResponse(w, map[string]interface{}{
		"issuer":                                baseURL,
		"authorization_endpoint":                baseURL + "/oauth/authorize",
		"token_endpoint":                        baseURL + "/oauth/token",
		"registration_endpoint":                 baseURL + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "client_credentials", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic", "none"},
		"scopes_supported":                      []string{"openid", "profile", "email", "mcp"},
	}, http.StatusOK)
}

// POST /oauth/register (RFC 7591 Dynamic Client Registration)
func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	var req auth.DCRRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.jsonError(w, "invalid registration payload", http.StatusBadRequest)
		return
	}

	client := auth.RegisterClient(req)
	if err := s.repo.CreateOAuthClient(r.Context(), client); err != nil {
		s.jsonError(w, "failed to register oauth client: "+err.Error(), http.StatusInternalServerError)
		return
	}

	authMethod := req.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}

	s.jsonResponse(w, map[string]interface{}{
		"client_id":                  client.ID,
		"client_secret":              client.ClientSecret,
		"client_secret_expires_at":   0,
		"client_name":                client.ClientName,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                client.GrantTypes,
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": authMethod,
	}, http.StatusCreated)
}

// POST /oauth/token (Supports PKCE authorization_code, refresh_token & client_credentials)
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	if err := r.ParseForm(); err != nil {
		s.jsonError(w, "invalid form data", http.StatusBadRequest)
		return
	}

	grantType := r.FormValue("grant_type")
	clientID := r.FormValue("client_id")
	clientSecret := r.FormValue("client_secret")
	if clientID == "" {
		if u, p, ok := r.BasicAuth(); ok {
			clientID = u
			if clientSecret == "" {
				clientSecret = p
			}
		}
	}

	var client *domain.OAuthClient
	if clientID != "" {
		client, _ = s.repo.GetOAuthClient(r.Context(), clientID)
	}

	switch grantType {
	case "client_credentials":
		if client == nil || client.ClientSecret == "" || subtle.ConstantTimeCompare([]byte(client.ClientSecret), []byte(clientSecret)) != 1 {
			s.jsonError(w, "invalid client credentials", http.StatusUnauthorized)
			return
		}

		// Agent headless user identity: provision in users table with its own tenant
		agentUser, err := s.repo.GetUserByID(r.Context(), client.ID)
		if err != nil {
			tenantID := uuid.NewString()
			agentUser = &domain.User{
				ID:        client.ID,
				TenantID:  &tenantID,
				Email:     client.ID + "@agent.local",
				Name:      client.ClientName,
				Role:      domain.RoleAdmin, // autonomous agent client credential has admin rights for its tenant
				CreatedAt: time.Now().UTC(),
			}
			if createErr := s.repo.CreateUser(r.Context(), agentUser); createErr != nil {
				s.jsonError(w, "failed provisioning agent user: "+createErr.Error(), http.StatusInternalServerError)
				return
			}
		}

		if agentUser.TokenVersion == "" {
			agentUser.TokenVersion = uuid.NewString()
			_ = s.repo.UpdateUserTokenVersion(r.Context(), agentUser.ID, agentUser.TokenVersion)
		}

		token, err := s.tokenManager.GenerateToken(agentUser)
		if err != nil {
			s.jsonError(w, "failed generating token", http.StatusInternalServerError)
			return
		}

		rawRT, rtHash, _ := auth.GenerateRefreshToken()
		_ = s.repo.CreateRefreshToken(r.Context(), &domain.RefreshToken{
			ID:           uuid.NewString(),
			UserID:       agentUser.ID,
			ClientID:     clientID,
			TokenHash:    rtHash,
			TokenVersion: agentUser.TokenVersion,
			ExpiresAt:    time.Now().UTC().Add(s.tokenManager.RefreshTTL()),
			CreatedAt:    time.Now().UTC(),
		})

		s.jsonResponse(w, map[string]interface{}{
			"access_token":  token,
			"token_type":    "Bearer",
			"expires_in":    int(s.tokenManager.TTL().Seconds()),
			"refresh_token": rawRT,
		}, http.StatusOK)

	case "authorization_code":
		if client == nil {
			s.jsonError(w, "invalid client_id", http.StatusBadRequest)
			return
		}

		code := r.FormValue("code")
		codeVerifier := r.FormValue("code_verifier")

		authCode, err := s.repo.ConsumeAuthCode(r.Context(), code)
		if err != nil {
			s.jsonError(w, "invalid or expired authorization code", http.StatusBadRequest)
			return
		}

		// Verify PKCE challenge (sufficient to bind the authorization request even if multiple
		// local mcp-remote instances raced to register different DCR client_ids).
		if !auth.VerifyPKCE(codeVerifier, authCode.CodeChallenge, authCode.CodeChallengeMethod) {
			s.jsonError(w, "PKCE code_verifier challenge failed", http.StatusBadRequest)
			return
		}

		user, err := s.repo.GetUserByID(r.Context(), authCode.UserID)
		if err != nil {
			s.jsonError(w, "user not found", http.StatusInternalServerError)
			return
		}

		if user.TokenVersion == "" {
			user.TokenVersion = uuid.NewString()
			_ = s.repo.UpdateUserTokenVersion(r.Context(), user.ID, user.TokenVersion)
		}

		token, err := s.tokenManager.GenerateToken(user)
		if err != nil {
			s.jsonError(w, "failed generating token", http.StatusInternalServerError)
			return
		}

		rawRT, rtHash, _ := auth.GenerateRefreshToken()
		_ = s.repo.CreateRefreshToken(r.Context(), &domain.RefreshToken{
			ID:           uuid.NewString(),
			UserID:       user.ID,
			ClientID:     clientID,
			TokenHash:    rtHash,
			TokenVersion: user.TokenVersion,
			ExpiresAt:    time.Now().UTC().Add(s.tokenManager.RefreshTTL()),
			CreatedAt:    time.Now().UTC(),
		})

		s.jsonResponse(w, map[string]interface{}{
			"access_token":  token,
			"token_type":    "Bearer",
			"expires_in":    int(s.tokenManager.TTL().Seconds()),
			"refresh_token": rawRT,
		}, http.StatusOK)

	case "refresh_token":
		refreshToken := r.FormValue("refresh_token")
		if refreshToken == "" {
			s.jsonError(w, "missing refresh_token", http.StatusBadRequest)
			return
		}

		rtHash := auth.HashRefreshToken(refreshToken)
		storedRT, err := s.repo.GetRefreshTokenByHash(r.Context(), rtHash)
		if err != nil {
			s.jsonError(w, "invalid or expired refresh token", http.StatusUnauthorized)
			return
		}

		if storedRT.RevokedAt != nil {
			s.jsonError(w, "refresh token revoked", http.StatusUnauthorized)
			return
		}

		if time.Now().UTC().After(storedRT.ExpiresAt) {
			s.jsonError(w, "refresh token expired", http.StatusUnauthorized)
			return
		}

		user, err := s.repo.GetUserByID(r.Context(), storedRT.UserID)
		if err != nil {
			s.jsonError(w, "user not found", http.StatusInternalServerError)
			return
		}

		// Enforce token_version check: if versions mismatch, reject as revoked
		if user.TokenVersion != "" && storedRT.TokenVersion != "" && user.TokenVersion != storedRT.TokenVersion {
			s.jsonError(w, "session revoked: token version mismatch", http.StatusUnauthorized)
			return
		}

		if user.TokenVersion == "" {
			user.TokenVersion = uuid.NewString()
			_ = s.repo.UpdateUserTokenVersion(r.Context(), user.ID, user.TokenVersion)
		}

		// Issue new access token while keeping the refresh token valid until ExpiresAt or explicit logout/revoke-all.
		// This prevents multi-process clients (such as multiple mcp-remote instances in Claude Desktop)
		// from invalidating each other's shared refresh token on concurrent refreshes.
		newAccess, err := s.tokenManager.GenerateToken(user)
		if err != nil {
			s.jsonError(w, "failed generating token", http.StatusInternalServerError)
			return
		}

		s.jsonResponse(w, map[string]interface{}{
			"access_token":  newAccess,
			"token_type":    "Bearer",
			"expires_in":    int(s.tokenManager.TTL().Seconds()),
			"refresh_token": refreshToken,
		}, http.StatusOK)

	default:
		s.jsonError(w, "unsupported grant_type", http.StatusBadRequest)
	}
}

// GET /oauth/authorize (RFC 6749 / RFC 7636 Authorization Endpoint)
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	redirectURI := r.URL.Query().Get("redirect_uri")
	responseType := r.URL.Query().Get("response_type")

	if clientID == "" || redirectURI == "" {
		s.jsonError(w, "missing required parameters: client_id and redirect_uri", http.StatusBadRequest)
		return
	}

	client, err := s.repo.GetOAuthClient(r.Context(), clientID)
	if err != nil {
		s.jsonError(w, "invalid client_id", http.StatusBadRequest)
		return
	}

	if err := auth.ValidateRedirectURI(client, redirectURI); err != nil {
		s.jsonError(w, "unauthorized redirect_uri", http.StatusBadRequest)
		return
	}

	if responseType != "" && responseType != "code" {
		s.jsonError(w, "unsupported response_type: must be 'code'", http.StatusBadRequest)
		return
	}

	// Forward user to frontend login / consent page preserving full query string
	http.Redirect(w, r, "/login?"+r.URL.RawQuery, http.StatusFound)
}

// POST /oauth/authorize (Consent submission from /login)
func (s *Server) handleOAuthAuthorizeSubmit(w http.ResponseWriter, r *http.Request) {
	var caller *domain.User
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		caller, _ = s.authenticateToken(r.Context(), tokenStr)
	}

	var clientID, redirectURI, codeChallenge, codeChallengeMethod, state string
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			ClientID            string `json:"client_id"`
			RedirectURI         string `json:"redirect_uri"`
			CodeChallenge       string `json:"code_challenge"`
			CodeChallengeMethod string `json:"code_challenge_method"`
			State               string `json:"state"`
			Token               string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		clientID = body.ClientID
		redirectURI = body.RedirectURI
		codeChallenge = body.CodeChallenge
		codeChallengeMethod = body.CodeChallengeMethod
		state = body.State
		if caller == nil && body.Token != "" {
			caller, _ = s.authenticateToken(r.Context(), body.Token)
		}
	} else {
		if err := r.ParseForm(); err != nil {
			s.jsonError(w, "invalid form data", http.StatusBadRequest)
			return
		}
		clientID = r.FormValue("client_id")
		redirectURI = r.FormValue("redirect_uri")
		codeChallenge = r.FormValue("code_challenge")
		codeChallengeMethod = r.FormValue("code_challenge_method")
		state = r.FormValue("state")
		if caller == nil && r.FormValue("token") != "" {
			caller, _ = s.authenticateToken(r.Context(), r.FormValue("token"))
		}
	}

	if caller == nil {
		s.jsonError(w, "unauthenticated user", http.StatusUnauthorized)
		return
	}

	client, err := s.repo.GetOAuthClient(r.Context(), clientID)
	if err != nil {
		s.jsonError(w, "invalid client_id", http.StatusBadRequest)
		return
	}

	if err := auth.ValidateRedirectURI(client, redirectURI); err != nil {
		s.jsonError(w, "unauthorized redirect_uri", http.StatusBadRequest)
		return
	}

	if codeChallengeMethod == "" {
		codeChallengeMethod = "plain"
	}

	authCode := &domain.AuthCode{
		Code:                "code_" + auth.GenerateRandomString(32),
		ClientID:            clientID,
		UserID:              caller.ID,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		ExpiresAt:           time.Now().UTC().Add(5 * time.Minute),
	}

	if err := s.repo.CreateAuthCode(r.Context(), authCode); err != nil {
		s.jsonError(w, "failed generating auth code: "+err.Error(), http.StatusInternalServerError)
		return
	}

	targetURL := redirectURI
	parsedRedirect, err := url.Parse(redirectURI)
	if err == nil {
		q := parsedRedirect.Query()
		q.Set("code", authCode.Code)
		if state != "" {
			q.Set("state", state)
		}
		parsedRedirect.RawQuery = q.Encode()
		targetURL = parsedRedirect.String()
	} else {
		sep := "?"
		if strings.Contains(redirectURI, "?") {
			sep = "&"
		}
		targetURL = fmt.Sprintf("%s%scode=%s", redirectURI, sep, url.QueryEscape(authCode.Code))
		if state != "" {
			targetURL += "&state=" + url.QueryEscape(state)
		}
	}

	if strings.Contains(r.Header.Get("Accept"), "application/json") || strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		s.jsonResponse(w, map[string]interface{}{
			"redirect_uri": targetURL,
			"code":         authCode.Code,
		}, http.StatusOK)
		return
	}

	http.Redirect(w, r, targetURL, http.StatusFound)
}

// GET /oauth/client-info
func (s *Server) handleOAuthClientInfo(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		s.jsonError(w, "missing client_id", http.StatusBadRequest)
		return
	}

	client, err := s.repo.GetOAuthClient(r.Context(), clientID)
	if err != nil {
		s.jsonError(w, "client not found", http.StatusNotFound)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"client_id":     client.ID,
		"client_name":   client.ClientName,
		"redirect_uris": client.RedirectURIs,
		"logo_uri":      s.getBaseURL(r) + "/logo.svg",
	}, http.StatusOK)
}

func (s *Server) isAllowedSSORedirectURI(r *http.Request, redirectURI string) bool {
	if redirectURI == "" || redirectURI == "null" || redirectURI == "postmessage" || redirectURI == "popup" {
		return true
	}
	if strings.HasPrefix(redirectURI, "/") && !strings.HasPrefix(redirectURI, "//") && !strings.HasPrefix(redirectURI, "/\\") {
		return true
	}
	if strings.HasPrefix(redirectURI, "file://") {
		return true
	}
	u, err := url.Parse(redirectURI)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	if r != nil && strings.EqualFold(u.Host, r.Host) {
		return true
	}
	baseURL := strings.TrimRight(s.getBaseURL(r), "/")
	if baseURL != "" && (strings.EqualFold(redirectURI, baseURL) || strings.HasPrefix(strings.ToLower(redirectURI), strings.ToLower(baseURL)+"/")) {
		return true
	}
	canonical := strings.ToLower(strings.TrimSpace(os.Getenv("CANONICAL_DOMAIN")))
	if canonical != "" && (host == canonical || strings.HasSuffix(host, "."+canonical)) {
		return true
	}
	if cols, err := s.repo.ListCollections(r.Context()); err == nil {
		lowerTarget := strings.ToLower(redirectURI)
		for _, col := range cols {
			for _, origin := range col.OriginURLs {
				normOrigin := strings.ToLower(strings.TrimRight(strings.TrimSpace(origin), "/"))
				if normOrigin == "" {
					continue
				}
				if lowerTarget == normOrigin ||
					strings.HasPrefix(lowerTarget, normOrigin+"/") ||
					strings.HasPrefix(lowerTarget, normOrigin+"?") ||
					strings.HasPrefix(lowerTarget, normOrigin+"#") {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) resolveCollectionFromAppURL(ctx context.Context, rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	if strings.HasPrefix(u.Path, "/apps/") {
		seg := strings.TrimPrefix(u.Path, "/apps/")
		if idx := strings.IndexByte(seg, '/'); idx != -1 {
			seg = seg[:idx]
		}
		if seg != "" {
			if col, err := s.repo.GetCollection(ctx, seg); err == nil && col != nil {
				return col.ID
			}
		}
	}
	host := strings.ToLower(u.Hostname())
	canonical := strings.ToLower(strings.TrimSpace(os.Getenv("CANONICAL_DOMAIN")))
	if canonical == "" {
		canonical = "vibebac.kr"
	}
	if strings.HasSuffix(host, "."+canonical) {
		sub := strings.TrimSuffix(host, "."+canonical)
		if sub != "" && !strings.Contains(sub, ".") && sub != "www" && sub != "api" {
			if col, err := s.repo.GetCollection(ctx, sub); err == nil && col != nil {
				return col.ID
			}
		}
	}
	return ""
}

func (s *Server) redirectNotRegistered(w http.ResponseWriter, r *http.Request, redirectURI string, isPopup bool, email, providerName, collectionID string) {
	isPopupFlow := isPopup || redirectURI == "postmessage" || redirectURI == "popup"
	isAppLogin := isPopupFlow || collectionID != "" || (redirectURI != "" && !strings.Contains(redirectURI, "/login"))

	targetURL := redirectURI
	if targetURL == "" || targetURL == "postmessage" || targetURL == "popup" {
		targetURL = "/login"
	} else if !strings.Contains(targetURL, "/login") {
		targetURL = "/login?return_to=" + url.QueryEscape(targetURL)
	}

	sep := "?"
	if strings.Contains(targetURL, "?") {
		sep = "&"
	}
	targetURL = fmt.Sprintf("%s%serror=not_registered&email=%s&provider=%s",
		targetURL, sep, url.QueryEscape(email), url.QueryEscape(providerName))
	if collectionID != "" {
		targetURL += "&collection_id=" + url.QueryEscape(collectionID)
	}
	if isAppLogin {
		targetURL += "&app=true"
	}
	if isPopupFlow {
		targetURL += "&popup=true"
	}
	http.Redirect(w, r, targetURL, http.StatusTemporaryRedirect)
}

// GET /auth/login/{provider}
func (s *Server) handleSSOLogin(w http.ResponseWriter, r *http.Request) {
	providerName := r.PathValue("provider")
	provider, err := s.ssoManager.GetProvider(providerName)
	if err != nil && providerName == "google" {
		if mockProv, mErr := s.ssoManager.GetProvider("mock"); mErr == nil {
			provider = mockProv
			err = nil
		}
	}
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	signupParam := r.URL.Query().Get("signup")
	isSignup := signupParam == "true" || signupParam == "1"
	popupParam := r.URL.Query().Get("popup") == "true" || r.URL.Query().Get("popup") == "1"

	redirectURI := r.URL.Query().Get("redirect_uri")
	collectionID := strings.TrimSpace(r.URL.Query().Get("collection_id"))
	if redirectURI == "" && !popupParam && collectionID != "" {
		if col, colErr := s.repo.GetCollection(r.Context(), collectionID); colErr == nil && col != nil && len(col.OriginURLs) > 0 && col.OriginURLs[0] != "*" {
			redirectURI = col.OriginURLs[0]
		}
	}
	if collectionID == "" {
		collectionID = s.resolveCollectionFromAppURL(r.Context(), redirectURI)
		if collectionID == "" {
			collectionID = s.resolveCollectionFromAppURL(r.Context(), r.Header.Get("Referer"))
		}
	}

	if !s.isAllowedSSORedirectURI(r, redirectURI) {
		s.jsonError(w, "unauthorized redirect_uri", http.StatusBadRequest)
		return
	}

	state := auth.GenerateRandomString(16)
	if redirectURI != "" && redirectURI != "null" {
		state = state + "|" + redirectURI
	}
	if collectionID != "" {
		state = state + "|col=" + collectionID
	}
	if isSignup {
		state = state + "|signup=true"
	}
	if popupParam {
		state = state + "|popup=true"
	}

	authURL := provider.GetAuthURL(state)
	if prompt := r.URL.Query().Get("prompt"); prompt != "" && !strings.Contains(authURL, "prompt=") {
		sep := "?"
		if strings.Contains(authURL, "?") {
			sep = "&"
		}
		authURL += sep + "prompt=" + url.QueryEscape(prompt)
	}
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

// GET /auth/callback/{provider}
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	providerName := r.PathValue("provider")
	provider, err := s.ssoManager.GetProvider(providerName)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		s.jsonError(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	isSignup := r.URL.Query().Get("signup") == "true" || r.URL.Query().Get("signup") == "1"
	stateParam := r.URL.Query().Get("state")
	isPopup := r.URL.Query().Get("popup") == "true" || r.URL.Query().Get("popup") == "1"
	collectionID := strings.TrimSpace(r.URL.Query().Get("collection_id"))
	if strings.Contains(stateParam, "signup=true") {
		isSignup = true
	}
	if strings.Contains(stateParam, "popup=true") {
		isPopup = true
	}

	redirectURI := ""
	if stateParam != "" {
		cleanState := stateParam
		for {
			if strings.HasSuffix(cleanState, "|popup=true") {
				isPopup = true
				cleanState = strings.TrimSuffix(cleanState, "|popup=true")
				continue
			}
			if strings.HasSuffix(cleanState, "|signup=true") {
				isSignup = true
				cleanState = strings.TrimSuffix(cleanState, "|signup=true")
				continue
			}
			if idx := strings.LastIndex(cleanState, "|col="); idx != -1 {
				colPart := cleanState[idx+len("|col="):]
				if !strings.Contains(colPart, "|") {
					if collectionID == "" {
						collectionID = colPart
					}
					cleanState = cleanState[:idx]
					continue
				}
			}
			break
		}
		parts := strings.SplitN(cleanState, "|", 2)
		if len(parts) == 2 && parts[1] != "" {
			redirectURI = parts[1]
		}
	}

	if redirectURI == "" {
		redirectURI = r.URL.Query().Get("redirect_uri")
	}
	if redirectURI == "null" {
		redirectURI = ""
	}
	if collectionID == "" && redirectURI != "" {
		collectionID = s.resolveCollectionFromAppURL(r.Context(), redirectURI)
	}
	if !s.isAllowedSSORedirectURI(r, redirectURI) {
		s.jsonError(w, "unauthorized redirect_uri", http.StatusBadRequest)
		return
	}

	userInfo, err := provider.ExchangeCode(r.Context(), code)
	if err != nil {
		s.jsonError(w, "SSO exchange failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	user, err := s.ssoManager.ProvisionOrGetUser(r.Context(), userInfo, isSignup)
	if err != nil {
		if !isSignup && strings.Contains(err.Error(), "unauthorized") {
			s.redirectNotRegistered(w, r, redirectURI, isPopup, userInfo.Email, providerName, collectionID)
			return
		}
		s.jsonError(w, "provisioning failed: "+err.Error(), http.StatusForbidden)
		return
	}

	if !isSignup && collectionID != "" {
		if col, colErr := s.repo.GetCollection(r.Context(), collectionID); colErr == nil && col != nil && !s.isCollectionTenantAuthorized(r.Context(), user, col) {
			if recs, rErr := s.repo.ListRecords(r.Context(), col.ID, user); rErr != nil || len(recs) == 0 {
				s.redirectNotRegistered(w, r, redirectURI, isPopup, userInfo.Email, providerName, col.ID)
				return
			}
		}
	}

	if user.TokenVersion == "" {
		user.TokenVersion = uuid.NewString()
		_ = s.repo.UpdateUserTokenVersion(r.Context(), user.ID, user.TokenVersion)
	}

	token, err := s.tokenManager.GenerateToken(user)
	if err != nil {
		s.jsonError(w, "failed generating token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rawRT, rtHash, _ := auth.GenerateRefreshToken()
	_ = s.repo.CreateRefreshToken(r.Context(), &domain.RefreshToken{
		ID:           uuid.NewString(),
		UserID:       user.ID,
		ClientID:     "web-console",
		TokenHash:    rtHash,
		TokenVersion: user.TokenVersion,
		ExpiresAt:    time.Now().UTC().Add(s.tokenManager.RefreshTTL()),
		CreatedAt:    time.Now().UTC(),
	})

	// 1. Popup mode: transmit token back to local app window via postMessage
	if isPopup || redirectURI == "postmessage" || redirectURI == "popup" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		userJSON, _ := json.Marshal(user)
		fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>Authentication Successful - VibeBackr</title>
  <link rel="preconnect" href="https://fonts.googleapis.com"/>
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin/>
  <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto:wght@100;300;400;500;700&display=swap"/>
  <link rel="stylesheet" href="/style.css"/>
  <link rel="icon" type="image/svg+xml" href="/logo.svg"/>
  <style>
    :root {
        --bg: #fbf9f8;
        --border: rgba(23, 25, 27, 0.09);
        --fg: #17191b;
        --muted: #6b7270;
        --gradient-from: #6366f1;
        --gradient-to: #06b6d4;
        --font-sans: 'Roboto', ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif;
        --font-mono: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace;
    }
    * {
        box-sizing: border-box;
    }
    html, body {
        margin: 0;
        padding: 24px;
        background: var(--bg);
        color: var(--fg);
        font-family: var(--font-sans);
        min-height: 100dvh;
        display: flex;
        align-items: safe center;
        justify-content: safe center;
        -webkit-font-smoothing: antialiased;
    }
    .callback__card {
        background: var(--bg);
        border: 1px solid var(--border);
        border-radius: 8px;
        padding: 32px;
        text-align: center;
        max-width: 420px;
        width: 100%%;
    }
    .callback__logo {
        display: block;
        margin: 0 auto 12px;
    }
    .callback__eyebrow {
        font-family: var(--font-mono);
        font-size: 0.72rem;
        font-weight: 500;
        letter-spacing: 0.06em;
        text-transform: uppercase;
        color: var(--gradient-from);
        margin-bottom: 10px;
    }
    .callback__title {
        margin: 0 0 8px 0;
        font-size: 1.45rem;
        font-weight: 300;
        letter-spacing: -0.015em;
        color: var(--fg);
        text-wrap: balance;
    }
    .callback__status {
        color: var(--muted);
        font-size: 0.9rem;
        margin: 0;
        line-height: 1.6;
    }
    .callback__manual {
        display: none;
        text-align: left;
        margin-top: 20px;
        padding-top: 16px;
        border-top: 1px solid var(--border);
    }
    .callback__label {
        font-family: var(--font-mono);
        font-size: 0.74rem;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        color: var(--muted);
        margin: 0 0 6px 0;
    }
    .callback__input {
        width: 100%%;
        padding: 9px 12px;
        font-size: 0.78rem;
        background: transparent;
        color: var(--fg);
        border: 1px solid var(--border);
        border-radius: 6px;
        font-family: var(--font-mono);
        margin-bottom: 10px;
    }
    .btn-primary {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        width: 100%%;
        padding: 10px 18px;
        background: var(--fg);
        color: var(--bg);
        border: 1px solid var(--fg);
        border-radius: 6px;
        font-family: var(--font-sans);
        font-size: 0.9rem;
        font-weight: 500;
        cursor: pointer;
        text-decoration: none;
        transition: opacity 0.15s ease;
    }
    .btn-primary:hover {
        opacity: 0.86;
    }
  </style>
</head>
<body>
  <div class="callback__card">
    <img src="/logo.svg" alt="" width="44" height="44" class="callback__logo"/>
    <div class="callback__eyebrow">// AUTHENTICATED</div>
    <h2 class="callback__title">Signed In Successfully</h2>
    <p id="statusMsg" class="callback__status">Completing sign in...</p>
    <div id="manual" class="callback__manual">
      <p class="callback__label">Access Token</p>
      <input id="tokenInput" class="callback__input" type="text" readonly value="%s"/>
      <button id="copyBtn" class="btn-primary" onclick="navigator.clipboard.writeText(document.getElementById('tokenInput').value); this.innerText='Copied!';">Copy Token</button>
    </div>
  </div>
  <script>
    const authData = {
        access_token: %q,
        refresh_token: %q,
        token_type: 'Bearer',
        expires_in: %d,
        user: %s
    };
    if(window.opener) {
        try {
            window.opener.postMessage(authData, '*');
            document.getElementById('statusMsg').textContent = 'Authenticated! Closing window...';
            setTimeout(window.close.bind(window), 600);
        } catch(e) {
            document.getElementById('manual').style.display = 'block';
        }
    } else {
        document.getElementById('statusMsg').textContent = 'Authenticated!';
        document.getElementById('manual').style.display = 'block';
    }
  </script>
</body>
</html>`, token, token, rawRT, int(s.tokenManager.TTL().Seconds()), string(userJSON))
		return
	}

	// 2. Direct file:// URL return page (browser blocks 307 redirects to file://)
	if strings.HasPrefix(redirectURI, "file://") {
		separator := "#"
		if strings.Contains(redirectURI, "#") {
			separator = "&"
		}
		targetURL := fmt.Sprintf("%s%saccess_token=%s&token_type=Bearer&expires_in=%d&refresh_token=%s", redirectURI, separator, token, int(s.tokenManager.TTL().Seconds()), rawRT)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>Authentication Complete - VibeBackr</title>
  <link rel="preconnect" href="https://fonts.googleapis.com"/>
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin/>
  <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto:wght@100;300;400;500;700&display=swap"/>
  <link rel="stylesheet" href="/style.css"/>
  <link rel="icon" type="image/svg+xml" href="/logo.svg"/>
  <style>
    :root {
        --bg: #fbf9f8;
        --border: rgba(23, 25, 27, 0.09);
        --fg: #17191b;
        --muted: #6b7270;
        --gradient-from: #6366f1;
        --gradient-to: #06b6d4;
        --font-sans: 'Roboto', ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif;
        --font-mono: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace;
    }
    * {
        box-sizing: border-box;
    }
    html, body {
        margin: 0;
        padding: 24px;
        background: var(--bg);
        color: var(--fg);
        font-family: var(--font-sans);
        min-height: 100dvh;
        display: flex;
        align-items: safe center;
        justify-content: safe center;
        -webkit-font-smoothing: antialiased;
    }
    .callback__card {
        background: var(--bg);
        border: 1px solid var(--border);
        border-radius: 8px;
        padding: 32px;
        text-align: center;
        max-width: 440px;
        width: 100%%;
    }
    .callback__logo {
        display: block;
        margin: 0 auto 12px;
    }
    .callback__eyebrow {
        font-family: var(--font-mono);
        font-size: 0.72rem;
        font-weight: 500;
        letter-spacing: 0.06em;
        text-transform: uppercase;
        color: var(--gradient-from);
        margin-bottom: 10px;
    }
    .callback__title {
        margin: 0 0 8px 0;
        font-size: 1.45rem;
        font-weight: 300;
        letter-spacing: -0.015em;
        color: var(--fg);
        text-wrap: balance;
    }
    .callback__status {
        color: var(--muted);
        font-size: 0.9rem;
        margin: 0 0 20px 0;
        line-height: 1.6;
    }
    .btn-primary {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        width: 100%%;
        padding: 10px 18px;
        background: var(--fg);
        color: var(--bg) !important;
        border: 1px solid var(--fg);
        border-radius: 6px;
        font-family: var(--font-sans);
        font-size: 0.9rem;
        font-weight: 500;
        cursor: pointer;
        text-decoration: none !important;
        transition: opacity 0.15s ease;
    }
    .btn-primary:hover {
        opacity: 0.86;
    }
  </style>
</head>
<body>
  <div class="callback__card">
    <img src="/logo.svg" alt="" width="44" height="44" class="callback__logo"/>
    <div class="callback__eyebrow">// AUTHENTICATED</div>
    <h2 class="callback__title">Signed In Successfully</h2>
    <p class="callback__status">Click below to return to your local application:</p>
    <a href="%s" class="btn-primary">Return to Local Application -&gt;</a>
  </div>
  <script>
    if(window.opener) {
        try {
            window.opener.postMessage({access_token: %q, refresh_token: %q, token_type: 'Bearer', expires_in: %d}, '*');
            setTimeout(window.close.bind(window), 600);
        } catch(e) {
            //ignore cross-origin opener errors
        }
    }
  </script>
</body>
</html>`, targetURL, token, rawRT, int(s.tokenManager.TTL().Seconds()))
		return
	}

	// 3. Standard HTTP/HTTPS redirect back to application (e.g. http://localhost:3000, https://myapp.com)
	if redirectURI != "" {
		separator := "#"
		if strings.Contains(redirectURI, "#") {
			separator = "&"
		}
		targetURL := fmt.Sprintf("%s%saccess_token=%s&token_type=Bearer&expires_in=%d&refresh_token=%s", redirectURI, separator, token, int(s.tokenManager.TTL().Seconds()), rawRT)
		http.Redirect(w, r, targetURL, http.StatusTemporaryRedirect)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"access_token":  token,
		"token_type":    "Bearer",
		"expires_in":    int(s.tokenManager.TTL().Seconds()),
		"refresh_token": rawRT,
		"user":          user,
	}, http.StatusOK)
}

// POST /api/auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if rt := r.FormValue("refresh_token"); rt != "" {
		rtHash := auth.HashRefreshToken(rt)
		if stored, err := s.repo.GetRefreshTokenByHash(r.Context(), rtHash); err == nil {
			_ = s.repo.RevokeRefreshToken(r.Context(), stored.ID)
		}
	}
	s.jsonResponse(w, map[string]string{"message": "successfully logged out"}, http.StatusOK)
}

// POST /api/auth/revoke-all (Sign out of all sessions/devices)
func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	newVersion := uuid.NewString()
	user.TokenVersion = newVersion

	if err := s.repo.UpdateUserTokenVersion(r.Context(), user.ID, newVersion); err != nil {
		s.jsonError(w, "failed updating token version: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.repo.RevokeAllUserRefreshTokens(r.Context(), user.ID); err != nil {
		s.jsonError(w, "failed revoking refresh tokens: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"message":       "all sessions revoked successfully",
		"token_version": newVersion,
	}, http.StatusOK)
}

// GET /api/auth/providers
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	var providers []string
	var details []auth.ProviderMetadata
	if s.ssoManager != nil {
		providers = s.ssoManager.ListProviderNames()
		details = s.ssoManager.ListProviderMetadata()
	}
	if providers == nil {
		providers = []string{}
	}
	if details == nil {
		details = []auth.ProviderMetadata{}
	}
	s.jsonResponse(w, map[string]interface{}{
		"providers":        providers,
		"provider_details": details,
	}, http.StatusOK)
}

func (s *Server) canViewUser(ctx context.Context, caller, target *domain.User) bool {
	if caller == nil || target == nil {
		return false
	}
	if caller.ID == target.ID {
		return true
	}
	if caller.TenantID != nil && *caller.TenantID != "" && s.isTenantMember(ctx, target, *caller.TenantID) {
		return true
	}
	if target.TenantID != nil && *target.TenantID != "" && s.isTenantMember(ctx, caller, *target.TenantID) {
		return true
	}
	callerMemberships, _ := s.repo.ListUserTenants(ctx, caller.ID)
	for _, cm := range callerMemberships {
		if cm.TenantID != "" && s.isTenantMember(ctx, target, cm.TenantID) {
			return true
		}
	}
	return false
}

// GET /api/users/{id}
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	caller := currentUser(r)
	user, err := s.repo.GetUserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "user not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.canViewUser(r.Context(), caller, user) {
		s.jsonError(w, "forbidden: cannot view user in another tenant", http.StatusForbidden)
		return
	}
	if caller.ID != user.ID {
		user.TokenVersion = ""
	}
	s.jsonResponse(w, user, http.StatusOK)
}

// GET /api/client-guide
func (s *Server) handleClientGuide(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	collectionID := r.URL.Query().Get("collection_id")
	baseURL := s.getBaseURL(r)
	ctx := context.WithValue(r.Context(), mcp.BaseURLContextKey, baseURL)
	var caller *domain.User
	if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		caller, _ = s.authenticateToken(r.Context(), strings.TrimPrefix(authHeader, "Bearer "))
	}
	guide, err := s.mcpServer.GenerateClientGuideForUser(ctx, topic, collectionID, caller)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.URL.Query().Get("format") == "json" {
		var providers []string
		if s.ssoManager != nil {
			providers = s.ssoManager.ListProviderNames()
		}
		if providers == nil {
			providers = []string{}
		}
		s.jsonResponse(w, map[string]interface{}{
			"guide":     guide,
			"providers": providers,
		}, http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(guide))
}

// GET /vibebackr.mcpb
func (s *Server) handleMCPBundle(w http.ResponseWriter, r *http.Request) {
	baseURL := s.getBaseURL(r)
	ctx := context.WithValue(r.Context(), mcp.BaseURLContextKey, baseURL)
	bundleBytes, err := s.mcpServer.BuildMCPB(ctx)
	if err != nil {
		s.jsonError(w, "failed generating MCP bundle: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="vibebackr.mcpb"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bundleBytes)
}

// GET /api/collections
func (s *Server) handleListCollections(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	cols, err := s.repo.ListCollectionsForUser(r.Context(), user)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.jsonResponse(w, cols, http.StatusOK)
}

// GET /api/tenants
func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	memberships, err := s.repo.ListUserTenants(r.Context(), user.ID)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.jsonResponse(w, memberships, http.StatusOK)
}

// POST /api/tenants
func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user.TenantID != nil && *user.TenantID != "" {
		s.jsonResponse(w, map[string]interface{}{
			"message":   "user already has a tenant",
			"tenant_id": *user.TenantID,
			"user":      user,
		}, http.StatusOK)
		return
	}

	newTenantID := uuid.NewString()
	user.TenantID = &newTenantID
	user.Role = domain.RoleAdmin // admin of their new tenant

	if err := s.repo.UpdateUser(r.Context(), user); err != nil {
		s.jsonError(w, "failed updating user tenant: "+err.Error(), http.StatusInternalServerError)
		return
	}

	newToken, err := s.tokenManager.GenerateToken(user)
	if err != nil {
		s.jsonError(w, "failed generating token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"tenant_id":    newTenantID,
		"access_token": newToken,
		"token_type":   "Bearer",
		"expires_in":   int(s.tokenManager.TTL().Seconds()),
		"user":         user,
	}, http.StatusCreated)
}

// DELETE /api/tenants/{tenant_id}
func (s *Server) handleLeaveTenant(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	user := currentUser(r)

	if user.TenantID != nil && *user.TenantID == tenantID {
		s.jsonError(w, "cannot leave your own primary tenant", http.StatusForbidden)
		return
	}

	if err := s.repo.RemoveTenantMembership(r.Context(), user.ID, tenantID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "tenant membership not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"message":   "left tenant successfully",
		"tenant_id": tenantID,
	}, http.StatusOK)
}

func (s *Server) isTenantMember(ctx context.Context, user *domain.User, tenantID string) bool {
	if user == nil || tenantID == "" {
		return false
	}
	if user.TenantID != nil && *user.TenantID == tenantID {
		return true
	}
	memberships, err := s.repo.ListUserTenants(ctx, user.ID)
	if err != nil {
		return false
	}
	for _, m := range memberships {
		if m.TenantID == tenantID {
			return true
		}
	}
	return false
}

func (s *Server) isCollectionTenantAuthorized(ctx context.Context, user *domain.User, col *domain.Collection) bool {
	if col == nil || col.TenantID == "" {
		return true
	}
	if user == nil {
		return false
	}
	if s.isTenantMember(ctx, user, col.TenantID) {
		return true
	}
	parts := strings.Split(user.Email, "@")
	if len(parts) == 2 {
		for _, d := range col.AllowedDomains {
			if strings.EqualFold(d, parts[1]) {
				return true
			}
		}
	}
	return false
}

func (s *Server) isTenantAdmin(ctx context.Context, user *domain.User, tenantID string) bool {
	if user == nil {
		return false
	}
	userTenantID := ""
	if user.TenantID != nil {
		userTenantID = *user.TenantID
	}
	if user.Role == domain.RoleAdmin && userTenantID == tenantID {
		return true
	}
	if tenantID != "" && userTenantID == tenantID {
		return true
	}
	if tenantID != "" {
		memberships, err := s.repo.ListUserTenants(ctx, user.ID)
		if err == nil {
			for _, m := range memberships {
				if m.TenantID == tenantID && m.Role == domain.RoleAdmin {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) isTenantAdminOrManager(ctx context.Context, user *domain.User, tenantID string) bool {
	if s.isTenantAdmin(ctx, user, tenantID) {
		return true
	}
	if user == nil || tenantID == "" {
		return false
	}
	if user.TenantID != nil && *user.TenantID == tenantID && user.Role == domain.RoleManager {
		return true
	}
	memberships, err := s.repo.ListUserTenants(ctx, user.ID)
	if err == nil {
		for _, m := range memberships {
			if m.TenantID == tenantID && (m.Role == domain.RoleAdmin || m.Role == domain.RoleManager) {
				return true
			}
		}
	}
	return false
}

func (s *Server) defaultAdminOrManagerTenant(ctx context.Context, user *domain.User) string {
	if user == nil {
		return ""
	}
	if user.TenantID != nil && *user.TenantID != "" {
		return *user.TenantID
	}
	memberships, err := s.repo.ListUserTenants(ctx, user.ID)
	if err == nil {
		for _, m := range memberships {
			if m.TenantID != "" && (m.Role == domain.RoleAdmin || m.Role == domain.RoleManager) {
				return m.TenantID
			}
		}
	}
	return ""
}

// GET /api/invites
func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	requestedTenant := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if requestedTenant != "" {
		if !s.isTenantAdminOrManager(r.Context(), user, requestedTenant) {
			s.jsonError(w, "forbidden: admin or manager role required to view invites for this tenant", http.StatusForbidden)
			return
		}
		invites, err := s.repo.ListInvitesByTenant(r.Context(), requestedTenant)
		if err != nil {
			s.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.jsonResponse(w, invites, http.StatusOK)
		return
	}

	seenTenants := make(map[string]bool)
	var tenantIDs []string
	if user.TenantID != nil && *user.TenantID != "" {
		seenTenants[*user.TenantID] = true
		tenantIDs = append(tenantIDs, *user.TenantID)
	}
	if memberships, err := s.repo.ListUserTenants(r.Context(), user.ID); err == nil {
		for _, m := range memberships {
			if m.TenantID != "" && (m.Role == domain.RoleAdmin || m.Role == domain.RoleManager) && !seenTenants[m.TenantID] {
				seenTenants[m.TenantID] = true
				tenantIDs = append(tenantIDs, m.TenantID)
			}
		}
	}

	allInvites := []domain.Invite{}
	for _, tid := range tenantIDs {
		if invs, err := s.repo.ListInvitesByTenant(r.Context(), tid); err == nil {
			allInvites = append(allInvites, invs...)
		}
	}
	s.jsonResponse(w, allInvites, http.StatusOK)
}

// POST /api/invites
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var payload struct {
		Email        string `json:"email"`
		Role         string `json:"role"`
		TenantID     string `json:"tenant_id"`
		ManagerEmail string `json:"manager_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(payload.Email)
	if email == "" || !strings.Contains(email, "@") {
		s.jsonError(w, "valid email is required", http.StatusBadRequest)
		return
	}

	role := strings.ToLower(strings.TrimSpace(payload.Role))
	if role == "" {
		role = domain.RoleMember
	}
	if role != domain.RoleAdmin && role != domain.RoleManager && role != domain.RoleMember {
		s.jsonError(w, "role must be 'admin', 'manager', or 'member'", http.StatusBadRequest)
		return
	}

	targetTenant := strings.TrimSpace(payload.TenantID)
	if targetTenant != "" {
		if !s.isTenantAdminOrManager(r.Context(), user, targetTenant) {
			s.jsonError(w, "forbidden: admin or manager role required to invite users to this tenant", http.StatusForbidden)
			return
		}
	} else {
		targetTenant = s.defaultAdminOrManagerTenant(r.Context(), user)
		if targetTenant == "" {
			var err error
			targetTenant, err = s.ensureCallerTenant(r.Context(), user)
			if err != nil {
				s.jsonError(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	var mgrID *string
	if strings.TrimSpace(payload.ManagerEmail) != "" {
		if mgr, err := s.repo.GetUserByEmail(r.Context(), strings.TrimSpace(payload.ManagerEmail)); err == nil && mgr != nil {
			mgrID = &mgr.ID
		}
	}

	inv := &domain.Invite{
		ID:        uuid.NewString(),
		TenantID:  targetTenant,
		Email:     email,
		Role:      role,
		ManagerID: mgrID,
		CreatedBy: user.ID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.repo.CreateInvite(r.Context(), inv); err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.jsonResponse(w, inv, http.StatusCreated)
}

// DELETE /api/invites/{id}
func (s *Server) handleDeleteInvite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := currentUser(r)

	inv, err := s.repo.GetInviteByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "invite not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if inv.CreatedBy != user.ID && !s.isTenantAdminOrManager(r.Context(), user, inv.TenantID) {
		s.jsonError(w, "forbidden: cannot revoke another tenant's invite", http.StatusForbidden)
		return
	}
	if err := s.repo.DeleteInvite(r.Context(), id); err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.jsonResponse(w, map[string]string{"message": "invite revoked", "id": id}, http.StatusOK)
}

func normalizeRecordData(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if str == "" {
			return "{}"
		}
		return str
	}
	s := string(raw)
	if s == "null" {
		return "{}"
	}
	return s
}

func (s *Server) matchesCollection(ctx context.Context, recordColID, pathColID string) bool {
	if pathColID == "" || recordColID == pathColID {
		return true
	}
	col, err := s.repo.GetCollection(ctx, pathColID)
	return err == nil && col != nil && col.ID == recordColID
}

// GET /api/collections/{collection_id}
func (s *Server) handleGetCollection(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	user := currentUser(r)

	col, err := s.repo.GetCollection(r.Context(), colID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "collection not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !s.isCollectionTenantAuthorized(r.Context(), user, col) {
		records, err := s.repo.ListRecords(r.Context(), col.ID, user)
		if err != nil {
			s.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(records) == 0 {
			s.jsonError(w, "forbidden: cannot access another tenant's collection", http.StatusForbidden)
			return
		}
	}

	s.jsonResponse(w, col, http.StatusOK)
}

// POST /api/collections
func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	var payload struct {
		Name                string   `json:"name"`
		Slug                string   `json:"slug"`
		TenantID            string   `json:"tenant_id"`
		DefaultVisibility   string   `json:"default_visibility"`
		AllowedDomains      []string `json:"allowed_domains"`
		OriginURLs          []string `json:"origin_urls"`
		VectorSearchEnabled bool     `json:"vector_search_enabled"`
		AutoInviteShares    bool     `json:"auto_invite_shares"`
		Records             []struct {
			Title        string              `json:"title"`
			Content      string              `json:"content"`
			Data         json.RawMessage     `json:"data"`
			Visibility   string              `json:"visibility"`
			Shares       []string            `json:"shares"`
			AccessPolicy domain.AccessPolicy `json:"access_policy"`
		} `json:"records"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if payload.Name == "" {
		s.jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	targetTenant := strings.TrimSpace(payload.TenantID)
	if targetTenant != "" {
		if !s.isTenantAdminOrManager(r.Context(), user, targetTenant) {
			s.jsonError(w, "forbidden: admin or manager role required to create collection in target tenant", http.StatusForbidden)
			return
		}
	} else {
		targetTenant = s.defaultAdminOrManagerTenant(r.Context(), user)
		if targetTenant == "" {
			// Auto-provision tenant for user deciding to build their own backend
			newTenantID := uuid.NewString()
			user.TenantID = &newTenantID
			user.Role = domain.RoleAdmin
			if err := s.repo.UpdateUser(r.Context(), user); err != nil {
				s.jsonError(w, "failed provisioning tenant for user: "+err.Error(), http.StatusInternalServerError)
				return
			}
			targetTenant = newTenantID
		}
	}

	vis := payload.DefaultVisibility
	if vis == "" {
		vis = domain.VisibilityPrivate
	}

	col := &domain.Collection{
		ID:                  uuid.NewString(),
		TenantID:            targetTenant,
		Name:                payload.Name,
		Slug:                payload.Slug,
		DefaultVisibility:   vis,
		AllowedDomains:      payload.AllowedDomains,
		OriginURLs:          payload.OriginURLs,
		VectorSearchEnabled: payload.VectorSearchEnabled,
		AutoInviteShares:    payload.AutoInviteShares,
		CreatedAt:           time.Now().UTC(),
	}

	if err := s.repo.CreateCollection(r.Context(), col); err != nil {
		if s.handleWriteError(w, err) {
			return
		}
		if errors.Is(err, domain.ErrInvalidSlug) {
			s.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if errors.Is(err, repository.ErrSlugTaken) {
			s.jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Insert any seed records provided in JSON configuration
	for _, recInput := range payload.Records {
		dataStr := normalizeRecordData(recInput.Data)
		recVis := recInput.AccessPolicy.Visibility
		if recVis == "" {
			recVis = recInput.Visibility
		}
		if recVis == "" {
			recVis = col.DefaultVisibility
		}
		recShares := recInput.AccessPolicy.Shares
		if len(recShares) == 0 && len(recInput.Shares) > 0 {
			recShares = recInput.Shares
		}
		rec := &domain.Record{
			ID:           uuid.NewString(),
			CollectionID: col.ID,
			Title:        recInput.Title,
			Content:      recInput.Content,
			Data:         dataStr,
			CreatedBy:    user.ID,
			AccessPolicy: domain.AccessPolicy{
				Visibility: recVis,
				Shares:     recShares,
			},
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		_ = s.repo.CreateRecord(r.Context(), rec)
	}

	s.jsonResponse(w, col, http.StatusCreated)
}

// DELETE /api/collections/{collection_id}
func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	user := currentUser(r)

	col, err := s.repo.GetCollection(r.Context(), colID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "collection not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !s.isTenantAdmin(r.Context(), user, col.TenantID) {
		s.jsonError(w, "forbidden: cannot delete another tenant's collection", http.StatusForbidden)
		return
	}

	if err := s.repo.DeleteCollection(r.Context(), col.ID); err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if s.mcpServer != nil {
		if store := s.mcpServer.GetAppStore(); store != nil {
			_ = store.DeleteApp(r.Context(), col.ID)
			if col.Slug != "" {
				_ = store.DeleteApp(r.Context(), col.Slug)
			}
		}
	}

	s.jsonResponse(w, map[string]interface{}{
		"message": "collection deleted successfully",
		"id":      col.ID,
	}, http.StatusOK)
}

// PUT /api/collections/{collection_id}
func (s *Server) handleUpdateCollection(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	user := currentUser(r)

	col, err := s.repo.GetCollection(r.Context(), colID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "collection not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !s.isTenantAdmin(r.Context(), user, col.TenantID) {
		s.jsonError(w, "only admin of this tenant can update collection settings", http.StatusForbidden)
		return
	}

	var payload struct {
		Name                string   `json:"name"`
		Slug                *string  `json:"slug"`
		DefaultVisibility   string   `json:"default_visibility"`
		AllowedDomains      []string `json:"allowed_domains"`
		OriginURLs          []string `json:"origin_urls"`
		VectorSearchEnabled *bool    `json:"vector_search_enabled"`
		AutoInviteShares    *bool    `json:"auto_invite_shares"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	oldSlug := col.Slug
	if payload.Name != "" {
		col.Name = payload.Name
	}
	if payload.Slug != nil {
		col.Slug = *payload.Slug
	}
	if payload.DefaultVisibility != "" {
		col.DefaultVisibility = payload.DefaultVisibility
	}
	if payload.AllowedDomains != nil {
		col.AllowedDomains = payload.AllowedDomains
	}
	if payload.OriginURLs != nil {
		col.OriginURLs = payload.OriginURLs
	}
	if payload.VectorSearchEnabled != nil {
		col.VectorSearchEnabled = *payload.VectorSearchEnabled
	}
	if payload.AutoInviteShares != nil {
		col.AutoInviteShares = *payload.AutoInviteShares
	}

	if err := s.repo.UpdateCollection(r.Context(), col); err != nil {
		if s.handleWriteError(w, err) {
			return
		}
		if errors.Is(err, domain.ErrInvalidSlug) {
			s.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if errors.Is(err, repository.ErrSlugTaken) {
			s.jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if oldSlug != col.Slug && s.mcpServer != nil {
		if store := s.mcpServer.GetAppStore(); store != nil {
			var existingHTML string
			if oldSlug != "" {
				existingHTML, _ = store.GetApp(r.Context(), oldSlug)
			}
			if existingHTML == "" {
				existingHTML, _ = store.GetApp(r.Context(), col.ID)
			}
			if existingHTML != "" {
				if col.Slug != "" {
					_ = store.PutApp(r.Context(), col.Slug, existingHTML)
				}
				if oldSlug != "" && oldSlug != col.Slug {
					_ = store.DeleteApp(r.Context(), oldSlug)
				}
			}
		}
	}

	s.jsonResponse(w, col, http.StatusOK)
}

// PUT /api/collections/{collection_id}/app
func (s *Server) handlePublishApp(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	user := currentUser(r)

	col, err := s.repo.GetCollection(r.Context(), colID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "collection not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !s.isTenantAdmin(r.Context(), user, col.TenantID) {
		s.jsonError(w, "only admin of this tenant can publish collection app", http.StatusForbidden)
		return
	}

	if s.billing != nil && s.billing.Enabled() {
		if err := s.billing.CheckWriteAccess(r.Context(), col.TenantID, false); err != nil {
			s.handleWriteError(w, err)
			return
		}
	}

	var payload struct {
		HTML string  `json:"html"`
		Slug *string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := storage.ValidateAppHTML(payload.HTML); err != nil {
		s.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	oldSlug := col.Slug
	slugUpdated := false
	if payload.Slug != nil {
		normSlug, sErr := domain.ValidateSlug(*payload.Slug)
		if sErr != nil {
			s.jsonError(w, sErr.Error(), http.StatusBadRequest)
			return
		}
		if normSlug != col.Slug {
			col.Slug = normSlug
			slugUpdated = true
			if uErr := s.repo.UpdateCollection(r.Context(), col); uErr != nil {
				if s.handleWriteError(w, uErr) {
					return
				}
				if errors.Is(uErr, domain.ErrInvalidSlug) {
					s.jsonError(w, uErr.Error(), http.StatusBadRequest)
					return
				}
				if errors.Is(uErr, repository.ErrSlugTaken) {
					s.jsonError(w, uErr.Error(), http.StatusConflict)
					return
				}
				s.jsonError(w, uErr.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	effectiveSlug := storage.EffectiveSlug(col.ID, col.Slug)
	uuidSlug := storage.CollectionSlug(col.ID)
	if s.mcpServer != nil {
		if store := s.mcpServer.GetAppStore(); store != nil {
			if err := store.PutApp(r.Context(), effectiveSlug, payload.HTML); err != nil {
				s.jsonError(w, "failed publishing app: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if effectiveSlug != uuidSlug {
				_ = store.PutApp(r.Context(), uuidSlug, payload.HTML)
			}
			if slugUpdated && oldSlug != "" && oldSlug != effectiveSlug {
				_ = store.DeleteApp(r.Context(), oldSlug)
			}
		}
	}

	baseURL := s.getBaseURL(r)
	appURL := storage.AppURL(baseURL, effectiveSlug)

	hasOrigin := false
	for _, o := range col.OriginURLs {
		if strings.EqualFold(o, appURL) {
			hasOrigin = true
			break
		}
	}
	if !hasOrigin {
		col.OriginURLs = append(col.OriginURLs, appURL)
		_ = s.repo.UpdateCollection(r.Context(), col)
	}

	s.jsonResponse(w, map[string]interface{}{
		"collection_id": col.ID,
		"slug":          effectiveSlug,
		"app_url":       appURL,
		"origin_urls":   col.OriginURLs,
		"message":       fmt.Sprintf("Application published successfully at %s", appURL),
	}, http.StatusOK)
}

// GET /apps/{slug}
func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	rawSlug := r.PathValue("slug")
	slug := storage.CollectionSlug(rawSlug)
	if slug == "" || s.mcpServer == nil || s.mcpServer.GetAppStore() == nil {
		http.NotFound(w, r)
		return
	}

	store := s.mcpServer.GetAppStore()
	htmlContent, err := store.GetApp(r.Context(), slug)
	if err != nil {
		if col, cErr := s.repo.GetCollection(r.Context(), rawSlug); cErr == nil && col != nil {
			if col.Slug != "" {
				htmlContent, err = store.GetApp(r.Context(), col.Slug)
			}
			if err != nil {
				htmlContent, err = store.GetApp(r.Context(), col.ID)
			}
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=0, s-maxage=5, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(htmlContent))
}

// GET /api/collections/{collection_id}/records
func (s *Server) handleListRecords(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	user := currentUser(r)

	col, err := s.repo.GetCollection(r.Context(), colID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "collection not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.isCollectionTenantAuthorized(r.Context(), user, col) {
		records, err := s.repo.ListRecords(r.Context(), col.ID, user)
		if err != nil {
			s.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(records) == 0 {
			s.jsonError(w, "forbidden: cannot access another tenant's collection", http.StatusForbidden)
			return
		}
		s.jsonResponse(w, records, http.StatusOK)
		return
	}

	records, err := s.repo.ListRecords(r.Context(), col.ID, user)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.jsonResponse(w, records, http.StatusOK)
}

// POST /api/collections/{collection_id}/records
func (s *Server) handleCreateRecord(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	user := currentUser(r)

	col, err := s.repo.GetCollection(r.Context(), colID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "collection not found", http.StatusNotFound)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.isCollectionTenantAuthorized(r.Context(), user, col) {
		s.jsonError(w, "forbidden: cannot write to another tenant's collection", http.StatusForbidden)
		return
	}

	var payload struct {
		Title        string              `json:"title"`
		Content      string              `json:"content"`
		Data         json.RawMessage     `json:"data"`
		Visibility   string              `json:"visibility"`
		Shares       []string            `json:"shares"`
		AccessPolicy domain.AccessPolicy `json:"access_policy"`
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	dataStr := normalizeRecordData(payload.Data)
	policy := payload.AccessPolicy
	if policy.Visibility == "" && payload.Visibility != "" {
		policy.Visibility = payload.Visibility
	}
	if len(policy.Shares) == 0 && len(payload.Shares) > 0 {
		policy.Shares = payload.Shares
	}

	rec := &domain.Record{
		CollectionID: col.ID,
		Title:        payload.Title,
		Content:      payload.Content,
		Data:         dataStr,
		CreatedBy:    user.ID,
		AccessPolicy: policy,
	}

	if err := s.repo.CreateRecord(r.Context(), rec); err != nil {
		if s.handleWriteError(w, err) {
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, rec, http.StatusCreated)
}

// GET /api/collections/{collection_id}/records/{record_id}
func (s *Server) handleGetRecord(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	recID := r.PathValue("record_id")
	user := currentUser(r)

	rec, err := s.repo.GetRecord(r.Context(), recID, user)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "record not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, repository.ErrForbidden) {
			s.jsonError(w, "access denied by ACL / hierarchy policy", http.StatusForbidden)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.matchesCollection(r.Context(), rec.CollectionID, colID) {
		s.jsonError(w, "record not found", http.StatusNotFound)
		return
	}

	s.jsonResponse(w, rec, http.StatusOK)
}

// PUT /api/collections/{collection_id}/records/{record_id}
func (s *Server) handleUpdateRecord(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	recID := r.PathValue("record_id")
	user := currentUser(r)

	var payload struct {
		Title        *string              `json:"title"`
		Content      *string              `json:"content"`
		Data         json.RawMessage      `json:"data"`
		Visibility   *string              `json:"visibility"`
		Shares       *[]string            `json:"shares"`
		AccessPolicy *domain.AccessPolicy `json:"access_policy"`
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	existing, err := s.repo.GetRecord(r.Context(), recID, user)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "record not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, repository.ErrForbidden) {
			s.jsonError(w, "access denied by ACL / hierarchy policy", http.StatusForbidden)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.matchesCollection(r.Context(), existing.CollectionID, colID) {
		s.jsonError(w, "record not found", http.StatusNotFound)
		return
	}

	rec := *existing
	if payload.Title != nil {
		rec.Title = *payload.Title
	}
	if payload.Content != nil {
		rec.Content = *payload.Content
	}
	if len(payload.Data) > 0 && string(payload.Data) != "null" {
		rec.Data = normalizeRecordData(payload.Data)
	}
	if payload.AccessPolicy != nil {
		if payload.AccessPolicy.Visibility != "" {
			rec.AccessPolicy.Visibility = payload.AccessPolicy.Visibility
		}
		if payload.AccessPolicy.Shares != nil {
			rec.AccessPolicy.Shares = payload.AccessPolicy.Shares
		}
	}
	if payload.Visibility != nil && *payload.Visibility != "" {
		rec.AccessPolicy.Visibility = *payload.Visibility
	}
	if payload.Shares != nil {
		rec.AccessPolicy.Shares = *payload.Shares
	}

	if err := s.repo.UpdateRecord(r.Context(), &rec, user); err != nil {
		if s.handleWriteError(w, err) {
			return
		}
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "record not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, repository.ErrForbidden) {
			s.jsonError(w, "access denied by ACL / hierarchy policy", http.StatusForbidden)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	updated, _ := s.repo.GetRecord(r.Context(), recID, user)
	s.jsonResponse(w, updated, http.StatusOK)
}

// DELETE /api/collections/{collection_id}/records/{record_id}
func (s *Server) handleDeleteRecord(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	recID := r.PathValue("record_id")
	user := currentUser(r)

	existing, err := s.repo.GetRecord(r.Context(), recID, user)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "record not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, repository.ErrForbidden) {
			s.jsonError(w, "access denied", http.StatusForbidden)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.matchesCollection(r.Context(), existing.CollectionID, colID) {
		s.jsonError(w, "record not found", http.StatusNotFound)
		return
	}

	if err := s.repo.DeleteRecord(r.Context(), recID, user); err != nil {
		if s.handleWriteError(w, err) {
			return
		}
		if errors.Is(err, repository.ErrNotFound) {
			s.jsonError(w, "record not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, repository.ErrForbidden) {
			s.jsonError(w, "access denied", http.StatusForbidden)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, map[string]string{"message": "record deleted"}, http.StatusOK)
}

// POST /api/collections/{collection_id}/search
func (s *Server) handleSearchRecords(w http.ResponseWriter, r *http.Request) {
	colID := r.PathValue("collection_id")
	user := currentUser(r)

	var payload struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.jsonError(w, "invalid search payload", http.StatusBadRequest)
		return
	}

	results, err := s.searchEngine.Search(r.Context(), colID, payload.Query, user, payload.Limit)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.jsonResponse(w, results, http.StatusOK)
}

func (s *Server) handleWriteError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, billing.ErrPaymentRequired) {
		s.jsonError(w, err.Error(), http.StatusPaymentRequired)
		return true
	}
	if errors.Is(err, billing.ErrCollectionQuotaExceeded) || errors.Is(err, repository.ErrCollectionSizeExceeded) {
		s.jsonError(w, err.Error(), http.StatusForbidden)
		return true
	}
	if errors.Is(err, repository.ErrRecordTooLarge) {
		s.jsonError(w, err.Error(), http.StatusRequestEntityTooLarge)
		return true
	}
	return false
}

func (s *Server) ensureCallerTenant(ctx context.Context, user *domain.User) (string, error) {
	if user == nil {
		return "", errors.New("authentication required")
	}
	if user.TenantID != nil && *user.TenantID != "" {
		return *user.TenantID, nil
	}
	newTenantID := uuid.NewString()
	user.TenantID = &newTenantID
	user.Role = domain.RoleAdmin
	if err := s.repo.UpdateUser(ctx, user); err != nil {
		return "", err
	}
	return newTenantID, nil
}

// GET /api/billing/status
func (s *Server) handleBillingStatus(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil || !s.billing.Enabled() {
		s.jsonResponse(w, &billing.StatusResponse{Enabled: false}, http.StatusOK)
		return
	}
	user := currentUser(r)
	tenantID := ""
	if user != nil && user.TenantID != nil {
		tenantID = *user.TenantID
	}
	status, err := s.billing.GetStatus(r.Context(), tenantID)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.jsonResponse(w, status, http.StatusOK)
}

// POST /api/billing/voucher
func (s *Server) handleRedeemVoucher(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil || !s.billing.Enabled() {
		s.jsonError(w, "billing module is not enabled", http.StatusNotFound)
		return
	}
	user := currentUser(r)
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || strings.TrimSpace(payload.Code) == "" {
		s.jsonError(w, "voucher code is required", http.StatusBadRequest)
		return
	}
	tenantID, err := s.ensureCallerTenant(r.Context(), user)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status, err := s.billing.RedeemVoucher(r.Context(), tenantID, payload.Code)
	if err != nil {
		if errors.Is(err, billing.ErrInvalidVoucher) {
			s.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.rateLimiter != nil {
		s.rateLimiter.InvalidateTenant(tenantID)
	}
	s.jsonResponse(w, status, http.StatusOK)
}

// POST /api/billing/checkout
func (s *Server) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil || !s.billing.Enabled() {
		s.jsonError(w, "billing module is not enabled", http.StatusNotFound)
		return
	}
	user := currentUser(r)
	var payload struct {
		Plan string `json:"plan"`
	}
	_ = json.NewDecoder(r.Body).Decode(&payload)

	tenantID, err := s.ensureCallerTenant(r.Context(), user)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	baseURL := s.getBaseURL(r)
	redirectURI := fmt.Sprintf("%s/api/billing/callback?tenant_id=%s", baseURL, url.QueryEscape(tenantID))
	exitURI := fmt.Sprintf("%s/login", baseURL)

	authURL, err := s.billing.StartCheckout(r.Context(), tenantID, payload.Plan, redirectURI, exitURI)
	if err != nil {
		if errors.Is(err, billing.ErrBillingNotConfigured) {
			s.jsonError(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	s.jsonResponse(w, map[string]interface{}{
		"authorisation_url": authURL,
		"tenant_id":         tenantID,
		"plan":              billing.NormalizePlan(payload.Plan),
	}, http.StatusOK)
}

// GET /api/billing/callback
func (s *Server) handleBillingCallback(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil || !s.billing.Enabled() {
		s.jsonError(w, "billing module is not enabled", http.StatusNotFound)
		return
	}
	tenantID := r.URL.Query().Get("tenant_id")
	billingReqID := r.URL.Query().Get("billing_request")

	status, err := s.billing.CompleteCheckout(r.Context(), tenantID, billingReqID)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.rateLimiter != nil && status != nil {
		s.rateLimiter.InvalidateTenant(status.TenantID)
	}

	if strings.Contains(r.Header.Get("Accept"), "application/json") || r.URL.Query().Get("format") == "json" {
		s.jsonResponse(w, status, http.StatusOK)
		return
	}

	http.Redirect(w, r, "/login?billing=success", http.StatusFound)
}

// POST /api/billing/upgrade
func (s *Server) handleBillingUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil || !s.billing.Enabled() {
		s.jsonError(w, "billing module is not enabled", http.StatusNotFound)
		return
	}
	user := currentUser(r)
	tenantID, err := s.ensureCallerTenant(r.Context(), user)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	baseURL := s.getBaseURL(r)
	redirectURI := fmt.Sprintf("%s/api/billing/callback?tenant_id=%s", baseURL, url.QueryEscape(tenantID))
	exitURI := fmt.Sprintf("%s/login", baseURL)

	res, err := s.billing.UpgradePlan(r.Context(), tenantID, redirectURI, exitURI)
	if err != nil {
		if errors.Is(err, billing.ErrBillingNotConfigured) {
			s.jsonError(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if s.rateLimiter != nil {
		s.rateLimiter.InvalidateTenant(tenantID)
	}

	s.jsonResponse(w, res, http.StatusOK)
}

// POST /api/billing/webhook
func (s *Server) handleBillingWebhook(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil || !s.billing.Enabled() {
		s.jsonError(w, "billing module is not enabled", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		s.jsonError(w, "failed reading webhook body", http.StatusBadRequest)
		return
	}
	sig := r.Header.Get("Webhook-Signature")
	if err := s.billing.HandleWebhook(r.Context(), body, sig); err != nil {
		s.jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const selfHostedLandingHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>{{TITLE}}</title>
  <link rel="preconnect" href="https://fonts.googleapis.com"/>
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin/>
  <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto:wght@100;300;400;500;700&display=swap"/>
  <link rel="stylesheet" href="/style.css"/>
  <link rel="icon" type="image/svg+xml" href="/logo.svg"/>
  <style>
    :root {
        --bg: #fbf9f8;
        --border: rgba(23, 25, 27, 0.09);
        --fg: #17191b;
        --muted: #6b7270;
        --gradient-from: #6366f1;
        --gradient-to: #06b6d4;
        --font-sans: 'Roboto', ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif;
        --font-mono: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace;
    }
    * {
        box-sizing: border-box;
    }
    html, body {
        margin: 0;
        padding: 0;
        background: var(--bg);
        color: var(--fg);
        font-family: var(--font-sans);
        min-height: 100dvh;
        display: flex;
        flex-direction: column;
        -webkit-font-smoothing: antialiased;
    }
    main.os-shell {
        flex: 1;
        display: flex;
        flex-direction: column;
        align-items: safe center;
        justify-content: safe center;
        text-align: center;
        padding: 40px 24px;
    }
    .ascii-logo {
        display: inline-block;
        margin: 0 auto 24px;
        padding: 0;
        font-family: var(--font-mono);
        font-size: clamp(0.42rem, 1.3vw, 0.82rem);
        line-height: 1.15;
        background: linear-gradient(90deg, var(--gradient-from), var(--gradient-to));
        -webkit-background-clip: text;
        background-clip: text;
        color: transparent;
        user-select: none;
        text-align: left;
        border: none;
    }
    .os-title {
        font-size: clamp(1.8rem, 3.8vw, 2.4rem);
        font-weight: 100;
        margin: 0 0 8px;
        letter-spacing: -0.015em;
    }
    .os-subtitle {
        font-family: var(--font-mono);
        font-size: 0.78rem;
        color: var(--muted);
        margin: 0 0 32px;
        letter-spacing: 0.04em;
        text-transform: uppercase;
    }
    .cta-row {
        display: flex;
        gap: 12px;
        justify-content: center;
        align-items: center;
        flex-wrap: wrap;
    }
    .btn {
        display: inline-flex;
        align-items: center;
        gap: 6px;
        padding: 10px 20px;
        border-radius: 6px;
        border: 1px solid var(--border);
        text-decoration: none !important;
        color: var(--fg);
        font-size: 0.9rem;
        background: transparent;
        cursor: pointer;
        transition: border-color 0.15s ease, opacity 0.15s ease;
    }
    .btn:hover {
        border-color: var(--fg);
    }
    .btn-primary {
        background: var(--fg);
        color: var(--bg) !important;
        border-color: var(--fg);
        font-weight: 500;
    }
    .btn-primary:hover {
        opacity: 0.86;
    }
    .os-site-link {
        margin-top: 24px;
        font-family: var(--font-mono);
        font-size: 0.8rem;
        color: var(--muted);
    }
    .os-site-link a {
        color: var(--fg);
        text-decoration: underline;
        text-decoration-color: var(--border);
        text-underline-offset: 3px;
    }
    .os-site-link a:hover {
        text-decoration-color: var(--gradient-from);
    }
  </style>
</head>
<body>
  <main class="os-shell">
    <pre class="ascii-logo" aria-hidden="true">_  _ _ ___  ____ ___  ____ ____ _  _ ____   ____ ____
|  | | |__] |___ |__] |__| |    |_/  |__/   |  | [__ 
 \/  | |__] |___ |__] |  | |___ | \_ |  \   |__| ___]</pre>
    <h1 class="os-title">{{TITLE}}</h1>
    <p class="os-subtitle">{{SUBTITLE}}</p>
    <div class="cta-row">
      <a class="btn btn-primary" href="/login">Sign In</a>
      <a class="btn" href="https://vibebac.kr" target="_blank" rel="noopener">Main VibeBackr Site &nearr;</a>
    </div>
    <p class="os-site-link">Powered by <a href="https://vibebac.kr" target="_blank" rel="noopener">vibebac.kr</a></p>
  </main>
  <script>
    (function initAsciiGlitch() {
        if(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
            return;
        }
        const logo = document.querySelector('.ascii-logo');
        if(!logo) {
            return;
        }
        const rawLines = logo.textContent.split('\n');
        const maxLen = Math.max(...rawLines.map(l => l.length));
        const baseLines = rawLines.map(l => l.padEnd(maxLen, ' '));
        const baseText = baseLines.join('\n');
        logo.textContent = baseText;
        const GLITCH_CHARS = '_|/\\[]=-~:;+*#░▒▓';
        function renderCorruptedFrame(intensity) {
            const nextLines = baseLines.map(line => line.split(''));
            const sliceWidth = 8 + Math.floor(Math.random() * 10 * intensity);
            const sliceStart = Math.floor(Math.random() * Math.max(1, maxLen - sliceWidth));
            const sliceEnd = Math.min(maxLen, sliceStart + sliceWidth);
            baseLines.forEach((line, r) => {
                for(let c = sliceStart; c < sliceEnd; c++) {
                    const orig = nextLines[r][c];
                    const isInk = orig !== ' ';
                    if(Math.random() < (isInk ? 0.5 * intensity : 0.12 * intensity)) {
                        nextLines[r][c] = GLITCH_CHARS[Math.floor(Math.random() * GLITCH_CHARS.length)];
                    }
                }
            });
            logo.textContent = nextLines.map(chars => chars.join('')).join('\n');
        }
        function triggerBurst(intensity) {
            const frames = 3 + Math.floor(Math.random() * 3);
            let step = 0;
            (function tick() {
                if(step < frames) {
                    renderCorruptedFrame(intensity || 1);
                    step++;
                    setTimeout(tick, 50);
                } else {
                    logo.textContent = baseText;
                }
            })();
        }
        setTimeout(triggerBurst.bind(null, 0.85), 800);
        logo.addEventListener('mouseenter', triggerBurst.bind(null, 1.1));
    })();
  </script>
</body>
</html>
`

func (s *Server) resolveInstanceName() string {
	if s.instanceName != "" {
		return s.instanceName
	}
	for _, key := range []string{"INSTANCE_NAME", "COMPANY_NAME", "ORG_NAME", "VIBEBACKR_INSTANCE_NAME"} {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val
		}
	}
	return ""
}

// GET /{$} and GET /index.html (Self-hosted VibeBackr OS landing page)
func (s *Server) handleSelfHostedRoot(w http.ResponseWriter, r *http.Request) {
	title := "VibeBackr OS"
	subtitle := "Self-Hosted Open Source Instance"
	if customName := s.resolveInstanceName(); customName != "" {
		title = html.EscapeString(customName)
		subtitle = "Powered by VibeBackr OS"
	}

	page := strings.NewReplacer(
		"{{TITLE}}", title,
		"{{SUBTITLE}}", subtitle,
	).Replace(selfHostedLandingHTML)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}

// GET /logo.svg, GET /favicon.svg, and GET /favicon.ico
func (s *Server) handleLogoSVG(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(mcp.LogoSVG))
}
