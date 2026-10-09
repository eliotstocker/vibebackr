package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eliotstocker/vibebackr/internal/api"
	"github.com/eliotstocker/vibebackr/internal/auth"
	"github.com/eliotstocker/vibebackr/internal/billing"
	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/mcp"
	"github.com/eliotstocker/vibebackr/internal/ratelimit"
	"github.com/eliotstocker/vibebackr/internal/repository"
	"github.com/eliotstocker/vibebackr/internal/search"
)

func setupTestServer(t *testing.T) (http.Handler, repository.Repository, *auth.TokenManager, *domain.User) {
	t.Helper()
	t.Setenv("ENABLE_MOCK_AUTH", "true")
	database, err := db.Connect("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db error: %v", err)
	}

	repo := repository.New(database)
	tm := auth.NewTokenManager("test-secret", 15*time.Minute)
	sm := auth.NewSSOManager(repo)
	se := search.NewSearchEngine(database, repo, nil)
	mcpServer := mcp.NewServer(repo, se)

	server := api.NewServer(repo, tm, sm, se, mcpServer)

	user := &domain.User{
		ID:    "user_123",
		Email: "tester@example.com",
		Name:  "Test User",
		Role:  domain.RoleMember,
	}
	if err := repo.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("failed creating user: %v", err)
	}

	return server.Routes(), repo, tm, user
}

func TestDCRAndOAuthFlow(t *testing.T) {
	handler, repo, _, user := setupTestServer(t)

	// 1. Dynamic Client Registration (RFC 7591)
	regPayload := `{"client_name": "Autonomous Agent Alpha", "redirect_uris": ["https://agent.local/cb"]}`
	req := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(regPayload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("DCR failed with status %d: %s", w.Code, w.Body.String())
	}

	var client domain.OAuthClient
	if err := json.Unmarshal(w.Body.Bytes(), &client); err != nil {
		t.Fatalf("failed decoding client: %v", err)
	}

	// 2. Token exchange via client_credentials
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", client.ID)
	form.Set("client_secret", client.ClientSecret)

	tokenReq := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wToken := httptest.NewRecorder()
	handler.ServeHTTP(wToken, tokenReq)

	if wToken.Code != http.StatusOK {
		t.Fatalf("token exchange failed with status %d: %s", wToken.Code, wToken.Body.String())
	}

	var tokenRes map[string]interface{}
	_ = json.Unmarshal(wToken.Body.Bytes(), &tokenRes)
	if tokenRes["access_token"] == nil {
		t.Fatalf("missing access_token in response: %v", tokenRes)
	}

	// 3. PKCE Authorization Code flow test
	codeVerifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authCode := &domain.AuthCode{
		Code:                "test_code_999",
		ClientID:            client.ID,
		UserID:              user.ID,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: "S256",
		ExpiresAt:           time.Now().Add(5 * time.Minute),
	}
	_ = repo.CreateAuthCode(context.Background(), authCode)

	pkceForm := url.Values{}
	pkceForm.Set("grant_type", "authorization_code")
	pkceForm.Set("client_id", client.ID)
	pkceForm.Set("code", "test_code_999")
	pkceForm.Set("code_verifier", codeVerifier)

	pkceReq := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(pkceForm.Encode()))
	pkceReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wPKCE := httptest.NewRecorder()
	handler.ServeHTTP(wPKCE, pkceReq)

	if wPKCE.Code != http.StatusOK {
		t.Fatalf("PKCE token exchange failed with status %d: %s", wPKCE.Code, wPKCE.Body.String())
	}
}

func TestOAuthAuthorizeFlow(t *testing.T) {
	handler, repo, tm, user := setupTestServer(t)
	ctx := context.Background()

	// Register client
	client := &domain.OAuthClient{
		ID:           "test_client_claude",
		ClientName:   "Claude Assistant",
		RedirectURIs: domain.JSONStrings{"https://claude.ai/api/mcp/oauth/callback"},
		GrantTypes:   domain.JSONStrings{"authorization_code"},
		CreatedAt:    time.Now().UTC(),
	}
	if err := repo.CreateOAuthClient(ctx, client); err != nil {
		t.Fatalf("failed creating client: %v", err)
	}

	// 1. GET /oauth/client-info
	reqInfo := httptest.NewRequest("GET", "/oauth/client-info?client_id=test_client_claude", nil)
	wInfo := httptest.NewRecorder()
	handler.ServeHTTP(wInfo, reqInfo)
	if wInfo.Code != http.StatusOK {
		t.Fatalf("expected 200 for client-info, got %d", wInfo.Code)
	}
	var infoRes map[string]interface{}
	_ = json.Unmarshal(wInfo.Body.Bytes(), &infoRes)
	if infoRes["client_name"] != "Claude Assistant" {
		t.Fatalf("unexpected client_name: %v", infoRes["client_name"])
	}

	// 2. GET /oauth/authorize invalid redirect_uri
	reqInvalid := httptest.NewRequest("GET", "/oauth/authorize?client_id=test_client_claude&redirect_uri=https://evil.com/cb", nil)
	wInvalid := httptest.NewRecorder()
	handler.ServeHTTP(wInvalid, reqInvalid)
	if wInvalid.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unauthorized redirect_uri, got %d", wInvalid.Code)
	}

	// 3. GET /oauth/authorize valid -> redirects to /login with query string intact
	codeVerifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authParams := url.Values{}
	authParams.Set("client_id", "test_client_claude")
	authParams.Set("redirect_uri", "https://claude.ai/api/mcp/oauth/callback")
	authParams.Set("response_type", "code")
	authParams.Set("code_challenge", codeChallenge)
	authParams.Set("code_challenge_method", "S256")
	authParams.Set("state", "mystate123")

	reqAuth := httptest.NewRequest("GET", "/oauth/authorize?"+authParams.Encode(), nil)
	wAuth := httptest.NewRecorder()
	handler.ServeHTTP(wAuth, reqAuth)
	if wAuth.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect to /login, got %d", wAuth.Code)
	}
	loc := wAuth.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?") || !strings.Contains(loc, "client_id=test_client_claude") {
		t.Fatalf("unexpected redirect location: %s", loc)
	}

	// 4. POST /oauth/authorize consent submission without token -> 401
	submitPayload := map[string]string{
		"client_id":             "test_client_claude",
		"redirect_uri":          "https://claude.ai/api/mcp/oauth/callback",
		"code_challenge":        codeChallenge,
		"code_challenge_method": "S256",
		"state":                 "mystate123",
	}
	payloadBytes, _ := json.Marshal(submitPayload)
	reqNoToken := httptest.NewRequest("POST", "/oauth/authorize", bytes.NewReader(payloadBytes))
	reqNoToken.Header.Set("Content-Type", "application/json")
	wNoToken := httptest.NewRecorder()
	handler.ServeHTTP(wNoToken, reqNoToken)
	if wNoToken.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", wNoToken.Code)
	}

	// 5. POST /oauth/authorize consent submission with user token -> returns redirect_uri with auth code
	userToken, _ := tm.GenerateToken(user)
	reqConsent := httptest.NewRequest("POST", "/oauth/authorize", bytes.NewReader(payloadBytes))
	reqConsent.Header.Set("Content-Type", "application/json")
	reqConsent.Header.Set("Authorization", "Bearer "+userToken)
	wConsent := httptest.NewRecorder()
	handler.ServeHTTP(wConsent, reqConsent)
	if wConsent.Code != http.StatusOK {
		t.Fatalf("expected 200 for JSON consent response, got %d: %s", wConsent.Code, wConsent.Body.String())
	}

	var consentRes map[string]string
	_ = json.Unmarshal(wConsent.Body.Bytes(), &consentRes)
	targetURI := consentRes["redirect_uri"]
	if !strings.HasPrefix(targetURI, "https://claude.ai/api/mcp/oauth/callback?code=") || !strings.Contains(targetURI, "state=mystate123") {
		t.Fatalf("unexpected target redirect_uri: %s", targetURI)
	}
	authCodeVal := consentRes["code"]
	if authCodeVal == "" {
		t.Fatalf("missing auth code in consent response")
	}

	// 6. Token exchange via POST /oauth/token using returned auth code and code_verifier
	tokenForm := url.Values{}
	tokenForm.Set("grant_type", "authorization_code")
	tokenForm.Set("client_id", "test_client_claude")
	tokenForm.Set("code", authCodeVal)
	tokenForm.Set("code_verifier", codeVerifier)

	reqExchange := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(tokenForm.Encode()))
	reqExchange.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wExchange := httptest.NewRecorder()
	handler.ServeHTTP(wExchange, reqExchange)
	if wExchange.Code != http.StatusOK {
		t.Fatalf("expected 200 for token exchange, got %d: %s", wExchange.Code, wExchange.Body.String())
	}

	var tokenData map[string]interface{}
	_ = json.Unmarshal(wExchange.Body.Bytes(), &tokenData)
	accessToken, ok := tokenData["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("missing access_token in exchange response: %v", tokenData)
	}
	refreshToken, ok := tokenData["refresh_token"].(string)
	if !ok || refreshToken == "" {
		t.Fatalf("missing refresh_token in exchange response: %v", tokenData)
	}

	// Verify token belongs to user
	claims, err := tm.ValidateToken(accessToken)
	if err != nil {
		t.Fatalf("failed validating exchanged token: %v", err)
	}
	if claims.UserID != user.ID {
		t.Fatalf("expected token user ID %s, got %s", user.ID, claims.UserID)
	}

	// 7. Refresh token exchange via POST /oauth/token grant_type=refresh_token
	refreshForm := url.Values{}
	refreshForm.Set("grant_type", "refresh_token")
	refreshForm.Set("refresh_token", refreshToken)
	refreshForm.Set("client_id", "test_client_claude")

	reqRefresh := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(refreshForm.Encode()))
	reqRefresh.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wRefresh := httptest.NewRecorder()
	handler.ServeHTTP(wRefresh, reqRefresh)
	if wRefresh.Code != http.StatusOK {
		t.Fatalf("expected 200 for refresh exchange, got %d: %s", wRefresh.Code, wRefresh.Body.String())
	}

	var refreshData map[string]interface{}
	_ = json.Unmarshal(wRefresh.Body.Bytes(), &refreshData)
	newAccessToken, _ := refreshData["access_token"].(string)
	newRefreshToken, _ := refreshData["refresh_token"].(string)
	if newAccessToken == "" || newRefreshToken == "" {
		t.Fatalf("expected new access_token and refresh_token, got: %v", refreshData)
	}
	if newRefreshToken != refreshToken {
		t.Fatalf("expected refresh token to remain stable across refreshes for multi-instance clients, got %s vs %s", newRefreshToken, refreshToken)
	}

	// 8. Concurrent/second mcp-remote instance refreshing with the same refresh token must succeed (200 OK)
	reqSecondInstance := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(refreshForm.Encode()))
	reqSecondInstance.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wSecondInstance := httptest.NewRecorder()
	handler.ServeHTTP(wSecondInstance, reqSecondInstance)
	if wSecondInstance.Code != http.StatusOK {
		t.Fatalf("expected 200 OK when second mcp-remote instance refreshes with shared token, got %d", wSecondInstance.Code)
	}

	// 9. Logout revokes the active refresh token
	logoutForm := url.Values{}
	logoutForm.Set("refresh_token", newRefreshToken)
	reqLogout := httptest.NewRequest("POST", "/api/auth/logout", strings.NewReader(logoutForm.Encode()))
	reqLogout.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wLogout := httptest.NewRecorder()
	handler.ServeHTTP(wLogout, reqLogout)
	if wLogout.Code != http.StatusOK {
		t.Fatalf("expected 200 for logout, got %d", wLogout.Code)
	}

	// Attempting to refresh with revoked token must fail with 401
	refreshRevokedForm := url.Values{}
	refreshRevokedForm.Set("grant_type", "refresh_token")
	refreshRevokedForm.Set("refresh_token", newRefreshToken)
	refreshRevokedForm.Set("client_id", "test_client_claude")
	reqRevoked := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(refreshRevokedForm.Encode()))
	reqRevoked.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wRevoked := httptest.NewRecorder()
	handler.ServeHTTP(wRevoked, reqRevoked)
	if wRevoked.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when using revoked refresh token, got %d", wRevoked.Code)
	}
}

func TestRecordsAPIAndSearch(t *testing.T) {
	handler, repo, tm, user := setupTestServer(t)
	ctx := context.Background()

	token, _ := tm.GenerateToken(user)

	col := &domain.Collection{
		ID:                "col_records",
		Name:              "Project Specs",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	_ = repo.CreateCollection(ctx, col)

	// Create Record
	payload := map[string]interface{}{
		"title":   "Agent Architecture Guide",
		"content": "This document outlines how autonomous agents use VibeBackr.",
		"data":    map[string]interface{}{"version": "1.0"},
		"access_policy": map[string]interface{}{
			"visibility": "private",
		},
	}
	body, _ := json.Marshal(payload)
	createReq := httptest.NewRequest("POST", "/api/collections/col_records/records", bytes.NewReader(body))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("Content-Type", "application/json")
	wCreate := httptest.NewRecorder()
	handler.ServeHTTP(wCreate, createReq)

	if wCreate.Code != http.StatusCreated {
		t.Fatalf("failed creating record: %d - %s", wCreate.Code, wCreate.Body.String())
	}

	var createdRec domain.Record
	_ = json.Unmarshal(wCreate.Body.Bytes(), &createdRec)

	// Search Records (SQL fallback)
	searchBody := `{"query": "Architecture", "limit": 10}`
	searchReq := httptest.NewRequest("POST", "/api/collections/col_records/search", strings.NewReader(searchBody))
	searchReq.Header.Set("Authorization", "Bearer "+token)
	searchReq.Header.Set("Content-Type", "application/json")
	wSearch := httptest.NewRecorder()
	handler.ServeHTTP(wSearch, searchReq)

	if wSearch.Code != http.StatusOK {
		t.Fatalf("search failed with code %d: %s", wSearch.Code, wSearch.Body.String())
	}

	var results []search.SearchResult
	_ = json.Unmarshal(wSearch.Body.Bytes(), &results)
	if len(results) == 0 {
		t.Fatalf("expected search results, got 0")
	}
	if results[0].Record.ID != createdRec.ID {
		t.Errorf("expected found record ID %s, got %s", createdRec.ID, results[0].Record.ID)
	}
}

func TestRecordCRUDAndUserProfile(t *testing.T) {
	handler, repo, tm, user := setupTestServer(t)
	ctx := context.Background()
	token, _ := tm.GenerateToken(user)

	col := &domain.Collection{
		ID:                "col_crud",
		Name:              "CRUD Collection",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	_ = repo.CreateCollection(ctx, col)

	// 1. GET User
	reqUser := httptest.NewRequest("GET", "/api/users/"+user.ID, nil)
	reqUser.Header.Set("Authorization", "Bearer "+token)
	wUser := httptest.NewRecorder()
	handler.ServeHTTP(wUser, reqUser)
	if wUser.Code != http.StatusOK {
		t.Fatalf("expected 200 for user profile, got %d", wUser.Code)
	}

	// 2. Create Record
	rec := &domain.Record{
		ID:           "rec_crud_1",
		CollectionID: col.ID,
		Title:        "Original Title",
		Content:      "Original Content",
		CreatedBy:    user.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
	}
	_ = repo.CreateRecord(ctx, rec)

	// 3. GET Record
	reqGet := httptest.NewRequest("GET", "/api/collections/"+col.ID+"/records/"+rec.ID, nil)
	reqGet.Header.Set("Authorization", "Bearer "+token)
	wGet := httptest.NewRecorder()
	handler.ServeHTTP(wGet, reqGet)
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 for get record, got %d", wGet.Code)
	}

	// 4. LIST Records
	reqList := httptest.NewRequest("GET", "/api/collections/"+col.ID+"/records", nil)
	reqList.Header.Set("Authorization", "Bearer "+token)
	wList := httptest.NewRecorder()
	handler.ServeHTTP(wList, reqList)
	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200 for list records, got %d", wList.Code)
	}

	// 5. PUT Record
	updatePayload := `{"title": "Updated Title", "content": "Updated Content", "access_policy": {"visibility": "private"}}`
	reqPut := httptest.NewRequest("PUT", "/api/collections/"+col.ID+"/records/"+rec.ID, strings.NewReader(updatePayload))
	reqPut.Header.Set("Authorization", "Bearer "+token)
	reqPut.Header.Set("Content-Type", "application/json")
	wPut := httptest.NewRecorder()
	handler.ServeHTTP(wPut, reqPut)
	if wPut.Code != http.StatusOK {
		t.Fatalf("expected 200 for update record, got %d", wPut.Code)
	}

	// 6. DELETE Record
	reqDel := httptest.NewRequest("DELETE", "/api/collections/"+col.ID+"/records/"+rec.ID, nil)
	reqDel.Header.Set("Authorization", "Bearer "+token)
	wDel := httptest.NewRecorder()
	handler.ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("expected 200 for delete record, got %d", wDel.Code)
	}
}

func TestSSOEndpointsAndMCPHTTP(t *testing.T) {
	handler, repo, tm, user := setupTestServer(t)
	token, _ := tm.GenerateToken(user)

	// Allow domain for mock user (developer@example.com)
	_ = repo.CreateCollection(context.Background(), &domain.Collection{
		ID:             "col_mock",
		Name:           "Mock Docs",
		AllowedDomains: domain.JSONStrings{"example.com"},
	})

	// 1. GET /auth/login/mock (Redirect)
	reqLogin := httptest.NewRequest("GET", "/auth/login/mock", nil)
	wLogin := httptest.NewRecorder()
	handler.ServeHTTP(wLogin, reqLogin)
	if wLogin.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected redirect for sso login, got %d", wLogin.Code)
	}

	// 2. GET /auth/callback/mock
	reqCallback := httptest.NewRequest("GET", "/auth/callback/mock?code=mock_test_code", nil)
	wCallback := httptest.NewRecorder()
	handler.ServeHTTP(wCallback, reqCallback)
	if wCallback.Code != http.StatusOK {
		t.Fatalf("expected 200 for sso callback, got %d - %s", wCallback.Code, wCallback.Body.String())
	}

	// 3. POST /api/auth/logout
	reqLogout := httptest.NewRequest("POST", "/api/auth/logout", nil)
	wLogout := httptest.NewRecorder()
	handler.ServeHTTP(wLogout, reqLogout)
	if wLogout.Code != http.StatusOK {
		t.Fatalf("expected 200 for logout, got %d", wLogout.Code)
	}

	// 4. POST /mcp (JSON-RPC tool call over HTTP)
	mcpPayload := `{"jsonrpc": "2.0", "id": 100, "method": "tools/list"}`
	reqMCP := httptest.NewRequest("POST", "/mcp", strings.NewReader(mcpPayload))
	reqMCP.Header.Set("Authorization", "Bearer "+token)
	reqMCP.Header.Set("Content-Type", "application/json")
	wMCP := httptest.NewRecorder()
	handler.ServeHTTP(wMCP, reqMCP)
	if wMCP.Code != http.StatusOK {
		t.Fatalf("expected 200 for MCP over HTTP, got %d", wMCP.Code)
	}
	if wMCP.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("expected Mcp-Session-Id header in POST response")
	}

	// 5. GET /mcp with query token (?token=...)
	sseCtx, cancelSSE := context.WithCancel(context.Background())
	defer cancelSSE()
	reqMCPSSE := httptest.NewRequest("GET", "/mcp?token="+token, nil).WithContext(sseCtx)
	wMCPSSE := httptest.NewRecorder()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelSSE()
	}()
	handler.ServeHTTP(wMCPSSE, reqMCPSSE)
	if wMCPSSE.Code != http.StatusOK {
		t.Fatalf("expected 200 for MCP SSE with query token, got %d", wMCPSSE.Code)
	}
	if !strings.Contains(wMCPSSE.Body.String(), "event: endpoint") {
		t.Fatalf("expected SSE endpoint event, got %s", wMCPSSE.Body.String())
	}
}

func TestAuthMiddlewareErrors(t *testing.T) {
	handler, _, _, _ := setupTestServer(t)

	// Missing token -> 401
	reqNoAuth := httptest.NewRequest("GET", "/api/users/user_123", nil)
	wNoAuth := httptest.NewRecorder()
	handler.ServeHTTP(wNoAuth, reqNoAuth)
	if wNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth header, got %d", wNoAuth.Code)
	}

	// Invalid token -> 401
	reqBadAuth := httptest.NewRequest("GET", "/api/users/user_123", nil)
	reqBadAuth.Header.Set("Authorization", "Bearer invalid-jwt-token")
	wBadAuth := httptest.NewRecorder()
	handler.ServeHTTP(wBadAuth, reqBadAuth)
	if wBadAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with invalid token, got %d", wBadAuth.Code)
	}
}

func TestClientGuideAndCollectionsAPI(t *testing.T) {
	handler, repo, tm, _ := setupTestServer(t)
	ctx := context.Background()

	admin := &domain.User{ID: "admin_test", Email: "admin@test.com", Role: domain.RoleAdmin}
	_ = repo.CreateUser(ctx, admin)
	token, _ := tm.GenerateToken(admin)

	// Create a collection
	col := &domain.Collection{ID: "test_col_guide", Name: "Guide Collection"}
	_ = repo.CreateCollection(ctx, col)

	// 1. GET /api/client-guide (unauthenticated, markdown format)
	reqGuideMD := httptest.NewRequest("GET", "/api/client-guide?topic=all", nil)
	wGuideMD := httptest.NewRecorder()
	handler.ServeHTTP(wGuideMD, reqGuideMD)
	if wGuideMD.Code != http.StatusOK {
		t.Fatalf("expected 200 for client guide, got %d", wGuideMD.Code)
	}
	if !strings.Contains(wGuideMD.Body.String(), "VibeBackr Frontend & UI Integration Guide") {
		t.Fatalf("expected guide content, got %s", wGuideMD.Body.String())
	}

	// 2. GET /api/client-guide?format=json&collection_id=test_col_guide
	reqGuideJSON := httptest.NewRequest("GET", "/api/client-guide?format=json&collection_id=test_col_guide", nil)
	wGuideJSON := httptest.NewRecorder()
	handler.ServeHTTP(wGuideJSON, reqGuideJSON)
	if wGuideJSON.Code != http.StatusOK {
		t.Fatalf("expected 200 for JSON client guide, got %d", wGuideJSON.Code)
	}
	var jsonResp map[string]interface{}
	if err := json.Unmarshal(wGuideJSON.Body.Bytes(), &jsonResp); err != nil || jsonResp["guide"] == "" {
		t.Fatalf("expected valid JSON guide response, got %v", wGuideJSON.Body.String())
	}

	// 2b. GET /api/client-guide with custom Host header
	reqCustomHost := httptest.NewRequest("GET", "https://another-endpoint.org/api/client-guide", nil)
	wCustomHost := httptest.NewRecorder()
	handler.ServeHTTP(wCustomHost, reqCustomHost)
	if wCustomHost.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wCustomHost.Code)
	}
	if !strings.Contains(wCustomHost.Body.String(), "https://another-endpoint.org") {
		t.Fatalf("expected custom host in guide, got: %s", wCustomHost.Body.String())
	}

	// 3. GET /api/collections (authenticated)
	reqCols := httptest.NewRequest("GET", "/api/collections", nil)
	reqCols.Header.Set("Authorization", "Bearer "+token)
	wCols := httptest.NewRecorder()
	handler.ServeHTTP(wCols, reqCols)
	if wCols.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET /api/collections, got %d", wCols.Code)
	}
	var cols []domain.Collection
	if err := json.Unmarshal(wCols.Body.Bytes(), &cols); err != nil || len(cols) == 0 {
		t.Fatalf("expected collections array, got %v", wCols.Body.String())
	}

	// 4. PUT /api/collections/{collection_id} (authenticated as admin)
	putPayload := `{"name": "Guide Collection Renamed", "origin_urls": ["http://localhost:3000"]}`
	reqPutCol := httptest.NewRequest("PUT", "/api/collections/test_col_guide", strings.NewReader(putPayload))
	reqPutCol.Header.Set("Authorization", "Bearer "+token)
	reqPutCol.Header.Set("Content-Type", "application/json")
	wPutCol := httptest.NewRecorder()
	handler.ServeHTTP(wPutCol, reqPutCol)
	if wPutCol.Code != http.StatusOK {
		t.Fatalf("expected 200 for PUT collection, got %d (body: %s)", wPutCol.Code, wPutCol.Body.String())
	}
	var updatedCol domain.Collection
	if err := json.Unmarshal(wPutCol.Body.Bytes(), &updatedCol); err != nil || updatedCol.Name != "Guide Collection Renamed" || len(updatedCol.OriginURLs) != 1 {
		t.Fatalf("unexpected updated collection: %+v", updatedCol)
	}
}

func TestAuthRedirectAndCORSWithOrigin(t *testing.T) {
	handler, repo, _, _ := setupTestServer(t)
	ctx := context.Background()

	// 1. Create collection with OriginURLs
	col := &domain.Collection{
		ID:         "col_with_origin",
		Name:       "Frontend App",
		OriginURLs: domain.JSONStrings{"http://localhost:5173"},
	}
	if err := repo.CreateCollection(ctx, col); err != nil {
		t.Fatalf("failed creating collection: %v", err)
	}

	retrieved, err := repo.GetCollection(ctx, "col_with_origin")
	if err != nil || len(retrieved.OriginURLs) != 1 || retrieved.OriginURLs[0] != "http://localhost:5173" {
		t.Fatalf("expected origin url preserved on collection: %+v", retrieved)
	}

	// 2. CORS reflection and credentials
	reqCORS := httptest.NewRequest("OPTIONS", "/api/client-guide", nil)
	reqCORS.Header.Set("Origin", "http://localhost:5173")
	wCORS := httptest.NewRecorder()
	handler.ServeHTTP(wCORS, reqCORS)
	if wCORS.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatalf("expected reflected origin, got %s", wCORS.Header().Get("Access-Control-Allow-Origin"))
	}
	if wCORS.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("expected credentials true for origin, got %s", wCORS.Header().Get("Access-Control-Allow-Credentials"))
	}

	// 2b. Null origin from local file:// URLs
	reqNullCORS := httptest.NewRequest("OPTIONS", "/api/client-guide", nil)
	reqNullCORS.Header.Set("Origin", "null")
	wNullCORS := httptest.NewRecorder()
	handler.ServeHTTP(wNullCORS, reqNullCORS)
	if wNullCORS.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected wildcard origin for null origin, got %s", wNullCORS.Header().Get("Access-Control-Allow-Origin"))
	}
	if wNullCORS.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("expected no credentials header for null origin, got %s", wNullCORS.Header().Get("Access-Control-Allow-Credentials"))
	}

	// 3. SSO Login with redirect_uri
	reqLogin := httptest.NewRequest("GET", "/auth/login/mock?redirect_uri=http://localhost:5173/auth/callback", nil)
	wLogin := httptest.NewRecorder()
	handler.ServeHTTP(wLogin, reqLogin)
	if wLogin.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 for login redirect, got %d", wLogin.Code)
	}
	loc := wLogin.Header().Get("Location")
	unescapedLoc, _ := url.QueryUnescape(loc)
	if !strings.Contains(unescapedLoc, "http://localhost:5173/auth/callback") {
		t.Fatalf("expected encoded redirect_uri in location, got %s", loc)
	}

	// 4. SSO Callback following the redirect from step 3 (verifies state carrying redirect_uri)
	reqCallback := httptest.NewRequest("GET", loc, nil)
	wCallback := httptest.NewRecorder()
	handler.ServeHTTP(wCallback, reqCallback)
	if wCallback.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect for callback, got %d", wCallback.Code)
	}
	cbLoc := wCallback.Header().Get("Location")
	if !strings.HasPrefix(cbLoc, "http://localhost:5173/auth/callback#access_token=") {
		t.Fatalf("expected redirect to frontend with token in hash, got %s", cbLoc)
	}

	// 4b. SSO Login with popup=true (verifies HTML postMessage response for local apps)
	reqPopupLogin := httptest.NewRequest("GET", "/auth/login/mock?popup=true", nil)
	wPopupLogin := httptest.NewRecorder()
	handler.ServeHTTP(wPopupLogin, reqPopupLogin)
	popupLoc := wPopupLogin.Header().Get("Location")
	reqPopupCB := httptest.NewRequest("GET", popupLoc, nil)
	wPopupCB := httptest.NewRecorder()
	handler.ServeHTTP(wPopupCB, reqPopupCB)
	if wPopupCB.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for popup callback, got %d", wPopupCB.Code)
	}
	if !strings.Contains(wPopupCB.Body.String(), "postMessage") {
		t.Fatalf("expected postMessage in popup callback body, got %s", wPopupCB.Body.String())
	}

	// 4c. SSO Login with file:// redirect_uri
	reqFileLogin := httptest.NewRequest("GET", "/auth/login/mock?redirect_uri=file:///Users/dev/app/index.html", nil)
	wFileLogin := httptest.NewRecorder()
	handler.ServeHTTP(wFileLogin, reqFileLogin)
	fileLoc := wFileLogin.Header().Get("Location")
	reqFileCB := httptest.NewRequest("GET", fileLoc, nil)
	wFileCB := httptest.NewRecorder()
	handler.ServeHTTP(wFileCB, reqFileCB)
	if wFileCB.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for file:// callback, got %d", wFileCB.Code)
	}
	if !strings.Contains(wFileCB.Body.String(), "Return to Local Application") {
		t.Fatalf("expected Return to Local Application in file:// callback body, got %s", wFileCB.Body.String())
	}
}

func TestMultiTenancyAndCollectionIsolation(t *testing.T) {
	handler, repo, tm, _ := setupTestServer(t)
	sm := auth.NewSSOManager(repo)
	ctx := context.Background()

	// 1. Tenant A owner explicitly signs up
	userA, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{
		Email: "ownerA@alpha.com",
		Name:  "Alpha Owner",
		Sub:   "sub-alpha-1",
	}, true)
	if err != nil {
		t.Fatalf("failed provisioning tenant A: %v", err)
	}
	if userA.TenantID == nil || *userA.TenantID == "" {
		t.Fatalf("expected userA to have a TenantID")
	}
	tokenA, _ := tm.GenerateToken(userA)

	// 2. Tenant B owner explicitly signs up
	userB, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{
		Email: "ownerB@beta.com",
		Name:  "Beta Owner",
		Sub:   "sub-beta-2",
	}, true)
	if err != nil {
		t.Fatalf("failed provisioning tenant B: %v", err)
	}
	if userB.TenantID == nil || *userB.TenantID == "" {
		t.Fatalf("expected userB to have a TenantID")
	}
	if *userA.TenantID == *userB.TenantID {
		t.Fatalf("expected different TenantIDs for userA and userB")
	}
	tokenB, _ := tm.GenerateToken(userB)

	// 3. Tenant A creates collection via POST /api/collections
	payloadA := `{"name": "Alpha Collection", "default_visibility": "private"}`
	reqCreateA := httptest.NewRequest("POST", "/api/collections", strings.NewReader(payloadA))
	reqCreateA.Header.Set("Authorization", "Bearer "+tokenA)
	reqCreateA.Header.Set("Content-Type", "application/json")
	wCreateA := httptest.NewRecorder()
	handler.ServeHTTP(wCreateA, reqCreateA)
	if wCreateA.Code != http.StatusCreated {
		t.Fatalf("expected 201 for Tenant A creating collection, got %d (body: %s)", wCreateA.Code, wCreateA.Body.String())
	}
	var colA domain.Collection
	_ = json.Unmarshal(wCreateA.Body.Bytes(), &colA)
	if colA.TenantID != *userA.TenantID {
		t.Fatalf("expected colA to have tenantID %s, got %s", *userA.TenantID, colA.TenantID)
	}

	// 4. Tenant B creates collection via POST /api/collections
	payloadB := `{"name": "Beta Collection", "default_visibility": "private"}`
	reqCreateB := httptest.NewRequest("POST", "/api/collections", strings.NewReader(payloadB))
	reqCreateB.Header.Set("Authorization", "Bearer "+tokenB)
	reqCreateB.Header.Set("Content-Type", "application/json")
	wCreateB := httptest.NewRecorder()
	handler.ServeHTTP(wCreateB, reqCreateB)
	if wCreateB.Code != http.StatusCreated {
		t.Fatalf("expected 201 for Tenant B creating collection, got %d (body: %s)", wCreateB.Code, wCreateB.Body.String())
	}
	var colB domain.Collection
	_ = json.Unmarshal(wCreateB.Body.Bytes(), &colB)
	if colB.TenantID != *userB.TenantID {
		t.Fatalf("expected colB to have tenantID %s, got %s", *userB.TenantID, colB.TenantID)
	}

	// 5. Tenant A lists collections: should only see Alpha Collection
	reqListA := httptest.NewRequest("GET", "/api/collections", nil)
	reqListA.Header.Set("Authorization", "Bearer "+tokenA)
	wListA := httptest.NewRecorder()
	handler.ServeHTTP(wListA, reqListA)
	if wListA.Code != http.StatusOK {
		t.Fatalf("expected 200 for listing collections, got %d", wListA.Code)
	}
	var listA []domain.Collection
	_ = json.Unmarshal(wListA.Body.Bytes(), &listA)
	if len(listA) != 1 || listA[0].ID != colA.ID {
		t.Fatalf("expected Tenant A to only see colA, got %+v", listA)
	}

	// 6. Tenant B lists collections: should only see Beta Collection
	reqListB := httptest.NewRequest("GET", "/api/collections", nil)
	reqListB.Header.Set("Authorization", "Bearer "+tokenB)
	wListB := httptest.NewRecorder()
	handler.ServeHTTP(wListB, reqListB)
	if wListB.Code != http.StatusOK {
		t.Fatalf("expected 200 for listing collections, got %d", wListB.Code)
	}
	var listB []domain.Collection
	_ = json.Unmarshal(wListB.Body.Bytes(), &listB)
	if len(listB) != 1 || listB[0].ID != colB.ID {
		t.Fatalf("expected Tenant B to only see colB, got %+v", listB)
	}

	// 7. Tenant A creates record in colA: should succeed
	recPayload := `{"title": "Secret Doc", "content": "Alpha confidential"}`
	reqRecA := httptest.NewRequest("POST", "/api/collections/"+colA.ID+"/records", strings.NewReader(recPayload))
	reqRecA.Header.Set("Authorization", "Bearer "+tokenA)
	wRecA := httptest.NewRecorder()
	handler.ServeHTTP(wRecA, reqRecA)
	if wRecA.Code != http.StatusCreated {
		t.Fatalf("expected 201 for Tenant A creating record in colA, got %d", wRecA.Code)
	}

	// 8. Tenant B attempts to read records in colA: should be forbidden 403
	reqReadB := httptest.NewRequest("GET", "/api/collections/"+colA.ID+"/records", nil)
	reqReadB.Header.Set("Authorization", "Bearer "+tokenB)
	wReadB := httptest.NewRecorder()
	handler.ServeHTTP(wReadB, reqReadB)
	if wReadB.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when Tenant B reads colA records, got %d", wReadB.Code)
	}

	// 9. Tenant B attempts to write record to colA: should be forbidden 403
	reqWriteB := httptest.NewRequest("POST", "/api/collections/"+colA.ID+"/records", strings.NewReader(recPayload))
	reqWriteB.Header.Set("Authorization", "Bearer "+tokenB)
	wWriteB := httptest.NewRecorder()
	handler.ServeHTTP(wWriteB, reqWriteB)
	if wWriteB.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when Tenant B writes to colA, got %d", wWriteB.Code)
	}

	// 10. Auto-created user (invited to colA) has no tenant
	inv := &domain.Invite{
		Email: "invited@alpha.com",
		Role:  domain.RoleMember,
	}
	_ = repo.CreateInvite(ctx, inv)
	invitedUser, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{
		Email: "invited@alpha.com",
		Name:  "Invited User",
	}, false)
	if err != nil {
		t.Fatalf("failed auto-creating user via invite: %v", err)
	}
	if invitedUser.TenantID != nil {
		t.Fatalf("expected auto-created user to have NO tenant, got %v", *invitedUser.TenantID)
	}

	// 11. Auto-created user decides to create a tenant via POST /api/tenants
	tokenInvited, _ := tm.GenerateToken(invitedUser)
	reqTenantCreate := httptest.NewRequest("POST", "/api/tenants", nil)
	reqTenantCreate.Header.Set("Authorization", "Bearer "+tokenInvited)
	wTenantCreate := httptest.NewRecorder()
	handler.ServeHTTP(wTenantCreate, reqTenantCreate)
	if wTenantCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 for creating tenant, got %d (body: %s)", wTenantCreate.Code, wTenantCreate.Body.String())
	}
	var tenantResp struct {
		TenantID    string `json:"tenant_id"`
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(wTenantCreate.Body.Bytes(), &tenantResp)
	if tenantResp.TenantID == "" || tenantResp.AccessToken == "" {
		t.Fatalf("expected tenant_id and access_token in response, got %+v", tenantResp)
	}

	// 12. Upgraded user can now create their own collection
	payloadInvCol := `{"name": "Invited User's New Collection", "default_visibility": "private"}`
	reqInvCreate := httptest.NewRequest("POST", "/api/collections", strings.NewReader(payloadInvCol))
	reqInvCreate.Header.Set("Authorization", "Bearer "+tenantResp.AccessToken)
	reqInvCreate.Header.Set("Content-Type", "application/json")
	wInvCreate := httptest.NewRecorder()
	handler.ServeHTTP(wInvCreate, reqInvCreate)
	if wInvCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 for upgraded user creating collection, got %d (body: %s)", wInvCreate.Code, wInvCreate.Body.String())
	}
	var newCol domain.Collection
	_ = json.Unmarshal(wInvCreate.Body.Bytes(), &newCol)
	if newCol.TenantID != tenantResp.TenantID {
		t.Fatalf("expected new collection to belong to new tenant %s, got %s", tenantResp.TenantID, newCol.TenantID)
	}

	// 13. Auto-provision on collection creation for non-tenant user without prior POST /api/tenants
	inv2 := &domain.Invite{Email: "user3@example.com", Role: domain.RoleMember}
	_ = repo.CreateInvite(ctx, inv2)
	user3, _ := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "user3@example.com", Name: "User 3"}, false)
	token3, _ := tm.GenerateToken(user3)

	reqAutoCreate := httptest.NewRequest("POST", "/api/collections", strings.NewReader(`{"name": "Auto Tenant Col"}`))
	reqAutoCreate.Header.Set("Authorization", "Bearer "+token3)
	reqAutoCreate.Header.Set("Content-Type", "application/json")
	wAutoCreate := httptest.NewRecorder()
	handler.ServeHTTP(wAutoCreate, reqAutoCreate)
	if wAutoCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 for auto-provisioning tenant on collection creation, got %d", wAutoCreate.Code)
	}
	var col3 domain.Collection
	_ = json.Unmarshal(wAutoCreate.Body.Bytes(), &col3)
	if col3.TenantID == "" {
		t.Fatalf("expected auto-created collection to have a tenant_id")
	}
}

type unregMockProvider struct{}

func (m *unregMockProvider) Name() string { return "unreg" }
func (m *unregMockProvider) GetAuthURL(state string) string {
	return "/auth/callback/unreg?code=xyz&state=" + url.QueryEscape(state)
}
func (m *unregMockProvider) ExchangeCode(ctx context.Context, code string) (*auth.UserInfo, error) {
	return &auth.UserInfo{Email: "stranger@unknown.org", Name: "Stranger Danger", Sub: "sub-999"}, nil
}

func TestUnregisteredUserSignInRedirectsToSignupFlow(t *testing.T) {
	database, err := db.Connect("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db error: %v", err)
	}
	repo := repository.New(database)
	tm := auth.NewTokenManager("test-secret", 15*time.Minute)
	sm := auth.NewSSOManager(repo)
	sm.RegisterProvider(&unregMockProvider{})
	se := search.NewSearchEngine(database, repo, nil)
	mcpServer := mcp.NewServer(repo, se)

	// Ensure system has at least 1 existing user so auto-bootstrap doesn't trigger
	_ = repo.CreateUser(context.Background(), &domain.User{
		ID: "existing_admin", Email: "admin@corp.com", Name: "Admin", Role: domain.RoleAdmin,
	})

	server := api.NewServer(repo, tm, sm, se, mcpServer)
	handler := server.Routes()

	// 1. User tries to sign in to MCP via OAuth authorize without having an account yet
	oauthRedirect := "/login?client_id=claude-code&redirect_uri=http://localhost:3344/cb&code_challenge=xyz"
	reqCB := httptest.NewRequest("GET", "/auth/callback/unreg?code=xyz&state="+url.QueryEscape("state123|"+oauthRedirect), nil)
	wCB := httptest.NewRecorder()
	handler.ServeHTTP(wCB, reqCB)

	if wCB.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect for unregistered user, got %d", wCB.Code)
	}
	loc := wCB.Header().Get("Location")
	if !strings.Contains(loc, "error=not_registered") {
		t.Fatalf("expected error=not_registered in redirect URL, got %s", loc)
	}
	if !strings.Contains(loc, "email=stranger%40unknown.org") && !strings.Contains(loc, "email=stranger@unknown.org") {
		t.Fatalf("expected email in redirect URL, got %s", loc)
	}
	if !strings.Contains(loc, "client_id=claude-code") {
		t.Fatalf("expected OAuth client_id preserved in redirect URL, got %s", loc)
	}
	if strings.Contains(loc, "app=true") {
		t.Fatalf("expected OAuth login NOT to have app=true, got %s", loc)
	}

	// 1b. User tries to sign in to a private frontend app without an invite -> redirects with app=true
	reqAppCB := httptest.NewRequest("GET", "/auth/callback/unreg?code=xyz&state="+url.QueryEscape("state123|http://localhost:5173"), nil)
	wAppCB := httptest.NewRecorder()
	handler.ServeHTTP(wAppCB, reqAppCB)
	appLoc := wAppCB.Header().Get("Location")
	if !strings.Contains(appLoc, "error=not_registered") || !strings.Contains(appLoc, "app=true") {
		t.Fatalf("expected app login to redirect with error=not_registered and app=true, got %s", appLoc)
	}

	// 1c. User clicks "Try Different Account" -> /auth/login/{provider}?prompt=select_account forwards prompt
	reqTryDiff := httptest.NewRequest("GET", "/auth/login/unreg?popup=true&prompt=select_account", nil)
	wTryDiff := httptest.NewRecorder()
	handler.ServeHTTP(wTryDiff, reqTryDiff)
	if wTryDiff.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect for SSO login with prompt=select_account, got %d", wTryDiff.Code)
	}
	if !strings.Contains(wTryDiff.Header().Get("Location"), "prompt=select_account") {
		t.Fatalf("expected SSO login redirect to include prompt=select_account, got %s", wTryDiff.Header().Get("Location"))
	}

	// 2. User clicks "Create Workspace & Sign Up" which triggers signup flow with signup=true
	reqSignup := httptest.NewRequest("GET", "/auth/callback/unreg?code=xyz&state="+url.QueryEscape("state123|"+oauthRedirect+"|signup=true"), nil)
	wSignup := httptest.NewRecorder()
	handler.ServeHTTP(wSignup, reqSignup)

	if wSignup.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect for successful signup, got %d", wSignup.Code)
	}
	signupLoc := wSignup.Header().Get("Location")
	if !strings.Contains(signupLoc, "access_token=") {
		t.Fatalf("expected access_token in redirect location hash, got %s", signupLoc)
	}
	if !strings.Contains(signupLoc, "client_id=claude-code") {
		t.Fatalf("expected client_id preserved in location, got %s", signupLoc)
	}

	// 3. User is now provisioned in database with an Admin tenant
	newUser, err := repo.GetUserByEmail(context.Background(), "stranger@unknown.org")
	if err != nil {
		t.Fatalf("expected newly registered user in DB: %v", err)
	}
	if newUser.TenantID == nil || *newUser.TenantID == "" {
		t.Fatalf("expected newly registered user to have a TenantID")
	}
	if newUser.Role != domain.RoleAdmin {
		t.Fatalf("expected newly registered user to be admin, got %s", newUser.Role)
	}
}

func TestRevokeAllSessionsAndTokenVersion(t *testing.T) {
	handler, repo, tm, user := setupTestServer(t)

	// User should have a token_version generated
	dbUser, err := repo.GetUserByID(context.Background(), user.ID)
	if err != nil || dbUser.TokenVersion == "" {
		t.Fatalf("expected user to have token_version, got %v (err: %v)", dbUser, err)
	}

	// 1. Generate access token & refresh token
	token, err := tm.GenerateToken(dbUser)
	if err != nil {
		t.Fatalf("failed generating token: %v", err)
	}

	rawRT, rtHash, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("failed generating refresh token: %v", err)
	}

	rt := &domain.RefreshToken{
		ID:           "rt_123",
		UserID:       dbUser.ID,
		ClientID:     "web-console",
		TokenHash:    rtHash,
		TokenVersion: dbUser.TokenVersion,
		ExpiresAt:    time.Now().UTC().Add(30 * 24 * time.Hour),
		CreatedAt:    time.Now().UTC(),
	}
	if err := repo.CreateRefreshToken(context.Background(), rt); err != nil {
		t.Fatalf("failed saving refresh token: %v", err)
	}

	// 2. Validate access token works on protected endpoint
	reqProtected := httptest.NewRequest("GET", "/api/collections", nil)
	reqProtected.Header.Set("Authorization", "Bearer "+token)
	wProtected := httptest.NewRecorder()
	handler.ServeHTTP(wProtected, reqProtected)
	if wProtected.Code != http.StatusOK {
		t.Fatalf("expected 200 OK before revocation, got %d", wProtected.Code)
	}

	// 3. User calls POST /api/auth/revoke-all
	reqRevoke := httptest.NewRequest("POST", "/api/auth/revoke-all", nil)
	reqRevoke.Header.Set("Authorization", "Bearer "+token)
	wRevoke := httptest.NewRecorder()
	handler.ServeHTTP(wRevoke, reqRevoke)
	if wRevoke.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for revoke-all, got %d", wRevoke.Code)
	}

	// Verify token version in DB changed
	updatedUser, err := repo.GetUserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("failed fetching user after revoke: %v", err)
	}
	if updatedUser.TokenVersion == dbUser.TokenVersion {
		t.Fatalf("expected token_version to change, old: %s, new: %s", dbUser.TokenVersion, updatedUser.TokenVersion)
	}

	// 4. Verify old access token is now rejected on protected endpoints
	reqOldAccess := httptest.NewRequest("GET", "/api/collections", nil)
	reqOldAccess.Header.Set("Authorization", "Bearer "+token)
	wOldAccess := httptest.NewRecorder()
	handler.ServeHTTP(wOldAccess, reqOldAccess)
	if wOldAccess.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for old access token after revoke-all, got %d", wOldAccess.Code)
	}

	// 5. Verify old refresh token is rejected on POST /oauth/token
	refForm := url.Values{}
	refForm.Set("grant_type", "refresh_token")
	refForm.Set("refresh_token", rawRT)
	reqRef := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(refForm.Encode()))
	reqRef.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wRef := httptest.NewRecorder()
	handler.ServeHTTP(wRef, reqRef)
	if wRef.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for old refresh token, got %d: %s", wRef.Code, wRef.Body.String())
	}

	// 6. Verify an active refresh token with mismatched token_version is also rejected
	rawRT2, rtHash2, _ := auth.GenerateRefreshToken()
	rtMismatch := &domain.RefreshToken{
		ID:           "rt_mismatch",
		UserID:       user.ID,
		ClientID:     "web-console",
		TokenHash:    rtHash2,
		TokenVersion: "outdated_version_999",
		ExpiresAt:    time.Now().UTC().Add(30 * 24 * time.Hour),
		CreatedAt:    time.Now().UTC(),
	}
	_ = repo.CreateRefreshToken(context.Background(), rtMismatch)

	refForm2 := url.Values{}
	refForm2.Set("grant_type", "refresh_token")
	refForm2.Set("refresh_token", rawRT2)
	reqRef2 := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(refForm2.Encode()))
	reqRef2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wRef2 := httptest.NewRecorder()
	handler.ServeHTTP(wRef2, reqRef2)
	if wRef2.Code != http.StatusUnauthorized || !strings.Contains(wRef2.Body.String(), "token version mismatch") {
		t.Fatalf("expected 401 token version mismatch, got %d: %s", wRef2.Code, wRef2.Body.String())
	}
}

func TestListProvidersAndClientGuide(t *testing.T) {
	handler, _, _, _ := setupTestServer(t)

	// 1. GET /api/auth/providers
	req := httptest.NewRequest("GET", "/api/auth/providers", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/auth/providers, got %d", w.Code)
	}

	var provRes struct {
		Providers []string `json:"providers"`
	}
	if err := json.NewDecoder(w.Body).Decode(&provRes); err != nil {
		t.Fatalf("failed decoding providers json: %v", err)
	}
	if len(provRes.Providers) == 0 || provRes.Providers[0] != "mock" {
		t.Fatalf("expected ['mock'] provider, got %v", provRes.Providers)
	}

	// 2. GET /api/client-guide?format=json
	reqGuideJSON := httptest.NewRequest("GET", "/api/client-guide?format=json", nil)
	wGuideJSON := httptest.NewRecorder()
	handler.ServeHTTP(wGuideJSON, reqGuideJSON)

	if wGuideJSON.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/client-guide?format=json, got %d", wGuideJSON.Code)
	}

	var guideRes struct {
		Guide     string   `json:"guide"`
		Providers []string `json:"providers"`
	}
	if err := json.NewDecoder(wGuideJSON.Body).Decode(&guideRes); err != nil {
		t.Fatalf("failed decoding guide json: %v", err)
	}
	if len(guideRes.Providers) == 0 || guideRes.Providers[0] != "mock" {
		t.Fatalf("expected providers in json guide, got %v", guideRes.Providers)
	}
	if !strings.Contains(guideRes.Guide, "Available Identity Providers on this Instance") {
		t.Fatalf("expected dynamic providers section in guide")
	}

	// 3. GET /api/client-guide (markdown)
	reqGuideMD := httptest.NewRequest("GET", "/api/client-guide", nil)
	wGuideMD := httptest.NewRecorder()
	handler.ServeHTTP(wGuideMD, reqGuideMD)

	if wGuideMD.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/client-guide, got %d", wGuideMD.Code)
	}
	mdBody := wGuideMD.Body.String()
	if !strings.Contains(mdBody, "GET http://example.com/auth/login/mock") && !strings.Contains(mdBody, "/auth/login/mock") {
		t.Fatalf("expected mock provider login url in markdown guide, got: %s", mdBody)
	}

	// 4. GET /relay and GET /relay.html
	reqRelay := httptest.NewRequest("GET", "/relay", nil)
	wRelay := httptest.NewRecorder()
	handler.ServeHTTP(wRelay, reqRelay)
	if wRelay.Code != http.StatusOK || !strings.Contains(wRelay.Body.String(), "vb_relay_request") {
		t.Fatalf("expected 200 and relay content for /relay, got %d", wRelay.Code)
	}

	reqRelayHTML := httptest.NewRequest("GET", "/relay.html", nil)
	wRelayHTML := httptest.NewRecorder()
	handler.ServeHTTP(wRelayHTML, reqRelayHTML)
	if wRelayHTML.Code != http.StatusOK || !strings.Contains(wRelayHTML.Body.String(), "vb_relay_request") {
		t.Fatalf("expected 200 and relay content for /relay.html, got %d", wRelayHTML.Code)
	}

	// 5. GET /vibebackr.js and GET /vibe.js
	reqJS := httptest.NewRequest("GET", "/vibebackr.js", nil)
	wJS := httptest.NewRecorder()
	handler.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK || !strings.Contains(wJS.Body.String(), "VibeBackr") || !strings.Contains(wJS.Header().Get("Content-Type"), "application/javascript") {
		t.Fatalf("expected 200 and javascript content for /vibebackr.js, got %d (%s)", wJS.Code, wJS.Header().Get("Content-Type"))
	}

	reqVibeJS := httptest.NewRequest("GET", "/vibe.js", nil)
	wVibeJS := httptest.NewRecorder()
	handler.ServeHTTP(wVibeJS, reqVibeJS)
	if wVibeJS.Code != http.StatusOK || !strings.Contains(wVibeJS.Body.String(), "createVibeClient") || !strings.Contains(wVibeJS.Header().Get("Content-Type"), "application/javascript") {
		t.Fatalf("expected 200 and javascript content for /vibe.js, got %d (%s)", wVibeJS.Code, wVibeJS.Header().Get("Content-Type"))
	}

	// 6. GET /vibebackr.d.ts and GET /vibe.d.ts
	reqDTS := httptest.NewRequest("GET", "/vibebackr.d.ts", nil)
	wDTS := httptest.NewRecorder()
	handler.ServeHTTP(wDTS, reqDTS)
	if wDTS.Code != http.StatusOK || !strings.Contains(wDTS.Body.String(), "type VibeClient") || !strings.Contains(wDTS.Header().Get("Content-Type"), "application/typescript") {
		t.Fatalf("expected 200 and typescript definitions for /vibebackr.d.ts, got %d (%s)", wDTS.Code, wDTS.Header().Get("Content-Type"))
	}

	reqVibeDTS := httptest.NewRequest("GET", "/vibe.d.ts", nil)
	wVibeDTS := httptest.NewRecorder()
	handler.ServeHTTP(wVibeDTS, reqVibeDTS)
	if wVibeDTS.Code != http.StatusOK || !strings.Contains(wVibeDTS.Body.String(), "vibebackr") || !strings.Contains(wVibeDTS.Header().Get("Content-Type"), "application/typescript") {
		t.Fatalf("expected 200 and typescript definitions for /vibe.d.ts, got %d (%s)", wVibeDTS.Code, wVibeDTS.Header().Get("Content-Type"))
	}
}

func TestCollectionJSONImportAndDeletion(t *testing.T) {
	handler, repo, tm, _ := setupTestServer(t)

	// Create user
	tenantID := "tenant_gemini"
	user := &domain.User{
		ID:       "user_gemini",
		TenantID: &tenantID,
		Email:    "gemini_user@example.com",
		Name:     "Gemini User",
		Role:     domain.RoleAdmin,
	}
	_ = repo.CreateUser(context.Background(), user)
	token, _ := tm.GenerateToken(user)

	// 1. POST /api/collections with JSON configuration and seed records
	jsonConfig := `{
		"name": "gemini_tasks",
		"default_visibility": "private",
		"origin_urls": ["http://localhost:3000", "*"],
		"records": [
			{
				"title": "Task 1: Setup VibeBackr",
				"content": "Configure collections and records",
				"data": { "completed": true, "priority": 1 }
			},
			{
				"title": "Task 2: Build UI with Gemini",
				"content": "Use client SDK to display items",
				"data": { "completed": false, "priority": 2 }
			}
		]
	}`

	reqCreate := httptest.NewRequest("POST", "/api/collections", strings.NewReader(jsonConfig))
	reqCreate.Header.Set("Authorization", "Bearer "+token)
	reqCreate.Header.Set("Content-Type", "application/json")
	wCreate := httptest.NewRecorder()
	handler.ServeHTTP(wCreate, reqCreate)

	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created from POST /api/collections, got %d: %s", wCreate.Code, wCreate.Body.String())
	}

	var createdCol domain.Collection
	_ = json.Unmarshal(wCreate.Body.Bytes(), &createdCol)
	if createdCol.ID == "" || createdCol.Name != "gemini_tasks" {
		t.Fatalf("invalid created collection: %+v", createdCol)
	}

	// 2. GET /api/collections/{col_id}/records to verify seed records were inserted
	reqRecords := httptest.NewRequest("GET", "/api/collections/"+createdCol.ID+"/records", nil)
	reqRecords.Header.Set("Authorization", "Bearer "+token)
	wRecords := httptest.NewRecorder()
	handler.ServeHTTP(wRecords, reqRecords)

	if wRecords.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from GET records, got %d: %s", wRecords.Code, wRecords.Body.String())
	}

	var records []domain.Record
	_ = json.Unmarshal(wRecords.Body.Bytes(), &records)
	if len(records) != 2 {
		t.Fatalf("expected 2 seed records, got %d", len(records))
	}
	titles := map[string]bool{records[0].Title: true, records[1].Title: true}
	if !titles["Task 1: Setup VibeBackr"] || !titles["Task 2: Build UI with Gemini"] {
		t.Fatalf("unexpected record titles: %v, %v", records[0].Title, records[1].Title)
	}

	// 3. DELETE /api/collections/{col_id}
	reqDelete := httptest.NewRequest("DELETE", "/api/collections/"+createdCol.ID, nil)
	reqDelete.Header.Set("Authorization", "Bearer "+token)
	wDelete := httptest.NewRecorder()
	handler.ServeHTTP(wDelete, reqDelete)

	if wDelete.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from DELETE collection, got %d: %s", wDelete.Code, wDelete.Body.String())
	}

	// 4. Verify collection is gone
	reqGetCol := httptest.NewRequest("GET", "/api/collections/"+createdCol.ID+"/records", nil)
	reqGetCol.Header.Set("Authorization", "Bearer "+token)
	wGetCol := httptest.NewRecorder()
	handler.ServeHTTP(wGetCol, reqGetCol)

	if wGetCol.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found after deletion, got %d", wGetCol.Code)
	}
}

func TestManyToManyTenantMembershipsAndLeave(t *testing.T) {
	handler, repo, tm, _ := setupTestServer(t)
	sm := auth.NewSSOManager(repo)
	ctx := context.Background()

	// 1. User A signs up and owns Tenant A
	userA, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{
		Email: "ownerA@alpha.io",
		Name:  "User A",
	}, true)
	if err != nil {
		t.Fatalf("failed provisioning userA: %v", err)
	}
	tokenA, _ := tm.GenerateToken(userA)

	// User A creates a collection in Tenant A
	colA := &domain.Collection{
		ID:                "col_alpha",
		TenantID:          *userA.TenantID,
		Name:              "Alpha Workspace Docs",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	_ = repo.CreateCollection(ctx, colA)

	// 2. User B signs up and owns Tenant B
	userB, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{
		Email: "ownerB@beta.io",
		Name:  "User B",
	}, true)
	if err != nil {
		t.Fatalf("failed provisioning userB: %v", err)
	}
	tokenB, _ := tm.GenerateToken(userB)

	// User B creates a collection in Tenant B
	colB := &domain.Collection{
		ID:                "col_beta",
		TenantID:          *userB.TenantID,
		Name:              "Beta Workspace Docs",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	_ = repo.CreateCollection(ctx, colB)

	// 3. User B invites User A (who already owns Tenant A!) to Tenant B
	inv := &domain.Invite{
		TenantID:  *userB.TenantID,
		Email:     userA.Email,
		Role:      domain.RoleMember,
		ManagerID: &userB.ID,
		CreatedBy: userB.ID,
	}
	if err := repo.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("failed creating invite for userA to Tenant B: %v", err)
	}

	// 4. User A logs in again -> ProvisionOrGetUser auto-accepts pending invites for any tenant
	loggedInUserA, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{
		Email: "ownerA@alpha.io",
		Name:  "User A",
	}, false)
	if err != nil {
		t.Fatalf("failed login for userA: %v", err)
	}
	// User A still owns Tenant A
	if loggedInUserA.TenantID == nil || *loggedInUserA.TenantID != *userA.TenantID {
		t.Fatalf("expected userA to retain primary Tenant A (%s), got %v", *userA.TenantID, loggedInUserA.TenantID)
	}

	// Verify invite was consumed
	if remInv, _ := repo.ListInvitesByEmail(ctx, userA.Email); len(remInv) != 0 {
		t.Fatalf("expected pending invite to be consumed on login, got %d remaining", len(remInv))
	}

	// 5. GET /api/tenants for User A should list both Tenant A and Tenant B
	reqTenants := httptest.NewRequest("GET", "/api/tenants", nil)
	reqTenants.Header.Set("Authorization", "Bearer "+tokenA)
	wTenants := httptest.NewRecorder()
	handler.ServeHTTP(wTenants, reqTenants)
	if wTenants.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET /api/tenants, got %d", wTenants.Code)
	}
	var memberships []domain.TenantMembership
	_ = json.Unmarshal(wTenants.Body.Bytes(), &memberships)
	if len(memberships) != 2 {
		t.Fatalf("expected 2 tenant memberships for userA, got %d: %+v", len(memberships), memberships)
	}

	// 6. GET /api/collections for User A should only return collections owned by User A (col_alpha), not col_beta
	reqCols := httptest.NewRequest("GET", "/api/collections", nil)
	reqCols.Header.Set("Authorization", "Bearer "+tokenA)
	wCols := httptest.NewRecorder()
	handler.ServeHTTP(wCols, reqCols)
	var visibleCols []domain.Collection
	_ = json.Unmarshal(wCols.Body.Bytes(), &visibleCols)
	if len(visibleCols) != 1 || visibleCols[0].ID != "col_alpha" {
		t.Fatalf("expected userA to only see owned collection (col_alpha), got %d: %+v", len(visibleCols), visibleCols)
	}

	// 7. User A can create a record in Tenant B's collection (col_beta), and User B (manager in Tenant B) can read it
	recInBeta := `{"title": "Cross-Tenant Note", "content": "Created by User A in Tenant B"}`
	reqCreateRec := httptest.NewRequest("POST", "/api/collections/col_beta/records", strings.NewReader(recInBeta))
	reqCreateRec.Header.Set("Authorization", "Bearer "+tokenA)
	reqCreateRec.Header.Set("Content-Type", "application/json")
	wCreateRec := httptest.NewRecorder()
	handler.ServeHTTP(wCreateRec, reqCreateRec)
	if wCreateRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 when member User A creates record in col_beta, got %d: %s", wCreateRec.Code, wCreateRec.Body.String())
	}

	// User A cannot delete Tenant B's collection (since User A is only a member of Tenant B, not owner/admin of Tenant B)
	reqDelBeta := httptest.NewRequest("DELETE", "/api/collections/col_beta", nil)
	reqDelBeta.Header.Set("Authorization", "Bearer "+tokenA)
	wDelBeta := httptest.NewRecorder()
	handler.ServeHTTP(wDelBeta, reqDelBeta)
	if wDelBeta.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when User A tries to delete Tenant B's collection, got %d", wDelBeta.Code)
	}

	// 8. User A cannot leave their own primary tenant (Tenant A)
	reqLeaveOwn := httptest.NewRequest("DELETE", "/api/tenants/"+*userA.TenantID, nil)
	reqLeaveOwn.Header.Set("Authorization", "Bearer "+tokenA)
	wLeaveOwn := httptest.NewRecorder()
	handler.ServeHTTP(wLeaveOwn, reqLeaveOwn)
	if wLeaveOwn.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when trying to leave own primary tenant, got %d", wLeaveOwn.Code)
	}

	// 9. User A leaves Tenant B via DELETE /api/tenants/{tenantB}
	reqLeaveB := httptest.NewRequest("DELETE", "/api/tenants/"+*userB.TenantID, nil)
	reqLeaveB.Header.Set("Authorization", "Bearer "+tokenA)
	wLeaveB := httptest.NewRecorder()
	handler.ServeHTTP(wLeaveB, reqLeaveB)
	if wLeaveB.Code != http.StatusOK {
		t.Fatalf("expected 200 OK when leaving Tenant B, got %d: %s", wLeaveB.Code, wLeaveB.Body.String())
	}

	// Verify User B still has col_beta intact
	reqListB := httptest.NewRequest("GET", "/api/collections", nil)
	reqListB.Header.Set("Authorization", "Bearer "+tokenB)
	wListB := httptest.NewRecorder()
	handler.ServeHTTP(wListB, reqListB)
	var colsB []domain.Collection
	_ = json.Unmarshal(wListB.Body.Bytes(), &colsB)
	if len(colsB) != 1 || colsB[0].ID != "col_beta" {
		t.Fatalf("expected Tenant B collection to remain intact after User A left, got %+v", colsB)
	}

	// Verify User A now only belongs to 1 tenant (Tenant A)
	reqTenantsAfter := httptest.NewRequest("GET", "/api/tenants", nil)
	reqTenantsAfter.Header.Set("Authorization", "Bearer "+tokenA)
	wTenantsAfter := httptest.NewRecorder()
	handler.ServeHTTP(wTenantsAfter, reqTenantsAfter)
	var membershipsAfter []domain.TenantMembership
	_ = json.Unmarshal(wTenantsAfter.Body.Bytes(), &membershipsAfter)
	if len(membershipsAfter) != 1 || membershipsAfter[0].TenantID != *userA.TenantID {
		t.Fatalf("expected userA to only have primary tenant membership after leaving Tenant B, got %+v", membershipsAfter)
	}
}

func TestPublishAndServeApp(t *testing.T) {
	handler, repo, tm, user := setupTestServer(t)
	ctx := context.Background()

	tenantID := "tenant_app_test"
	user.TenantID = &tenantID
	user.Role = domain.RoleAdmin
	_ = repo.UpdateUser(ctx, user)

	colID := "550e8400-e29b-41d4-a716-446655440000"
	col := &domain.Collection{
		ID:                colID,
		TenantID:          tenantID,
		Name:              "My Hosted App",
		DefaultVisibility: domain.VisibilityPrivate,
	}
	if err := repo.CreateCollection(ctx, col); err != nil {
		t.Fatalf("failed creating collection: %v", err)
	}

	token, _ := tm.GenerateToken(user)

	// 1. Publish HTML app via PUT /api/collections/{id}/app
	appHTML := `<!DOCTYPE html><html><head><title>Vibe App</title></head><body><h1>Hello VibeBackr!</h1></body></html>`
	pubBody, _ := json.Marshal(map[string]string{"html": appHTML})
	reqPub := httptest.NewRequest("PUT", "/api/collections/"+colID+"/app", bytes.NewReader(pubBody))
	reqPub.Host = "vibebac.kr"
	reqPub.Header.Set("Authorization", "Bearer "+token)
	reqPub.Header.Set("Content-Type", "application/json")
	wPub := httptest.NewRecorder()
	handler.ServeHTTP(wPub, reqPub)

	if wPub.Code != http.StatusOK {
		t.Fatalf("expected 200 OK publishing app, got %d: %s", wPub.Code, wPub.Body.String())
	}

	var pubRes map[string]interface{}
	_ = json.Unmarshal(wPub.Body.Bytes(), &pubRes)
	expectedSlug := "550e8400e29b41d4a716446655440000"
	expectedAppURL := "https://" + expectedSlug + ".vibebac.kr"
	if pubRes["slug"] != expectedSlug || pubRes["app_url"] != expectedAppURL {
		t.Fatalf("unexpected publish response: %+v", pubRes)
	}

	// Verify collection's origin_urls was automatically updated
	updatedCol, _ := repo.GetCollection(ctx, colID)
	if len(updatedCol.OriginURLs) != 1 || updatedCol.OriginURLs[0] != expectedAppURL {
		t.Fatalf("expected origin_urls to include %s, got %+v", expectedAppURL, updatedCol.OriginURLs)
	}

	// 2. Serve the app via GET /apps/{slug}
	reqGet := httptest.NewRequest("GET", "/apps/"+expectedSlug, nil)
	wGet := httptest.NewRecorder()
	handler.ServeHTTP(wGet, reqGet)
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 OK serving app, got %d", wGet.Code)
	}
	if wGet.Body.String() != appHTML {
		t.Fatalf("unexpected served HTML: %s", wGet.Body.String())
	}
	if !strings.Contains(wGet.Header().Get("Cache-Control"), "s-maxage=5") {
		t.Fatalf("expected Cache-Control with s-maxage=5, got %s", wGet.Header().Get("Cache-Control"))
	}

	// 3. Delete collection removes the app
	reqDel := httptest.NewRequest("DELETE", "/api/collections/"+colID, nil)
	reqDel.Header.Set("Authorization", "Bearer "+token)
	wDel := httptest.NewRecorder()
	handler.ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("expected 200 OK deleting collection, got %d", wDel.Code)
	}

	reqGetAfter := httptest.NewRequest("GET", "/apps/"+expectedSlug, nil)
	wGetAfter := httptest.NewRecorder()
	handler.ServeHTTP(wGetAfter, reqGetAfter)
	if wGetAfter.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after collection deletion, got %d", wGetAfter.Code)
	}
}

func TestEnterpriseSecurityHardening(t *testing.T) {
	handler, repo, tm, _ := setupTestServer(t)
	sm := auth.NewSSOManager(repo)
	ctx := context.Background()

	// 1. Provision Tenant A and Tenant B owners
	userA, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "ceo@tenant-a.com", Name: "Alice A", Sub: "sub-a"}, true)
	if err != nil {
		t.Fatalf("failed provisioning userA: %v", err)
	}
	tokenA, _ := tm.GenerateToken(userA)

	userB, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "ceo@tenant-b.com", Name: "Bob B", Sub: "sub-b"}, true)
	if err != nil {
		t.Fatalf("failed provisioning userB: %v", err)
	}
	tokenB, _ := tm.GenerateToken(userB)

	// Tenant A creates a private collection and record
	colA := &domain.Collection{ID: "col_sec_a", TenantID: *userA.TenantID, Name: "Tenant A Vault", DefaultVisibility: domain.VisibilityPrivate}
	_ = repo.CreateCollection(ctx, colA)
	recA := &domain.Record{
		ID:           "rec_sec_a",
		CollectionID: colA.ID,
		Title:        "Top Secret A",
		Content:      "Classified A",
		Data:         `{"secretKey": "alpha"}`,
		CreatedBy:    userA.ID,
		AccessPolicy: domain.AccessPolicy{Visibility: domain.VisibilityPrivate},
	}
	_ = repo.CreateRecord(ctx, recA)

	// Tenant B creates a private collection
	colB := &domain.Collection{ID: "col_sec_b", TenantID: *userB.TenantID, Name: "Tenant B Vault", DefaultVisibility: domain.VisibilityPrivate}
	_ = repo.CreateCollection(ctx, colB)

	// 2. Critical Check: Tenant B invites a new user with Role="admin" -> must NOT become global super-admin
	_ = repo.CreateInvite(ctx, &domain.Invite{
		TenantID:  *userB.TenantID,
		Email:     "co-admin@tenant-b.com",
		Role:      domain.RoleAdmin,
		CreatedBy: userB.ID,
	})
	coAdminB, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "co-admin@tenant-b.com", Name: "CoAdmin B", Sub: "sub-cob"}, false)
	if err != nil {
		t.Fatalf("failed provisioning coAdminB: %v", err)
	}
	if coAdminB.Role == domain.RoleAdmin && (coAdminB.TenantID == nil || *coAdminB.TenantID == "") {
		t.Fatalf("SECURITY BUG: tenant-scoped admin invite promoted user to global super-admin!")
	}
	tokenCoAdminB, _ := tm.GenerateToken(coAdminB)

	// coAdminB can access Tenant B's collection, but MUST be denied access to Tenant A's collection
	reqCoBReadA := httptest.NewRequest("GET", "/api/collections/"+colA.ID+"/records", nil)
	reqCoBReadA.Header.Set("Authorization", "Bearer "+tokenCoAdminB)
	wCoBReadA := httptest.NewRecorder()
	handler.ServeHTTP(wCoBReadA, reqCoBReadA)
	if wCoBReadA.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when Tenant B co-admin reads Tenant A records, got %d", wCoBReadA.Code)
	}

	// 3. Cross-Tenant User Profile IDOR & TokenVersion Redaction (before userA joins Tenant B)
	reqIDORUser := httptest.NewRequest("GET", "/api/users/"+coAdminB.ID, nil)
	reqIDORUser.Header.Set("Authorization", "Bearer "+tokenA)
	wIDORUser := httptest.NewRecorder()
	handler.ServeHTTP(wIDORUser, reqIDORUser)
	if wIDORUser.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for cross-tenant GET /api/users/{id}, got %d", wIDORUser.Code)
	}

	// Same-tenant lookup (userB viewing coAdminB) succeeds but redacts token_version
	reqSameTenantUser := httptest.NewRequest("GET", "/api/users/"+coAdminB.ID, nil)
	reqSameTenantUser.Header.Set("Authorization", "Bearer "+tokenB)
	wSameTenantUser := httptest.NewRecorder()
	handler.ServeHTTP(wSameTenantUser, reqSameTenantUser)
	if wSameTenantUser.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for same-tenant GET /api/users/{id}, got %d", wSameTenantUser.Code)
	}
	var fetchedCoAdmin domain.User
	_ = json.Unmarshal(wSameTenantUser.Body.Bytes(), &fetchedCoAdmin)
	if fetchedCoAdmin.TokenVersion != "" {
		t.Fatalf("expected token_version to be redacted when viewing another user, got %s", fetchedCoAdmin.TokenVersion)
	}

	// 4. Critical Check: Cross-Tenant Manager Hierarchy Isolation in Recursive CTE
	// Suppose User B invites User A into Tenant B with User B as User A's manager in Tenant B.
	_ = repo.CreateInvite(ctx, &domain.Invite{
		TenantID:  *userB.TenantID,
		Email:     userA.Email,
		Role:      domain.RoleMember,
		ManagerID: &userB.ID,
		CreatedBy: userB.ID,
	})
	_, _ = sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: userA.Email, Name: userA.Name, Sub: "sub-a"}, false)

	// User B is now User A's manager in Tenant B (user_tenants), but MUST NOT be able to read or update User A's record in Tenant A!
	reqMgrAttackRead := httptest.NewRequest("GET", "/api/collections/"+colA.ID+"/records/"+recA.ID, nil)
	reqMgrAttackRead.Header.Set("Authorization", "Bearer "+tokenB)
	wMgrAttackRead := httptest.NewRecorder()
	handler.ServeHTTP(wMgrAttackRead, reqMgrAttackRead)
	if wMgrAttackRead.Code != http.StatusForbidden {
		t.Fatalf("SECURITY BUG: cross-tenant manager edge leaked access to Tenant A record! status=%d", wMgrAttackRead.Code)
	}

	reqMgrAttackUpdate := httptest.NewRequest("PUT", "/api/collections/"+colA.ID+"/records/"+recA.ID, strings.NewReader(`{"title":"Pwned"}`))
	reqMgrAttackUpdate.Header.Set("Authorization", "Bearer "+tokenB)
	wMgrAttackUpdate := httptest.NewRecorder()
	handler.ServeHTTP(wMgrAttackUpdate, reqMgrAttackUpdate)
	if wMgrAttackUpdate.Code != http.StatusForbidden {
		t.Fatalf("SECURITY BUG: cross-tenant manager edge allowed updating Tenant A record! status=%d", wMgrAttackUpdate.Code)
	}

	// 5. SSO Open Redirect Protection
	reqEvilLogin := httptest.NewRequest("GET", "/auth/login/mock?redirect_uri=https://evil.attacker.com/steal", nil)
	wEvilLogin := httptest.NewRecorder()
	handler.ServeHTTP(wEvilLogin, reqEvilLogin)
	if wEvilLogin.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for open redirect on /auth/login, got %d", wEvilLogin.Code)
	}

	reqEvilCB := httptest.NewRequest("GET", "/auth/callback/mock?code=mock_code&state=nonce|https://evil.attacker.com/steal", nil)
	wEvilCB := httptest.NewRecorder()
	handler.ServeHTTP(wEvilCB, reqEvilCB)
	if wEvilCB.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for open redirect on /auth/callback, got %d", wEvilCB.Code)
	}

	// 6. Cross-Collection Record ID Mismatch -> 404 Not Found
	reqWrongCol := httptest.NewRequest("GET", "/api/collections/"+colB.ID+"/records/"+recA.ID, nil)
	reqWrongCol.Header.Set("Authorization", "Bearer "+tokenA)
	wWrongCol := httptest.NewRecorder()
	handler.ServeHTTP(wWrongCol, reqWrongCol)
	if wWrongCol.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found when collection_id in URL does not match record's collection, got %d", wWrongCol.Code)
	}

	// 7. Single-Use OAuth AuthCode Replay Prevention
	dcrClient := auth.RegisterClient(auth.DCRRegistrationRequest{
		ClientName:   "Replay Test Client",
		RedirectURIs: []string{"http://localhost:8888/cb"},
	})
	_ = repo.CreateOAuthClient(ctx, dcrClient)

	codeVerifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	authBody := `{"client_id":"` + dcrClient.ID + `","redirect_uri":"http://localhost:8888/cb","code_challenge":"` + codeVerifier + `","code_challenge_method":"plain"}`
	reqAuthz := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(authBody))
	reqAuthz.Header.Set("Content-Type", "application/json")
	reqAuthz.Header.Set("Authorization", "Bearer "+tokenA)
	wAuthz := httptest.NewRecorder()
	handler.ServeHTTP(wAuthz, reqAuthz)
	if wAuthz.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from POST /oauth/authorize, got %d", wAuthz.Code)
	}
	var authzResp struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(wAuthz.Body.Bytes(), &authzResp)

	// First exchange succeeds
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {dcrClient.ID},
		"code":          {authzResp.Code},
		"code_verifier": {codeVerifier},
	}
	reqTok1 := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(tokenForm.Encode()))
	reqTok1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wTok1 := httptest.NewRecorder()
	handler.ServeHTTP(wTok1, reqTok1)
	if wTok1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on first auth_code exchange, got %d", wTok1.Code)
	}

	// Second exchange with the same authorization_code MUST fail
	reqTok2 := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(tokenForm.Encode()))
	reqTok2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wTok2 := httptest.NewRecorder()
	handler.ServeHTTP(wTok2, reqTok2)
	if wTok2.Code != http.StatusBadRequest {
		t.Fatalf("SECURITY BUG: authorization_code replay succeeded! status=%d", wTok2.Code)
	}

	// 8. Revoked Token Rejection on /mcp and POST /oauth/authorize
	reqRevoke := httptest.NewRequest("POST", "/api/auth/revoke-all", nil)
	reqRevoke.Header.Set("Authorization", "Bearer "+tokenA)
	wRevoke := httptest.NewRecorder()
	handler.ServeHTTP(wRevoke, reqRevoke)
	if wRevoke.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from revoke-all, got %d", wRevoke.Code)
	}

	reqMCPRevoked := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	reqMCPRevoked.Header.Set("Authorization", "Bearer "+tokenA)
	reqMCPRevoked.Header.Set("Content-Type", "application/json")
	wMCPRevoked := httptest.NewRecorder()
	handler.ServeHTTP(wMCPRevoked, reqMCPRevoked)
	if wMCPRevoked.Code != http.StatusUnauthorized {
		t.Fatalf("SECURITY BUG: revoked JWT was accepted on /mcp! status=%d", wMCPRevoked.Code)
	}

	reqAuthzRevoked := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(authBody))
	reqAuthzRevoked.Header.Set("Content-Type", "application/json")
	reqAuthzRevoked.Header.Set("Authorization", "Bearer "+tokenA)
	wAuthzRevoked := httptest.NewRecorder()
	handler.ServeHTTP(wAuthzRevoked, reqAuthzRevoked)
	if wAuthzRevoked.Code != http.StatusUnauthorized {
		t.Fatalf("SECURITY BUG: revoked JWT was accepted on POST /oauth/authorize! status=%d", wAuthzRevoked.Code)
	}
}

func TestProjectSlugsAPIAndAppHosting(t *testing.T) {
	handler, repo, tm, user := setupTestServer(t)
	ctx := context.Background()

	tenantID := "tenant_slug_api"
	user.TenantID = &tenantID
	user.Role = domain.RoleAdmin
	_ = repo.UpdateUser(ctx, user)
	token, _ := tm.GenerateToken(user)

	// 1. Create collection with invalid slug -> 400 Bad Request
	invalidBody := `{"name":"Bad Slug","slug":"invalid_slug_with_underscore"}`
	reqInvalid := httptest.NewRequest("POST", "/api/collections", strings.NewReader(invalidBody))
	reqInvalid.Header.Set("Authorization", "Bearer "+token)
	reqInvalid.Header.Set("Content-Type", "application/json")
	wInvalid := httptest.NewRecorder()
	handler.ServeHTTP(wInvalid, reqInvalid)
	if wInvalid.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid slug, got %d: %s", wInvalid.Code, wInvalid.Body.String())
	}

	// 2. Create collection with valid mixed-case slug -> 201 Created with normalized lowercase slug
	validBody := `{"name":"Acme Portal","slug":"Acme-Portal-2026","default_visibility":"private"}`
	reqCreate := httptest.NewRequest("POST", "/api/collections", strings.NewReader(validBody))
	reqCreate.Header.Set("Authorization", "Bearer "+token)
	reqCreate.Header.Set("Content-Type", "application/json")
	wCreate := httptest.NewRecorder()
	handler.ServeHTTP(wCreate, reqCreate)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", wCreate.Code, wCreate.Body.String())
	}
	var createdCol domain.Collection
	_ = json.Unmarshal(wCreate.Body.Bytes(), &createdCol)
	if createdCol.Slug != "acme-portal-2026" {
		t.Fatalf("expected slug 'acme-portal-2026', got %q", createdCol.Slug)
	}

	// 3. Create another collection with duplicate slug -> 409 Conflict
	reqDup := httptest.NewRequest("POST", "/api/collections", strings.NewReader(`{"name":"Dup","slug":"ACME-PORTAL-2026"}`))
	reqDup.Header.Set("Authorization", "Bearer "+token)
	reqDup.Header.Set("Content-Type", "application/json")
	wDup := httptest.NewRecorder()
	handler.ServeHTTP(wDup, reqDup)
	if wDup.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for duplicate slug, got %d: %s", wDup.Code, wDup.Body.String())
	}

	// 4. Create & read records using the slug in the URL (case-insensitive)
	recBody := `{"title":"First Portal Item","content":"Created via slug URL"}`
	reqRecCreate := httptest.NewRequest("POST", "/api/collections/ACME-PORTAL-2026/records", strings.NewReader(recBody))
	reqRecCreate.Header.Set("Authorization", "Bearer "+token)
	reqRecCreate.Header.Set("Content-Type", "application/json")
	wRecCreate := httptest.NewRecorder()
	handler.ServeHTTP(wRecCreate, reqRecCreate)
	if wRecCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating record via slug URL, got %d: %s", wRecCreate.Code, wRecCreate.Body.String())
	}
	var createdRec domain.Record
	_ = json.Unmarshal(wRecCreate.Body.Bytes(), &createdRec)
	if createdRec.CollectionID != createdCol.ID {
		t.Fatalf("expected canonical CollectionID %q on record, got %q", createdCol.ID, createdRec.CollectionID)
	}

	reqRecGet := httptest.NewRequest("GET", "/api/collections/acme-portal-2026/records/"+createdRec.ID, nil)
	reqRecGet.Header.Set("Authorization", "Bearer "+token)
	wRecGet := httptest.NewRecorder()
	handler.ServeHTTP(wRecGet, reqRecGet)
	if wRecGet.Code != http.StatusOK {
		t.Fatalf("expected 200 OK getting record via slug URL, got %d", wRecGet.Code)
	}

	// 5. Publish app via slug URL and update slug inline via publish payload
	appHTML := `<!DOCTYPE html><html><body><h1>Acme App</h1></body></html>`
	pubPayload, _ := json.Marshal(map[string]string{
		"html": appHTML,
		"slug": "Acme-Hub",
	})
	reqPub := httptest.NewRequest("PUT", "/api/collections/acme-portal-2026/app", bytes.NewReader(pubPayload))
	reqPub.Host = "vibebac.kr"
	reqPub.Header.Set("Authorization", "Bearer "+token)
	reqPub.Header.Set("Content-Type", "application/json")
	wPub := httptest.NewRecorder()
	handler.ServeHTTP(wPub, reqPub)
	if wPub.Code != http.StatusOK {
		t.Fatalf("expected 200 OK publishing app with custom slug, got %d: %s", wPub.Code, wPub.Body.String())
	}

	var pubRes map[string]interface{}
	_ = json.Unmarshal(wPub.Body.Bytes(), &pubRes)
	if pubRes["slug"] != "acme-hub" || pubRes["app_url"] != "https://acme-hub.vibebac.kr" {
		t.Fatalf("unexpected publish response: %+v", pubRes)
	}

	// Both /apps/acme-hub and /apps/{dashless_uuid} should serve the HTML
	uuidSlug := strings.ReplaceAll(createdCol.ID, "-", "")
	for _, path := range []string{"/apps/acme-hub", "/apps/ACME-HUB", "/apps/" + uuidSlug} {
		reqApp := httptest.NewRequest("GET", path, nil)
		wApp := httptest.NewRecorder()
		handler.ServeHTTP(wApp, reqApp)
		if wApp.Code != http.StatusOK || wApp.Body.String() != appHTML {
			t.Fatalf("expected 200 OK with HTML from %s, got %d: %s", path, wApp.Code, wApp.Body.String())
		}
	}
}

func TestBillingEndpointsAndEnforcement(t *testing.T) {
	database, err := db.Connect("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db error: %v", err)
	}
	repo := repository.New(database)
	tm := auth.NewTokenManager("test-secret", 15*time.Minute)
	sm := auth.NewSSOManager(repo)
	se := search.NewSearchEngine(database, repo, nil)
	mcpServer := mcp.NewServer(repo, se)

	billingMod, err := billing.NewModule(database, billing.Config{
		Enabled:      true,
		SeedVouchers: "FREEPRO25:pro:5",
	})
	if err != nil {
		t.Fatalf("failed creating billing module: %v", err)
	}
	repo.SetWriteGuard(billingMod.CheckWriteAccess)
	mcpServer.SetBilling(billingMod)

	srv := api.NewServer(repo, tm, sm, se, mcpServer)
	srv.SetBilling(billingMod)
	handler := srv.Routes()

	tenantID := "tenant_billing_api"
	user := &domain.User{
		ID:       "user_billing_1",
		TenantID: &tenantID,
		Email:    "billing@example.com",
		Name:     "Billing User",
		Role:     domain.RoleAdmin,
	}
	_ = repo.CreateUser(context.Background(), user)
	token, _ := tm.GenerateToken(user)

	// 1. GET /api/billing/status -> trialing on Starter (10 collections)
	reqStatus := httptest.NewRequest("GET", "/api/billing/status", nil)
	reqStatus.Header.Set("Authorization", "Bearer "+token)
	wStatus := httptest.NewRecorder()
	handler.ServeHTTP(wStatus, reqStatus)
	if wStatus.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/billing/status, got %d: %s", wStatus.Code, wStatus.Body.String())
	}
	var st billing.StatusResponse
	_ = json.Unmarshal(wStatus.Body.Bytes(), &st)
	if !st.Enabled || st.Plan != "starter" || st.MaxCollections != 10 || st.ReadOnly {
		t.Fatalf("unexpected billing status: %+v", st)
	}

	// 2. Expire trial -> POST /api/collections should return 402 Payment Required
	tb, _ := billingMod.GetTenantBilling(context.Background(), tenantID)
	tb.TrialEndsAt = time.Now().UTC().Add(-2 * time.Hour)
	_ = billingMod.UpdateTenantBilling(context.Background(), tb)

	reqCreateLocked := httptest.NewRequest("POST", "/api/collections", strings.NewReader(`{"name":"Locked Col"}`))
	reqCreateLocked.Header.Set("Authorization", "Bearer "+token)
	reqCreateLocked.Header.Set("Content-Type", "application/json")
	wCreateLocked := httptest.NewRecorder()
	handler.ServeHTTP(wCreateLocked, reqCreateLocked)
	if wCreateLocked.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 Payment Required when trial expired, got %d: %s", wCreateLocked.Code, wCreateLocked.Body.String())
	}

	// 3. Redeem voucher via POST /api/billing/voucher -> unlocks Pro (25 collections)
	reqVoucher := httptest.NewRequest("POST", "/api/billing/voucher", strings.NewReader(`{"code":"freepro25"}`))
	reqVoucher.Header.Set("Authorization", "Bearer "+token)
	reqVoucher.Header.Set("Content-Type", "application/json")
	wVoucher := httptest.NewRecorder()
	handler.ServeHTTP(wVoucher, reqVoucher)
	if wVoucher.Code != http.StatusOK {
		t.Fatalf("expected 200 OK redeeming voucher, got %d: %s", wVoucher.Code, wVoucher.Body.String())
	}
	var stAfter billing.StatusResponse
	_ = json.Unmarshal(wVoucher.Body.Bytes(), &stAfter)
	if stAfter.BillingStatus != "free" || stAfter.Plan != "pro" || stAfter.MaxCollections != 25 || stAfter.ReadOnly || stAfter.VoucherCode != "FR****" {
		t.Fatalf("unexpected status after voucher redemption: %+v", stAfter)
	}

	// 4. Creating a collection now succeeds
	reqCreateUnlocked := httptest.NewRequest("POST", "/api/collections", strings.NewReader(`{"name":"Unlocked Col"}`))
	reqCreateUnlocked.Header.Set("Authorization", "Bearer "+token)
	reqCreateUnlocked.Header.Set("Content-Type", "application/json")
	wCreateUnlocked := httptest.NewRecorder()
	handler.ServeHTTP(wCreateUnlocked, reqCreateUnlocked)
	if wCreateUnlocked.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created after voucher unlock, got %d: %s", wCreateUnlocked.Code, wCreateUnlocked.Body.String())
	}
}

func TestRateLimitingAndSharedTenantInvitesAPI(t *testing.T) {
	database, err := db.Connect("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db error: %v", err)
	}
	repo := repository.New(database)
	tm := auth.NewTokenManager("test-secret", 15*time.Minute)
	sm := auth.NewSSOManager(repo)
	se := search.NewSearchEngine(database, repo, nil)
	mcpServer := mcp.NewServer(repo, se)

	billingMod, err := billing.NewModule(database, billing.Config{
		Enabled:      true,
		SeedVouchers: "PROBOOST:pro:5",
	})
	if err != nil {
		t.Fatalf("failed creating billing module: %v", err)
	}
	repo.SetWriteGuard(billingMod.CheckWriteAccess)

	rlMod := ratelimit.NewModule(
		ratelimit.Config{
			Enabled:    true,
			StarterRPM: 60,
			ProRPM:     180,
		},
		func(ctx context.Context, idOrSlug string) (string, string, error) {
			col, err := repo.GetCollection(ctx, idOrSlug)
			if err != nil {
				return "", "", err
			}
			return col.ID, col.TenantID, nil
		},
		func(ctx context.Context, tenantID string) string {
			if tb, err := billingMod.EnsureTenant(ctx, tenantID); err == nil && tb != nil {
				return tb.Plan
			}
			return "starter"
		},
	)

	mcpServer.SetBilling(billingMod)
	mcpServer.SetRateLimiter(rlMod)

	srv := api.NewServer(repo, tm, sm, se, mcpServer)
	srv.SetBilling(billingMod)
	srv.SetRateLimiter(rlMod)
	handler := srv.Routes()

	ctx := context.Background()
	ownerA, _ := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "ownerA@workspace.io", Name: "Owner A"}, true)
	collabB, _ := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "collabB@workspace.io", Name: "Collab B"}, true)
	tokenA, _ := tm.GenerateToken(ownerA)
	tokenB, _ := tm.GenerateToken(collabB)

	// 1. Collaborator B cannot create a collection inside Owner A's tenant before being invited
	unauthCreate := `{"name":"Unauthorized Shared Col","tenant_id":"` + *ownerA.TenantID + `"}`
	reqUnauth := httptest.NewRequest("POST", "/api/collections", strings.NewReader(unauthCreate))
	reqUnauth.Header.Set("Authorization", "Bearer "+tokenB)
	reqUnauth.Header.Set("Content-Type", "application/json")
	wUnauth := httptest.NewRecorder()
	handler.ServeHTTP(wUnauth, reqUnauth)
	if wUnauth.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden creating collection in uninvited tenant, got %d: %s", wUnauth.Code, wUnauth.Body.String())
	}

	// 2. Owner A invites Collaborator B as 'admin' to Tenant A via POST /api/invites
	invBody := `{"email":"collabB@workspace.io","role":"admin"}`
	reqInv := httptest.NewRequest("POST", "/api/invites", strings.NewReader(invBody))
	reqInv.Header.Set("Authorization", "Bearer "+tokenA)
	reqInv.Header.Set("Content-Type", "application/json")
	wInv := httptest.NewRecorder()
	handler.ServeHTTP(wInv, reqInv)
	if wInv.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created from POST /api/invites, got %d: %s", wInv.Code, wInv.Body.String())
	}
	var createdInv domain.Invite
	_ = json.Unmarshal(wInv.Body.Bytes(), &createdInv)

	// 3. GET /api/invites lists the invite
	reqListInv := httptest.NewRequest("GET", "/api/invites", nil)
	reqListInv.Header.Set("Authorization", "Bearer "+tokenA)
	wListInv := httptest.NewRecorder()
	handler.ServeHTTP(wListInv, reqListInv)
	if wListInv.Code != http.StatusOK || !strings.Contains(wListInv.Body.String(), "collabB@workspace.io") {
		t.Fatalf("expected GET /api/invites to list collabB@workspace.io, got %d: %s", wListInv.Code, wListInv.Body.String())
	}

	// 4. Because collabB already exists in users, CreateInvite bound TenantMembership immediately!
	// Collaborator B can now create a collection inside Owner A's tenant without logging out/in.
	sharedCreate := `{"name":"Shared Team Roadmap","slug":"team-roadmap","tenant_id":"` + *ownerA.TenantID + `"}`
	reqShared := httptest.NewRequest("POST", "/api/collections", strings.NewReader(sharedCreate))
	reqShared.Header.Set("Authorization", "Bearer "+tokenB)
	reqShared.Header.Set("Content-Type", "application/json")
	wShared := httptest.NewRecorder()
	handler.ServeHTTP(wShared, reqShared)
	if wShared.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created when invited admin creates collection in Tenant A, got %d: %s", wShared.Code, wShared.Body.String())
	}
	var sharedCol domain.Collection
	_ = json.Unmarshal(wShared.Body.Bytes(), &sharedCol)
	if sharedCol.TenantID != *ownerA.TenantID {
		t.Fatalf("expected shared collection to belong to Tenant A (%s), got %s", *ownerA.TenantID, sharedCol.TenantID)
	}

	// 5. Enforce Starter rate limit (60 RPM / burst 20) on GET /api/collections/team-roadmap/records
	for i := 0; i < 20; i++ {
		reqGet := httptest.NewRequest("GET", "/api/collections/team-roadmap/records", nil)
		reqGet.Header.Set("Authorization", "Bearer "+tokenA)
		wGet := httptest.NewRecorder()
		handler.ServeHTTP(wGet, reqGet)
		if wGet.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on burst request %d, got %d: %s", i+1, wGet.Code, wGet.Body.String())
		}
		if wGet.Header().Get("X-RateLimit-Limit") != "60" {
			t.Fatalf("expected X-RateLimit-Limit=60, got %s", wGet.Header().Get("X-RateLimit-Limit"))
		}
	}

	// 21st request hits 429 Too Many Requests
	reqRateLimited := httptest.NewRequest("GET", "/api/collections/team-roadmap/records", nil)
	reqRateLimited.Header.Set("Authorization", "Bearer "+tokenA)
	wRateLimited := httptest.NewRecorder()
	handler.ServeHTTP(wRateLimited, reqRateLimited)
	if wRateLimited.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on 21st burst request, got %d: %s", wRateLimited.Code, wRateLimited.Body.String())
	}
	if wRateLimited.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on 429 response")
	}

	// 6. Redeem Pro voucher on Tenant A -> invalidates rate-limit cache and boosts limit to 180 RPM (burst 45)
	reqVoucher := httptest.NewRequest("POST", "/api/billing/voucher", strings.NewReader(`{"code":"PROBOOST"}`))
	reqVoucher.Header.Set("Authorization", "Bearer "+tokenA)
	reqVoucher.Header.Set("Content-Type", "application/json")
	wVoucher := httptest.NewRecorder()
	handler.ServeHTTP(wVoucher, reqVoucher)
	if wVoucher.Code != http.StatusOK {
		t.Fatalf("expected 200 redeeming voucher, got %d", wVoucher.Code)
	}

	reqAfterPro := httptest.NewRequest("GET", "/api/collections/team-roadmap/records", nil)
	reqAfterPro.Header.Set("Authorization", "Bearer "+tokenA)
	wAfterPro := httptest.NewRecorder()
	handler.ServeHTTP(wAfterPro, reqAfterPro)
	if wAfterPro.Code != http.StatusOK || wAfterPro.Header().Get("X-RateLimit-Limit") != "180" {
		t.Fatalf("expected 200 OK and X-RateLimit-Limit=180 after Pro upgrade, got %d (limit=%s)", wAfterPro.Code, wAfterPro.Header().Get("X-RateLimit-Limit"))
	}

	// 7. DELETE /api/invites/{id} revokes the invite
	reqDelInv := httptest.NewRequest("DELETE", "/api/invites/"+createdInv.ID, nil)
	reqDelInv.Header.Set("Authorization", "Bearer "+tokenA)
	wDelInv := httptest.NewRecorder()
	handler.ServeHTTP(wDelInv, reqDelInv)
	if wDelInv.Code != http.StatusOK {
		t.Fatalf("expected 200 OK deleting invite, got %d: %s", wDelInv.Code, wDelInv.Body.String())
	}
}

func TestSelfHostedRootPage(t *testing.T) {
	handler, _, _, _ := setupTestServer(t)

	for _, path := range []string{"/", "/index.html"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on %s, got %d", path, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<h1 class="os-title">VibeBackr OS</h1>`) {
			t.Fatalf("expected default 'VibeBackr OS' title in %s response", path)
		}
		if !strings.Contains(body, `<p class="os-subtitle">Self-Hosted Open Source Instance</p>`) {
			t.Fatalf("expected default subtitle in %s response", path)
		}
		if !strings.Contains(body, `href="/login"`) {
			t.Fatalf("expected login button in %s response", path)
		}
		if !strings.Contains(body, `https://vibebac.kr`) {
			t.Fatalf("expected link back to main VibeBackr site in %s response", path)
		}
	}

	// Custom instance name via INSTANCE_NAME environment variable
	t.Setenv("INSTANCE_NAME", "Acme & Co.")
	reqCustom := httptest.NewRequest("GET", "/", nil)
	wCustom := httptest.NewRecorder()
	handler.ServeHTTP(wCustom, reqCustom)
	if wCustom.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with custom INSTANCE_NAME, got %d", wCustom.Code)
	}
	customBody := wCustom.Body.String()
	if !strings.Contains(customBody, `<h1 class="os-title">Acme &amp; Co.</h1>`) {
		t.Fatalf("expected escaped custom company name in heading, got: %s", customBody)
	}
	if !strings.Contains(customBody, `<p class="os-subtitle">Powered by VibeBackr OS</p>`) {
		t.Fatalf("expected 'Powered by VibeBackr OS' subtitle when custom name is set, got: %s", customBody)
	}

	// Verify /logo.svg and /favicon.svg serve the canonical SVG logo
	for _, svgPath := range []string{"/logo.svg", "/favicon.svg", "/favicon.ico"} {
		reqLogo := httptest.NewRequest("GET", svgPath, nil)
		wLogo := httptest.NewRecorder()
		handler.ServeHTTP(wLogo, reqLogo)
		if wLogo.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on %s, got %d", svgPath, wLogo.Code)
		}
		if !strings.Contains(wLogo.Header().Get("Content-Type"), "image/svg+xml") {
			t.Fatalf("expected image/svg+xml content-type on %s, got %s", svgPath, wLogo.Header().Get("Content-Type"))
		}
		if !strings.Contains(wLogo.Body.String(), `filter id="cyanGlow"`) {
			t.Fatalf("expected canonical SVG logo in %s response", svgPath)
		}
	}
}

func TestTenantPublicCollectionAndInviteOnlyAppSignIn(t *testing.T) {
	handler, repo, tm, _ := setupTestServer(t)
	sm := auth.NewSSOManager(repo)
	ctx := context.Background()

	// 1. Provision Owner A (Tenant A) and an uninvited user from another tenant (developer@example.com, returned by MockSSOProvider)
	ownerA, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "founder@tenant-a.io", Name: "Founder A", Sub: "sub-founder-a"}, true)
	if err != nil {
		t.Fatalf("failed provisioning ownerA: %v", err)
	}
	tokenOwnerA, _ := tm.GenerateToken(ownerA)

	outsider, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "developer@example.com", Name: "Test Developer", Sub: "mock-user-123"}, true)
	if err != nil {
		t.Fatalf("failed provisioning outsider: %v", err)
	}
	tokenOutsider, _ := tm.GenerateToken(outsider)

	// 2. Owner A creates a collection with default_visibility="public" and allowed_domains=["allowed-org.com"]
	colBody := `{"name":"Team Shared Board","slug":"team-board","default_visibility":"public","allowed_domains":["allowed-org.com"]}`
	reqCreateCol := httptest.NewRequest("POST", "/api/collections", strings.NewReader(colBody))
	reqCreateCol.Header.Set("Authorization", "Bearer "+tokenOwnerA)
	reqCreateCol.Header.Set("Content-Type", "application/json")
	wCreateCol := httptest.NewRecorder()
	handler.ServeHTTP(wCreateCol, reqCreateCol)
	if wCreateCol.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for collection, got %d: %s", wCreateCol.Code, wCreateCol.Body.String())
	}
	var createdCol domain.Collection
	_ = json.Unmarshal(wCreateCol.Body.Bytes(), &createdCol)

	// 3. Owner A creates a record without specifying visibility -> inherits "public" from collection default_visibility
	recBody := `{"title":"Sprint Goal","content":"Launch invite-only public board","data":{"priority":"high"}}`
	reqCreateRec := httptest.NewRequest("POST", "/api/collections/"+createdCol.ID+"/records", strings.NewReader(recBody))
	reqCreateRec.Header.Set("Authorization", "Bearer "+tokenOwnerA)
	reqCreateRec.Header.Set("Content-Type", "application/json")
	wCreateRec := httptest.NewRecorder()
	handler.ServeHTTP(wCreateRec, reqCreateRec)
	if wCreateRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for record, got %d: %s", wCreateRec.Code, wCreateRec.Body.String())
	}
	var createdRec domain.Record
	_ = json.Unmarshal(wCreateRec.Body.Bytes(), &createdRec)
	if createdRec.AccessPolicy.Visibility != domain.VisibilityPublic {
		t.Fatalf("expected record to inherit visibility 'public', got %q", createdRec.AccessPolicy.Visibility)
	}

	// 4. Invited member in Tenant A can see all public records by default
	_ = repo.CreateInvite(ctx, &domain.Invite{
		TenantID:  *ownerA.TenantID,
		Email:     "member@partner.io",
		Role:      domain.RoleMember,
		CreatedBy: ownerA.ID,
	})
	invitedMember, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "member@partner.io", Name: "Invited Member", Sub: "sub-member"}, false)
	if err != nil {
		t.Fatalf("failed provisioning invitedMember: %v", err)
	}
	tokenInvited, _ := tm.GenerateToken(invitedMember)

	reqInvitedList := httptest.NewRequest("GET", "/api/collections/"+createdCol.ID+"/records", nil)
	reqInvitedList.Header.Set("Authorization", "Bearer "+tokenInvited)
	wInvitedList := httptest.NewRecorder()
	handler.ServeHTTP(wInvitedList, reqInvitedList)
	if wInvitedList.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for invited member listing records, got %d: %s", wInvitedList.Code, wInvitedList.Body.String())
	}
	var invitedRecs []domain.Record
	_ = json.Unmarshal(wInvitedList.Body.Bytes(), &invitedRecs)
	if len(invitedRecs) != 1 || invitedRecs[0].ID != createdRec.ID {
		t.Fatalf("expected invited member to see the public record by default, got %+v", invitedRecs)
	}

	// 5. User in allowed_domains ("allowed-org.com") can also read public records in createdCol
	domainUser, err := sm.ProvisionOrGetUser(ctx, &auth.UserInfo{Email: "peer@allowed-org.com", Name: "Domain Peer", Sub: "sub-domain"}, false)
	if err != nil {
		t.Fatalf("failed provisioning domainUser: %v", err)
	}
	tokenDomain, _ := tm.GenerateToken(domainUser)

	reqDomainList := httptest.NewRequest("GET", "/api/collections/"+createdCol.ID+"/records", nil)
	reqDomainList.Header.Set("Authorization", "Bearer "+tokenDomain)
	wDomainList := httptest.NewRecorder()
	handler.ServeHTTP(wDomainList, reqDomainList)
	if wDomainList.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for allowed_domains user listing records, got %d: %s", wDomainList.Code, wDomainList.Body.String())
	}
	var domainRecs []domain.Record
	_ = json.Unmarshal(wDomainList.Body.Bytes(), &domainRecs)
	if len(domainRecs) != 1 || domainRecs[0].ID != createdRec.ID {
		t.Fatalf("expected allowed_domains user to see the public record, got %+v", domainRecs)
	}

	// 6. Uninvited user from another tenant (outsider: developer@example.com) is denied access to createdCol and its public records
	reqOutsiderCol := httptest.NewRequest("GET", "/api/collections/"+createdCol.ID, nil)
	reqOutsiderCol.Header.Set("Authorization", "Bearer "+tokenOutsider)
	wOutsiderCol := httptest.NewRecorder()
	handler.ServeHTTP(wOutsiderCol, reqOutsiderCol)
	if wOutsiderCol.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when uninvited user gets collection, got %d", wOutsiderCol.Code)
	}

	reqOutsiderList := httptest.NewRequest("GET", "/api/collections/"+createdCol.ID+"/records", nil)
	reqOutsiderList.Header.Set("Authorization", "Bearer "+tokenOutsider)
	wOutsiderList := httptest.NewRecorder()
	handler.ServeHTTP(wOutsiderList, reqOutsiderList)
	if wOutsiderList.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when uninvited user lists public records in another tenant, got %d: %s", wOutsiderList.Code, wOutsiderList.Body.String())
	}

	reqOutsiderGetRec := httptest.NewRequest("GET", "/api/collections/"+createdCol.ID+"/records/"+createdRec.ID, nil)
	reqOutsiderGetRec.Header.Set("Authorization", "Bearer "+tokenOutsider)
	wOutsiderGetRec := httptest.NewRecorder()
	handler.ServeHTTP(wOutsiderGetRec, reqOutsiderGetRec)
	if wOutsiderGetRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when uninvited user reads public record in another tenant, got %d", wOutsiderGetRec.Code)
	}

	reqOutsiderWrite := httptest.NewRequest("POST", "/api/collections/"+createdCol.ID+"/records", strings.NewReader(`{"title":"Unauthorized"}`))
	reqOutsiderWrite.Header.Set("Authorization", "Bearer "+tokenOutsider)
	reqOutsiderWrite.Header.Set("Content-Type", "application/json")
	wOutsiderWrite := httptest.NewRecorder()
	handler.ServeHTTP(wOutsiderWrite, reqOutsiderWrite)
	if wOutsiderWrite.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when uninvited user creates record in another tenant, got %d", wOutsiderWrite.Code)
	}

	// 7. Uninvited user (even though already registered in Tenant B) cannot sign into Tenant A's app via SSO
	reqAppLogin := httptest.NewRequest("GET", "/auth/login/mock?redirect_uri=/apps/team-board&collection_id="+createdCol.ID, nil)
	wAppLogin := httptest.NewRecorder()
	handler.ServeHTTP(wAppLogin, reqAppLogin)
	if wAppLogin.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect from /auth/login/mock, got %d", wAppLogin.Code)
	}
	cbURL := wAppLogin.Header().Get("Location")

	reqAppCB := httptest.NewRequest("GET", cbURL, nil)
	wAppCB := httptest.NewRecorder()
	handler.ServeHTTP(wAppCB, reqAppCB)
	if wAppCB.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect from /auth/callback/mock, got %d", wAppCB.Code)
	}
	deniedLoc := wAppCB.Header().Get("Location")
	if !strings.Contains(deniedLoc, "error=not_registered") || !strings.Contains(deniedLoc, "app=true") || strings.Contains(deniedLoc, "access_token=") {
		t.Fatalf("expected uninvited user to be rejected with error=not_registered&app=true, got Location: %s", deniedLoc)
	}

	// 8. Once Owner A invites developer@example.com to Tenant A, app SSO sign-in succeeds and records are visible
	_ = repo.CreateInvite(ctx, &domain.Invite{
		TenantID:  *ownerA.TenantID,
		Email:     "developer@example.com",
		Role:      domain.RoleMember,
		CreatedBy: ownerA.ID,
	})

	reqAppCBInvited := httptest.NewRequest("GET", cbURL, nil)
	wAppCBInvited := httptest.NewRecorder()
	handler.ServeHTTP(wAppCBInvited, reqAppCBInvited)
	if wAppCBInvited.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect after invite, got %d", wAppCBInvited.Code)
	}
	successLoc := wAppCBInvited.Header().Get("Location")
	if !strings.HasPrefix(successLoc, "/apps/team-board#access_token=") {
		t.Fatalf("expected invited user to receive access_token on app callback, got Location: %s", successLoc)
	}

	reqAfterInviteList := httptest.NewRequest("GET", "/api/collections/"+createdCol.ID+"/records", nil)
	reqAfterInviteList.Header.Set("Authorization", "Bearer "+tokenOutsider)
	wAfterInviteList := httptest.NewRecorder()
	handler.ServeHTTP(wAfterInviteList, reqAfterInviteList)
	if wAfterInviteList.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after invite when listing records, got %d: %s", wAfterInviteList.Code, wAfterInviteList.Body.String())
	}
	var afterInviteRecs []domain.Record
	_ = json.Unmarshal(wAfterInviteList.Body.Bytes(), &afterInviteRecs)
	if len(afterInviteRecs) != 1 || afterInviteRecs[0].ID != createdRec.ID {
		t.Fatalf("expected newly invited user to see all public records, got %+v", afterInviteRecs)
	}
}


