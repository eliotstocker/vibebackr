package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}

func defaultBillingEnabled() bool {
	if val := strings.TrimSpace(os.Getenv("ENABLE_BILLING")); val != "" {
		return strings.EqualFold(val, "true") || val == "1"
	}
	return os.Getenv("GOCARDLESS_ACCESS_TOKEN") != ""
}

func defaultRateLimitEnabled() bool {
	if val := strings.TrimSpace(os.Getenv("ENABLE_RATE_LIMIT")); val != "" {
		return strings.EqualFold(val, "true") || val == "1"
	}
	return false
}

func defaultCollectionLimitsEnabled() bool {
	if val := strings.TrimSpace(os.Getenv("ENABLE_COLLECTION_LIMITS")); val != "" {
		return strings.EqualFold(val, "true") || val == "1"
	}
	return strings.TrimSpace(os.Getenv("MAX_COLLECTION_RECORDS")) != "" || strings.TrimSpace(os.Getenv("MAX_RECORD_BYTES")) != ""
}

func main() {
	var (
		port             = flag.String("port", getEnv("PORT", "8080"), "HTTP server port")
		driverName       = flag.String("driver", getEnv("DB_DRIVER", "sqlite"), "Database driver (sqlite, postgres)")
		dsn              = flag.String("dsn", getEnv("DATABASE_URL", "vibebackr.db"), "Database connection string or SQLite file path")
		jwtSecret        = flag.String("jwt-secret", getEnv("JWT_SECRET", "super-secret-vibebackr-key-change-in-prod"), "JWT HMAC secret")
		geminiAPIKey     = flag.String("gemini-api-key", getEnv("GEMINI_API_KEY", getEnv("GOOGLE_API_KEY", "")), "Google Gemini API key for gemini-embedding-001 (optional)")
		embedderURL      = flag.String("embedder-url", getEnv("EMBEDDER_URL", ""), "EmbeddingGemma 2 / Ollama endpoint (optional)")
		baseURL          = flag.String("base-url", getEnv("VIBEBACKR_URL", getEnv("BASE_URL", "")), "Canonical VibeBackr URL (e.g. https://vibebac.kr)")
		jwtTTLStr        = flag.String("jwt-ttl", getEnv("JWT_TTL", "15m"), "JWT access token lifetime (default: 15m)")
		refreshTTLStr    = flag.String("refresh-ttl", getEnv("REFRESH_TOKEN_TTL", "720h"), "Refresh token lifetime (default: 720h / 30 days)")
		enableBilling    = flag.Bool("enable-billing", defaultBillingEnabled(), "Enable per-tenant GoCardless billing, quotas, and vouchers (ENABLE_BILLING=true)")
		enableRateLimit  = flag.Bool("enable-rate-limit", defaultRateLimitEnabled(), "Enable per-collection token-bucket rate limiting (ENABLE_RATE_LIMIT=true)")
		rateLimitStarter = flag.Int("rate-limit-starter", getEnvInt("RATE_LIMIT_STARTER_RPM", ratelimit.DefaultStarterRPM), "Starter plan requests per minute per collection")
		rateLimitPro     = flag.Int("rate-limit-pro", getEnvInt("RATE_LIMIT_PRO_RPM", ratelimit.DefaultProRPM), "Pro plan requests per minute per collection")
		enableColLimits  = flag.Bool("enable-collection-limits", defaultCollectionLimitsEnabled(), "Enable collection record count and per-record byte limits (ENABLE_COLLECTION_LIMITS=true)")
		maxColRecords    = flag.Int("max-collection-records", getEnvInt("MAX_COLLECTION_RECORDS", repository.DefaultMaxCollectionRecords), "Maximum records per collection when collection limits are enabled")
		maxRecordBytes   = flag.Int("max-record-bytes", getEnvInt("MAX_RECORD_BYTES", repository.DefaultMaxRecordBytes), "Maximum byte size per record when collection limits are enabled")
		instanceName     = flag.String("instance-name", getEnv("INSTANCE_NAME", getEnv("COMPANY_NAME", getEnv("ORG_NAME", ""))), "Custom company/organization name for self-hosted landing page (INSTANCE_NAME)")
		voucherCode      = flag.String("code", "", "Voucher code (for 'vibebackr voucher' command)")
		voucherPlan      = flag.String("plan", "pro", "Voucher plan: 'pro' (25 collections) or 'starter' (10 collections)")
		voucherUses      = flag.Int("max-uses", 1, "Maximum redemptions for voucher (0 for unlimited)")
	)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "VibeBackr - Self-contained cloud persistence, authentication, and vector search backend\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  vibebackr [command] [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  serve (default)   Run REST API and streamable HTTP MCP server\n")
		fmt.Fprintf(os.Stderr, "  mcp               Run local MCP JSON-RPC server over stdio\n")
		fmt.Fprintf(os.Stderr, "  voucher           Create or update a free-account voucher code\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}

	command := "serve"
	if len(os.Args) > 1 && os.Args[1][0] != '-' {
		command = os.Args[1]
		// Shift args for flag parsing
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}
	flag.Parse()

	// Initialize database and migrations
	database, err := db.Connect(*driverName, *dsn)
	if err != nil {
		log.Fatalf("Fatal: Database initialization failed: %v", err)
	}
	defer database.Close()

	repo := repository.New(database)

	explicitLimitFlag := false
	explicitEnableFlag := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "max-collection-records", "max-record-bytes":
			explicitLimitFlag = true
		case "enable-collection-limits":
			explicitEnableFlag = true
		}
	})
	if *enableColLimits || (explicitLimitFlag && !explicitEnableFlag) {
		repo.SetCollectionLimits(*maxColRecords, *maxRecordBytes)
	}

	var billingMod *billing.Module
	if *enableBilling || command == "voucher" {
		bMod, bErr := billing.NewModule(database, billing.Config{
			Enabled:       *enableBilling || command == "voucher",
			AccessToken:   os.Getenv("GOCARDLESS_ACCESS_TOKEN"),
			WebhookSecret: os.Getenv("GOCARDLESS_WEBHOOK_SECRET"),
			Environment:   os.Getenv("GOCARDLESS_ENV"),
			SeedVouchers:  os.Getenv("VOUCHER_CODES"),
		})
		if bErr != nil {
			log.Fatalf("Fatal: Billing module initialization failed: %v", bErr)
		}
		billingMod = bMod
		repo.SetWriteGuard(billingMod.CheckWriteAccess)
	}

	var rlMod *ratelimit.Module
	if *enableRateLimit {
		rlMod = ratelimit.NewModule(
			ratelimit.Config{
				Enabled:    true,
				StarterRPM: *rateLimitStarter,
				ProRPM:     *rateLimitPro,
			},
			func(ctx context.Context, idOrSlug string) (string, string, error) {
				col, err := repo.GetCollection(ctx, idOrSlug)
				if err != nil {
					return "", "", err
				}
				return col.ID, col.TenantID, nil
			},
			func(ctx context.Context, tenantID string) string {
				if billingMod != nil && billingMod.Enabled() && tenantID != "" {
					if tb, err := billingMod.EnsureTenant(ctx, tenantID); err == nil && tb != nil {
						return tb.Plan
					}
				}
				return "starter"
			},
		)
	}

	// Configure optional vector embedder
	var embedder search.Embedder
	if *geminiAPIKey != "" {
		embedder = search.NewGeminiEmbedder(*geminiAPIKey, getEnv("GEMINI_EMBEDDING_MODEL", "gemini-embedding-001"))
	} else if *embedderURL != "" {
		embedder = search.NewOllamaGemmaEmbedder(*embedderURL, "embeddinggemma2")
	}

	searchEngine := search.NewSearchEngine(database, repo, embedder)
	mcpServer := mcp.NewServer(repo, searchEngine)
	if billingMod != nil {
		mcpServer.SetBilling(billingMod)
	}
	if rlMod != nil {
		mcpServer.SetRateLimiter(rlMod)
	}

	canonicalBaseURL := *baseURL
	if canonicalBaseURL == "" {
		if dom := os.Getenv("CANONICAL_DOMAIN"); dom != "" {
			if strings.HasPrefix(dom, "http://") || strings.HasPrefix(dom, "https://") {
				canonicalBaseURL = dom
			} else {
				canonicalBaseURL = "https://" + dom
			}
		}
	}
	if canonicalBaseURL != "" {
		mcpServer.SetBaseURL(canonicalBaseURL)
	}

	// Setup SSO Manager and active providers
	ssoManager := auth.NewSSOManager(repo)
	if gClientID := os.Getenv("GOOGLE_CLIENT_ID"); gClientID != "" {
		gSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
		gRedirect := os.Getenv("GOOGLE_REDIRECT_URL")
		ssoManager.RegisterProvider(auth.NewGoogleSSOProvider(gClientID, gSecret, gRedirect))
	}
	if ghClientID := os.Getenv("GITHUB_CLIENT_ID"); ghClientID != "" {
		ghSecret := os.Getenv("GITHUB_CLIENT_SECRET")
		ghRedirect := os.Getenv("GITHUB_REDIRECT_URL")
		ssoManager.RegisterProvider(auth.NewGitHubSSOProvider(ghClientID, ghSecret, ghRedirect))
	}
	if oidcClientID := os.Getenv("OIDC_CLIENT_ID"); oidcClientID != "" {
		oidcName := getEnv("OIDC_NAME", "oidc")
		oidcSecret := os.Getenv("OIDC_CLIENT_SECRET")
		oidcAuthURL := os.Getenv("OIDC_AUTH_URL")
		oidcTokenURL := os.Getenv("OIDC_TOKEN_URL")
		oidcUserInfoURL := os.Getenv("OIDC_USERINFO_URL")
		oidcRedirect := os.Getenv("OIDC_REDIRECT_URL")
		ssoManager.RegisterProvider(auth.NewGenericOIDCProvider(
			oidcName, oidcClientID, oidcSecret, oidcAuthURL, oidcTokenURL, oidcUserInfoURL, oidcRedirect, nil,
		))
	}
	if len(ssoManager.ListProviderNames()) == 0 &&
		!strings.EqualFold(os.Getenv("ENABLE_MOCK_AUTH"), "false") &&
		!strings.EqualFold(os.Getenv("GO_ENV"), "production") &&
		!strings.EqualFold(os.Getenv("ENV"), "production") &&
		os.Getenv("AWS_LAMBDA_FUNCTION_NAME") == "" {
		_ = os.Setenv("ENABLE_MOCK_AUTH", "true")
		ssoManager.RegisterProvider(&auth.MockSSOProvider{})
	}
	mcpServer.SetProviderLister(ssoManager.ListProviderNames)

	switch command {
	case "voucher":
		if strings.TrimSpace(*voucherCode) == "" {
			log.Fatalf("Fatal: --code is required for 'vibebackr voucher' (e.g. vibebackr voucher --code VIP2026 --plan pro --max-uses 5)")
		}
		v := &billing.Voucher{
			Code:    *voucherCode,
			Plan:    *voucherPlan,
			MaxUses: *voucherUses,
		}
		if err := billingMod.UpsertVoucher(context.Background(), v); err != nil {
			log.Fatalf("Fatal: Failed saving voucher: %v", err)
		}
		fmt.Printf("Voucher %q saved (plan: %s, max_uses: %d)\n", v.Code, v.Plan, v.MaxUses)

	case "mcp":
		// Stdio MCP mode: local agent pair-programming
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigChan
			cancel()
		}()

		defaultAdmin := &domain.User{
			ID:    "system-mcp-agent",
			Email: "admin@agent.local",
			Name:  "Local MCP Agent",
			Role:  domain.RoleAdmin,
		}

		if err := mcpServer.ServeStdio(ctx, defaultAdmin); err != nil {
			log.Fatalf("MCP stdio error: %v", err)
		}

	case "serve":
		if *jwtSecret == "" || *jwtSecret == "super-secret-vibebackr-key-change-in-prod" {
			if strings.EqualFold(os.Getenv("GO_ENV"), "production") || strings.EqualFold(os.Getenv("ENV"), "production") {
				log.Fatalf("Fatal: JWT_SECRET must be set to a strong secret in production")
			}
			log.Printf("WARNING: Using default development JWT_SECRET. Set JWT_SECRET environment variable in production.")
		}
		jwtTTL, err := time.ParseDuration(*jwtTTLStr)
		if err != nil || jwtTTL <= 0 {
			jwtTTL = 1 * time.Hour
		}
		refreshTTL, err := time.ParseDuration(*refreshTTLStr)
		if err != nil || refreshTTL <= 0 {
			refreshTTL = 30 * 24 * time.Hour
		}
		tokenManager := auth.NewTokenManager(*jwtSecret, jwtTTL)
		tokenManager.SetRefreshTTL(refreshTTL)

		server := api.NewServer(repo, tokenManager, ssoManager, searchEngine, mcpServer)
		if *instanceName != "" {
			server.SetInstanceName(*instanceName)
		}
		if billingMod != nil {
			server.SetBilling(billingMod)
		}
		if rlMod != nil {
			server.SetRateLimiter(rlMod)
		}
		handler := server.Routes()

		httpServer := &http.Server{
			Addr:              ":" + *port,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
		}

		go func() {
			log.Printf("VibeBackr listening on http://localhost:%s (Driver: %s, Billing: %t, RateLimit: %t)", *port, *driverName, billingMod != nil && billingMod.Enabled(), rlMod != nil && rlMod.Enabled())
			log.Printf("• Streamable MCP endpoint: http://localhost:%s/mcp", *port)
			log.Printf("• REST API root: http://localhost:%s/api", *port)
			log.Printf("• Canonical URL: %s", mcpServer.GetBaseURL(nil))
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server failed: %v", err)
			}
		}()

		quit := make(chan os.Signal, 1)
		signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
		<-quit
		log.Println("Shutting down VibeBackr server gracefully...")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Fatalf("Server forced to shutdown: %v", err)
		}
		log.Println("Server exiting")

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		os.Exit(1)
	}
}
