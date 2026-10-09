---
title: Frontend Client Integration Guide
description: Everything you or your AI coding assistant needs to connect, host, and secure a web application on VibeBackr.
logo: true
---

# Frontend Client Integration Guide

Turn any React, Next.js, Vue, Svelte, or single-file HTML prototype into a live, multi-user web app with login, permissions, auto-invites, and cloud storage - zero backend code required.

:::cta
- `https://vibebac.kr/vibebackr.js`
- [Open Console & Sign In](/login){.btn-primary}
:::

## Overview & Data Model {#overview}

Every frontend connects to the same canonical endpoint and queries isolated tenant collections.

:::features
### Canonical Endpoint & Instant Hosting
Agents and frontends always target `https://vibebac.kr`, and single-file HTML apps can be published directly to `https://{dashless_collection_id}.vibebac.kr` via `publish_app`.

### Universal Records
Every record includes a `title`, markdown `content`, and a flexible JSON `data` object for arbitrary structured payloads.

### 5-Tier Access Control & Auto-Invites
Permissions evaluate automatically: Owner, Manager Hierarchy, Public Domain, Email Shares, and Admin. Enable `auto_invite_shares` on a collection to automatically invite non-users when a record is shared with their email.
:::

## Authentication for Frontends {#auth}

Every authenticated request uses an `Authorization: Bearer <token>` header. VibeBackr acts as a centralized OAuth broker so you never need to register Google Cloud OAuth credentials or configure `localhost` redirect URLs.

{{include:snippets/active-providers.html}}

:::flow
### Popup Login (`file://` & SPAs)
Opens a centered popup to sign in with Google, transmits session tokens back via `window.postMessage`, and works even from local `file:///.../index.html` pages with no dev server.

### Full-Page Redirect
Redirects the browser to `/auth/login/google?redirect_uri=...` and returns to your app with `#access_token=...` and `#refresh_token=...` in the URL hash.

### Local `file://` Null Origins
Browsers assign `Origin: null` to local `file://` documents. VibeBackr permits `null` origins with Bearer tokens out of the box via CORS.

### CSP & Sandboxed Iframe Relay
When strict CSP (`connect-src 'self'`) or sandboxed iframes block direct `fetch`, the SDK automatically falls back to a hidden `/relay` iframe bridge over `postMessage`.
:::

### Popup & Redirect Examples

```typescript
//popup sign-in works from file:// or http://localhost
client.login('google').then(auth => {
    console.log('Logged in as', auth.user.email);
});

//full-page redirect
window.location.href = client.getLoginUrl('google', 'http://localhost:3000');
client.handleAuthCallback();
```

## Core REST Endpoints {#endpoints}

All endpoints accept and return JSON over `https://vibebac.kr`.

| Endpoint | Description |
| :--- | :--- |
| **`GET /api/tenants`** | List all tenants the user owns or belongs to. |
| **`DELETE /api/tenants/{tenant_id}`** | Leave a joined tenant. |
| **`GET /api/collections`** | List all collections accessible to the authenticated user. |
| **`POST /api/collections`** | Create a collection (`name`, `default_visibility`, `origin_urls`, `vector_search_enabled`, `auto_invite_shares`, and optional seed `records`). |
| **`GET /api/collections/{id}`** | Fetch a single collection's metadata by ID. |
| **`PUT /api/collections/{id}`** | Update a collection's `name`, `default_visibility`, `origin_urls`, `vector_search_enabled`, or `auto_invite_shares`. |
| **`PUT /api/collections/{id}/app`** | Publish a single-file HTML frontend (`{ "html": "<!DOCTYPE html>..." }`) to `https://{dashless_id}.vibebac.kr`. |
| **`DELETE /api/collections/{id}`** | Permanently delete a collection. |
| **`GET /api/collections/{id}/records`** | List records in a collection visible to the current user. |
| **`POST /api/collections/{id}/records`** | Create a new record with `title`, `content`, `data`, and `access_policy`. |
| **`GET /api/collections/{id}/records/{rec_id}`** | Fetch a single record by ID. |
| **`PUT /api/collections/{id}/records/{rec_id}`** | Update a record's `title`, `content`, `data`, or `access_policy`. |
| **`DELETE /api/collections/{id}/records/{rec_id}`** | Permanently delete a record. |
| **`POST /api/collections/{id}/search`** | 768-d vector semantic search (EmbeddingGemma 2) with automatic text fallback. |

### Creating & Searching Records

```typescript
fetch('https://vibebac.kr/api/collections/col_123/records', {
    method: 'POST',
    headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json'
    },
    body: JSON.stringify({
        title: 'Weekly Standup Notes',
        content: 'Discussed sprint roadmap and migration.',
        data: {priority: 'high', tags: ['eng', 'q4']},
        access_policy: {visibility: 'shared', shares: ['colleague@example.com']}
    })
});

fetch('https://vibebac.kr/api/collections/col_123/search', {
    method: 'POST',
    headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json'
    },
    body: JSON.stringify({query: 'sprint goals and migration', limit: 10})
})
    .then(res => res.json())
    .then(results => {
        console.log(results);
    });
```

## Frontend SDK & Drop-In Client {#sdk}

Access tokens expire after 15 minutes. Both the hosted CDN SDK (`vibebackr.js`) and the drop-in TypeScript client automatically rotate refresh tokens via `POST /oauth/token` whenever a request receives `401 Unauthorized`.

### 1. CDN Script Tag (Zero Build Step)

Include [https://vibebac.kr/vibebackr.js](/vibebackr.js) (short alias: [/vibe.js](/vibe.js), TypeScript types at [/vibebackr.d.ts](/vibebackr.d.ts), full specification at [/llms-full.txt](/llms-full.txt)).

> **Note for AI Coding Assistants:** Never build token or API-key settings modals in generated apps. End users authenticate via `vibe.loginWithGoogle()`, or read/write public records directly when `default_visibility` is `"public"`.

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

Or use the multi-collection factory via `VibeBackr.createClient()` / `createVibeClient()`:

```html
<script src="https://vibebac.kr/vibebackr.js"></script>
<script>
    const client = createVibeClient();
</script>
```

### 2. Drop-In TypeScript Client (React, Next.js, Vue, Vite)

Copy this self-contained client into `src/lib/vibeBackr.ts`:

```typescript
export type AccessPolicy = {
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

export function createVibeClient(baseUrlOrOptions: string | VibeClientOptions = 'https://vibebac.kr', options: VibeClientOptions = {}) {
    let baseUrl = 'https://vibebac.kr';
    if(typeof baseUrlOrOptions === 'object' && baseUrlOrOptions !== null) {
        options = baseUrlOrOptions;
        baseUrl = options.baseUrl || 'https://vibebac.kr';
    } else {
        baseUrl = baseUrlOrOptions || options.baseUrl || 'https://vibebac.kr';
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

        const body = `grant_type=refresh_token&refresh_token=${encodeURIComponent(rt)}`;
        const refreshOpts: RequestInit = {
            method: 'POST',
            headers: {'Content-Type': 'application/x-www-form-urlencoded'},
            body
        };
        (refreshOpts as any)._skipRefresh = true;

        const refreshCall = options.useRelay
            ? requestViaRelay<any>('/oauth/token', refreshOpts)
            : fetch(`${baseUrl}/oauth/token`, refreshOpts).then(res => {
                if(!res.ok) {
                    return res.json()
                        .catch(() => ({error: res.statusText}))
                        .then(err => {
                            throw new Error(err.error || `Token refresh failed: ${res.status}`);
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
                frame.src = options.relayUrl || `${baseUrl}/relay`;
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
                ...(token ? {Authorization: `Bearer ${token}`} : {}),
                ...(opts.headers as any)
            };
            const reqId = `req_${Math.random().toString(36).substring(2, 9)}_${Date.now()}`;

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
                        || `Relay request failed: ${e.data.status}`;
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
            ...(token ? {Authorization: `Bearer ${token}`} : {}),
            ...(opts.headers as Record<string, string>)
        };
        return fetch(`${baseUrl}${path}`, {...opts, headers})
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
                            throw new Error(err.error || `HTTP ${res.status}`);
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
            const colParam = targetCol ? `&collection_id=${encodeURIComponent(targetCol)}` : '';
            if(typeof window !== 'undefined') {
                window.open(
                    `${baseUrl}/auth/login/${provider}?popup=true${colParam}`,
                    'vibebackr_login',
                    `width=${width},height=${height},left=${left},top=${top}`
                );
            }
            const onMessage = (e: any) => {
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
        request<VibeRecord<T>[]>(`/api/collections/${encodeURIComponent(resolveColId(colId))}/records`);

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
        return request<SearchResult<T>[]>(`/api/collections/${encodeURIComponent(targetCol)}/search`, {
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
        getLoginUrl(provider = 'google', redirectUri?: string, collectionId?: string) {
            if(!redirectUri && typeof window !== 'undefined') {
                const isFile = window.location.protocol === 'file:'
                    || window.location.origin === 'null';
                redirectUri = isFile ? 'http://localhost:3000' : window.location.origin;
            }
            const targetCol = collectionId || defaultCollectionId;
            const colParam = targetCol ? `&collection_id=${encodeURIComponent(targetCol)}` : '';
            return `${baseUrl}/auth/login/${provider}?redirect_uri=${encodeURIComponent(redirectUri || '')}${colParam}`;
        },
        handleAuthCallback() {
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
        logout() {
            const rt = getRefreshToken();
            setToken(null);
            setRefreshToken(null);
            const body = rt ? `refresh_token=${encodeURIComponent(rt)}` : '';
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
            request<{message: string; tenant_id: string}>(`/api/tenants/${encodeURIComponent(tenantId)}`, {method: 'DELETE'}),
        listCollections,
        getCollections: listCollections,
        createCollection: (nameOrConfig: string | Record<string, any>, defaultVisibility = 'private', originUrls: string[] = []) => {
            const payload = (typeof nameOrConfig === 'object' && nameOrConfig !== null)
                ? nameOrConfig
                : {name: nameOrConfig, default_visibility: defaultVisibility, origin_urls: originUrls};
            return request<any>('/api/collections', {method: 'POST', body: JSON.stringify(payload)});
        },
        getCollection: (colId?: string) => request<any>(`/api/collections/${encodeURIComponent(resolveColId(colId))}`),
        updateCollection: (colId: string, payload: Record<string, any>) =>
            request<any>(`/api/collections/${encodeURIComponent(colId)}`, {method: 'PUT', body: JSON.stringify(payload)}),
        deleteCollection: (colId: string) =>
            request<{message: string; id: string}>(`/api/collections/${encodeURIComponent(colId)}`, {method: 'DELETE'}),
        listRecords,
        getRecords: listRecords,
        getRecord: <T = any>(colIdOrRecId: string, recId?: string) => {
            const targetCol = recId === undefined && defaultCollectionId ? defaultCollectionId : resolveColId(colIdOrRecId);
            const targetRec = recId === undefined && defaultCollectionId ? colIdOrRecId : recId!;
            return request<VibeRecord<T>>(`/api/collections/${encodeURIComponent(targetCol)}/records/${encodeURIComponent(targetRec)}`);
        },
        createRecord: <T = any>(colIdOrPayload: string | Record<string, any>, payloadOrColId?: any) => {
            const isObj = typeof colIdOrPayload === 'object' && colIdOrPayload !== null;
            const targetCol = isObj ? resolveColId(payloadOrColId) : resolveColId(colIdOrPayload as string);
            const payload = (isObj ? colIdOrPayload : payloadOrColId) || {};
            const accessPolicy = payload.access_policy || {
                visibility: payload.visibility || '',
                shares: payload.shares || []
            };
            return request<VibeRecord<T>>(`/api/collections/${encodeURIComponent(targetCol)}/records`, {
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
            return request<VibeRecord<T>>(`/api/collections/${encodeURIComponent(targetCol)}/records/${encodeURIComponent(targetRec)}`, {
                method: 'PUT',
                body: JSON.stringify(body)
            });
        },
        deleteRecord: (colIdOrRecId: string, recId?: string) => {
            const targetCol = recId === undefined && defaultCollectionId ? defaultCollectionId : resolveColId(colIdOrRecId);
            const targetRec = recId === undefined && defaultCollectionId ? colIdOrRecId : recId!;
            return request<{message: string}>(`/api/collections/${encodeURIComponent(targetCol)}/records/${encodeURIComponent(targetRec)}`, {
                method: 'DELETE'
            });
        },
        searchRecords,
        search: searchRecords
    };
}
```

## Bootstrapping Without MCP (Gemini & ChatGPT) {#gemini}

When working in Google Gemini, AI Studio, or environments without an active MCP connection, you can bootstrap collections and seed records in one step.

:::flow
### Web Console JSON Import
Open the [Console](/login), click **+ New Collection / Import JSON**, copy the Gemini prompt, and paste the generated JSON back to create the collection and seed records.

### Direct REST API (`POST /api/collections`)
Copy your Personal Access Token from the [Console](/login) and `POST` a JSON payload containing `name`, `default_visibility`, and an optional `records` array.
:::

```json
{
  "name": "customer_support",
  "default_visibility": "private",
  "origin_urls": ["*"],
  "vector_search_enabled": true,
  "records": [
    {
      "title": "Welcome Ticket",
      "content": "First automated record created by Gemini",
      "data": { "status": "open", "priority": "high" }
    }
  ]
}
```
