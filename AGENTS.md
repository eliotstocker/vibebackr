# AGENTS.md

Instructions, guidelines, and guardrails for autonomous AI coding agents operating on the VibeBackr codebase.

## 🧠 Core Philosophy: Aggressive Pragmatism
* **Simplicity > Theory:** The "theoretically perfect" solution is a trap. Prioritize the simplest, most maintainable solution that solves the immediate problem.
* **Minimal Surface Area:** Change the absolute minimum amount of code necessary. Do not "clean up" unrelated files or refactor outside the scope of the task unless explicitly requested.
* **The Sparring Partner Rule:** **Do not be a "yes man."** I need a partner, not a mirror. If my suggestion is over-engineered, risky, or violates these principles, challenge it. Prevent my mistakes.

## 📦 Dependency & Resource Management
* **The "Few Lines" Rule:** Do not install a library for what can be achieved in a few lines of CSS or native code.
* **Pragmatic Packages:** Use established packages for complex logic (e.g., YAML parsers), but always consider **bundle size** and codebase bloat.
* **Lean Core:** Every new dependency must justify its existence. If we can write it ourselves quickly and cleanly, we do.

## 🛠️ Implementation Standards
* **Code Style:** Keep it concise. Explain the *why* behind complex logic with sparse inline comments, not just describing *what* the code does.
* **Naming & Idioms:** Use `camelCase` by default, but stay **idiomatic** to the language being written (e.g., follow Go conventions in Go, PEP8 in Python).
* **Typing:** Prioritize types for safety, but avoid "Type Gymnastics." If a strict type becomes a massive bottleneck, use a pragmatic fallback (like `any` or `unknown`) and move on.
* **Architecture:** Favor **Domain-Driven** organization. Keep functionality in small, decoupled chunks organized by domain rather than large, consolidated files.

## ✍️ Personal Code Style & Voice

### Mechanical Formatting
* **Indentation:** 4 spaces for code (JS/TS/TSX/CSS/Java/Groovy); 2 spaces for JSON.
* **Tight Control Keywords:** Omit the space between control keywords and parens across all languages: `if(cond)`, `catch(e)`, `for(let i = 0; ...)`, `while(cond)`, `switch(val)`.
* **Tight Braces:** Omit spaces inside inline curly braces, imports, and destructuring: `import {useQuery} from 'react-query'`, `const {cwd = '/', profile} = options;`, `{argv: this.arguments}`, `({type, string}) =>`.
* **Empty & Bounds Checks:** Prefer `< 1` over `=== 0` for empty length checks (`if(arr.length < 1)`, `if(!data || data.length < 1)`) and tight negative literals (`index > -1`, `entries.length -1`).
* **Block Braces vs. Short-Circuiting:** Never write braceless single-line `if (x) return;`. Always wrap `if` bodies in `{ ... }` on their own lines, OR use `&&` short-circuiting for optional one-line side effects (`awaited && awaited()`, `const next = () => hasNext && setEntry(index + 1)`).
* **Arrow Functions:** Omit parentheses around single parameters (`item => item.trim()`).
* **Multi-line Conditions:** Place boolean operators (`&&`, `||`) at the **beginning** of the new line, aligned with the condition.
* **Quotes:** Single quotes `'...'` by default; template literals `` `${...}` `` for string interpolation.
* **State Cleanup:** Prefer `delete this._prop` or `delete this.map[key]` over setting transient state flags/references to `null` or `undefined`.

### Async, Functional & Binding Idioms
* **`.then()` Chains Over `async`/`await`:** Default to composable `.then()` pipelines (`fetch(...).then(handleErr).then(resp => resp.json())`, `new Promise(...).then(...)`) rather than `async`/`await`.
* **`.bind()` Partial Application:** Prefer `fn.bind(null, arg)` or `this._fn.bind(this, arg)` over wrapper arrow functions (`() => fn(arg)`) when passing callbacks to hooks, event handlers (`onClick={setOpen.bind(null, true)}`), timers, or curried exports (`export const user = validate.bind(null, UserSchema)`).
* **Functional Pipelines:** Prefer `.reduce()` (for sequential promise chains, tree traversal, and immutable spread accumulation `{...acc, [key]: value}`) and `.flatMap()` over imperative `for` loops with mutable accumulators.

### TypeScript, React (TSX) & Express
* **`type` Over `interface`:** Always define object shapes, callbacks, and component props with `type Foo = { ... }` rather than `interface`.
* **Direct Unparenthesized JSX Returns:** Start multi-line JSX returns directly on the `return` line (`return <Container>...`) without wrapping parentheses `return (...)`. Use tight self-closing tags without a space before the slash (`<CircularProgress/>`, `<Login queryClient={queryClient}/>`).
* **Top-Down Component Files:** Place the primary `export default function` component at the **top** of the `.tsx` file, with smaller co-located helper components declared as plain `function SubComponent(...)` below it.
* **Express Route Destructuring:** Destructure `params`, `body`, and `user` directly in the handler signature: `router.put('/:domain/:entry', ({params: {domain, entry}, body, user}, res) => { ... })`.
* **Dynamic Barrel Mounting:** Keep barrel `index` files as simple arrays or direct re-exports (`export {default as api} from './api'`) and mount them dynamically with `Object.entries(handlers).forEach(...)`.

### Architecture & OOP Idioms
* **Small, Focused Modules:** One class/responsibility per file. Export the class at the bottom (`module.exports = ClassName` or `export default ClassName`).
* **Constructor Unpacking & Static Factories:** Destructure `options` with defaults on line 1 of `constructor(options)`, run flat guard clauses, then bind state. For data/message models, keep constructors as simple field assignments and put parsing/validation in a static factory (`ClassName.fromMessage(raw)`).
* **Getters & Setters for Ergonomics:** Use getters for capability flags (`get requiresFilesystem() { return true; }`) and setters when property assignment is cleaner than a setter method (`this.stdOut = line`).
* **Underscore Privacy:** Use `_methodName` and `_prop` for internal class members. Avoid `#private` fields, `Symbol` hiding, or `Proxy` wrappers unless strictly required.
* **Guard Clauses First:** Validate inputs and fail fast at the top of functions (or throw small custom `Error` subclasses like `APIError` caught by flat `instanceof` middleware) so the happy path stays flat and unindented.
* **CSS:** Use BEM-style naming (`.block__element`, `.block__element.modifier`) and modern native CSS features (e.g., `currentColor`, relative color syntax) over sprawling `:root` CSS variable indirection.

### Comments (Strict Anti-AI Filter)
* **No Section Banners:** Never write `// -------` or `// === Section ===` dividers.
* **No Boilerplate JSDoc:** Omit `/** ... */` blocks in apps and internal modules, only use JSDoc/TSDoc on public library methods when a doc generator (`jsdoc2md`, `tsdoc`) is wired into `package.json`.
* **Sparse, Casual Inline Notes:** Only comment non-obvious intent or edge-case workarounds. Write them as short, lowercase single-line comments without a space after the slashes (`//short circuit for aliases`, `//remove item by index`, `//its not intended for us`).
* **No LLM Punctuation:** Never use em-dashes (`—`) or Unicode arrows (`→`) in comments or error strings.

## ⚠️ Error Handling
* **Pragmatic Failure:** Error handling should not obscure the logic.
* **Silent vs. Loud:** If an error is trivial or doesn't break the core experience, handle it silently and move on. If it's a critical failure, bubble up a clear, actionable understanding to the user.

## 🧪 Workflow & Testing
* **Scrappy First:** It is acceptable to build "scrappy" prototypes to prove a concept before hardening.
* **Testing Debt:** Speed is king early on; we will circle back to add tests once the implementation is settled.
* **Execution vs. Clarification:**
    * If the path is clear: **Execute immediately.**
    * If you have doubts or see a high risk of "domain creep": **Stop and ask for guidance.**

## ⚔️ Conflict & Resolution (The Sparring Protocol)
* **The "Yellow Flag":** If a requested change violates our **Aggressive Pragmatism** or **Minimal Impact** rules, flag it before writing code and provide a "Pragmatic Alternative."
* **Constructive Pushback:** If my solution feels too "theoretical," use the phrase **"Keep it scrappy"** or **"YAGNI"** (You Ain't Gonna Need It). Immediately pivot to the simplest implementation.
* **The Tie-Breaker:** I provide the risk; **you provide the final decision.** Once you say "Proceed as requested," execute without further debate.

## 🗺️ Architecture & Directory Map

```text
cmd/vibebackr/main.go       # Single binary entrypoint (flags, DB init, graceful shutdown)
internal/
  ├── domain/               # Universal models: User, Collection, Record, Invite, OAuthClient
  ├── db/                   # Connection setup & auto-migrations (SQLite & PostgreSQL)
  ├── repository/           # Recursive CTE waterfall ACL permission queries & data access
  ├── auth/                 # JWT, PKCE, DCR (RFC 7591), SSO connectors (Google, GitHub, OIDC, Mock), JIT
  ├── billing/              # Per-tenant GoCardless billing, quotas, and vouchers
  ├── ratelimit/            # Per-collection token-bucket rate limiter
  ├── search/               # SearchEngine: pgvector RAG (EmbeddingGemma 2) + SQL text fallback
  ├── storage/              # Hosted single-file HTML app store (local FS & S3)
  ├── mcp/                  # Built-in MCP server (stdio, streamable HTTP / SSE, .mcpb bundle)
  └── api/                  # REST runtime router & Go 1.22+ method-based route handlers
web/                        # Static docs, console snippets, relay bridge, and JS/TS SDK
terraform/                  # AWS Lambda, S3, CloudFront, Route53, and Neon infrastructure
docs/                       # Guides for EmbeddingGemma 2, federation, and MCP routing
```

## 🛡️ VibeBackr Guardrails

1. **Zero-Friction Packaging (No CGO):**
   * Always maintain pure Go SQLite execution via `modernc.org/sqlite`. Never introduce CGO bindings that break zero-dependency cross-compilation.
2. **Database Portability:**
   * Business and permission logic must work seamlessly across both **SQLite** and **PostgreSQL**.
   * When altering queries, ensure recursive CTEs and JSON extraction syntax work across both engines via driver branching.
3. **Lean Core & Minimal Dependencies:**
   * Standard library first. Do not add routing frameworks or external packages when Go 1.22's `http.ServeMux` or native code achieves it cleanly.
4. **Recursive ACL Permission Guardrails:**
   * Never bypass the 5-tier waterfall: Ownership -> Manager Hierarchy (recursive CTE) -> Public Visibility -> Email Shares -> Admin Override.
   * Modifying records requires ownership, an ancestor manager relationship, or admin role.

## ✅ Verification Commands

Before finalizing any changes, verify all packages pass cleanly:

```bash
go test -count=1 -cover ./internal/...
go build -o vibebackr ./cmd/vibebackr
npm run build:site
```

## 🤖 Interacting with VibeBackr as an Agent

* **Local Agent Transport:** Launch `./vibebackr mcp --dsn ./vibebackr.db` to invoke MCP tools over `stdio`.
* **Remote Agent Transport:** Call `POST /mcp` (or listen to `GET /mcp` for Server-Sent Events).
* **Headless Dynamic Registration:** Register via `POST /oauth/register` (RFC 7591) and exchange credentials via `POST /oauth/token` with `grant_type=client_credentials`.
