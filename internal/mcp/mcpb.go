package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// LogoSVG is the canonical VibeBackr ASCII-phosphor V logo mark.
const LogoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="100%" height="100%" fill="none">
  <defs>
    <!-- Cyan glow filter for ASCII phosphor vibe -->
    <filter id="cyanGlow" x="-20%" y="-20%" width="140%" height="140%">
      <feGaussianBlur stdDeviation="4.5" result="blur1" />
      <feGaussianBlur stdDeviation="10" result="blur2" />
      <feMerge>
        <feMergeNode in="blur2" />
        <feMergeNode in="blur1" />
        <feMergeNode in="SourceGraphic" />
      </feMerge>
    </filter>

    <!-- Subtle chromatic glitch displacement -->
    <linearGradient id="cyanLineGrad" x1="0%" y1="0%" x2="100%" y2="100%">
      <stop offset="0%" stop-color="#72f1ff" />
      <stop offset="100%" stop-color="#38bdf8" />
    </linearGradient>
  </defs>

  <!-- Micro horizontal scanline glitches -->
  <g opacity="0.6" stroke="#38bdf8" stroke-width="1.5" stroke-linecap="square">
    <line x1="90" y1="140" x2="110" y2="140" />
    <line x1="172" y1="165" x2="204" y2="165" />
    <line x1="195" y1="215" x2="235" y2="215" />
    <line x1="330" y1="180" x2="385" y2="180" opacity="0.4" />
    <line x1="318" y1="185" x2="398" y2="185" />
    <line x1="285" y1="260" x2="320" y2="260" />
    <line x1="150" y1="318" x2="215" y2="318" />
    <line x1="155" y1="324" x2="200" y2="324" opacity="0.4" />
  </g>

  <!-- Micro binary & terminal data fragments -->
  <g fill="#38bdf8" font-family="'JetBrains Mono', 'Fira Code', 'Courier New', monospace" font-size="13" font-weight="700" opacity="0.55">
    <text x="398" y="170">0</text>
    <text x="406" y="198">1</text>
    <text x="345" y="328">1</text>
    <text x="325" y="348">0</text>
  </g>

  <g filter="url(#cyanGlow)" stroke="url(#cyanLineGrad)" stroke-width="8" stroke-linecap="square">
    
    <!-- Top Caps (ASCII horizontal segment markers) -->
    <line x1="112" y1="104" x2="162" y2="104" />
    <line x1="350" y1="104" x2="400" y2="104" />

    <!-- Left Outer Segment (Broken into discrete ASCII segments) -->
    <line x1="108" y1="110" x2="144" y2="206" />
    <line x1="147" y1="218" x2="183" y2="314" />
    <line x1="186" y1="326" x2="222" y2="422" />

    <!-- Left Inner Segment (Slanted inner edge with spacing) -->
    <line x1="166" y1="110" x2="202" y2="206" />
    <line x1="205" y1="218" x2="256" y2="354" />

    <!-- Right Inner Segment -->
    <line x1="256" y1="354" x2="307" y2="218" />
    <line x1="310" y1="206" x2="346" y2="110" />

    <!-- Right Outer Segment (Broken into discrete ASCII segments) -->
    <line x1="290" y1="422" x2="326" y2="326" />
    <line x1="329" y1="314" x2="365" y2="218" />
    <line x1="368" y1="206" x2="404" y2="110" />

    <!-- Bottom Base Segment -->
    <line x1="226" y1="426" x2="286" y2="426" />
  </g>

  <g stroke="#e0faff" stroke-width="3" stroke-linecap="square">
    <!-- Top Cap Cores -->
    <line x1="114" y1="104" x2="160" y2="104" />
    <line x1="352" y1="104" x2="398" y2="104" />

    <!-- Outer Left Cores -->
    <line x1="110" y1="112" x2="142" y2="204" />
    <line x1="149" y1="220" x2="181" y2="312" />
    <line x1="188" y1="328" x2="220" y2="420" />

    <!-- Inner Core V-apex -->
    <line x1="168" y1="112" x2="200" y2="204" />
    <line x1="207" y1="220" x2="256" y2="352" />
    <line x1="256" y1="352" x2="305" y2="220" />
    <line x1="312" y1="204" x2="344" y2="112" />

    <!-- Outer Right Cores -->
    <line x1="292" y1="420" x2="324" y2="328" />
    <line x1="331" y1="312" x2="363" y2="220" />
    <line x1="370" y1="204" x2="402" y2="112" />

    <!-- Bottom Cap Core -->
    <line x1="228" y1="426" x2="284" y2="426" />
  </g>
</svg>`

// BuildMCPB generates a one-click installable Claude Desktop MCP Bundle (.mcpb) ZIP archive.
func (s *Server) BuildMCPB(ctx context.Context) ([]byte, error) {
	baseURL := s.GetBaseURL(ctx)
	tools := s.GetToolDefinitionsWithContext(ctx)

	type manifestTool struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	mTools := make([]manifestTool, 0, len(tools))
	for _, t := range tools {
		mTools = append(mTools, manifestTool{
			Name:        t.Name,
			Description: t.Description,
		})
	}

	manifest := map[string]interface{}{
		"manifest_version": "0.3",
		"name":             "vibebackr",
		"display_name":     "VibeBackr",
		"version":          "1.0.0",
		"description":      "Cloud persistence, authentication, and hybrid vector search backend for AI assistants and web applications - collections, universal records, and 5-tier ACLs.",
		"long_description": s.GetInstructions(ctx),
		"author": map[string]string{
			"name": "VibeBackr",
			"url":  baseURL,
		},
		"homepage":      baseURL,
		"documentation": baseURL + "/guide",
		"server": map[string]interface{}{
			"type":        "node",
			"entry_point": "server/index.js",
			"mcp_config": map[string]interface{}{
				"command": "node",
				"args":    []string{"${__dirname}/server/index.js"},
				"env": map[string]string{
					"VIBEBACKR_URL":   baseURL,
					"VIBEBACKR_TOKEN": "${user_config.access_token}",
				},
			},
		},
		"user_config": map[string]interface{}{
			"access_token": map[string]interface{}{
				"type":        "string",
				"title":       "Personal Access Token (Optional)",
				"description": fmt.Sprintf("Optional: paste a token from %s/login, or leave blank to sign in automatically in your browser on first use.", baseURL),
				"sensitive":   true,
				"required":    false,
			},
		},
		"tools": mTools,
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}

	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	instructionsJSON, err := json.Marshal(s.GetInstructions(ctx))
	if err != nil {
		return nil, err
	}

	bridgeScript := fmt.Sprintf(`#!/usr/bin/env node
//stdio bridge to %s/mcp with browser oauth sign-in and token refresh
const http = require('http');
const fs = require('fs');
const path = require('path');
const os = require('os');
const {execFile} = require('child_process');
const readline = require('readline');

const BASE_URL = (process.env.VIBEBACKR_URL || %q).replace(/\/+$/, '');
const CRED_DIR = path.join(os.homedir(), '.vibebackr');
const CRED_FILE = path.join(CRED_DIR, 'credentials.json');
const SERVER_INSTRUCTIONS = %s;
const STATIC_TOOLS = %s;

function loadCredentials() {
    const envToken = process.env.VIBEBACKR_TOKEN;
    if(
        envToken
        && !envToken.startsWith('${')
        && envToken.trim() !== ''
    ) {
        return {access_token: envToken.trim(), refresh_token: null};
    }
    try {
        if(fs.existsSync(CRED_FILE)) {
            return JSON.parse(fs.readFileSync(CRED_FILE, 'utf8'));
        }
    } catch(_) {}
    return {access_token: null, refresh_token: null};
}

function saveCredentials(creds) {
    try {
        if(!fs.existsSync(CRED_DIR)) {
            fs.mkdirSync(CRED_DIR, {recursive: true, mode: 0o700});
        }
        const tmpFile = CRED_FILE + '.' + process.pid + '.tmp';
        fs.writeFileSync(tmpFile, JSON.stringify(creds, null, 2), {mode: 0o600});
        fs.renameSync(tmpFile, CRED_FILE);
    } catch(_) {}
}

let creds = loadCredentials();
const state = {};

function refreshAccessToken() {
    const diskCreds = loadCredentials();
    if(
        diskCreds.access_token
        && diskCreds.access_token !== creds.access_token
    ) {
        creds = diskCreds;
        return Promise.resolve(true);
    }
    if(diskCreds.refresh_token) {
        creds.refresh_token = diskCreds.refresh_token;
    }
    if(!creds.refresh_token) {
        return Promise.resolve(false);
    }
    return fetch(BASE_URL + '/oauth/token', {
        method: 'POST',
        headers: {'Content-Type': 'application/x-www-form-urlencoded'},
        body: 'grant_type=refresh_token&refresh_token=' + encodeURIComponent(creds.refresh_token)
    })
        .then(res => (res.ok ? res.json() : null))
        .then(data => {
            if(data && data.access_token) {
                creds.access_token = data.access_token;
                if(data.refresh_token) {
                    creds.refresh_token = data.refresh_token;
                }
                saveCredentials(creds);
                return true;
            }
            return false;
        })
        .catch(() => false);
}

function openBrowser(url) {
    if(
        typeof url !== 'string'
        || (!url.startsWith('https://') && !url.startsWith('http://'))
    ) {
        return;
    }
    if(process.platform === 'darwin') {
        execFile('open', [url]);
    } else if(process.platform === 'win32') {
        execFile('cmd.exe', ['/c', 'start', '', url]);
    } else {
        execFile('xdg-open', [url]);
    }
}

function browserLogin() {
    const diskCreds = loadCredentials();
    if(diskCreds.access_token) {
        creds = diskCreds;
        return Promise.resolve(creds);
    }
    if(state.loginPromise) {
        return state.loginPromise;
    }
    state.loginPromise = new Promise((resolve, reject) => {
        const server = http.createServer((req, res) => {
            if(req.method === 'GET' && req.url.startsWith('/callback')) {
                res.writeHead(200, {'Content-Type': 'text/html; charset=utf-8'});
                res.end('<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connected to VibeBackr</title><link rel="icon" type="image/svg+xml" href="' + BASE_URL + '/logo.svg"><link rel="preconnect" href="https://fonts.googleapis.com"><link rel="preconnect" href="https://fonts.gstatic.com" crossorigin><link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Roboto:wght@100;300;400;500;700&display=swap"></head><body style="font-family:Roboto,ui-sans-serif,system-ui,-apple-system,sans-serif;background:#fbf9f8;color:#17191b;display:flex;align-items:safe center;justify-content:safe center;min-height:100dvh;margin:0;padding:24px;box-sizing:border-box;-webkit-font-smoothing:antialiased;"><div style="background:#fbf9f8;border:1px solid rgba(23,25,27,0.09);padding:32px;border-radius:8px;text-align:center;max-width:420px;width:calc(100vw - 48px);box-sizing:border-box;"><img src="' + BASE_URL + '/logo.svg" alt="" width="44" height="44" style="display:block;margin:0 auto 12px;"><div style="font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;font-size:0.72rem;font-weight:500;letter-spacing:0.06em;text-transform:uppercase;color:#6366f1;margin-bottom:10px;">// AUTHENTICATED</div><h2 style="margin:0 0 8px 0;font-size:1.45rem;font-weight:300;letter-spacing:-0.015em;color:#17191b;text-wrap:balance;">Connected to VibeBackr</h2><p style="color:#6b7270;font-size:0.9rem;margin:0;line-height:1.6;">You can close this tab and return to Claude.</p></div><script>const p=new URLSearchParams(location.hash.slice(1));fetch("/token",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({access_token:p.get("access_token"),refresh_token:p.get("refresh_token")})}).then(()=>setTimeout(window.close.bind(window),500));</script></body></html>');
                return;
            }
            if(req.method === 'POST' && req.url === '/token') {
                let body = '';
                req.on('data', chunk => {
                    body += chunk;
                });
                req.on('end', () => {
                    res.writeHead(200, {'Content-Type': 'application/json'});
                    res.end('{"ok":true}');
                    try {
                        const parsed = JSON.parse(body);
                        if(parsed.access_token) {
                            creds.access_token = parsed.access_token;
                            if(parsed.refresh_token) {
                                creds.refresh_token = parsed.refresh_token;
                            }
                            saveCredentials(creds);
                            clearTimeout(timer);
                            server.close();
                            delete state.loginPromise;
                            resolve(creds);
                        }
                    } catch(e) {}
                });
                return;
            }
            res.writeHead(404);
            res.end();
        });

        const timer = setTimeout(() => {
            server.close();
            delete state.loginPromise;
            reject(new Error('Browser sign-in timed out. Please try again or configure a Personal Access Token from ' + BASE_URL + '/login'));
        }, 120000);

        server.listen(0, '127.0.0.1', () => {
            const port = server.address().port;
            const redirectUri = 'http://127.0.0.1:' + port + '/callback';
            const loginUrl = BASE_URL + '/auth/login/google?redirect_uri=' + encodeURIComponent(redirectUri);
            openBrowser(loginUrl);
        });
    });
    return state.loginPromise;
}

function postMCP(msg) {
    const headers = {
        'Content-Type': 'application/json',
        Accept: 'application/json, text/event-stream',
        'MCP-Protocol-Version': '2025-06-18',
        Authorization: 'Bearer ' + creds.access_token
    };
    if(state.mcpSessionId) {
        headers['Mcp-Session-Id'] = state.mcpSessionId;
    }
    return fetch(BASE_URL + '/mcp', {
        method: 'POST',
        headers,
        body: JSON.stringify(msg)
    }).then(res => {
        const sid = res.headers.get('mcp-session-id');
        if(sid) {
            state.mcpSessionId = sid;
        }
        return res;
    });
}

function isTokenExpiringSoon(token) {
    if(!token || typeof token !== 'string') {
        return false;
    }
    const parts = token.split('.');
    if(parts.length !== 3) {
        return false;
    }
    try {
        const payload = JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf8'));
        return typeof payload.exp === 'number' && payload.exp * 1000 < Date.now() + 30000;
    } catch(_) {
        return false;
    }
}

function sendToRemote(msg, allowInteractiveLogin) {
    let ensureAuth = Promise.resolve();
    if(!creds.access_token && allowInteractiveLogin) {
        ensureAuth = browserLogin();
    } else if(creds.access_token && creds.refresh_token && isTokenExpiringSoon(creds.access_token)) {
        ensureAuth = refreshAccessToken();
    }

    return ensureAuth.then(() => {
        if(!creds.access_token) {
            return null;
        }
        return postMCP(msg).then(res => {
            if(res.status !== 401) {
                return res;
            }
            return refreshAccessToken().then(refreshed => {
                if(refreshed) {
                    return postMCP(msg);
                }
                if(allowInteractiveLogin) {
                    delete creds.access_token;
                    return browserLogin().then(() => postMCP(msg));
                }
                return null;
            });
        }).then(res => {
            if(!res) {
                return null;
            }
            if(res.status === 202) {
                return undefined;
            }
            return res.json();
        });
    });
}

function sendStdout(obj) {
    if(obj !== undefined && obj !== null) {
        process.stdout.write(JSON.stringify(obj) + '\n');
    }
}

const rl = readline.createInterface({input: process.stdin, terminal: false});

rl.on('line', line => {
    const trimmed = line.trim();
    if(!trimmed) {
        return;
    }
    let msg;
    try {
        msg = JSON.parse(trimmed);
    } catch(_) {
        return;
    }

    //notifications require no response
    if(msg.id === undefined || msg.id === null) {
        return;
    }

    if(msg.method === 'initialize') {
        sendToRemote(msg, false)
            .catch(() => null)
            .then(remoteInit => {
                if(remoteInit) {
                    sendStdout(remoteInit);
                    return;
                }
                sendStdout({
                    jsonrpc: '2.0',
                    id: msg.id,
                    result: {
                        protocolVersion: (msg.params && msg.params.protocolVersion) || '2025-06-18',
                        capabilities: {tools: {}},
                        serverInfo: {name: 'vibebackr-mcp', version: '1.0.0'},
                        instructions: SERVER_INSTRUCTIONS
                    }
                });
            });
        return;
    }

    if(msg.method === 'ping') {
        sendStdout({jsonrpc: '2.0', id: msg.id, result: {}});
        return;
    }

    if(msg.method === 'tools/list') {
        sendToRemote(msg, false)
            .catch(() => null)
            .then(remoteTools => {
                if(remoteTools) {
                    sendStdout(remoteTools);
                    return;
                }
                sendStdout({
                    jsonrpc: '2.0',
                    id: msg.id,
                    result: {tools: STATIC_TOOLS}
                });
            });
        return;
    }

    sendToRemote(msg, true)
        .then(reply => {
            reply && sendStdout(reply);
        })
        .catch(err => {
            sendStdout({
                jsonrpc: '2.0',
                id: msg.id,
                error: {code: -32000, message: err.message || String(err)}
            });
        });
});
`, baseURL, baseURL, string(instructionsJSON), string(toolsJSON))

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	mfWriter, err := zw.Create("manifest.json")
	if err != nil {
		return nil, err
	}
	if _, err := mfWriter.Write(manifestBytes); err != nil {
		return nil, err
	}

	jsWriter, err := zw.Create("server/index.js")
	if err != nil {
		return nil, err
	}
	if _, err := jsWriter.Write([]byte(bridgeScript)); err != nil {
		return nil, err
	}

	logoWriter, err := zw.Create("logo.svg")
	if err != nil {
		return nil, err
	}
	if _, err := logoWriter.Write([]byte(LogoSVG)); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
