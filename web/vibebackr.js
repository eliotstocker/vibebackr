/// <reference types="./vibebackr.d.ts" />
(function(root, factory) {
    if(typeof define === 'function' && define.amd) {
        define([], factory);
    } else if(typeof module === 'object' && module.exports) {
        module.exports = factory();
    } else {
        const exports = factory();
        root.VibeBackr = exports;
        root.createVibeClient = exports.createVibeClient;
    }
})(typeof globalThis !== 'undefined' ? globalThis : typeof window !== 'undefined' ? window : this, function() {
    'use strict';

    const DEFAULT_BASE_URL = 'https://vibebac.kr';

    class VibeClient {
        constructor(options = {}) {
            const {
                baseUrl = DEFAULT_BASE_URL,
                collectionId = null,
                tokenKey = 'vb_token',
                refreshTokenKey = 'vb_refresh_token',
                useRelay = false,
                relayUrl = null
            } = options;

            this._baseUrl = String(baseUrl || DEFAULT_BASE_URL).replace(/\/+$/, '');
            this._collectionId = collectionId;
            this._tokenKey = tokenKey;
            this._refreshTokenKey = refreshTokenKey;
            this._useRelay = Boolean(useRelay);
            this._relayUrl = relayUrl;
        }

        get baseUrl() {
            return this._baseUrl;
        }

        get collectionId() {
            return this._collectionId;
        }

        getToken() {
            if(typeof window === 'undefined') {
                return null;
            }
            return localStorage.getItem(this._tokenKey);
        }

        setRefreshToken(token) {
            if(typeof window === 'undefined') {
                return;
            }
            if(token) {
                localStorage.setItem(this._refreshTokenKey, token);
                return;
            }
            localStorage.removeItem(this._refreshTokenKey);
        }

        setToken(token, refreshTokenVal) {
            if(typeof window !== 'undefined') {
                if(token) {
                    localStorage.setItem(this._tokenKey, token);
                } else {
                    localStorage.removeItem(this._tokenKey);
                }
            }
            if(refreshTokenVal !== undefined) {
                this.setRefreshToken(refreshTokenVal);
            }
        }

        getRefreshToken() {
            if(typeof window === 'undefined') {
                return null;
            }
            return localStorage.getItem(this._refreshTokenKey);
        }

        _resolveColId(colId) {
            const id = colId || this._collectionId;
            if(!id) {
                throw new Error('collectionId is required (pass it to the method or set collectionId in client options)');
            }
            return id;
        }

        _getRelayWindow() {
            if(typeof window === 'undefined') {
                return Promise.reject(new Error('Relay transport requires a browser window environment'));
            }
            if(this._relayPromise) {
                return this._relayPromise;
            }

            this._relayPromise = new Promise(resolve => {
                let frame = document.getElementById('vb-relay-frame');
                if(!frame) {
                    frame = document.createElement('iframe');
                    frame.id = 'vb-relay-frame';
                    frame.src = this._relayUrl || `${this._baseUrl}/relay`;
                    frame.style.display = 'none';
                    frame.setAttribute('aria-hidden', 'true');
                    document.body.appendChild(frame);
                }

                const onMsg = e => {
                    if(
                        e.data
                        && (e.data.type === 'vb_relay_ready' || e.data.type === 'vb_relay_pong')
                    ) {
                        window.removeEventListener('message', onMsg);
                        resolve(frame.contentWindow);
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
            return this._relayPromise;
        }

        initRelay() {
            return this._getRelayWindow();
        }

        refreshToken() {
            if(this._refreshPromise) {
                return this._refreshPromise;
            }
            const rt = this.getRefreshToken();
            if(!rt) {
                return Promise.reject(new Error('No refresh token available'));
            }

            const body = `grant_type=refresh_token&refresh_token=${encodeURIComponent(rt)}`;
            const refreshOpts = {
                method: 'POST',
                headers: {'Content-Type': 'application/x-www-form-urlencoded'},
                body,
                _skipRefresh: true
            };

            const refreshCall = this._useRelay
                ? this._requestViaRelay('/oauth/token', refreshOpts)
                : fetch(`${this._baseUrl}/oauth/token`, refreshOpts).then(res => {
                    if(!res.ok) {
                        return res.json()
                            .catch(() => ({error: res.statusText}))
                            .then(err => {
                                throw new Error(err.error || `Token refresh failed: ${res.status}`);
                            });
                    }
                    return res.json();
                });

            this._refreshPromise = refreshCall
                .then(data => {
                    delete this._refreshPromise;
                    if(data && data.access_token) {
                        this.setToken(data.access_token);
                    }
                    if(data && data.refresh_token) {
                        this.setRefreshToken(data.refresh_token);
                    }
                    return data;
                })
                .catch(err => {
                    delete this._refreshPromise;
                    this.setToken(null);
                    this.setRefreshToken(null);
                    throw err;
                });

            return this._refreshPromise;
        }

        _requestViaRelay(path, opts = {}) {
            return this._getRelayWindow().then(relayWin => {
                const token = this.getToken();
                const headers = Object.assign(
                    {'Content-Type': 'application/json'},
                    token ? {Authorization: `Bearer ${token}`} : {},
                    opts.headers || {}
                );
                const reqId = `req_${Math.random().toString(36).substring(2, 9)}_${Date.now()}`;

                return new Promise((resolve, reject) => {
                    const handler = e => {
                        if(
                            !e.data
                            || e.data.type !== 'vb_relay_response'
                            || e.data.id !== reqId
                        ) {
                            return;
                        }
                        window.removeEventListener('message', handler);
                        if(e.data.ok) {
                            resolve(e.data.data);
                            return;
                        }
                        if(
                            e.data.status === 401
                            && !opts._retry
                            && !opts._skipRefresh
                            && this.getRefreshToken()
                        ) {
                            opts._retry = true;
                            this.refreshToken()
                                .then(this._requestViaRelay.bind(this, path, opts))
                                .then(resolve)
                                .catch(reject);
                            return;
                        }
                        const errMsg = (e.data.data && e.data.data.error)
                            || e.data.error
                            || `Relay request failed: ${e.data.status}`;
                        reject(new Error(errMsg));
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

        _request(path, opts = {}) {
            if(this._useRelay) {
                return this._requestViaRelay(path, opts);
            }
            const token = this.getToken();
            const headers = Object.assign(
                {'Content-Type': 'application/json'},
                token ? {Authorization: `Bearer ${token}`} : {},
                opts.headers || {}
            );
            const fetchOptions = Object.assign({}, opts, {headers});

            return fetch(`${this._baseUrl}${path}`, fetchOptions)
                .then(res => {
                    if(
                        res.status === 401
                        && !opts._retry
                        && !opts._skipRefresh
                        && this.getRefreshToken()
                    ) {
                        opts._retry = true;
                        return this.refreshToken().then(this._request.bind(this, path, opts));
                    }
                    if(!res.ok) {
                        return res.json()
                            .catch(() => ({error: res.statusText}))
                            .then(err => {
                                throw new Error(err.error || `Request failed: ${res.status}`);
                            });
                    }
                    return res.json();
                })
                .catch(err => {
                    //fallback to iframe relay when csp or cors blocks direct fetch
                    if(
                        err.name === 'TypeError'
                        && typeof window !== 'undefined'
                        && !this._useRelay
                    ) {
                        console.warn('[VibeBackr] Direct fetch blocked (CSP/CORS). Falling back to iframe relay...');
                        return this._requestViaRelay(path, opts);
                    }
                    throw err;
                });
        }

        login(provider = 'google', collectionId) {
            return new Promise(resolve => {
                const width = 500;
                const height = 650;
                const left = typeof window !== 'undefined' ? (window.screenX || 0) + (window.outerWidth - width) / 2 : 0;
                const top = typeof window !== 'undefined' ? (window.screenY || 0) + (window.outerHeight - height) / 2 : 0;
                const targetCol = collectionId || this._collectionId;
                const colParam = targetCol ? `&collection_id=${encodeURIComponent(targetCol)}` : '';
                if(typeof window !== 'undefined') {
                    window.open(
                        `${this._baseUrl}/auth/login/${provider}?popup=true${colParam}`,
                        'vibebackr_login',
                        `width=${width},height=${height},left=${left},top=${top}`
                    );
                }

                const onMessage = e => {
                    if(
                        e.origin
                        && e.origin !== 'null'
                        && !this._baseUrl.startsWith(e.origin)
                    ) {
                        try {
                            const bHost = new URL(this._baseUrl).hostname;
                            const oHost = new URL(e.origin).hostname;
                            if(
                                bHost !== oHost
                                && !bHost.endsWith(`.${oHost}`)
                                && !oHost.endsWith(`.${bHost}`)
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
                        this.setToken(e.data.access_token);
                        if(e.data.refresh_token) {
                            this.setRefreshToken(e.data.refresh_token);
                        }
                        resolve(e.data);
                    }
                };
                if(typeof window !== 'undefined') {
                    window.addEventListener('message', onMessage);
                }
            });
        }

        loginWithGoogle() {
            return this.login('google');
        }

        loginWithGitHub() {
            return this.login('github');
        }

        loginMock() {
            return this.login('mock');
        }

        getLoginUrl(provider = 'google', redirectUri, collectionId) {
            let targetUri = redirectUri;
            if(!targetUri && typeof window !== 'undefined') {
                const isFile = window.location.protocol === 'file:'
                    || window.location.origin === 'null';
                targetUri = isFile ? 'http://localhost:3000' : window.location.origin;
            }
            const targetCol = collectionId || this._collectionId;
            const colParam = targetCol ? `&collection_id=${encodeURIComponent(targetCol)}` : '';
            return `${this._baseUrl}/auth/login/${provider}?redirect_uri=${encodeURIComponent(targetUri || '')}${colParam}`;
        }

        handleAuthCallback() {
            if(typeof window === 'undefined') {
                return null;
            }
            if(window.location.hash.includes('access_token=')) {
                const params = new URLSearchParams(window.location.hash.substring(1));
                const token = params.get('access_token');
                if(token) {
                    this.setToken(token);
                    const refToken = params.get('refresh_token');
                    if(refToken) {
                        this.setRefreshToken(refToken);
                    }
                    window.location.hash = '';
                    return token;
                }
            }
            return this.getToken();
        }

        logout() {
            const rt = this.getRefreshToken();
            this.setToken(null);
            this.setRefreshToken(null);
            const body = rt ? `refresh_token=${encodeURIComponent(rt)}` : '';
            return this._request('/api/auth/logout', {
                method: 'POST',
                headers: {'Content-Type': 'application/x-www-form-urlencoded'},
                body,
                _skipRefresh: true
            }).catch(() => ({message: 'logged out locally'}));
        }

        revokeAllSessions() {
            return this._request('/api/auth/revoke-all', {method: 'POST'});
        }

        listProviders() {
            return this._request('/api/auth/providers').then(res => res.providers || []);
        }

        listTenants() {
            return this._request('/api/tenants');
        }

        createTenant() {
            return this._request('/api/tenants', {method: 'POST'});
        }

        leaveTenant(tenantId) {
            return this._request(`/api/tenants/${encodeURIComponent(tenantId)}`, {
                method: 'DELETE'
            });
        }

        listCollections() {
            return this._request('/api/collections');
        }

        getCollections() {
            return this.listCollections();
        }

        createCollection(nameOrConfig, defaultVisibility, originUrls) {
            const payload = (typeof nameOrConfig === 'object' && nameOrConfig !== null)
                ? nameOrConfig
                : {
                    name: nameOrConfig,
                    default_visibility: defaultVisibility || 'private',
                    origin_urls: originUrls || []
                };
            return this._request('/api/collections', {
                method: 'POST',
                body: JSON.stringify(payload)
            });
        }

        getCollection(colId) {
            const targetCol = this._resolveColId(colId);
            return this._request(`/api/collections/${encodeURIComponent(targetCol)}`);
        }

        updateCollection(colId, payload) {
            return this._request(`/api/collections/${encodeURIComponent(colId)}`, {
                method: 'PUT',
                body: JSON.stringify(payload)
            });
        }

        publishApp(colIdOrHtml, htmlContent, slug) {
            let targetCol;
            let html;
            let customSlug;
            if(htmlContent === undefined && this._collectionId) {
                targetCol = this._collectionId;
                html = colIdOrHtml;
            } else {
                targetCol = this._resolveColId(colIdOrHtml);
                html = htmlContent;
                customSlug = slug;
            }
            const body = {html};
            if(customSlug !== undefined) {
                body.slug = customSlug;
            }
            return this._request(`/api/collections/${encodeURIComponent(targetCol)}/app`, {
                method: 'PUT',
                body: JSON.stringify(body)
            });
        }

        deleteCollection(colId) {
            return this._request(`/api/collections/${encodeURIComponent(colId)}`, {
                method: 'DELETE'
            });
        }

        listRecords(colId) {
            const targetCol = this._resolveColId(colId);
            return this._request(`/api/collections/${encodeURIComponent(targetCol)}/records`);
        }

        getRecords(colId) {
            return this.listRecords(colId);
        }

        getRecord(colIdOrRecordId, recordId) {
            let targetCol;
            let targetRec;
            if(recordId === undefined && this._collectionId) {
                targetCol = this._collectionId;
                targetRec = colIdOrRecordId;
            } else {
                targetCol = this._resolveColId(colIdOrRecordId);
                targetRec = recordId;
            }
            return this._request(`/api/collections/${encodeURIComponent(targetCol)}/records/${encodeURIComponent(targetRec)}`);
        }

        createRecord(colIdOrPayload, payloadOrColId) {
            let targetCol;
            let payload;
            if(typeof colIdOrPayload === 'object' && colIdOrPayload !== null) {
                payload = colIdOrPayload;
                targetCol = this._resolveColId(payloadOrColId);
            } else {
                targetCol = this._resolveColId(colIdOrPayload);
                payload = payloadOrColId || {};
            }
            const accessPolicy = payload.access_policy || {
                visibility: payload.visibility || '',
                shares: payload.shares || []
            };
            return this._request(`/api/collections/${encodeURIComponent(targetCol)}/records`, {
                method: 'POST',
                body: JSON.stringify({
                    title: payload.title,
                    content: payload.content || '',
                    data: payload.data !== undefined ? payload.data : {},
                    access_policy: accessPolicy
                })
            });
        }

        updateRecord(colIdOrRecId, recIdOrPayload, payloadOrColId) {
            let targetCol;
            let targetRec;
            let payload;
            if(typeof recIdOrPayload === 'object' && recIdOrPayload !== null) {
                targetRec = colIdOrRecId;
                payload = recIdOrPayload;
                targetCol = this._resolveColId(payloadOrColId);
            } else {
                targetCol = this._resolveColId(colIdOrRecId);
                targetRec = recIdOrPayload;
                payload = payloadOrColId || {};
            }
            const body = Object.assign({}, payload);
            if(!body.access_policy && (body.visibility || body.shares)) {
                body.access_policy = {
                    visibility: body.visibility || '',
                    shares: body.shares || []
                };
            }
            return this._request(`/api/collections/${encodeURIComponent(targetCol)}/records/${encodeURIComponent(targetRec)}`, {
                method: 'PUT',
                body: JSON.stringify(body)
            });
        }

        deleteRecord(colIdOrRecordId, recordId) {
            let targetCol;
            let targetRec;
            if(recordId === undefined && this._collectionId) {
                targetCol = this._collectionId;
                targetRec = colIdOrRecordId;
            } else {
                targetCol = this._resolveColId(colIdOrRecordId);
                targetRec = recordId;
            }
            return this._request(`/api/collections/${encodeURIComponent(targetCol)}/records/${encodeURIComponent(targetRec)}`, {
                method: 'DELETE'
            });
        }

        searchRecords(colIdOrQuery, queryOrLimit, limit) {
            let targetCol;
            let query;
            let maxResults;
            if(
                this._collectionId
                && (queryOrLimit === undefined || typeof queryOrLimit === 'number')
            ) {
                targetCol = this._collectionId;
                query = colIdOrQuery;
                maxResults = queryOrLimit || 20;
            } else {
                targetCol = this._resolveColId(colIdOrQuery);
                query = queryOrLimit;
                maxResults = limit || 20;
            }
            return this._request(`/api/collections/${encodeURIComponent(targetCol)}/search`, {
                method: 'POST',
                body: JSON.stringify({query, limit: maxResults})
            });
        }

        search(colIdOrQuery, queryOrLimit, limit) {
            return this.searchRecords(colIdOrQuery, queryOrLimit, limit);
        }
    }

    function createVibeClient(baseUrlOrOptions, options = {}) {
        if(typeof baseUrlOrOptions === 'object' && baseUrlOrOptions !== null) {
            return new VibeClient(baseUrlOrOptions);
        }
        const merged = Object.assign({}, options);
        if(baseUrlOrOptions) {
            merged.baseUrl = baseUrlOrOptions;
        }
        return new VibeClient(merged);
    }

    function VibeBackr(baseUrlOrOptions, options) {
        return createVibeClient(baseUrlOrOptions, options);
    }

    VibeBackr.prototype = VibeClient.prototype;
    VibeBackr.VibeClient = VibeClient;
    VibeBackr.createClient = createVibeClient;
    VibeBackr.createVibeClient = createVibeClient;
    VibeBackr.VibeBackr = VibeBackr;

    return VibeBackr;
});
