---
title: For Nerds - How VibeBackr Works Under the Hood
description: Technical architecture, MCP server setup, 5-tier recursive waterfall ACLs, 768-d semantic vector search, and developer internals of VibeBackr.
logo: true
---

# How VibeBackr Works Under the Hood

For developers, indie hackers, and curious builders who want to know how VibeBackr turns a single AI prompt into a hosted, multi-user application with zero infrastructure setup.

:::cta
- [Client Guide (SDK & REST API)](/guide){.btn-primary}
- [Self-Hosting Guide](#self-hosting)
- `https://vibebac.kr/mcp`
:::

## Architecture at a Glance {#architecture}

VibeBackr is a single, zero-CGO Go binary that combines a remote Model Context Protocol (MCP) server, a centralized OAuth 2.0 / OIDC broker, a multi-tenant document & vector database, and an edge-hosted single-file HTML application store.

:::features
### Single-File Edge App Hosting
When an AI agent calls `publish_app`, VibeBackr validates and stores a self-contained HTML application (up to 512 KB) and serves it from `https://{slug_or_id}.vibebac.kr` via CloudFront & S3 (or local filesystem when self-hosted), automatically wiring the collection's allowed CORS and OAuth origins.

### Centralized OAuth Broker
Instead of requiring users to visit Google Cloud Console or configure redirect URIs, VibeBackr acts as an OAuth broker (supporting Google, GitHub, OIDC, PKCE, and RFC 7591 Dynamic Client Registration). Frontends sign users in with a single `vibe.loginWithGoogle()` call.

### 5-Tier Recursive Waterfall ACLs
Every query evaluates a 5-tier permission waterfall in SQL: **1) Record Ownership**, **2) Manager Hierarchy** (evaluated via recursive CTEs across the tenant's org chart), **3) Public Collection/Domain Visibility**, **4) Explicit Email Shares** (with optional `auto_invite_shares`), and **5) Workspace Admin Override**.

### Universal Records & 768-d Vector RAG
Each record stores a `title`, markdown `content`, and an arbitrary structured JSON `data` payload (up to 64 KB per record and 500 records per collection on managed cloud, uncapped by default when self-hosted). When vector search is enabled, records are indexed with 768-dimensional embeddings (`EmbeddingGemma 2` via `pgvector`) alongside a full-text SQL fallback.

### Zero-Dependency Browser SDK & CSP Relay
The client SDK (`vibebackr.js`) handles token refresh, `file://` null-origin CORS, and automatically falls back to a hidden `/relay` iframe `postMessage` bridge when running inside strict-CSP AI sandboxes (like Claude Artifacts).

### Per-Collection Token-Bucket Rate Limiting
Rate limits are isolated **per individual collection** so one noisy app never starves your other tools: **Standard** allows **60 req/min** (20-request burst), **Pro** allows **180 req/min** (45-request burst), and **VibeBackr OS** runs uncapped on your own hardware.
:::

## Connecting MCP Clients (Claude, Cursor, Copilot) {#mcp-setup}

Connect any Model Context Protocol (MCP) client to `https://vibebac.kr/mcp` over Streamable HTTP (`POST`) or Server-Sent Events (`GET`).

### 1. Claude Desktop & Claude Code

Install in one click with the Claude Desktop MCP Bundle (`.mcpb`), or add VibeBackr to your `claude_desktop_config.json`:

:::cta
- [Download `vibebackr.mcpb` (One-Click Claude Extension)](/vibebackr.mcpb){.btn-primary}
- `https://vibebac.kr/mcp`
:::

```json
{
  "mcpServers": {
    "vibebackr": {
      "url": "https://vibebac.kr/mcp"
    }
  }
}
```

### 2. Cursor, GitHub Copilot & Other MCP Clients

Add a remote MCP server in your editor settings:

```text
Server URL: https://vibebac.kr/mcp
Transport:  Streamable HTTP (POST) or Server-Sent Events (SSE)
Auth:       OAuth 2.1 with Dynamic Client Registration (RFC 7591) & PKCE
```

### 3. Available MCP Tools

Once connected, your AI agent has full tool access to provision, populate, publish, and administrate applications:

:::cli
- `create_collection`: Create an isolated collection (`name`, `slug`, `default_visibility`, `allowed_domains`, `origin_urls`, `vector_search_enabled`, `auto_invite_shares`).
- `update_collection`: Update collection metadata, visibility rules, custom slug, auto-invite settings, or allowed CORS origins.
- `publish_app`: Deploy a live single-file HTML web app for a collection to `https://{slug_or_id}.vibebac.kr`.
- `list_collections`: List all collections in the current tenant workspace.
- `create_record`: Insert a universal record (`collection_id`, `title`, `content`, `data`, `visibility`, `shares`).
- `query_records`: Query records honoring the 5-tier recursive waterfall permission engine.
- `search_records`: Hybrid 768-d semantic vector search & full-text keyword search.
- `sync_org_directory`: Upsert organization hierarchy (`email`, `name`, `role`, `manager_email`) for recursive manager ACL inheritance.
- `batch_create_invites`: Pre-authorize team members with role and manager assignments.
- `get_client_integration_guide`: Retrieve copy-pasteable frontend SDK integration code and tailored TypeScript types.
:::

## Client Guide: Browser SDK & REST API {#client-guide}

Building a custom React, Next.js, Vue, Svelte, or vanilla JS frontend - or writing an autonomous agent that interacts directly with the VibeBackr HTTP API? The **[Client Guide](/guide)** covers everything you need:

:::features
### Drop-In Browser SDK (`vibebackr.js`)
Zero-dependency ES module and global `<script>` bundle (`https://vibebac.kr/vibe.js`) with full TypeScript definitions (`vibebackr.d.ts`), automatic JWT refresh, and 1-line SSO (`vibe.loginWithGoogle()`).

### Complete REST & OAuth 2.1 Reference
Full endpoint specs for collections, universal records (`GET`/`POST`/`PUT`/`DELETE`), hybrid semantic search (`POST /api/collections/{id}/search`), team invites, and RFC 7591 Dynamic Client Registration (`/oauth/register`).

### Strict-CSP Sandbox Relay (`/relay`)
Automatic `postMessage` iframe bridge that lets apps running inside locked-down AI preview sandboxes (such as Claude Artifacts) authenticate and persist records without CORS or `connect-src` failures.
:::

> **Tip for AI Agents:** Agents can also retrieve this documentation on demand by calling the `get_client_integration_guide` MCP tool or fetching `GET /api/client-guide` (or `/llms-full.txt`).

:::cta
- [Open the Client Guide](/guide){.btn-primary}
- [Download `llms-full.txt`](/llms-full.txt)
:::

## Self-Hosting (VibeBackr OS) {#self-hosting}

The entire VibeBackr backend is open-source (MIT) and compiles into a **single, zero-CGO Go binary**. Because it uses a pure-Go SQLite implementation (`modernc.org/sqlite`), you can cross-compile and run it on any laptop, Raspberry Pi, VPS, or container with zero external system dependencies.

When self-hosted, **billing quotas and rate limits are disabled by default** - giving you unlimited collections, unlimited records, and uncapped throughput on your own hardware.

### 1. Quick Start (Zero-Config SQLite)

Build from source and launch the server in seconds:

```bash
git clone https://github.com/eliotstocker/vibebackr.git
cd vibebackr
go build -o vibebackr ./cmd/vibebackr
./vibebackr serve
```

By default, `./vibebackr serve` listens on `http://localhost:8080`, automatically migrates a local `vibebackr.db` SQLite database, enables a local Mock SSO provider for instant development, and automatically bootstraps the first user who signs in as the workspace `admin`.

### 2. Production Setup (PostgreSQL, `pgvector` & SSO)

For production deployments or high-dimensional semantic vector search, point VibeBackr OS at PostgreSQL and configure your identity provider via flags or environment variables:

```bash
export JWT_SECRET="replace-with-a-strong-production-secret"
export GOOGLE_CLIENT_ID="your-client-id.apps.googleusercontent.com"
export GOOGLE_CLIENT_SECRET="your-client-secret"
export GOOGLE_REDIRECT_URL="https://vibebackr.example.com/auth/callback/google"

./vibebackr serve \
  --port 8080 \
  --driver postgres \
  --dsn "postgres://user:pass@localhost:5432/vibebackr" \
  --base-url "https://vibebackr.example.com" \
  --instance-name "Acme Internal VibeBackr" \
  --gemini-api-key "$GEMINI_API_KEY"
```

### 3. Self-Hosting Modes & Configuration

:::cli
- `Local stdio MCP (./vibebackr mcp --dsn ./vibebackr.db)`: Connect Claude Desktop, Cursor, or local CLI agents directly to your SQLite/Postgres database over `stdio` without exposing an HTTP port.
- `Local & S3 App Hosting`: Published single-file HTML apps (`publish_app`) are saved to local disk (`web/dist/apps/{slug}`) and served at `/apps/{slug}`, or deployed to your own S3 bucket + wildcard domain when `S3_BUCKET` and `BASE_URL` are configured.
- `Pluggable SSO (Google, GitHub & Generic OIDC)`: Configure `GOOGLE_CLIENT_ID`, `GITHUB_CLIENT_ID`, or any enterprise OIDC provider (`OIDC_CLIENT_ID`, `OIDC_AUTH_URL`, `OIDC_TOKEN_URL`, `OIDC_USERINFO_URL`) for Okta, Microsoft Entra ID, Keycloak, or Auth0.
- `Local Vector Embeddings`: Use `--gemini-api-key` for `gemini-embedding-001` or run 100% offline 768-d semantic RAG with Ollama (`--embedder-url "http://localhost:11434"` using `embeddinggemma2`).
- `Custom Branding & Optional Guardrails`: Set `--instance-name "Your Org"` (`INSTANCE_NAME`) to brand the self-hosted VibeBackr OS landing page, and optionally pass `--enable-rate-limit` if you want per-collection token-bucket protection on shared internal servers.
:::

:::cta
- [View Source & Self-Hosting Docs on GitHub](https://github.com/eliotstocker/vibebackr){.btn-primary}
- [Read the Client Guide](/guide)
:::

