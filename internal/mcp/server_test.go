package mcp_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eliotstocker/vibebackr/internal/billing"
	"github.com/eliotstocker/vibebackr/internal/db"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/mcp"
	"github.com/eliotstocker/vibebackr/internal/ratelimit"
	"github.com/eliotstocker/vibebackr/internal/repository"
	"github.com/eliotstocker/vibebackr/internal/search"
)

func TestMCPServerTools(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:mcptest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)
	server := mcp.NewServer(repo, searchEngine)

	admin := &domain.User{ID: "admin_1", Email: "admin@corp.com", Role: domain.RoleAdmin}
	_ = repo.CreateUser(ctx, admin)

	// Test 1: tools/list
	listReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/list",
	}
	resp := server.HandleRPC(ctx, listReq, admin)
	if resp.Error != nil {
		t.Fatalf("tools/list failed: %v", resp.Error)
	}

	// Test 2: create_collection via MCP
	createColReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "create_collection",
			"arguments": {
				"name": "Engineering Knowledgebase",
				"default_visibility": "shared",
				"allowed_domains": ["company.com"]
			}
		}`),
	}
	createResp := server.HandleRPC(ctx, createColReq, admin)
	if createResp.Error != nil {
		t.Fatalf("create_collection failed: %v", createResp.Error)
	}

	// Test 2b: update_collection via MCP
	updateColReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      22,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "update_collection",
			"arguments": {
				"collection_id": "Engineering Knowledgebase",
				"origin_urls": ["http://localhost:5173"],
				"vector_search_enabled": true
			}
		}`),
	}
	updateColResp := server.HandleRPC(ctx, updateColReq, admin)
	if updateColResp.Error != nil {
		t.Fatalf("update_collection failed: %v", updateColResp.Error)
	}

	// Test 2c: publish_app via MCP
	publishAppReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      23,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "publish_app",
			"arguments": {
				"collection_id": "Engineering Knowledgebase",
				"html": "<!DOCTYPE html><html><body><h1>Engineering App</h1></body></html>"
			}
		}`),
	}
	publishAppResp := server.HandleRPC(ctx, publishAppReq, admin)
	if publishAppResp.Error != nil {
		t.Fatalf("publish_app failed: %v", publishAppResp.Error)
	}
	pubResultBytes, _ := json.Marshal(publishAppResp.Result)
	if !strings.Contains(string(pubResultBytes), ".vibebac.kr") {
		t.Fatalf("expected published app URL in response, got: %s", string(pubResultBytes))
	}

	// Test 3: sync_org_directory via MCP
	syncReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "sync_org_directory",
			"arguments": {
				"users": [
					{"email": "manager@company.com", "name": "Manager Bob", "role": "manager"},
					{"email": "dev@company.com", "name": "Dev Alice", "role": "member", "manager_email": "manager@company.com"}
				]
			}
		}`),
	}
	syncResp := server.HandleRPC(ctx, syncReq, admin)
	if syncResp.Error != nil {
		t.Fatalf("sync_org_directory failed: %v", syncResp.Error)
	}

	// Test 4: batch_create_invites via MCP
	invReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      4,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "batch_create_invites",
			"arguments": {
				"invites": [
					{"email": "newhire@company.com", "role": "member", "manager_email": "manager@company.com"}
				]
			}
		}`),
	}
	invResp := server.HandleRPC(ctx, invReq, admin)
	if invResp.Error != nil {
		t.Fatalf("batch_create_invites failed: %v", invResp.Error)
	}

	// Test 5: list_collections
	listColReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      5,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name": "list_collections", "arguments": {}}`),
	}
	listColResp := server.HandleRPC(ctx, listColReq, admin)
	if listColResp.Error != nil {
		t.Fatalf("list_collections failed: %v", listColResp.Error)
	}

	// Test 6: create_record
	createRecReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      6,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "create_record",
			"arguments": {
				"collection_id": "test_col_mcp",
				"title": "MCP Integration Doc",
				"content": "Testing MCP record creation",
				"visibility": "public"
			}
		}`),
	}
	// Create collection first
	_ = repo.CreateCollection(ctx, &domain.Collection{ID: "test_col_mcp", Name: "MCP Col"})
	createRecResp := server.HandleRPC(ctx, createRecReq, admin)
	if createRecResp.Error != nil {
		t.Fatalf("create_record failed: %v", createRecResp.Error)
	}

	// Test 7: query_records
	queryRecReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      7,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "query_records",
			"arguments": {
				"collection_id": "test_col_mcp"
			}
		}`),
	}
	queryRecResp := server.HandleRPC(ctx, queryRecReq, admin)
	if queryRecResp.Error != nil {
		t.Fatalf("query_records failed: %v", queryRecResp.Error)
	}

	// Test 8: search_records
	searchRecReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      8,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "search_records",
			"arguments": {
				"collection_id": "test_col_mcp",
				"query": "Integration"
			}
		}`),
	}
	searchRecResp := server.HandleRPC(ctx, searchRecReq, admin)
	if searchRecResp.Error != nil {
		t.Fatalf("search_records failed: %v", searchRecResp.Error)
	}

	// Test 9: get_client_integration_guide (general)
	guideReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      9,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "get_client_integration_guide",
			"arguments": {
				"topic": "all"
			}
		}`),
	}
	guideResp := server.HandleRPC(ctx, guideReq, admin)
	if guideResp.Error != nil {
		t.Fatalf("get_client_integration_guide failed: %v", guideResp.Error)
	}

	// Test 10: get_client_integration_guide with collection_id introspection
	guideColReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      10,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "get_client_integration_guide",
			"arguments": {
				"topic": "typescript_client",
				"collection_id": "test_col_mcp"
			}
		}`),
	}
	guideColResp := server.HandleRPC(ctx, guideColReq, admin)
	if guideColResp.Error != nil {
		t.Fatalf("get_client_integration_guide with collection failed: %v", guideColResp.Error)
	}
}

func TestDynamicInstructionsAndBaseURL(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:dynamicurltest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)
	server := mcp.NewServer(repo, searchEngine)

	admin := &domain.User{ID: "admin_test", Email: "admin@corp.com", Role: domain.RoleAdmin}

	// 1. Default canonical URL should be https://vibebac.kr
	initReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2024-11-05"}`),
	}
	initResp := server.HandleRPC(ctx, initReq, admin)
	if initResp.Error != nil {
		t.Fatalf("initialize failed: %v", initResp.Error)
	}
	resMap, ok := initResp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map result, got %T", initResp.Result)
	}
	instructions, _ := resMap["instructions"].(string)
	if !strings.Contains(instructions, "https://vibebac.kr") {
		t.Fatalf("expected instructions to contain default canonical URL https://vibebac.kr, got: %s", instructions)
	}
	if !strings.Contains(instructions, "VibeBackr") {
		t.Fatalf("expected instructions to describe the VibeBackr service, got: %s", instructions)
	}

	extractToolText := func(resp mcp.JSONRPCResponse) string {
		b, _ := json.Marshal(resp.Result)
		var res struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(b, &res)
		if len(res.Content) > 0 {
			return res.Content[0].Text
		}
		return ""
	}

	// 2. Guide tool output should contain canonical URL
	guideReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "get_client_integration_guide",
			"arguments": { "topic": "all" }
		}`),
	}
	guideResp := server.HandleRPC(ctx, guideReq, admin)
	guideText := extractToolText(guideResp)
	if !strings.Contains(guideText, "https://vibebac.kr") {
		t.Fatalf("expected guide to contain default canonical URL https://vibebac.kr, got: %s", guideText)
	}

	// 3. Dynamic custom URL via SetBaseURL
	customURL := "https://mycustomvibebackr.internal"
	server.SetBaseURL(customURL)

	initCustomResp := server.HandleRPC(ctx, initReq, admin)
	resCustomMap := initCustomResp.Result.(map[string]interface{})
	customInstructions := resCustomMap["instructions"].(string)
	if !strings.Contains(customInstructions, customURL) {
		t.Fatalf("expected custom URL %s in instructions, got: %s", customURL, customInstructions)
	}

	guideCustomResp := server.HandleRPC(ctx, guideReq, admin)
	guideCustomText := extractToolText(guideCustomResp)
	if !strings.Contains(guideCustomText, customURL) {
		t.Fatalf("expected custom URL %s in guide, got: %s", customURL, guideCustomText)
	}
	if !strings.Contains(guideCustomText, "baseUrl = '"+customURL+"'") {
		t.Fatalf("expected client template to default to %s, got: %s", customURL, guideCustomText)
	}

	// 4. Request context dynamic override
	ctxOverrideURL := "https://request-override.org"
	ctxOverride := context.WithValue(ctx, mcp.BaseURLContextKey, ctxOverrideURL)

	initOverrideResp := server.HandleRPC(ctxOverride, initReq, admin)
	resOverrideMap := initOverrideResp.Result.(map[string]interface{})
	overrideInstructions := resOverrideMap["instructions"].(string)
	if !strings.Contains(overrideInstructions, ctxOverrideURL) {
		t.Fatalf("expected override URL %s in instructions, got: %s", ctxOverrideURL, overrideInstructions)
	}

	guideOverrideResp := server.HandleRPC(ctxOverride, guideReq, admin)
	guideOverrideText := extractToolText(guideOverrideResp)
	if !strings.Contains(guideOverrideText, ctxOverrideURL) {
		t.Fatalf("expected override URL %s in guide, got: %s", ctxOverrideURL, guideOverrideText)
	}
}

func TestStreamableHTTP(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:streamtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)
	server := mcp.NewServer(repo, searchEngine)

	admin := &domain.User{ID: "admin_stream", Email: "stream@corp.com", Role: domain.RoleAdmin}
	_ = repo.CreateUser(ctx, admin)

	// 1. POST /mcp with standard JSON (Accept: application/json)
	t.Run("Standard JSON POST", func(t *testing.T) {
		reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()

		server.ServeHTTP(w, req, admin)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("expected application/json, got %s", ct)
		}
		sessionID := w.Header().Get("Mcp-Session-Id")
		if sessionID == "" {
			t.Fatalf("expected Mcp-Session-Id header")
		}
	})

	// 2. POST /mcp with Streamable HTTP (Accept: text/event-stream)
	t.Run("Streamable HTTP POST", func(t *testing.T) {
		reqBody := `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Mcp-Session-Id", "custom-session-123")
		w := httptest.NewRecorder()

		server.ServeHTTP(w, req, admin)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("expected text/event-stream, got %s", ct)
		}
		if sid := w.Header().Get("Mcp-Session-Id"); sid != "custom-session-123" {
			t.Fatalf("expected session ID custom-session-123, got %s", sid)
		}
		body := w.Body.String()
		if !strings.Contains(body, "event: message\ndata: ") || !strings.Contains(body, "2025-03-26") {
			t.Fatalf("expected SSE message with protocol version, got: %s", body)
		}
	})

	// 3. POST /mcp with Notification (id == nil) -> 202 Accepted
	t.Run("Notification POST returns 202", func(t *testing.T) {
		reqBody := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		server.ServeHTTP(w, req, admin)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted for notification, got %d", w.Code)
		}
	})

	// 4. POST /mcp with Batch Requests
	t.Run("Batch JSON POST", func(t *testing.T) {
		reqBody := `[{"jsonrpc":"2.0","id":10,"method":"ping"},{"jsonrpc":"2.0","id":11,"method":"ping"}]`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		server.ServeHTTP(w, req, admin)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resps []mcp.JSONRPCResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resps); err != nil || len(resps) != 2 {
			t.Fatalf("expected 2 responses in batch, got error %v or count %d", err, len(resps))
		}
	})

	// 5. Legacy SSE Bridging via in-memory ResponseRecorder
	t.Run("Legacy SSE Stream and POST Bridge", func(t *testing.T) {
		sseCtx, cancelSSE := context.WithCancel(context.Background())
		defer cancelSSE()

		wSSE := httptest.NewRecorder()
		sseReq := httptest.NewRequest(http.MethodGet, "/mcp?sessionId=sse-bridge-session", nil).WithContext(sseCtx)

		sseDone := make(chan struct{})
		go func() {
			server.ServeHTTP(wSSE, sseReq, admin)
			close(sseDone)
		}()

		// Allow endpoint event to flush and client registration to take place
		time.Sleep(50 * time.Millisecond)

		if wSSE.Header().Get("Content-Type") != "text/event-stream" {
			t.Fatalf("expected text/event-stream, got %s", wSSE.Header().Get("Content-Type"))
		}
		if !strings.Contains(wSSE.Body.String(), "event: endpoint") {
			t.Fatalf("expected endpoint event in SSE body")
		}

		// Send POST /mcp?sessionId=... request
		postReqBody := `{"jsonrpc":"2.0","id":99,"method":"ping"}`
		postReq := httptest.NewRequest(http.MethodPost, "/mcp?sessionId=sse-bridge-session", strings.NewReader(postReqBody))
		postReq.Header.Set("Content-Type", "application/json")
		wPost := httptest.NewRecorder()

		server.ServeHTTP(wPost, postReq, admin)

		if wPost.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted for bridged POST, got %d", wPost.Code)
		}

		// Wait briefly for response to be written to SSE stream
		time.Sleep(50 * time.Millisecond)
		cancelSSE()
		<-sseDone

		if !strings.Contains(wSSE.Body.String(), `"id":99`) {
			t.Fatalf("expected response with id:99 on SSE stream, got: %s", wSSE.Body.String())
		}

		// 6. DELETE /mcp terminates session
		delReq := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
		delReq.Header.Set("Mcp-Session-Id", "sse-bridge-session")
		wDel := httptest.NewRecorder()
		server.ServeHTTP(wDel, delReq, admin)
		if wDel.Code != http.StatusNoContent {
			t.Fatalf("expected 204 No Content for DELETE, got %d", wDel.Code)
		}
	})

	// 7. Streamable HTTP 2025-06-18: GET SSE notification stream does not emit legacy endpoint event or hijack POST responses
	t.Run("Streamable HTTP 2025-06-18 Concurrent GET and POST", func(t *testing.T) {
		sseCtx, cancelSSE := context.WithCancel(context.Background())
		defer cancelSSE()

		wSSE := httptest.NewRecorder()
		sseReq := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(sseCtx)
		sseReq.Header.Set("Mcp-Session-Id", "streamable-2025-06-18-session")
		sseReq.Header.Set("MCP-Protocol-Version", "2025-06-18")

		sseDone := make(chan struct{})
		go func() {
			server.ServeHTTP(wSSE, sseReq, admin)
			close(sseDone)
		}()

		time.Sleep(50 * time.Millisecond)

		if strings.Contains(wSSE.Body.String(), "event: endpoint") {
			t.Fatalf("expected Streamable HTTP GET stream NOT to emit legacy event: endpoint, got: %s", wSSE.Body.String())
		}

		// Subsequent POST with Mcp-Session-Id header and Accept: application/json, text/event-stream must return 200 OK directly
		postReqBody := `{"jsonrpc":"2.0","id":105,"method":"ping"}`
		postReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(postReqBody))
		postReq.Header.Set("Content-Type", "application/json")
		postReq.Header.Set("Accept", "application/json, text/event-stream")
		postReq.Header.Set("Mcp-Session-Id", "streamable-2025-06-18-session")
		postReq.Header.Set("MCP-Protocol-Version", "2025-06-18")
		wPost := httptest.NewRecorder()

		server.ServeHTTP(wPost, postReq, admin)

		if wPost.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for Streamable HTTP POST, got %d", wPost.Code)
		}
		if wPost.Header().Get("MCP-Protocol-Version") != "2025-06-18" {
			t.Fatalf("expected MCP-Protocol-Version 2025-06-18, got %s", wPost.Header().Get("MCP-Protocol-Version"))
		}
		if !strings.Contains(wPost.Body.String(), `"id":105`) {
			t.Fatalf("expected direct JSON-RPC response on POST, got: %s", wPost.Body.String())
		}

		cancelSSE()
		<-sseDone
	})
}

func TestBuildMCPB(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:mcpbtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	defer database.Close()

	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)
	server := mcp.NewServer(repo, searchEngine)

	bundleBytes, err := server.BuildMCPB(ctx)
	if err != nil {
		t.Fatalf("BuildMCPB failed: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(bundleBytes), int64(len(bundleBytes)))
	if err != nil {
		t.Fatalf("invalid zip archive: %v", err)
	}

	files := make(map[string][]byte)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("failed opening %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("failed reading %s: %v", f.Name, err)
		}
		files[f.Name] = data
	}

	manifestRaw, ok := files["manifest.json"]
	if !ok {
		t.Fatalf("manifest.json missing from .mcpb archive")
	}
	var manifest map[string]interface{}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("manifest.json invalid JSON: %v", err)
	}
	if manifest["manifest_version"] != "0.3" {
		t.Fatalf("expected manifest_version 0.3, got %v", manifest["manifest_version"])
	}
	if manifest["name"] != "vibebackr" {
		t.Fatalf("expected name vibebackr, got %v", manifest["name"])
	}

	bridgeJS, ok := files["server/index.js"]
	if !ok || !strings.Contains(string(bridgeJS), "https://vibebac.kr") {
		t.Fatalf("server/index.js missing or invalid in .mcpb archive")
	}
	if logoSVG, ok := files["logo.svg"]; !ok || !strings.Contains(string(logoSVG), `filter id="cyanGlow"`) {
		t.Fatalf("logo.svg missing or invalid in .mcpb archive")
	}

	// Keep static web/vibebackr.mcpb and web/dist/vibebackr.mcpb synced
	_ = os.WriteFile("../../web/vibebackr.mcpb", bundleBytes, 0644)
	_ = os.WriteFile("../../web/dist/vibebackr.mcpb", bundleBytes, 0644)
}

func TestMCPProjectSlugs(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:mcpslugtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	defer database.Close()

	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)
	server := mcp.NewServer(repo, searchEngine)

	tenantID := "tenant_mcp_slug"
	admin := &domain.User{
		ID:       "admin_mcp_slug",
		TenantID: &tenantID,
		Email:    "admin@mcpslug.com",
		Role:     domain.RoleAdmin,
	}
	_ = repo.CreateUser(ctx, admin)

	// 1. create_collection with custom slug
	createColReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "create_collection",
			"arguments": {
				"name": "Kanban Board",
				"slug": "Team-Kanban-2026"
			}
		}`),
	}
	createColResp := server.HandleRPC(ctx, createColReq, admin)
	if createColResp.Error != nil {
		t.Fatalf("create_collection with slug failed: %v", createColResp.Error)
	}
	col, err := repo.GetCollection(ctx, "TEAM-KANBAN-2026")
	if err != nil || col.Slug != "team-kanban-2026" {
		t.Fatalf("expected collection with normalized slug 'team-kanban-2026', got %+v, err=%v", col, err)
	}

	// 2. create_record and query_records using project slug
	createRecReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "create_record",
			"arguments": {
				"collection_id": "TEAM-KANBAN-2026",
				"title": "Ship Project Slugs",
				"content": "Done"
			}
		}`),
	}
	createRecResp := server.HandleRPC(ctx, createRecReq, admin)
	if createRecResp.Error != nil {
		t.Fatalf("create_record via slug failed: %v", createRecResp.Error)
	}

	queryRecReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "query_records",
			"arguments": {
				"collection_id": "team-kanban-2026"
			}
		}`),
	}
	queryRecResp := server.HandleRPC(ctx, queryRecReq, admin)
	if queryRecResp.Error != nil {
		t.Fatalf("query_records via slug failed: %v", queryRecResp.Error)
	}
	b, _ := json.Marshal(queryRecResp.Result)
	if !strings.Contains(string(b), "Ship Project Slugs") {
		t.Fatalf("expected query_records via slug to return created record, got: %s", string(b))
	}
}

func TestMCPBillingTools(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:mcpbillingtest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)
	server := mcp.NewServer(repo, searchEngine)

	// When billing module is not set/enabled, tools/list must NOT include get_billing_status
	listReq := mcp.JSONRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}
	tenantID := "tenant_mcp_bill"
	admin := &domain.User{ID: "admin_bill", TenantID: &tenantID, Email: "bill@corp.com", Role: domain.RoleAdmin}
	_ = repo.CreateUser(ctx, admin)

	listDisabled := server.HandleRPC(ctx, listReq, admin)
	disabledBytes, _ := json.Marshal(listDisabled.Result)
	if strings.Contains(string(disabledBytes), "get_billing_status") {
		t.Fatalf("expected billing tools to be omitted when module is disabled")
	}

	// Enable billing module with a seeded voucher
	billingMod, err := billing.NewModule(database, billing.Config{
		Enabled:      true,
		SeedVouchers: "MCPVIP:pro:3",
	})
	if err != nil {
		t.Fatalf("NewModule error: %v", err)
	}
	repo.SetWriteGuard(billingMod.CheckWriteAccess)
	server.SetBilling(billingMod)

	// Now tools/list MUST include get_billing_status, upgrade_tenant_plan, redeem_voucher
	listEnabled := server.HandleRPC(ctx, listReq, admin)
	enabledBytes, _ := json.Marshal(listEnabled.Result)
	for _, toolName := range []string{"get_billing_status", "upgrade_tenant_plan", "redeem_voucher"} {
		if !strings.Contains(string(enabledBytes), toolName) {
			t.Fatalf("expected %s in tools/list when billing module is enabled", toolName)
		}
	}

	// Call redeem_voucher tool via MCP
	redeemReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "redeem_voucher",
			"arguments": { "code": "mcpvip" }
		}`),
	}
	redeemResp := server.HandleRPC(ctx, redeemReq, admin)
	if redeemResp.Error != nil {
		t.Fatalf("redeem_voucher failed: %v", redeemResp.Error)
	}
	redeemBytes, _ := json.Marshal(redeemResp.Result)
	if !strings.Contains(string(redeemBytes), `25`) || !strings.Contains(string(redeemBytes), `MC****`) || strings.Contains(string(redeemBytes), `MCPVIP`) {
		t.Fatalf("unexpected redeem_voucher result: %s", string(redeemBytes))
	}
}

func TestMCPRateLimitingAndSharedTenantCreation(t *testing.T) {
	ctx := context.Background()
	database, err := db.Connect("sqlite", "file:mcpratelimittest?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	repo := repository.New(database)
	searchEngine := search.NewSearchEngine(database, repo, nil)
	server := mcp.NewServer(repo, searchEngine)

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
		nil,
	)
	server.SetRateLimiter(rlMod)

	tenantA := "tenant_mcp_alpha"
	ownerA := &domain.User{ID: "owner_a", TenantID: &tenantA, Email: "owner@alpha.io", Role: domain.RoleAdmin}
	_ = repo.CreateUser(ctx, ownerA)

	tenantB := "tenant_mcp_beta"
	collabB := &domain.User{ID: "collab_b", TenantID: &tenantB, Email: "collab@beta.io", Role: domain.RoleAdmin}
	_ = repo.CreateUser(ctx, collabB)

	// 1. Owner A invites Collab B as manager via MCP batch_create_invites
	invReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "batch_create_invites",
			"arguments": {
				"invites": [
					{"email": "collab@beta.io", "role": "manager"}
				]
			}
		}`),
	}
	invResp := server.HandleRPC(ctx, invReq, ownerA)
	if invResp.Error != nil {
		t.Fatalf("batch_create_invites failed: %v", invResp.Error)
	}

	// 2. Collab B creates a collection inside Owner A's tenant via MCP create_collection with tenant_id
	createColReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "create_collection",
			"arguments": {
				"name": "Shared MCP Tasks",
				"slug": "shared-mcp-tasks",
				"tenant_id": "tenant_mcp_alpha"
			}
		}`),
	}
	createColResp := server.HandleRPC(ctx, createColReq, collabB)
	if createColResp.Error != nil {
		t.Fatalf("expected invited manager collabB to create collection in tenant_mcp_alpha, got error: %v", createColResp.Error)
	}
	col, err := repo.GetCollection(ctx, "shared-mcp-tasks")
	if err != nil || col.TenantID != tenantA {
		t.Fatalf("expected collection to belong to %s, got %+v (err=%v)", tenantA, col, err)
	}

	// 3. Burst 20 query_records calls succeed, 21st is rate-limited
	queryReq := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: json.RawMessage(`{
			"name": "query_records",
			"arguments": {
				"collection_id": "shared-mcp-tasks"
			}
		}`),
	}
	for i := 0; i < 20; i++ {
		r := server.HandleRPC(ctx, queryReq, ownerA)
		resBytes, _ := json.Marshal(r.Result)
		if r.Error != nil || strings.Contains(string(resBytes), `"isError":true`) {
			t.Fatalf("expected query %d to succeed, got err=%v result=%s", i+1, r.Error, string(resBytes))
		}
	}
	rLimited := server.HandleRPC(ctx, queryReq, ownerA)
	limBytes, _ := json.Marshal(rLimited.Result)
	if !strings.Contains(string(limBytes), `"isError":true`) || !strings.Contains(string(limBytes), "rate limit exceeded") {
		t.Fatalf("expected 21st MCP query_records call to fail with rate limit exceeded, got %s", string(limBytes))
	}
}


