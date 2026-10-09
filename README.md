# VibeBackr

## Turn what you build in AI chat into a real app anyone can use.**  
Give any tool you build in Claude, Gemini, ChatGPT, or Cursor its own live web link (`your-app.vibebac.kr`), 1-click Google & GitHub sign-in, and permanent cloud memory - backed by a single, zero-CGO Go binary (`VibeBackr OS`).

> **Note on Creation & AI Assistance:**  
> The idea, problem definition, and architectural direction behind `VibeBackr` were conceived, designed, and guided by human engineers. The underlying implementation, unit/E2E test suites, deployment manifests, and documentation were developed with strong assistance and augmentation from AI.

* **Live Cloud & Console:** [vibebac.kr](https://vibebac.kr) · [Sign In & Console](https://vibebac.kr/login)
* **Remote MCP Server:** `https://vibebac.kr/mcp` · [Download 1-Click Claude Extension (`vibebackr.mcpb`)](https://vibebac.kr/vibebackr.mcpb)
* **Documentation:** [For Nerds (Architecture & Self-Hosting)](https://vibebac.kr/nerds) · [Frontend Client & SDK Guide](https://vibebac.kr/guide) · [`llms-full.txt`](https://vibebac.kr/llms-full.txt)

---

## The Problem (Why This Exists)

When you ask Claude, Gemini, or ChatGPT to build you a tool, the preview is trapped inside your chat window. Refresh the page and everything you typed disappears, and nobody else can open it without setting up a database, configuring OAuth redirect URIs in Google Cloud Console, and deploying a backend.

**VibeBackr gives your AI the missing backend and hosting layer in a single prompt:**
* **A Real Web Link You Can Share:** Every app gets its own live web address (`https://{slug_or_id}.vibebac.kr`) that opens on any phone, tablet, or laptop.
* **Cloud Memory That Never Disappears:** Universal records (`title`, markdown `content`, and arbitrary JSON `data`) persist across devices with zero table migrations.
* **1-Click Google & GitHub Sign-In:** Keep your app **private** just for you, **share** specific items with someone's email address (with automatic email invites), or make it **public** for anyone with the link.

---

## Architecture & Highlights

VibeBackr is a single, self-contained Go binary that combines a remote Model Context Protocol (MCP) server, a centralized OAuth 2.1 / OIDC broker, a multi-tenant document & vector database, and an edge-hosted single-file HTML application store:

* **Single Self-Contained Binary (`VibeBackr OS`):** Pure-Go SQLite driver (`modernc.org/sqlite`) allows zero-dependency execution without CGO or GCC, with seamless switching to PostgreSQL in production.
* **Single-File Edge App Hosting (`publish_app`):** Validates and stores self-contained HTML applications (up to 512 KB on managed cloud, uncapped when self-hosted) and serves them from `https://{slug_or_id}.vibebac.kr` via CloudFront & S3 (or `/apps/{slug}` from local disk when self-hosted), automatically wiring CORS `origin_urls`.
* **Centralized OAuth 2.1 Broker & Zero-Setup SSO:** Built-in Google, GitHub, Generic OIDC, and Mock SSO providers alongside PKCE and RFC 7591 Dynamic Client Registration (`/oauth/register`). Frontends sign users in with a single `vibe.loginWithGoogle()` call.
* **5-Tier Recursive Waterfall ACLs & Auto-Invites:** Every query evaluates permissions in SQL across five tiers:
  1. **Record Ownership** (`created_by`)
  2. **Manager Hierarchy** (evaluated up/down the org chart via recursive CTEs)
  3. **Public Collection / Domain Visibility** (`allowed_domains`)
  4. **Explicit Email Shares** (with optional `auto_invite_shares` to automatically invite non-users when a record is shared with their email)
  5. **Workspace Admin Override**
* **Universal Records & 768-d Vector RAG:** Stores `title`, markdown `content`, and structured JSON `data` payloads. When vector search is enabled, records are indexed with 768-dimensional embeddings (`gemini-embedding-001` or local `EmbeddingGemma 2` via `pgvector`) with automatic SQL full-text fallback (`LIKE` on SQLite, `ILIKE`/`tsvector` on PostgreSQL).
* **Zero-Dependency Browser SDK & Strict-CSP Relay:** The client SDK ([`vibebackr.js`](https://vibebac.kr/vibebackr.js) / [`vibe.js`](https://vibebac.kr/vibe.js)) handles automatic JWT refresh, `file://` null-origin CORS, and automatically falls back to a hidden `/relay` iframe `postMessage` bridge when running inside strict-CSP AI sandboxes (such as Claude Artifacts).
* **Per-Collection Token-Bucket Rate Limiting:** Rate limits are isolated per individual collection so one busy app never starves your other tools (`60 req/min` on Standard, `180 req/min` on Pro, and uncapped by default when self-hosted).

---

## Connect Your AI Assistant in 60 Seconds

### 1. Claude Desktop, Claude Web & Claude Code
Install in one click with the [Claude Desktop MCP Bundle (`vibebackr.mcpb`)](https://vibebac.kr/vibebackr.mcpb), add `https://vibebac.kr/mcp` under **Claude Settings > Connectors**, or add it to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "vibebackr": {
      "url": "https://vibebac.kr/mcp"
    }
  }
}
```

### 2. Gemini, ChatGPT, Cursor & GitHub Copilot
Connect any MCP client over Streamable HTTP (`POST /mcp`) or Server-Sent Events (`GET /mcp`):
* **Gemini:** Open **Customise > Connectors > + Custom**, paste `https://vibebac.kr/mcp`, and sign in once with Google.
* **ChatGPT:** Open **Settings > Connectors > Create Custom Connector**, paste `https://vibebac.kr/mcp`, and sign in with Google.
* **Cursor / Windsurf / VS Code:** Add `https://vibebac.kr/mcp` in **MCP Settings** (authenticates automatically via OAuth 2.1 DCR & PKCE).

### 3. Local `stdio` Mode (Self-Hosted CLI Agents)
When running the binary locally, agents can connect directly over `stdio` without exposing an HTTP port:

```json
{
  "mcpServers": {
    "vibebackr": {
      "command": "/path/to/vibebackr",
      "args": ["mcp", "--dsn", "/path/to/vibebackr.db"]
    }
  }
}
```

### 4. Any Other AI Chat (Copy-Paste Starter Prompt)
Using a standard web chat without custom MCP connectors? Copy and paste this starter prompt alongside your app idea:

```text
Use VibeBackr (https://vibebac.kr/llms.txt) to build, store, and host my app so it saves data in the cloud and has Google sign-in.
- If you support MCP: connect to https://vibebac.kr/mcp, create a collection (turn on auto_invite_shares if people can share items), and publish the finished app with publish_app so I get a live shareable link.
- If you don't support MCP: give me the collection setup to paste into https://vibebac.kr/login, then build the single-page app using <script src="https://vibebac.kr/vibebackr.js"></script> with Google sign-in (vibe.loginWithGoogle()).
```

### Available MCP Tools
* `create_collection`: Create an isolated collection (`name`, `slug`, `default_visibility`, `allowed_domains`, `origin_urls`, `vector_search_enabled`, `auto_invite_shares`).
* `update_collection`: Update collection metadata, custom `slug`, visibility rules, `auto_invite_shares`, vector search settings, or allowed CORS `origin_urls`.
* `publish_app`: Deploy or update a single-file HTML web app for a collection to `https://{slug_or_id}.vibebac.kr` (or `/apps/{slug}` locally).
* `list_collections`: Enumerate all collections in the current tenant workspace.
* `create_record`: Insert a universal record (`collection_id`, `title`, `content`, `data`, `visibility`, `shares`).
* `query_records`: Query records with automatic 5-tier recursive CTE permission filtering.
* `search_records`: Hybrid 768-d semantic vector search with automatic SQL full-text fallback.
* `sync_org_directory`: Batch upsert organization hierarchy (`email`, `name`, `role`, `manager_email`) for recursive manager ACL inheritance.
* `batch_create_invites`: Pre-authorize team members with assigned roles and managers.
* `get_client_integration_guide`: Retrieve endpoint specs, auth instructions, tailored TypeScript types, and copy-pasteable SDK code for target collections.
* `get_billing_status` / `upgrade_tenant_plan` / `redeem_voucher`: Inspect workspace quotas, upgrade plans, or redeem promo vouchers.

---

## Frontend SDK & REST API

### 1. Zero-Build CDN Script (`vibebackr.js`)
Include [`https://vibebac.kr/vibebackr.js`](https://vibebac.kr/vibebackr.js) (short alias: [`/vibe.js`](https://vibebac.kr/vibe.js), TypeScript definitions at [`/vibebackr.d.ts`](https://vibebac.kr/vibebackr.d.ts)). The SDK automatically rotates 15-minute access tokens using refresh tokens and transparently falls back to the `/relay` bridge inside strict-CSP sandboxes:

```html
<script src="https://vibebac.kr/vibebackr.js"></script>
<script>
    const vibe = new VibeBackr({collectionId: 'YOUR_COLLECTION_ID'});

    vibe.loginWithGoogle()
        .then(() => vibe.createRecord({
            title: 'Project Alpha',
            content: 'Initial requirements and roadmap notes',
            data: {priority: 'high', status: 'in-progress'}
        }))
        .then(() => Promise.all([
            vibe.listRecords(),
            vibe.searchRecords('roadmap priorities')
        ]))
        .then(([items, results]) => {
            console.log(items, results);
        });
</script>
```

### 2. Core REST Endpoints

| Endpoint | Description |
| :--- | :--- |
| **`GET /api/tenants`** | List all tenant workspaces the user owns or belongs to. |
| **`POST /api/tenants`** | Explicitly create a tenant workspace. |
| **`DELETE /api/tenants/{tenant_id}`** | Leave a joined tenant workspace. |
| **`GET /api/collections`** | List all collections accessible to the authenticated user. |
| **`POST /api/collections`** | Create a collection (`name`, `slug`, `default_visibility`, `origin_urls`, `vector_search_enabled`, `auto_invite_shares`, and optional seed `records`). |
| **`GET /api/collections/{id}`** | Fetch a single collection's metadata by ID or slug. |
| **`PUT /api/collections/{id}`** | Update a collection's configuration, `slug`, `origin_urls`, or `auto_invite_shares`. |
| **`PUT /api/collections/{id}/app`** | Publish a single-file HTML frontend (`{"html": "<!DOCTYPE html>..."}`) to `https://{slug_or_id}.vibebac.kr`. |
| **`DELETE /api/collections/{id}`** | Permanently delete a collection. |
| **`GET /api/collections/{id}/records`** | List records in a collection visible to the current user. |
| **`POST /api/collections/{id}/records`** | Create a new record with `title`, `content`, `data`, and `access_policy`. |
| **`GET /api/collections/{id}/records/{rec_id}`** | Fetch a single record by ID. |
| **`PUT /api/collections/{id}/records/{rec_id}`** | Update a record's `title`, `content`, `data`, or `access_policy`. |
| **`DELETE /api/collections/{id}/records/{rec_id}`** | Permanently delete a record. |
| **`POST /api/collections/{id}/search`** | 768-d semantic vector search with automatic SQL full-text fallback. |
| **`GET /api/client-guide`** | Machine- and human-readable client integration guide (`?format=json` supported). |

See the full **[Frontend Client Integration Guide](https://vibebac.kr/guide)** (or [`web/guide.md`](web/guide.md)) for the drop-in TypeScript client, popup vs. redirect auth flows, and schema introspection.

---

## Self-Hosting (`VibeBackr OS`)

The entire VibeBackr backend is open-source (MIT) and compiles into a **single, zero-CGO Go binary**. When self-hosted, **billing quotas and rate limits are disabled by default** - giving you unlimited collections, unlimited records, and uncapped throughput on your own hardware.

### 1. Quick Start (Zero-Config SQLite)
```bash
git clone https://github.com/eliotstocker/vibebackr.git
cd vibebackr
go build -o vibebackr ./cmd/vibebackr
./vibebackr serve
```
By default, `./vibebackr serve` listens on `http://localhost:8080`, auto-migrates a local `vibebackr.db` SQLite database, enables the local Mock SSO provider (`/auth/login/mock`), serves published HTML apps from `/apps/{slug}`, and bootstraps the first user who signs in as the workspace `admin`.

### 2. Production Setup (PostgreSQL, `pgvector` & SSO)
For production deployments or high-dimensional semantic vector search, point VibeBackr OS at PostgreSQL and configure your identity providers and embedder:

```bash
export JWT_SECRET="replace-with-a-strong-production-secret"
export GOOGLE_CLIENT_ID="your-client-id.apps.googleusercontent.com"
export GOOGLE_CLIENT_SECRET="your-client-secret"
export GOOGLE_REDIRECT_URL="https://vibebackr.example.com/auth/callback/google"

./vibebackr serve \
  --port 8080 \
  --driver postgres \
  --dsn "postgres://user:password@localhost:5432/vibebackr?sslmode=disable" \
  --base-url "https://vibebackr.example.com" \
  --instance-name "Acme Internal VibeBackr" \
  --gemini-api-key "$GEMINI_API_KEY"
```

> **Offline Local Embeddings:** Prefer running 100% offline vector search without an external API key? Pass `--embedder-url "http://localhost:11434"` to use Ollama with **EmbeddingGemma 2** (see [`internal/search`](internal/search)).

---

## SSO Connectors, JIT Provisioning & OAuth 2.1 DCR

### Pluggable SSO Providers ([`internal/auth/sso.go`](internal/auth/sso.go))
1. **Mock Provider (`mock`):** Built-in for local dev (`GET /auth/login/mock`).
2. **Google OIDC (`google`):** Configured via `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, and `GOOGLE_REDIRECT_URL`.
3. **GitHub OAuth (`github`):** Configured via `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, and `GITHUB_REDIRECT_URL`.
4. **Generic Enterprise OIDC (`oidc` / custom `OIDC_NAME`):** Compatible with Okta, Auth0, Keycloak, and Microsoft Entra ID via `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_AUTH_URL`, `OIDC_TOKEN_URL`, `OIDC_USERINFO_URL`, and `OIDC_REDIRECT_URL`.

### JIT Onboarding Rules
* **Explicit Tenant Sign-Up (`?signup=true`):** Logging in via `/auth/login/{provider}?signup=true` provisions a dedicated `tenant_id` and `admin` role.
* **Initial Admin Auto-Bootstrap:** If the database has `0` users, the first user to sign in is automatically granted `admin` with a default tenant.
* **Invites & Allowed Domains:** Users matching an **Invite** (including automatic shares via `auto_invite_shares`) or a collection's `allowed_domains` are provisioned Just-In-Time with access scoped strictly to the resources shared with them.

### OAuth 2.1 Dynamic Client Registration (RFC 7591)
Headless agents and MCP clients can dynamically register and exchange credentials:
* **`POST /oauth/register`**: Register a client (`client_name`, `redirect_uris`, `grant_types`).
* **`POST /oauth/token`**: Exchange `authorization_code` (with PKCE), `refresh_token`, or `client_credentials` for JWT access tokens.

---

## Verification & Testing

Run the full test suite (SQLite & PostgreSQL ACL waterfall CTEs, REST API, billing quotas, rate limiting, and MCP server), build the binary, and compile the static site:

```bash
go test -count=1 -cover ./internal/...
go build -o vibebackr ./cmd/vibebackr
npm run build:site
```

---

## The Stac.kr Developer Toolchain

VibeBackr is part of the `stac.kr` developer ecosystem built for engineers and autonomous AI coding agents:
* **[vibestac.kr](https://vibestac.kr)** - Portable local dev stack TUI, task runner, and MCP server.
* **[vibebac.kr](https://vibebac.kr)** - Instant cloud persistence, collections, app hosting, and authentication backend for AI coding assistants.
* **[metastac.kr](https://metastac.kr)** - Event-driven multi-repo Git submodule orchestration platform.
* **[deploystac.kr](https://deploystac.kr)** - Lightweight GitOps promotion dashboard and version bumper.

---

## License

MIT. Built for AI creators, developers, and autonomous agents who want real apps without backend busywork.

