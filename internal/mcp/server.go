package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/eliotstocker/vibebackr/internal/billing"
	"github.com/eliotstocker/vibebackr/internal/domain"
	"github.com/eliotstocker/vibebackr/internal/ratelimit"
	"github.com/eliotstocker/vibebackr/internal/repository"
	"github.com/eliotstocker/vibebackr/internal/search"
	"github.com/eliotstocker/vibebackr/internal/storage"
	"github.com/google/uuid"
)

// JSONRPCRequest represents an incoming JSON-RPC 2.0 message.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse represents an outgoing JSON-RPC 2.0 message.
type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

// RPCError is a standard JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Tool represents an MCP tool definition.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

type contextKey string

// BaseURLContextKey is the context key for passing the dynamically resolved canonical VibeBackr URL.
const BaseURLContextKey contextKey = "vibebackr_base_url"

type sseSession struct {
	userID string
	ch     chan []byte
}

// Server provides MCP capabilities over stdio and streamable HTTP.
type Server struct {
	repo           repository.Repository
	searchEngine   *search.SearchEngine
	appStore       storage.AppStore
	billing        *billing.Module
	rateLimiter    *ratelimit.Module
	baseURL        string
	providerLister func() []string
	mu             sync.Mutex
	sseClients     map[string]sseSession
}

// NewServer initializes an MCP server.
func NewServer(repo repository.Repository, searchEngine *search.SearchEngine) *Server {
	return &Server{
		repo:         repo,
		searchEngine: searchEngine,
		appStore:     storage.NewAppStoreFromEnv(),
		sseClients:   make(map[string]sseSession),
	}
}

// SetBilling attaches the optional billing module to the MCP server.
func (s *Server) SetBilling(b *billing.Module) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.billing = b
}

// GetBilling returns the active billing module (or nil if disabled).
func (s *Server) GetBilling() *billing.Module {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.billing
}

// SetRateLimiter attaches the optional per-collection rate limiter to the MCP server.
func (s *Server) SetRateLimiter(rl *ratelimit.Module) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimiter = rl
}

// GetRateLimiter returns the active per-collection rate limiter (or nil if disabled).
func (s *Server) GetRateLimiter() *ratelimit.Module {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rateLimiter
}

func (s *Server) checkRateLimit(ctx context.Context, colID string) error {
	rl := s.GetRateLimiter()
	if rl == nil || !rl.Enabled() {
		return nil
	}
	dec, err := rl.Allow(ctx, colID)
	if err != nil {
		return fmt.Errorf("%w (limit: %d req/min, retry after %ds)", err, dec.Limit, int(dec.RetryAfter.Seconds()))
	}
	return nil
}

// SetAppStore overrides the AppStore used for publishing single-file HTML apps.
func (s *Server) SetAppStore(store storage.AppStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appStore = store
}

// GetAppStore returns the active AppStore.
func (s *Server) GetAppStore() storage.AppStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appStore
}

// SetProviderLister configures the provider discovery function.
func (s *Server) SetProviderLister(fn func() []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providerLister = fn
}

// GetProviderNames returns the names of active identity providers.
func (s *Server) GetProviderNames() []string {
	s.mu.Lock()
	fn := s.providerLister
	s.mu.Unlock()
	if fn != nil {
		return fn()
	}
	var names []string
	if os.Getenv("GOOGLE_CLIENT_ID") != "" {
		names = append(names, "google")
	}
	if os.Getenv("GITHUB_CLIENT_ID") != "" {
		names = append(names, "github")
	}
	if os.Getenv("OIDC_CLIENT_ID") != "" {
		oidcName := os.Getenv("OIDC_NAME")
		if oidcName == "" {
			oidcName = "oidc"
		}
		names = append(names, oidcName)
	}
	return names
}

// SetBaseURL configures the canonical base URL for the server.
func (s *Server) SetBaseURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseURL = strings.TrimRight(url, "/")
}

// GetBaseURL resolves the canonical VibeBackr URL from context, configuration, environment, or default.
func (s *Server) GetBaseURL(ctx context.Context) string {
	if ctx != nil {
		if val, ok := ctx.Value(BaseURLContextKey).(string); ok && val != "" {
			return strings.TrimRight(val, "/")
		}
	}

	s.mu.Lock()
	configured := s.baseURL
	s.mu.Unlock()
	if configured != "" {
		return configured
	}

	if val := os.Getenv("VIBEBACKR_URL"); val != "" {
		return strings.TrimRight(val, "/")
	}
	if val := os.Getenv("BASE_URL"); val != "" {
		return strings.TrimRight(val, "/")
	}
	if val := os.Getenv("CANONICAL_DOMAIN"); val != "" {
		if strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://") {
			return strings.TrimRight(val, "/")
		}
		return "https://" + strings.TrimRight(val, "/")
	}

	return "https://vibebac.kr"
}

func isValidHostHeader(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == ':' || c == '[' || c == ']') {
			return false
		}
	}
	return true
}

// GetBaseURLFromRequest resolves the canonical VibeBackr URL given an incoming HTTP request.
func (s *Server) GetBaseURLFromRequest(r *http.Request) string {
	if r == nil {
		return s.GetBaseURL(nil)
	}

	if val := os.Getenv("VIBEBACKR_URL"); val != "" {
		return strings.TrimRight(val, "/")
	}
	if val := os.Getenv("BASE_URL"); val != "" {
		return strings.TrimRight(val, "/")
	}

	s.mu.Lock()
	configured := s.baseURL
	s.mu.Unlock()
	if configured != "" {
		return configured
	}

	scheme := "https"
	host := r.Header.Get("X-Forwarded-Host")
	if idx := strings.IndexByte(host, ','); idx != -1 {
		host = strings.TrimSpace(host[:idx])
	}
	if host == "" {
		host = r.Host
	}
	if !isValidHostHeader(host) {
		host = ""
	}

	canonical := os.Getenv("CANONICAL_DOMAIN")
	if canonical != "" && (strings.Contains(host, "lambda-url") || strings.Contains(host, "cloudfront.net") || host == "") {
		host = canonical
	} else if strings.Contains(host, "lambda-url") {
		host = "vibebac.kr"
	}

	if canonical != "" && (host == canonical || strings.HasSuffix(host, "."+canonical)) {
		return "https://" + host
	}

	if r.TLS == nil && !strings.Contains(r.Header.Get("X-Forwarded-Proto"), "https") && strings.HasPrefix(host, "localhost") {
		scheme = "http"
	}

	if host == "" {
		if canonical != "" {
			return "https://" + canonical
		}
		return "https://vibebac.kr"
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

// GetInstructions returns a clear description of the VibeBackr service and its canonical endpoint.
func (s *Server) GetInstructions(ctx context.Context) string {
	baseURL := s.GetBaseURL(ctx)
	return fmt.Sprintf(`VibeBackr (%s) is a cloud persistence, authentication, and vector search backend for web applications and tools.

Capabilities:
- Collections & Universal Records: JSON document store organized into collections. Records support a title, markdown/text content, and arbitrary structured JSON in 'data'.
- Authentication & Multi-Tenancy: Built-in OAuth 2.0 (PKCE & Dynamic Client Registration), SSO (Google, GitHub, OIDC), and 5-tier access control (ownership, manager hierarchy, explicit email shares, public visibility, and tenant admin).
- Hybrid Search: 768-dimensional semantic vector search (EmbeddingGemma 2) with automatic SQL keyword fallback.
- Instant Static App Hosting: Deploy a self-contained single-file HTML application for any collection via 'publish_app', served directly from edge storage at https://{dashless_collection_id}.vibebac.kr.
- Browser SDK & REST API: Canonical endpoint at %s (client SDK available via 'get_client_integration_guide').`, baseURL, baseURL)
}

// GetToolDefinitions returns the catalog of exposed agent tools.
func (s *Server) GetToolDefinitions() []Tool {
	return s.GetToolDefinitionsWithContext(context.Background())
}

// GetToolDefinitionsWithContext returns the catalog of exposed agent tools tailored with context information.
func (s *Server) GetToolDefinitionsWithContext(ctx context.Context) []Tool {
	baseURL := s.GetBaseURL(ctx)
	tools := []Tool{
		{
			Name:        "create_collection",
			Description: "Create a new record collection / namespace.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":                  map[string]interface{}{"type": "string", "description": "Collection name"},
					"slug":                  map[string]interface{}{"type": "string", "description": "Optional unique project slug (case-insensitive alphanumeric and dashes) used as a URL alias for the collection UUID"},
					"tenant_id":             map[string]interface{}{"type": "string", "description": "Optional target tenant ID when creating a collection inside a shared/invited tenant"},
					"default_visibility":    map[string]interface{}{"type": "string", "enum": []string{"private", "public", "shared"}},
					"allowed_domains":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					"origin_urls":           map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Allowed web frontend origin URLs for CORS and auth redirects (e.g. ['http://localhost:5173'])"},
					"vector_search_enabled": map[string]interface{}{"type": "boolean"},
					"auto_invite_shares":    map[string]interface{}{"type": "boolean", "description": "When true, sharing a record with a non-user email automatically invites them to the tenant"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "update_collection",
			Description: "Update an existing collection's name, slug, default_visibility, allowed_domains, origin_urls, vector search, or auto_invite_shares settings.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"collection_id":         map[string]interface{}{"type": "string", "description": "Collection ID, slug, or name"},
					"name":                  map[string]interface{}{"type": "string", "description": "New collection name"},
					"slug":                  map[string]interface{}{"type": "string", "description": "Unique project slug (case-insensitive alphanumeric and dashes) used as a URL alias for the collection UUID"},
					"default_visibility":    map[string]interface{}{"type": "string", "enum": []string{"private", "public", "shared"}},
					"allowed_domains":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					"origin_urls":           map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Allowed web frontend origin URLs for CORS and auth redirects (e.g. ['http://localhost:5173'])"},
					"vector_search_enabled": map[string]interface{}{"type": "boolean"},
					"auto_invite_shares":    map[string]interface{}{"type": "boolean", "description": "When true, sharing a record with a non-user email automatically invites them to the tenant"},
				},
				"required": []string{"collection_id"},
			},
		},
		{
			Name:        "publish_app",
			Description: "Publish or update a single-file HTML frontend application for a collection. Hosted directly on S3/CloudFront at https://{slug_or_dashless_id}.vibebac.kr and automatically added to the collection's origin_urls.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"collection_id": map[string]interface{}{"type": "string", "description": "Collection ID, slug, or name"},
					"html":          map[string]interface{}{"type": "string", "description": "Complete single-file HTML document (up to 512KB)"},
					"slug":          map[string]interface{}{"type": "string", "description": "Optional unique project slug (case-insensitive alphanumeric and dashes) to assign as the app's subdomain/path alias"},
				},
				"required": []string{"collection_id", "html"},
			},
		},
		{
			Name:        "list_collections",
			Description: "List all existing collections.",
			InputSchema: map[string]interface{}{
				"type": "object",
			},
		},
		{
			Name:        "sync_org_directory",
			Description: "Batch upsert employee directory (e.g. from HiBob) with managerial hierarchy.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"users": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"email":         map[string]interface{}{"type": "string"},
								"name":          map[string]interface{}{"type": "string"},
								"role":          map[string]interface{}{"type": "string", "enum": []string{"admin", "manager", "member"}},
								"manager_email": map[string]interface{}{"type": "string"},
							},
							"required": []string{"email", "name"},
						},
					},
				},
				"required": []string{"users"},
			},
		},
		{
			Name:        "batch_create_invites",
			Description: "Pre-authorizes a batch of new users and teams.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"tenant_id": map[string]interface{}{"type": "string", "description": "Optional target tenant ID to invite users into"},
					"invites": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"email":         map[string]interface{}{"type": "string"},
								"role":          map[string]interface{}{"type": "string", "enum": []string{"admin", "manager", "member"}},
								"manager_email": map[string]interface{}{"type": "string"},
							},
							"required": []string{"email"},
						},
					},
				},
				"required": []string{"invites"},
			},
		},
		{
			Name:        "create_record",
			Description: "Create a universal record in a collection with ACL policies.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"collection_id": map[string]interface{}{"type": "string"},
					"title":         map[string]interface{}{"type": "string"},
					"content":       map[string]interface{}{"type": "string"},
					"data":          map[string]interface{}{"type": "string", "description": "JSON payload for custom structured fields"},
					"visibility":    map[string]interface{}{"type": "string", "enum": []string{"private", "public", "shared"}},
					"shares":        map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				},
				"required": []string{"collection_id", "title"},
			},
		},
		{
			Name:        "query_records",
			Description: "Query records within a collection enforcing recursive waterfall ACLs.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"collection_id": map[string]interface{}{"type": "string"},
				},
				"required": []string{"collection_id"},
			},
		},
		{
			Name:        "search_records",
			Description: "Unified search across records with pgvector RAG and SQL fallback.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"collection_id": map[string]interface{}{"type": "string"},
					"query":         map[string]interface{}{"type": "string"},
					"limit":         map[string]interface{}{"type": "integer"},
				},
				"required": []string{"collection_id", "query"},
			},
		},
		{
			Name:        "get_client_integration_guide",
			Description: fmt.Sprintf("Provides endpoint documentation, auth flow instructions, data contracts, and copy-pasteable TypeScript/JavaScript client code for building frontends and UIs that connect to VibeBackr (%s).", baseURL),
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"topic": map[string]interface{}{
						"type":        "string",
						"description": "Integration topic: 'all' (default), 'overview', 'auth', 'records', 'search', or 'typescript_client'",
						"enum":        []string{"all", "overview", "auth", "records", "search", "typescript_client"},
					},
					"collection_id": map[string]interface{}{
						"type":        "string",
						"description": "Optional collection ID, slug, or name. If provided, inspects the collection schema and returns tailored TypeScript types and code examples.",
					},
				},
			},
		},
	}

	if b := s.GetBilling(); b != nil && b.Enabled() {
		tools = append(tools,
			Tool{
				Name:        "get_billing_status",
				Description: "Get current tenant subscription plan, collection quota usage (10 on Starter £3/mo, 25 on Pro £5/mo), trial countdown, and read-only status.",
				InputSchema: map[string]interface{}{
					"type": "object",
				},
			},
			Tool{
				Name:        "upgrade_tenant_plan",
				Description: "Upgrade the tenant to the Pro plan (25 collections, £5/month) or start a GoCardless Direct Debit setup flow.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"plan": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"starter", "pro"},
							"description": "Target plan: 'pro' (25 collections, £5/mo, default) or 'starter' (10 collections, £3/mo)",
						},
					},
				},
			},
			Tool{
				Name:        "redeem_voucher",
				Description: "Redeem a promotional voucher code for a complimentary Starter (10 collections) or Pro (25 collections) workspace.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"code": map[string]interface{}{
							"type":        "string",
							"description": "Voucher code to redeem",
						},
					},
					"required": []string{"code"},
				},
			},
		)
	}
	return tools
}

// HandleRPC processes a single JSON-RPC request.
func (s *Server) HandleRPC(ctx context.Context, req JSONRPCRequest, caller *domain.User) JSONRPCResponse {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}

	switch req.Method {
	case "initialize":
		protocolVersion := "2025-06-18"
		if len(req.Params) > 0 {
			var initParams struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(req.Params, &initParams); err == nil && initParams.ProtocolVersion != "" {
				protocolVersion = initParams.ProtocolVersion
			}
		}
		resp.Result = map[string]interface{}{
			"protocolVersion": protocolVersion,
			"serverInfo": map[string]interface{}{
				"name":    "vibebackr",
				"version": "1.0.0",
			},
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"instructions": s.GetInstructions(ctx),
		}
	case "notifications/initialized":
		return resp // no response expected for notifications, but returning empty object is safe
	case "ping":
		resp.Result = map[string]interface{}{}
	case "tools/list":
		resp.Result = map[string]interface{}{
			"tools": s.GetToolDefinitionsWithContext(ctx),
		}
	case "tools/call":
		var callParams struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			resp.Error = &RPCError{Code: -32602, Message: "Invalid params"}
			return resp
		}

		resultText, err := s.callTool(ctx, callParams.Name, callParams.Arguments, caller)
		if err != nil {
			resp.Result = map[string]interface{}{
				"isError": true,
				"content": []map[string]interface{}{
					{"type": "text", "text": fmt.Sprintf("Error: %v", err)},
				},
			}
			return resp
		}

		resp.Result = map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": resultText},
			},
		}
	default:
		resp.Error = &RPCError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)}
	}

	return resp
}

func (s *Server) callTool(ctx context.Context, name string, args map[string]interface{}, caller *domain.User) (string, error) {
	if caller == nil {
		caller = &domain.User{
			ID:    "system-agent",
			Email: "agent@vibebackr.local",
			Role:  domain.RoleAdmin,
		}
	}

	switch name {
	case "create_collection":
		colName, _ := args["name"].(string)
		if colName == "" {
			return "", errors.New("name is required")
		}
		slug, _ := args["slug"].(string)
		vis, _ := args["default_visibility"].(string)
		if vis == "" {
			vis = domain.VisibilityPrivate
		}
		var domains []string
		if rawDomains, ok := args["allowed_domains"].([]interface{}); ok {
			for _, d := range rawDomains {
				if ds, ok := d.(string); ok {
					domains = append(domains, ds)
				}
			}
		}
		var origins []string
		if rawOrigins, ok := args["origin_urls"].([]interface{}); ok {
			for _, o := range rawOrigins {
				if os, ok := o.(string); ok {
					origins = append(origins, os)
				}
			}
		}
		vecEnabled, _ := args["vector_search_enabled"].(bool)
		autoInvite, _ := args["auto_invite_shares"].(bool)

		var tenantID string
		requestedTenant, _ := args["tenant_id"].(string)
		requestedTenant = strings.TrimSpace(requestedTenant)
		if requestedTenant != "" {
			if !s.isTenantAdminOrManagerFor(ctx, caller, requestedTenant) {
				return "", errors.New("forbidden: admin or manager role required to create collection in target tenant")
			}
			tenantID = requestedTenant
		} else if caller != nil {
			if caller.TenantID != nil && *caller.TenantID != "" {
				tenantID = *caller.TenantID
			} else if allowed, effTenant := s.isCallerAdminOrManager(ctx, caller); allowed && effTenant != "" {
				tenantID = effTenant
			} else {
				// Non-tenant user creating a collection gets auto-provisioned tenant
				newTenantID := uuid.NewString()
				caller.TenantID = &newTenantID
				caller.Role = domain.RoleAdmin
				_ = s.repo.UpdateUser(ctx, caller)
				tenantID = newTenantID
			}
		}

		col := &domain.Collection{
			ID:                  uuid.NewString(),
			TenantID:            tenantID,
			Name:                colName,
			Slug:                slug,
			DefaultVisibility:   vis,
			AllowedDomains:      domains,
			OriginURLs:          origins,
			VectorSearchEnabled: vecEnabled,
			AutoInviteShares:    autoInvite,
		}
		if err := s.repo.CreateCollection(ctx, col); err != nil {
			return "", err
		}
		bytes, _ := json.MarshalIndent(col, "", "  ")
		return string(bytes), nil

	case "update_collection":
		colID, _ := args["collection_id"].(string)
		if colID == "" {
			return "", errors.New("collection_id is required")
		}
		if err := s.checkRateLimit(ctx, colID); err != nil {
			return "", err
		}

		col, err := s.repo.GetCollection(ctx, colID)
		if err != nil {
			if cols, lErr := s.repo.ListCollectionsForUser(ctx, caller); lErr == nil {
				for _, c := range cols {
					if strings.EqualFold(c.Name, colID) || c.ID == colID || (c.Slug != "" && strings.EqualFold(c.Slug, colID)) {
						colCopy := c
						col = &colCopy
						err = nil
						break
					}
				}
			}
		}
		if err != nil || col == nil {
			return "", fmt.Errorf("collection not found: %s", colID)
		}

		if !s.isCollectionAdmin(ctx, caller, col) {
			return "", errors.New("forbidden: admin privileges required to update collection")
		}

		oldSlug := col.Slug
		if newName, ok := args["name"].(string); ok && newName != "" {
			col.Name = newName
		}
		if newSlug, ok := args["slug"].(string); ok {
			col.Slug = newSlug
		}
		if vis, ok := args["default_visibility"].(string); ok && vis != "" {
			col.DefaultVisibility = vis
		}
		if rawDomains, ok := args["allowed_domains"].([]interface{}); ok {
			var domains []string
			for _, d := range rawDomains {
				if ds, ok := d.(string); ok {
					domains = append(domains, ds)
				}
			}
			col.AllowedDomains = domains
		}
		if rawOrigins, ok := args["origin_urls"].([]interface{}); ok {
			var origins []string
			for _, o := range rawOrigins {
				if os, ok := o.(string); ok {
					origins = append(origins, os)
				}
			}
			col.OriginURLs = origins
		}
		if vecEnabled, ok := args["vector_search_enabled"].(bool); ok {
			col.VectorSearchEnabled = vecEnabled
		}
		if autoInvite, ok := args["auto_invite_shares"].(bool); ok {
			col.AutoInviteShares = autoInvite
		}

		if err := s.repo.UpdateCollection(ctx, col); err != nil {
			return "", err
		}

		if oldSlug != col.Slug {
			if store := s.GetAppStore(); store != nil {
				var existingHTML string
				if oldSlug != "" {
					existingHTML, _ = store.GetApp(ctx, oldSlug)
				}
				if existingHTML == "" {
					existingHTML, _ = store.GetApp(ctx, col.ID)
				}
				if existingHTML != "" {
					if col.Slug != "" {
						_ = store.PutApp(ctx, col.Slug, existingHTML)
					}
					if oldSlug != "" && oldSlug != col.Slug {
						_ = store.DeleteApp(ctx, oldSlug)
					}
				}
			}
		}

		bytes, _ := json.MarshalIndent(col, "", "  ")
		return string(bytes), nil

	case "publish_app":
		colID, _ := args["collection_id"].(string)
		if colID == "" {
			return "", errors.New("collection_id is required")
		}
		if err := s.checkRateLimit(ctx, colID); err != nil {
			return "", err
		}
		htmlContent, _ := args["html"].(string)
		if err := storage.ValidateAppHTML(htmlContent); err != nil {
			return "", err
		}

		col, err := s.repo.GetCollection(ctx, colID)
		if err != nil {
			if cols, lErr := s.repo.ListCollectionsForUser(ctx, caller); lErr == nil {
				for _, c := range cols {
					if strings.EqualFold(c.Name, colID) || c.ID == colID || (c.Slug != "" && strings.EqualFold(c.Slug, colID)) || storage.CollectionSlug(c.ID) == storage.CollectionSlug(colID) {
						colCopy := c
						col = &colCopy
						err = nil
						break
					}
				}
			}
		}
		if err != nil || col == nil {
			return "", fmt.Errorf("collection not found: %s", colID)
		}

		if !s.isCollectionAdmin(ctx, caller, col) {
			return "", errors.New("forbidden: admin privileges required to publish app to collection")
		}
		if b := s.GetBilling(); b != nil && b.Enabled() {
			if err := b.CheckWriteAccess(ctx, col.TenantID, false); err != nil {
				return "", err
			}
		}

		oldSlug := col.Slug
		slugUpdated := false
		if slugArg, hasSlugArg := args["slug"].(string); hasSlugArg {
			normSlug, sErr := domain.ValidateSlug(slugArg)
			if sErr != nil {
				return "", sErr
			}
			if normSlug != col.Slug {
				col.Slug = normSlug
				slugUpdated = true
				if uErr := s.repo.UpdateCollection(ctx, col); uErr != nil {
					return "", uErr
				}
			}
		}

		effectiveSlug := storage.EffectiveSlug(col.ID, col.Slug)
		uuidSlug := storage.CollectionSlug(col.ID)
		if store := s.GetAppStore(); store != nil {
			if err := store.PutApp(ctx, effectiveSlug, htmlContent); err != nil {
				return "", fmt.Errorf("failed publishing app: %w", err)
			}
			if effectiveSlug != uuidSlug {
				_ = store.PutApp(ctx, uuidSlug, htmlContent)
			}
			if slugUpdated && oldSlug != "" && oldSlug != effectiveSlug {
				_ = store.DeleteApp(ctx, oldSlug)
			}
		}

		baseURL := s.GetBaseURL(ctx)
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
			_ = s.repo.UpdateCollection(ctx, col)
		}

		res := map[string]interface{}{
			"collection_id": col.ID,
			"slug":          effectiveSlug,
			"app_url":       appURL,
			"origin_urls":   col.OriginURLs,
			"message":       fmt.Sprintf("Application published successfully at %s", appURL),
		}
		bytes, _ := json.MarshalIndent(res, "", "  ")
		return string(bytes), nil

	case "list_collections":
		cols, err := s.repo.ListCollectionsForUser(ctx, caller)
		if err != nil {
			return "", err
		}
		bytes, _ := json.MarshalIndent(cols, "", "  ")
		return string(bytes), nil

	case "sync_org_directory":
		allowed, effTenant := s.isCallerAdminOrManager(ctx, caller)
		if !allowed {
			return "", errors.New("forbidden: admin or manager role required to sync directory")
		}

		rawUsers, ok := args["users"].([]interface{})
		if !ok {
			return "", errors.New("users array is required")
		}

		type inputUser struct {
			Email        string `json:"email"`
			Name         string `json:"name"`
			Role         string `json:"role"`
			ManagerEmail string `json:"manager_email"`
		}

		var parsed []inputUser
		for _, ru := range rawUsers {
			b, _ := json.Marshal(ru)
			var u inputUser
			if err := json.Unmarshal(b, &u); err == nil {
				parsed = append(parsed, u)
			}
		}

		var callerTenantID *string
		if effTenant != "" {
			callerTenantID = &effTenant
		} else if caller != nil && caller.TenantID != nil && *caller.TenantID != "" {
			callerTenantID = caller.TenantID
		}

		// First pass: upsert all users so they exist and have IDs
		var domainUsers []domain.User
		for _, pu := range parsed {
			role := pu.Role
			if role == "" {
				role = domain.RoleMember
			}
			domainUsers = append(domainUsers, domain.User{
				TenantID: callerTenantID,
				Email:    pu.Email,
				Name:     pu.Name,
				Role:     role,
			})
		}

		if err := s.repo.BatchUpsertUsers(ctx, domainUsers); err != nil {
			return "", err
		}

		// Second pass: map manager relationships
		allUsers, err := s.repo.ListUsers(ctx)
		if err != nil {
			return "", err
		}
		emailToID := make(map[string]string)
		for _, u := range allUsers {
			emailToID[u.Email] = u.ID
		}

		var updatedUsers []domain.User
		for _, pu := range parsed {
			if pu.ManagerEmail != "" {
				if mgrID, ok := emailToID[pu.ManagerEmail]; ok {
					mid := mgrID
					role := pu.Role
					if role == "" {
						role = domain.RoleMember
					}
					updatedUsers = append(updatedUsers, domain.User{
						ID:        emailToID[pu.Email],
						TenantID:  callerTenantID,
						Email:     pu.Email,
						Name:      pu.Name,
						Role:      role,
						ManagerID: &mid,
					})
				}
			}
		}

		if len(updatedUsers) > 0 {
			if err := s.repo.BatchUpsertUsers(ctx, updatedUsers); err != nil {
				return "", err
			}
		}

		return fmt.Sprintf("Successfully synced %d users in directory", len(parsed)), nil

	case "batch_create_invites":
		requestedTenant, _ := args["tenant_id"].(string)
		requestedTenant = strings.TrimSpace(requestedTenant)
		var callerTenant string
		if requestedTenant != "" {
			if !s.isTenantAdminOrManagerFor(ctx, caller, requestedTenant) {
				return "", errors.New("forbidden: admin or manager role required to create invites in target tenant")
			}
			callerTenant = requestedTenant
		} else {
			allowed, effTenant := s.isCallerAdminOrManager(ctx, caller)
			if !allowed {
				return "", errors.New("forbidden: admin or manager role required to create invites")
			}
			callerTenant = effTenant
			if callerTenant == "" && caller != nil && caller.TenantID != nil {
				callerTenant = *caller.TenantID
			}
		}

		rawInvites, ok := args["invites"].([]interface{})
		if !ok {
			return "", errors.New("invites array is required")
		}

		allUsers, _ := s.repo.ListUsers(ctx)
		emailToID := make(map[string]string)
		for _, u := range allUsers {
			emailToID[u.Email] = u.ID
		}

		var invites []domain.Invite
		for _, ri := range rawInvites {
			b, _ := json.Marshal(ri)
			var item struct {
				Email        string `json:"email"`
				Role         string `json:"role"`
				ManagerEmail string `json:"manager_email"`
			}
			if err := json.Unmarshal(b, &item); err == nil {
				var mgrID *string
				if item.ManagerEmail != "" {
					if mid, ok := emailToID[item.ManagerEmail]; ok {
						mgrID = &mid
					}
				}
				role := item.Role
				if role == "" {
					role = domain.RoleMember
				}
				invites = append(invites, domain.Invite{
					TenantID:  callerTenant,
					Email:     item.Email,
					Role:      role,
					ManagerID: mgrID,
					CreatedBy: caller.ID,
				})
			}
		}

		if err := s.repo.BatchCreateInvites(ctx, invites); err != nil {
			return "", err
		}
		return fmt.Sprintf("Successfully created %d invites", len(invites)), nil

	case "create_record":
		colID, _ := args["collection_id"].(string)
		if err := s.checkRateLimit(ctx, colID); err != nil {
			return "", err
		}
		col, err := s.repo.GetCollection(ctx, colID)
		if err != nil || col == nil {
			return "", fmt.Errorf("collection not found: %s", colID)
		}
		if !s.canAccessCollectionTenant(ctx, caller, col) {
			return "", errors.New("forbidden: cannot create record in another tenant's collection")
		}
		title, _ := args["title"].(string)
		content, _ := args["content"].(string)
		dataStr, _ := args["data"].(string)
		if dataStr == "" {
			dataStr = "{}"
		}
		vis, _ := args["visibility"].(string)
		if vis == "" {
			if col.DefaultVisibility != "" {
				vis = col.DefaultVisibility
			} else {
				vis = domain.VisibilityPrivate
			}
		}
		var shares []string
		if rawShares, ok := args["shares"].([]interface{}); ok {
			for _, sh := range rawShares {
				if s, ok := sh.(string); ok {
					shares = append(shares, s)
				}
			}
		}

		rec := &domain.Record{
			CollectionID: col.ID,
			Title:        title,
			Content:      content,
			Data:         dataStr,
			CreatedBy:    caller.ID,
			AccessPolicy: domain.AccessPolicy{
				Visibility: vis,
				Shares:     shares,
			},
		}

		if err := s.repo.CreateRecord(ctx, rec); err != nil {
			return "", err
		}
		b, _ := json.MarshalIndent(rec, "", "  ")
		return string(b), nil

	case "query_records":
		colID, _ := args["collection_id"].(string)
		if err := s.checkRateLimit(ctx, colID); err != nil {
			return "", err
		}
		records, err := s.repo.ListRecords(ctx, colID, caller)
		if err != nil {
			return "", err
		}
		b, _ := json.MarshalIndent(records, "", "  ")
		return string(b), nil

	case "search_records":
		colID, _ := args["collection_id"].(string)
		if err := s.checkRateLimit(ctx, colID); err != nil {
			return "", err
		}
		query, _ := args["query"].(string)
		limit := 20
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}

		results, err := s.searchEngine.Search(ctx, colID, query, caller, limit)
		if err != nil {
			return "", err
		}
		b, _ := json.MarshalIndent(results, "", "  ")
		return string(b), nil

	case "get_client_integration_guide":
		topic, _ := args["topic"].(string)
		if topic == "" {
			topic = "all"
		}
		colID, _ := args["collection_id"].(string)
		return s.GenerateClientGuideForUser(ctx, topic, colID, caller)

	case "get_billing_status":
		b := s.GetBilling()
		if b == nil || !b.Enabled() {
			out, _ := json.MarshalIndent(&billing.StatusResponse{Enabled: false}, "", "  ")
			return string(out), nil
		}
		tenantID := ""
		if caller != nil && caller.TenantID != nil {
			tenantID = *caller.TenantID
		}
		status, err := b.GetStatus(ctx, tenantID)
		if err != nil {
			return "", err
		}
		out, _ := json.MarshalIndent(status, "", "  ")
		return string(out), nil

	case "upgrade_tenant_plan":
		b := s.GetBilling()
		if b == nil || !b.Enabled() {
			return "", errors.New("billing module is not enabled on this instance")
		}
		if caller == nil {
			return "", errors.New("authentication required")
		}
		if caller.TenantID == nil || *caller.TenantID == "" {
			newTenantID := uuid.NewString()
			caller.TenantID = &newTenantID
			caller.Role = domain.RoleAdmin
			_ = s.repo.UpdateUser(ctx, caller)
		}
		tenantID := *caller.TenantID
		targetPlan, _ := args["plan"].(string)
		baseURL := s.GetBaseURL(ctx)
		redirectURI := fmt.Sprintf("%s/api/billing/callback?tenant_id=%s", baseURL, tenantID)
		exitURI := fmt.Sprintf("%s/login", baseURL)

		if strings.EqualFold(strings.TrimSpace(targetPlan), billing.PlanStarter) {
			authURL, err := b.StartCheckout(ctx, tenantID, billing.PlanStarter, redirectURI, exitURI)
			if err != nil {
				return "", err
			}
			status, _ := b.GetStatus(ctx, tenantID)
			res := &billing.UpgradeResult{
				UpgradedImmediately: false,
				AuthorisationURL:    authURL,
				Message:             "Complete GoCardless Direct Debit setup to activate the £3/month Starter plan (10 collections).",
				Status:              status,
			}
			out, _ := json.MarshalIndent(res, "", "  ")
			return string(out), nil
		}

		res, err := b.UpgradePlan(ctx, tenantID, redirectURI, exitURI)
		if err != nil {
			return "", err
		}
		if rl := s.GetRateLimiter(); rl != nil {
			rl.InvalidateTenant(tenantID)
		}
		out, _ := json.MarshalIndent(res, "", "  ")
		return string(out), nil

	case "redeem_voucher":
		b := s.GetBilling()
		if b == nil || !b.Enabled() {
			return "", errors.New("billing module is not enabled on this instance")
		}
		if caller == nil {
			return "", errors.New("authentication required")
		}
		code, _ := args["code"].(string)
		if strings.TrimSpace(code) == "" {
			return "", errors.New("code is required")
		}
		if caller.TenantID == nil || *caller.TenantID == "" {
			newTenantID := uuid.NewString()
			caller.TenantID = &newTenantID
			caller.Role = domain.RoleAdmin
			_ = s.repo.UpdateUser(ctx, caller)
		}
		status, err := b.RedeemVoucher(ctx, *caller.TenantID, code)
		if err != nil {
			return "", err
		}
		if rl := s.GetRateLimiter(); rl != nil {
			rl.InvalidateTenant(*caller.TenantID)
		}
		out, _ := json.MarshalIndent(status, "", "  ")
		return string(out), nil

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) isCollectionAdmin(ctx context.Context, caller *domain.User, col *domain.Collection) bool {
	if caller == nil || col == nil {
		return false
	}
	callerTenantID := ""
	if caller.TenantID != nil {
		callerTenantID = *caller.TenantID
	}
	if caller.Role == domain.RoleAdmin && callerTenantID == col.TenantID {
		return true
	}
	if col.TenantID != "" {
		memberships, _ := s.repo.ListUserTenants(ctx, caller.ID)
		for _, m := range memberships {
			if m.TenantID == col.TenantID && m.Role == domain.RoleAdmin {
				return true
			}
		}
	}
	return false
}

func (s *Server) isTenantAdminOrManagerFor(ctx context.Context, caller *domain.User, targetTenantID string) bool {
	if caller == nil || targetTenantID == "" {
		return false
	}
	if caller.TenantID != nil && *caller.TenantID == targetTenantID && (caller.Role == domain.RoleAdmin || caller.Role == domain.RoleManager) {
		return true
	}
	memberships, _ := s.repo.ListUserTenants(ctx, caller.ID)
	for _, m := range memberships {
		if m.TenantID == targetTenantID && (m.Role == domain.RoleAdmin || m.Role == domain.RoleManager) {
			return true
		}
	}
	return false
}

func (s *Server) canAccessCollectionTenant(ctx context.Context, caller *domain.User, col *domain.Collection) bool {
	if col == nil {
		return false
	}
	if col.TenantID == "" {
		return true
	}
	if caller == nil {
		return false
	}
	if caller.TenantID != nil && *caller.TenantID == col.TenantID {
		return true
	}
	memberships, _ := s.repo.ListUserTenants(ctx, caller.ID)
	for _, m := range memberships {
		if m.TenantID == col.TenantID {
			return true
		}
	}
	if parts := strings.Split(caller.Email, "@"); len(parts) == 2 {
		for _, d := range col.AllowedDomains {
			if strings.EqualFold(d, parts[1]) {
				return true
			}
		}
	}
	if recs, err := s.repo.ListRecords(ctx, col.ID, caller); err == nil && len(recs) > 0 {
		return true
	}
	return false
}

func (s *Server) isCallerAdminOrManager(ctx context.Context, caller *domain.User) (bool, string) {
	if caller == nil {
		return false, ""
	}
	tenantID := ""
	if caller.TenantID != nil {
		tenantID = *caller.TenantID
	}
	if caller.Role == domain.RoleAdmin || caller.Role == domain.RoleManager {
		if tenantID == "" {
			memberships, _ := s.repo.ListUserTenants(ctx, caller.ID)
			for _, m := range memberships {
				if m.Role == domain.RoleAdmin || m.Role == domain.RoleManager {
					return true, m.TenantID
				}
			}
		}
		return true, tenantID
	}
	memberships, _ := s.repo.ListUserTenants(ctx, caller.ID)
	for _, m := range memberships {
		if (tenantID == "" || m.TenantID == tenantID) && (m.Role == domain.RoleAdmin || m.Role == domain.RoleManager) {
			return true, m.TenantID
		}
	}
	return false, ""
}

// ServeStdio starts the MCP JSON-RPC server listening on standard input and output.
func (s *Server) ServeStdio(ctx context.Context, defaultUser *domain.User) error {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			errResp := JSONRPCResponse{
				JSONRPC: "2.0",
				Error:   &RPCError{Code: -32700, Message: "Parse error"},
			}
			out, _ := json.Marshal(errResp)
			fmt.Println(string(out))
			continue
		}

		resp := s.HandleRPC(ctx, req, defaultUser)
		// notifications don't return responses
		if req.ID != nil {
			out, _ := json.Marshal(resp)
			fmt.Println(string(out))
		}
	}
	return scanner.Err()
}

func (s *Server) isSSEClientActiveForUser(sessionID, userID string) bool {
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sseClients[sessionID]
	return ok && sess.userID == userID
}

func (s *Server) sendToSSEClientForUser(sessionID, userID string, data []byte) bool {
	s.mu.Lock()
	sess, ok := s.sseClients[sessionID]
	s.mu.Unlock()
	if !ok || sess.userID != userID {
		return false
	}
	select {
	case sess.ch <- data:
		return true
	default:
		return false
	}
}

// ServeHTTP handles streamable MCP over HTTP:
// POST /mcp: Streamable HTTP JSON-RPC request-response or streaming response (Accept: text/event-stream)
// GET /mcp: Server-Sent Events (SSE) stream for real-time events and legacy SSE clients
// DELETE /mcp: Closes and cleans up an active session
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request, defaultUser *domain.User) {
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, MCP-Protocol-Version, Last-Event-ID")
	w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, MCP-Protocol-Version")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()
	if ctx.Value(BaseURLContextKey) == nil {
		baseURL := s.GetBaseURLFromRequest(r)
		ctx = context.WithValue(ctx, BaseURLContextKey, baseURL)
		r = r.WithContext(ctx)
	}

	protocolVersion := r.Header.Get("MCP-Protocol-Version")
	if protocolVersion == "" {
		protocolVersion = "2025-06-18"
	}
	w.Header().Set("MCP-Protocol-Version", protocolVersion)

	callerID := ""
	if defaultUser != nil {
		callerID = defaultUser.ID
	}

	headerSessionID := r.Header.Get("Mcp-Session-Id")
	querySessionID := r.URL.Query().Get("sessionId")
	if querySessionID == "" {
		querySessionID = r.URL.Query().Get("clientId")
	}
	sessionID := headerSessionID
	if sessionID == "" {
		sessionID = querySessionID
	}

	if r.Method == http.MethodDelete {
		if sessionID != "" {
			s.mu.Lock()
			if sess, ok := s.sseClients[sessionID]; ok && sess.userID == callerID {
				delete(s.sseClients, sessionID)
			}
			s.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sessionID)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method == http.MethodGet {
		// SSE stream
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		isStreamableHTTPGet := headerSessionID != "" || r.Header.Get("MCP-Protocol-Version") != ""
		if sessionID == "" {
			sessionID = uuid.NewString()
		}

		msgChan := make(chan []byte, 32)

		s.mu.Lock()
		if existing, exists := s.sseClients[sessionID]; exists && existing.userID != callerID {
			sessionID = uuid.NewString()
		}
		s.sseClients[sessionID] = sseSession{userID: callerID, ch: msgChan}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Mcp-Session-Id", sessionID)

		defer func() {
			s.mu.Lock()
			if sess, ok := s.sseClients[sessionID]; ok && sess.userID == callerID {
				delete(s.sseClients, sessionID)
			}
			s.mu.Unlock()
		}()

		// Only emit legacy 2024-11-05 endpoint discovery event if the client did not connect with Streamable HTTP headers
		if !isStreamableHTTPGet {
			fmt.Fprintf(w, "event: endpoint\ndata: /mcp?sessionId=%s&clientId=%s\n\n", sessionID, sessionID)
		}
		flusher.Flush()

		notify := r.Context().Done()
		for {
			select {
			case <-notify:
				return
			case msg, ok := <-msgChan:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", string(msg))
				flusher.Flush()
			}
		}
	}

	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 {
			http.Error(w, "Empty body", http.StatusBadRequest)
			return
		}

		if sessionID == "" {
			sessionID = uuid.NewString()
		}
		w.Header().Set("Mcp-Session-Id", sessionID)

		isBatch := trimmed[0] == '['
		var requests []JSONRPCRequest

		if isBatch {
			if err := json.Unmarshal(trimmed, &requests); err != nil {
				errResp := JSONRPCResponse{
					JSONRPC: "2.0",
					Error:   &RPCError{Code: -32700, Message: "Parse error"},
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]JSONRPCResponse{errResp})
				return
			}
		} else {
			var singleReq JSONRPCRequest
			if err := json.Unmarshal(trimmed, &singleReq); err != nil {
				errResp := JSONRPCResponse{
					JSONRPC: "2.0",
					Error:   &RPCError{Code: -32700, Message: "Parse error"},
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(errResp)
				return
			}
			requests = []JSONRPCRequest{singleReq}
		}

		var responses []JSONRPCResponse
		hasCallResponses := false

		for _, req := range requests {
			resp := s.HandleRPC(r.Context(), req, defaultUser)
			if req.Method == "initialize" {
				if resMap, ok := resp.Result.(map[string]interface{}); ok {
					if pv, ok := resMap["protocolVersion"].(string); ok && pv != "" {
						w.Header().Set("MCP-Protocol-Version", pv)
					}
				}
			}
			if req.ID != nil {
				responses = append(responses, resp)
				hasCallResponses = true
			}
		}

		// Legacy 2024-11-05 SSE client bridging: only bridge POST responses to the GET SSE stream
		// when the client used query-param session routing (?sessionId= / ?clientId=) without Streamable HTTP headers.
		isLegacySSEPost := headerSessionID == "" && r.Header.Get("MCP-Protocol-Version") == "" && querySessionID != ""
		if isLegacySSEPost && s.isSSEClientActiveForUser(sessionID, callerID) {
			for _, resp := range responses {
				respBytes, _ := json.Marshal(resp)
				s.sendToSSEClientForUser(sessionID, callerID, respBytes)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}

		// Notification-only request or batch (no ID on any request) -> 202 Accepted
		if !hasCallResponses {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		// Streamable HTTP: check Accept header.
		// If the client accepts application/json (e.g. "application/json, text/event-stream"),
		// return synchronous application/json; if it exclusively requests text/event-stream, stream SSE.
		acceptHeader := r.Header.Get("Accept")
		wantsEventStream := strings.Contains(acceptHeader, "text/event-stream") && !strings.Contains(acceptHeader, "application/json")

		flusher, canFlush := w.(http.Flusher)
		if wantsEventStream && canFlush {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			for _, resp := range responses {
				respBytes, _ := json.Marshal(resp)
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", string(respBytes))
				flusher.Flush()
			}
			return
		}

		// Standard synchronous JSON response
		w.Header().Set("Content-Type", "application/json")
		if isBatch {
			_ = json.NewEncoder(w).Encode(responses)
		} else if len(responses) > 0 {
			_ = json.NewEncoder(w).Encode(responses[0])
		}
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// GenerateClientGuide returns interactive documentation, API specifications, and ready-to-use client code
// for frontend developers and UI-scaffolding agents building web applications backed by VibeBackr.
func (s *Server) GenerateClientGuide(ctx context.Context, topic, collectionID string) (string, error) {
	return s.GenerateClientGuideForUser(ctx, topic, collectionID, nil)
}

// GenerateClientGuideForUser returns interactive documentation scoped to the caller's permissions when introspecting a collection.
func (s *Server) GenerateClientGuideForUser(ctx context.Context, topic, collectionID string, caller *domain.User) (string, error) {
	if topic == "" {
		topic = "all"
	}

	baseURL := s.GetBaseURL(ctx)
	var sb strings.Builder

	includeOverview := topic == "all" || topic == "overview"
	includeAuth := topic == "all" || topic == "auth"
	includeRecords := topic == "all" || topic == "records"
	includeSearch := topic == "all" || topic == "search"
	includeClient := topic == "all" || topic == "typescript_client"

	sb.WriteString("# VibeBackr Frontend & UI Integration Guide\n\n")

	if includeOverview {
		sb.WriteString("## 1. Overview\n")
		sb.WriteString("VibeBackr serves as the instant backend for UI applications (React, Next.js, Vue, Svelte, HTML/JS).\n")
		sb.WriteString(fmt.Sprintf("- **Canonical Backend URL**: `%s`\n", baseURL))
		sb.WriteString(fmt.Sprintf("  Applications and frontends built on this system build directly against `%s`. Apps do not need to be configurable to set the VibeBackr endpoint because the agent knows the canonical URL.\n", baseURL))
		sb.WriteString("- **Data Model**: Collections contain universal Records. Records store `title`, `content` (Markdown/text), and an arbitrary JSON `data` payload.\n")
		sb.WriteString("- **Permissions**: Automatic 5-tier waterfall ACLs based on caller's identity (Owner -> Manager Hierarchy -> Public -> Shared -> Admin).\n\n")
	}

	if includeAuth {
		sb.WriteString("## 2. Authentication for Frontends\n")
		sb.WriteString("Every API request requires an `Authorization: Bearer <token>` header.\n\n")
		sb.WriteString("### Centralized OAuth Broker Architecture\n")
		sb.WriteString(fmt.Sprintf("Google, GitHub, and OIDC providers **always redirect to VibeBackr** (`%s/auth/callback/{provider}`). You **never** need to create an OAuth app with Google Cloud Console, manage client secrets, or register `localhost` callback URLs. Once authenticated, VibeBackr finishes the token exchange and automatically redirects or transmits the session tokens back to your frontend application.\n\n", baseURL))

		activeProviders := s.GetProviderNames()
		sb.WriteString("### Available Identity Providers on this Instance\n")
		if len(activeProviders) > 0 {
			sb.WriteString("The following authentication providers are currently enabled and configured on this instance:\n")
			for _, p := range activeProviders {
				dispName := p
				if len(dispName) > 0 {
					dispName = strings.ToUpper(dispName[:1]) + dispName[1:]
				}
				sb.WriteString(fmt.Sprintf("- **%s**: `GET %s/auth/login/%s` (Popup: `?popup=true`, Redirect: `?redirect_uri=<uri>`)\n", dispName, baseURL, p))
			}
		} else {
			sb.WriteString("No third-party SSO providers are currently configured via environment variables. To enable, configure `GOOGLE_CLIENT_ID`, `GITHUB_CLIENT_ID`, or `OIDC_CLIENT_ID`.\n")
		}
		provJSON, _ := json.Marshal(activeProviders)
		sb.WriteString(fmt.Sprintf("\nClients can query supported providers dynamically at runtime via:\n`GET %s/api/auth/providers` -> `{ \"providers\": %s }`\n\n", baseURL, string(provJSON)))

		sb.WriteString("### Frontend Authentication Methods\n\n")
		sb.WriteString("#### Method A: Instant Popup Login (Recommended for Local Dev, `file://` URLs, & SPAs)\n")
		sb.WriteString("Opens a popup to sign in with Google. When completed, VibeBackr uses `postMessage` to transmit the tokens directly into your local application window and closes the popup. **This works seamlessly on local `file:///.../index.html` pages with zero local server setup needed!**\n")
		sb.WriteString("```typescript\n")
		sb.WriteString("client.login('google').then(auth => {\n")
		sb.WriteString("    console.log('Logged in!', auth.access_token);\n")
		sb.WriteString("});\n")
		sb.WriteString("```\n\n")

		sb.WriteString("#### Method B: Full-Page Redirect (Web Apps & Dev Servers)\n")
		sb.WriteString("Redirect the user to the login endpoint passing your local dev server or app URL as `redirect_uri`:\n")
		sb.WriteString("```typescript\n")
		sb.WriteString("window.location.href = client.getLoginUrl('google', 'http://localhost:3000');\n")
		sb.WriteString("client.handleAuthCallback();\n")
		sb.WriteString("```\n\n")

		sb.WriteString("### Local Development & `file://` URLs (Null Origins)\n")
		sb.WriteString("When testing frontends locally by opening HTML files directly in your browser (`file:///path/to/index.html`):\n")
		sb.WriteString("- **Why `window.location.origin` is `\"null\"`**: Browsers assign an opaque origin to `file://` documents. In JavaScript, `window.location.origin` evaluates to the string `\"null\"`, and network fetches carry an `Origin: null` header.\n")
		sb.WriteString("- **CORS Support**: VibeBackr permits `null` origins with Bearer tokens, so REST and MCP calls from local files succeed out of the box.\n")
		sb.WriteString("- **Best Solution**: Use Popup Login (`client.login('google')`). Because modern browsers block HTTP 307 redirects to `file://` schemes, popup login uses `postMessage` to pass the token back without any redirect barriers.\n\n")

		sb.WriteString("### CSP & Cross-Origin Fallback (Relay Page)\n")
		sb.WriteString("When developing in environments with a strict Content Security Policy (`connect-src` restrictions), sandboxed iframes, Figma plugins, or restricted webviews that block direct network calls to external APIs:\n")
		sb.WriteString(fmt.Sprintf("- **The Relay Bridge**: VibeBackr provides a lightweight same-origin bridge hosted at `%s/relay` (`%s/relay.html`).\n", baseURL, baseURL))
		sb.WriteString("- **How it Works**: Your application embeds a hidden `<iframe>` (or opens a child window) pointing to `/relay`. Because the relay runs on the VibeBackr domain, all its API requests are same-origin. Your application dispatches requests and receives responses securely via `window.postMessage`.\n")
		sb.WriteString("- **Automatic Fallback in TypeScript Client**: The provided client detects when direct `fetch` is blocked by CSP (`TypeError`) and automatically initiates the iframe relay fallback with zero extra code.\n")
		sb.WriteString("```typescript\n")
		sb.WriteString(fmt.Sprintf("const client = createVibeClient('%s', {useRelay: true});\n", baseURL))
		sb.WriteString("```\n\n")

		sb.WriteString("### Authenticated Request Pattern\n")
		sb.WriteString("```typescript\n")
		sb.WriteString("const token = localStorage.getItem('vb_token');\n")
		sb.WriteString(fmt.Sprintf("fetch('%s/api/collections', {\n", baseURL))
		sb.WriteString("    headers: {\n")
		sb.WriteString("        Authorization: `Bearer ${token}`,\n")
		sb.WriteString("        'Content-Type': 'application/json'\n")
		sb.WriteString("    }\n")
		sb.WriteString("}).then(res => res.json());\n")
		sb.WriteString("```\n\n")
	}

	if includeRecords {
		sb.WriteString("## 3. Core REST Endpoints\n\n")
		sb.WriteString("### Tenants\n")
		sb.WriteString("- `GET /api/tenants` -> Returns list of tenants the user owns or belongs to.\n")
		sb.WriteString("- `POST /api/tenants` -> Provisions a primary tenant for the user if not already provisioned.\n")
		sb.WriteString("- `DELETE /api/tenants/{tenant_id}` -> Leaves a joined tenant.\n\n")
		sb.WriteString("### Collections\n")
		sb.WriteString("- `GET /api/collections` -> Returns list of accessible collections `Collection[]`\n")
		sb.WriteString("- `POST /api/collections` -> Creates a collection (and optionally seeds initial `records`).\n")
		sb.WriteString("- `GET /api/collections/{collection_id}` -> Returns a single collection by ID.\n")
		sb.WriteString("- `PUT /api/collections/{collection_id}` -> Updates collection metadata, visibility, or allowed CORS origins.\n")
		sb.WriteString("- `PUT /api/collections/{collection_id}/app` -> Publishes a single-file HTML frontend (`{ \"html\": \"<!DOCTYPE html>...\" }`) to `https://{dashless_collection_id}.vibebac.kr`.\n")
		sb.WriteString("- `DELETE /api/collections/{collection_id}` -> Deletes a collection.\n\n")
		sb.WriteString("### Universal Records CRUD\n")
		sb.WriteString("- `GET /api/collections/{collection_id}/records`\n")
		sb.WriteString("  - Returns list of records visible to the current authenticated user.\n")
		sb.WriteString("- `POST /api/collections/{collection_id}/records`\n")
		sb.WriteString("  - Request payload:\n")
		sb.WriteString("    ```json\n")
		sb.WriteString("    {\n")
		sb.WriteString("      \"title\": \"Example Title\",\n")
		sb.WriteString("      \"content\": \"Optional description or markdown notes\",\n")
		sb.WriteString("      \"data\": { \"anyKey\": \"anyValue\", \"status\": \"active\" },\n")
		sb.WriteString("      \"access_policy\": {\n")
		sb.WriteString("        \"visibility\": \"public\",\n")
		sb.WriteString("        \"shares\": [\"teammate@company.com\"]\n")
		sb.WriteString("      }\n")
		sb.WriteString("    }\n")
		sb.WriteString("    ```\n")
		sb.WriteString("- `GET /api/collections/{collection_id}/records/{record_id}` -> Returns single record.\n")
		sb.WriteString("- `PUT /api/collections/{collection_id}/records/{record_id}` -> Updates title, content, data, or access_policy.\n")
		sb.WriteString("- `DELETE /api/collections/{collection_id}/records/{record_id}` -> Deletes record (returns `{ \"message\": \"record deleted\" }`).\n\n")
	}

	if includeSearch {
		sb.WriteString("## 4. Search & RAG\n")
		sb.WriteString("- `POST /api/collections/{collection_id}/search`\n")
		sb.WriteString("  - Hybrid vector search (EmbeddingGemma 2) with automatic SQL text search fallback.\n")
		sb.WriteString("  - Request payload:\n")
		sb.WriteString("    ```json\n")
		sb.WriteString("    {\n")
		sb.WriteString("      \"query\": \"search query string\",\n")
		sb.WriteString("      \"limit\": 20\n")
		sb.WriteString("    }\n")
		sb.WriteString("    ```\n")
		sb.WriteString("  - Returns array of `{ \"record\": Record, \"score\": float, \"method\": string }`.\n\n")
	}

	if includeClient {
		sb.WriteString("## 5. Frontend SDK & Client Integration\n\n")
		sb.WriteString("### Option A: CDN Script Tag (Fastest for HTML files, Prototypes, & Codepen/Web)\n")
		sb.WriteString("Include the complete VibeBackr client SDK directly via script tag - zero npm, build steps, or bundle configuration required:\n\n")
		sb.WriteString("```html\n")
		sb.WriteString(fmt.Sprintf("<script src=\"%s/vibebackr.js\"></script>\n\n<script>\n    const vibe = new VibeBackr({collectionId: 'YOUR_COLLECTION_ID'});\n\n    function init() {\n        vibe.loginWithGoogle()\n            .then(() => vibe.createRecord({\n                title: 'Project Alpha',\n                content: 'Initial requirements and roadmap notes',\n                data: {priority: 'high', status: 'in-progress'}\n            }))\n            .then(() => Promise.all([\n                vibe.listRecords(),\n                vibe.searchRecords('roadmap priorities')\n            ]))\n            .then(([items, results]) => {\n                console.log('Records:', items, 'Search:', results);\n            });\n    }\n</script>\n", baseURL))
		sb.WriteString("```\n\n")
		sb.WriteString(fmt.Sprintf("Also available at short alias `<script src=\"%s/vibe.js\"></script>` or via `const client = createVibeClient()`.\n\n", baseURL))
		sb.WriteString(fmt.Sprintf("#### TypeScript Bindings (.d.ts)\nType definitions are hosted directly alongside the script at [`%s/vibebackr.d.ts`](%s/vibebackr.d.ts) (alias: [`%s/vibe.d.ts`](%s/vibe.d.ts)). You can reference them directly in your project via:\n", baseURL, baseURL, baseURL, baseURL))
		sb.WriteString("```typescript\n")
		sb.WriteString(fmt.Sprintf("/// <reference path=\"%s/vibebackr.d.ts\" />\n", baseURL))
		sb.WriteString("```\n\n")
		sb.WriteString("> **Built-in Automatic Token Refresh:** Access tokens expire after 15 minutes. Both the CDN SDK (`vibebackr.js`) and the drop-in TypeScript client below automatically persist rotated refresh tokens in `localStorage`. When any API request receives a `401 Unauthorized`, the client transparently exchanges the refresh token at `POST /oauth/token`, rotates credentials, and retries the request seamlessly without prompting the user or throwing unhandled errors. You can also manually trigger `client.refreshToken()` at any time.\n\n")
		sb.WriteString("### Option B: Drop-in TypeScript Client (Copy & Paste into React / Next.js / Vite)\n")
		sb.WriteString("Save this as `src/lib/vibeBackr.ts` in your frontend project:\n\n")
		sb.WriteString("```typescript\n")
		sb.WriteString(fmt.Sprintf(`export type AccessPolicy = {
    visibility: 'private' | 'public' | 'shared';
    shares?: string[];
};

export type VibeRecord<T = Record<string, any>> = {
    id: string;
    collection_id: string;
    title: string;
    content: string;
    data: string; //json string payload
    created_by: string;
    created_at: string;
    updated_at: string;
    access_policy: AccessPolicy;
};

export type SearchResult<T = Record<string, any>> = {
    record: VibeRecord<T>;
    score: number;
    method?: string;
};

export type VibeClientOptions = {
    baseUrl?: string;
    collectionId?: string;
    tokenKey?: string;
    refreshTokenKey?: string;
    useRelay?: boolean;
    relayUrl?: string;
};

//built against canonical vibebackr backend: %s
export function createVibeClient(baseUrlOrOptions: string | VibeClientOptions = '%s', options: VibeClientOptions = {}) {
    let baseUrl = '%s';
    if(typeof baseUrlOrOptions === 'object' && baseUrlOrOptions !== null) {
        options = baseUrlOrOptions;
        baseUrl = options.baseUrl || '%s';
    } else {
        baseUrl = baseUrlOrOptions || options.baseUrl || '%s';
    }
    baseUrl = baseUrl.replace(/\/+$/, '');

    const defaultCollectionId = options.collectionId || null;
    const tokenKey = options.tokenKey || 'vb_token';
    const refreshTokenKey = options.refreshTokenKey || 'vb_refresh_token';
    const state: {refreshPromise?: Promise<any>; relayPromise?: Promise<Window>} = {};

    const getToken = () => (typeof window !== 'undefined' ? localStorage.getItem(tokenKey) : null);
    const setRefreshToken = (t: string | null) => {
        if(typeof window === 'undefined') {
            return;
        }
        if(t) {
            localStorage.setItem(refreshTokenKey, t);
        } else {
            localStorage.removeItem(refreshTokenKey);
        }
    };
    const setToken = (t: string | null, rt?: string | null) => {
        if(typeof window !== 'undefined') {
            if(t) {
                localStorage.setItem(tokenKey, t);
            } else {
                localStorage.removeItem(tokenKey);
            }
        }
        if(rt !== undefined) {
            setRefreshToken(rt);
        }
    };
    const getRefreshToken = () => (typeof window !== 'undefined' ? localStorage.getItem(refreshTokenKey) : null);

    function resolveColId(colId?: string): string {
        const id = colId || defaultCollectionId;
        if(!id) {
            throw new Error('collectionId is required');
        }
        return id;
    }

    function refreshToken(): Promise<any> {
        if(state.refreshPromise) {
            return state.refreshPromise;
        }
        const rt = getRefreshToken();
        if(!rt) {
            return Promise.reject(new Error('No refresh token available'));
        }

        const body = 'grant_type=refresh_token&refresh_token=' + encodeURIComponent(rt);
        const refreshOpts: RequestInit = {
            method: 'POST',
            headers: {'Content-Type': 'application/x-www-form-urlencoded'},
            body
        };
        (refreshOpts as any)._skipRefresh = true;

        const refreshCall = options.useRelay
            ? requestViaRelay<any>('/oauth/token', refreshOpts)
            : fetch(baseUrl + '/oauth/token', refreshOpts).then(res => {
                if(!res.ok) {
                    return res.json()
                        .catch(() => ({error: res.statusText}))
                        .then(err => {
                            throw new Error(err.error || ('Token refresh failed: ' + res.status));
                        });
                }
                return res.json();
            });

        state.refreshPromise = refreshCall
            .then((data: any) => {
                delete state.refreshPromise;
                if(data?.access_token) {
                    setToken(data.access_token);
                }
                if(data?.refresh_token) {
                    setRefreshToken(data.refresh_token);
                }
                return data;
            })
            .catch(err => {
                delete state.refreshPromise;
                setToken(null);
                setRefreshToken(null);
                throw err;
            });

        return state.refreshPromise;
    }

    function getRelayWindow(): Promise<Window> {
        if(typeof window === 'undefined') {
            return Promise.reject(new Error('Relay transport requires browser window'));
        }
        if(state.relayPromise) {
            return state.relayPromise;
        }

        state.relayPromise = new Promise(resolve => {
            let frame = document.getElementById('vb-relay-frame') as HTMLIFrameElement;
            if(!frame) {
                frame = document.createElement('iframe');
                frame.id = 'vb-relay-frame';
                frame.src = options.relayUrl || (baseUrl + '/relay');
                frame.style.display = 'none';
                frame.setAttribute('aria-hidden', 'true');
                document.body.appendChild(frame);
            }

            const onMsg = (e: MessageEvent) => {
                if(
                    e.data
                    && (e.data.type === 'vb_relay_ready' || e.data.type === 'vb_relay_pong')
                ) {
                    window.removeEventListener('message', onMsg);
                    resolve(frame.contentWindow!);
                }
            };
            window.addEventListener('message', onMsg);

            const pingTimer = setInterval(() => {
                if(frame.contentWindow) {
                    frame.contentWindow.postMessage({type: 'vb_relay_ping'}, '*');
                }
            }, 150);

            frame.onload = () => {
                clearInterval(pingTimer);
                if(frame.contentWindow) {
                    resolve(frame.contentWindow);
                }
            };
        });
        return state.relayPromise;
    }

    function requestViaRelay<T>(path: string, opts: RequestInit = {}): Promise<T> {
        return getRelayWindow().then(relayWin => {
            const token = getToken();
            const headers: Record<string, string> = {
                'Content-Type': 'application/json',
                ...(token ? {Authorization: 'Bearer ' + token} : {}),
                ...(opts.headers as any)
            };
            const reqId = 'req_' + Math.random().toString(36).substring(2, 9) + '_' + Date.now();

            return new Promise<T>((resolve, reject) => {
                const handler = (e: MessageEvent) => {
                    if(
                        !e.data
                        || e.data.type !== 'vb_relay_response'
                        || e.data.id !== reqId
                    ) {
                        return;
                    }
                    window.removeEventListener('message', handler);
                    if(e.data.ok) {
                        resolve(e.data.data as T);
                        return;
                    }
                    if(
                        e.data.status === 401
                        && !(opts as any)._retry
                        && !(opts as any)._skipRefresh
                        && getRefreshToken()
                    ) {
                        (opts as any)._retry = true;
                        refreshToken()
                            .then(requestViaRelay.bind(null, path, opts))
                            .then(resolve)
                            .catch(reject);
                        return;
                    }
                    const err = (e.data.data && e.data.data.error)
                        || e.data.error
                        || ('Relay request failed: ' + e.data.status);
                    reject(new Error(err));
                };
                window.addEventListener('message', handler);
                relayWin.postMessage({
                    type: 'vb_relay_request',
                    id: reqId,
                    path,
                    method: opts.method || 'GET',
                    headers,
                    body: opts.body
                }, '*');
            });
        });
    }

    function request<T = any>(path: string, opts: RequestInit = {}): Promise<T> {
        if(options.useRelay) {
            return requestViaRelay<T>(path, opts);
        }
        const token = getToken();
        const headers: Record<string, string> = {
            'Content-Type': 'application/json',
            ...(token ? {Authorization: 'Bearer ' + token} : {}),
            ...(opts.headers as Record<string, string>)
        };
        return fetch(baseUrl + path, {...opts, headers})
            .then(res => {
                if(
                    res.status === 401
                    && !(opts as any)._retry
                    && !(opts as any)._skipRefresh
                    && getRefreshToken()
                ) {
                    (opts as any)._retry = true;
                    return refreshToken().then(request.bind(null, path, opts));
                }
                if(!res.ok) {
                    return res.json()
                        .catch(() => ({error: res.statusText}))
                        .then(err => {
                            throw new Error(err.error || ('Request failed: ' + res.status));
                        });
                }
                return res.json();
            })
            .catch((err: any) => {
                if(
                    err.name === 'TypeError'
                    && typeof window !== 'undefined'
                    && !options.useRelay
                ) {
                    console.warn('[VibeBackr] Direct fetch blocked (CSP/CORS). Falling back to iframe relay...');
                    return requestViaRelay<T>(path, opts);
                }
                throw err;
            });
    }

    const login = (provider = 'google', collectionId?: string) =>
        new Promise<any>(resolve => {
            const width = 500;
            const height = 650;
            const left = typeof window !== 'undefined' ? (window.screenX || 0) + (window.outerWidth - width) / 2 : 0;
            const top = typeof window !== 'undefined' ? (window.screenY || 0) + (window.outerHeight - height) / 2 : 0;
            const targetCol = collectionId || defaultCollectionId;
            const colParam = targetCol ? '&collection_id=' + encodeURIComponent(targetCol) : '';
            if(typeof window !== 'undefined') {
                window.open(
                    baseUrl + '/auth/login/' + provider + '?popup=true' + colParam,
                    'vibebackr_login',
                    'width=' + width + ',height=' + height + ',left=' + left + ',top=' + top
                );
            }
            const onMessage = (e: any) => {
                if(
                    e.origin
                    && e.origin !== 'null'
                    && !baseUrl.startsWith(e.origin)
                ) {
                    try {
                        const bHost = new URL(baseUrl).hostname;
                        const oHost = new URL(e.origin).hostname;
                        if(
                            bHost !== oHost
                            && !bHost.endsWith('.' + oHost)
                            && !oHost.endsWith('.' + bHost)
                        ) {
                            return;
                        }
                    } catch(_) {
                        return;
                    }
                }
                if(e.data && e.data.access_token) {
                    if(typeof window !== 'undefined') {
                        window.removeEventListener('message', onMessage);
                    }
                    setToken(e.data.access_token);
                    if(e.data.refresh_token) {
                        setRefreshToken(e.data.refresh_token);
                    }
                    resolve(e.data);
                }
            };
            if(typeof window !== 'undefined') {
                window.addEventListener('message', onMessage);
            }
        });

    const listCollections = () => request<any[]>('/api/collections');
    const listRecords = <T = any>(colId?: string) =>
        request<VibeRecord<T>[]>('/api/collections/' + encodeURIComponent(resolveColId(colId)) + '/records');

    const searchRecords = <T = any>(colIdOrQuery: string, queryOrLimit?: string | number, limit = 20) => {
        let targetCol: string;
        let query: string;
        let maxResults: number;
        if(
            defaultCollectionId
            && (queryOrLimit === undefined || typeof queryOrLimit === 'number')
        ) {
            targetCol = defaultCollectionId;
            query = colIdOrQuery;
            maxResults = typeof queryOrLimit === 'number' ? queryOrLimit : 20;
        } else {
            targetCol = resolveColId(colIdOrQuery);
            query = String(queryOrLimit || '');
            maxResults = limit;
        }
        return request<SearchResult<T>[]>('/api/collections/' + encodeURIComponent(targetCol) + '/search', {
            method: 'POST',
            body: JSON.stringify({query, limit: maxResults})
        });
    };

    return {
        baseUrl,
        collectionId: defaultCollectionId,
        getToken,
        setToken,
        getRefreshToken,
        setRefreshToken,
        refreshToken,
        initRelay: getRelayWindow,
        login,
        loginWithGoogle: login.bind(null, 'google'),
        loginWithGitHub: login.bind(null, 'github'),
        loginMock: login.bind(null, 'mock'),
        getLoginUrl: (provider = 'google', redirectUri?: string, collectionId?: string) => {
            let uri = redirectUri;
            if(!uri && typeof window !== 'undefined') {
                const isFile = window.location.protocol === 'file:'
                    || window.location.origin === 'null';
                uri = isFile ? 'http://localhost:3000' : window.location.origin;
            }
            const targetCol = collectionId || defaultCollectionId;
            const colParam = targetCol ? '&collection_id=' + encodeURIComponent(targetCol) : '';
            return baseUrl + '/auth/login/' + provider + '?redirect_uri=' + encodeURIComponent(uri || '') + colParam;
        },
        handleAuthCallback: () => {
            if(typeof window === 'undefined') {
                return null;
            }
            if(window.location.hash.includes('access_token=')) {
                const params = new URLSearchParams(window.location.hash.substring(1));
                const token = params.get('access_token');
                if(token) {
                    setToken(token);
                    const refToken = params.get('refresh_token');
                    if(refToken) {
                        setRefreshToken(refToken);
                    }
                    window.location.hash = '';
                    return token;
                }
            }
            return getToken();
        },
        logout: () => {
            const rt = getRefreshToken();
            setToken(null);
            setRefreshToken(null);
            const body = rt ? 'refresh_token=' + encodeURIComponent(rt) : '';
            return request('/api/auth/logout', {
                method: 'POST',
                headers: {'Content-Type': 'application/x-www-form-urlencoded'},
                body,
                ...({_skipRefresh: true} as any)
            }).catch(() => ({message: 'logged out locally'}));
        },
        revokeAllSessions: () => request('/api/auth/revoke-all', {method: 'POST'}),
        listProviders: () => request<{providers: string[]}>('/api/auth/providers').then(r => r.providers || []),
        listTenants: () => request<any[]>('/api/tenants'),
        createTenant: () => request<any>('/api/tenants', {method: 'POST'}),
        leaveTenant: (tenantId: string) =>
            request<{message: string; tenant_id: string}>('/api/tenants/' + encodeURIComponent(tenantId), {method: 'DELETE'}),
        listCollections,
        getCollections: listCollections,
        createCollection: (nameOrConfig: string | Record<string, any>, defaultVisibility = 'private', originUrls: string[] = []) => {
            const payload = (typeof nameOrConfig === 'object' && nameOrConfig !== null)
                ? nameOrConfig
                : {name: nameOrConfig, default_visibility: defaultVisibility, origin_urls: originUrls};
            return request<any>('/api/collections', {method: 'POST', body: JSON.stringify(payload)});
        },
        getCollection: (colId?: string) => request<any>('/api/collections/' + encodeURIComponent(resolveColId(colId))),
        updateCollection: (colId: string, payload: Record<string, any>) =>
            request<any>('/api/collections/' + encodeURIComponent(colId), {method: 'PUT', body: JSON.stringify(payload)}),
        deleteCollection: (colId: string) =>
            request<{message: string; id: string}>('/api/collections/' + encodeURIComponent(colId), {method: 'DELETE'}),
        listRecords,
        getRecords: listRecords,
        getRecord: <T = any>(colIdOrRecId: string, recId?: string) => {
            const targetCol = recId === undefined && defaultCollectionId ? defaultCollectionId : resolveColId(colIdOrRecId);
            const targetRec = recId === undefined && defaultCollectionId ? colIdOrRecId : recId!;
            return request<VibeRecord<T>>('/api/collections/' + encodeURIComponent(targetCol) + '/records/' + encodeURIComponent(targetRec));
        },
        createRecord: <T = any>(colIdOrPayload: string | Record<string, any>, payloadOrColId?: any) => {
            const isObj = typeof colIdOrPayload === 'object' && colIdOrPayload !== null;
            const targetCol = isObj ? resolveColId(payloadOrColId) : resolveColId(colIdOrPayload as string);
            const payload = (isObj ? colIdOrPayload : payloadOrColId) || {};
            const accessPolicy = payload.access_policy || {
                visibility: payload.visibility || '',
                shares: payload.shares || []
            };
            return request<VibeRecord<T>>('/api/collections/' + encodeURIComponent(targetCol) + '/records', {
                method: 'POST',
                body: JSON.stringify({
                    title: payload.title,
                    content: payload.content || '',
                    data: payload.data !== undefined ? payload.data : {},
                    access_policy: accessPolicy
                })
            });
        },
        updateRecord: <T = any>(colIdOrRecId: string, recIdOrPayload: string | Record<string, any>, payloadOrColId?: any) => {
            const isSecondObj = typeof recIdOrPayload === 'object' && recIdOrPayload !== null;
            const targetCol = isSecondObj ? resolveColId(payloadOrColId) : resolveColId(colIdOrRecId);
            const targetRec = isSecondObj ? colIdOrRecId : (recIdOrPayload as string);
            const body = {...(isSecondObj ? recIdOrPayload : payloadOrColId)};
            if(!body.access_policy && (body.visibility || body.shares)) {
                body.access_policy = {visibility: body.visibility || '', shares: body.shares || []};
            }
            return request<VibeRecord<T>>('/api/collections/' + encodeURIComponent(targetCol) + '/records/' + encodeURIComponent(targetRec), {
                method: 'PUT',
                body: JSON.stringify(body)
            });
        },
        deleteRecord: (colIdOrRecId: string, recId?: string) => {
            const targetCol = recId === undefined && defaultCollectionId ? defaultCollectionId : resolveColId(colIdOrRecId);
            const targetRec = recId === undefined && defaultCollectionId ? colIdOrRecId : recId!;
            return request<{message: string}>('/api/collections/' + encodeURIComponent(targetCol) + '/records/' + encodeURIComponent(targetRec), {
                method: 'DELETE'
            });
        },
        searchRecords,
        search: searchRecords
    };
}
`, baseURL, baseURL, baseURL, baseURL, baseURL))
		sb.WriteString("```\n\n")
	}

	// Collection-specific introspection if collectionID is provided
	if collectionID != "" {
		var targetCol *domain.Collection
		if col, err := s.repo.GetCollection(ctx, collectionID); err == nil && col != nil {
			targetCol = col
		} else {
			if cols, err := s.repo.ListCollectionsForUser(ctx, caller); err == nil {
				for _, c := range cols {
					if strings.EqualFold(c.Name, collectionID) || c.ID == collectionID || (c.Slug != "" && strings.EqualFold(c.Slug, collectionID)) {
						colCopy := c
						targetCol = &colCopy
						break
					}
				}
			}
		}

		if targetCol != nil && !s.canAccessCollectionTenant(ctx, caller, targetCol) {
			targetCol = nil
		}

		if targetCol != nil {
			sb.WriteString(fmt.Sprintf("## 6. Targeted Collection: %s (ID: `%s`)\n\n", targetCol.Name, targetCol.ID))
			if targetCol.Slug != "" {
				sb.WriteString(fmt.Sprintf("- **Project Slug**: `%s`\n", targetCol.Slug))
			}
			sb.WriteString(fmt.Sprintf("- **Default Visibility**: `%s`\n", targetCol.DefaultVisibility))
			sb.WriteString(fmt.Sprintf("- **Vector Search Enabled**: `%t`\n", targetCol.VectorSearchEnabled))
			sb.WriteString(fmt.Sprintf("- **Auto-Invite Shares**: `%t`\n", targetCol.AutoInviteShares))
			if len(targetCol.OriginURLs) > 0 {
				sb.WriteString(fmt.Sprintf("- **Configured Origin URLs**: `%s`\n", strings.Join(targetCol.OriginURLs, ", ")))
			}
			sb.WriteString("\n")

			effectiveCaller := caller
			if effectiveCaller == nil {
				effectiveCaller = &domain.User{ID: "anonymous-guide", Role: domain.RoleMember}
			}
			records, _ := s.repo.ListRecords(ctx, targetCol.ID, effectiveCaller)

			var inferredFields []string
			if len(records) > 0 && records[0].Data != "" && records[0].Data != "{}" {
				var sampleMap map[string]interface{}
				if err := json.Unmarshal([]byte(records[0].Data), &sampleMap); err == nil {
					for k, v := range sampleMap {
						tsType := "any"
						switch v.(type) {
						case string:
							tsType = "string"
						case float64:
							tsType = "number"
						case bool:
							tsType = "boolean"
						case []interface{}:
							tsType = "any[]"
						case map[string]interface{}:
							tsType = "Record<string, any>"
						}
						inferredFields = append(inferredFields, fmt.Sprintf("    %s?: %s;", k, tsType))
					}
				}
			}

			typeName := toPascalCase(targetCol.Name)

			sb.WriteString("### Tailored TypeScript Types for this Collection:\n```typescript\n")
			sb.WriteString(fmt.Sprintf("export type %sData = {\n", typeName))
			if len(inferredFields) > 0 {
				for _, f := range inferredFields {
					sb.WriteString(f + "\n")
				}
			} else {
				sb.WriteString("    [key: string]: any;\n")
			}
			sb.WriteString("};\n\n")
			sb.WriteString(fmt.Sprintf("export type %sRecord = VibeRecord<%sData>;\n", typeName, typeName))
			sb.WriteString("```\n\n")

			sb.WriteString("### Usage Example:\n```typescript\n")
			sb.WriteString(fmt.Sprintf("const client = createVibeClient(); //defaults to %s\n", baseURL))
			sb.WriteString(fmt.Sprintf("client.listRecords<%sData>('%s').then(records => {\n", typeName, targetCol.ID))
			sb.WriteString("    console.log(records);\n")
			sb.WriteString("});\n")
			sb.WriteString("```\n\n")
		} else {
			sb.WriteString(fmt.Sprintf("## 6. Targeted Collection `%s`\nCollection not found. You can create it via `create_collection` first.\n\n", collectionID))
		}
	}

	return sb.String(), nil
}

func toPascalCase(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_'
	})
	var b strings.Builder
	for _, p := range parts {
		if len(p) > 0 {
			b.WriteString(strings.ToUpper(p[:1]) + strings.ToLower(p[1:]))
		}
	}
	res := b.String()
	if res == "" {
		return "Custom"
	}
	return res
}
