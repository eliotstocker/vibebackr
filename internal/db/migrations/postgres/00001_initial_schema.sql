-- +goose Up
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	tenant_id TEXT,
	email TEXT UNIQUE NOT NULL,
	name TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'member',
	manager_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	token_version TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS invites (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'member',
	manager_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
	created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS user_tenants (
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	tenant_id TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'member',
	manager_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, tenant_id)
);

CREATE TABLE IF NOT EXISTS collections (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	slug TEXT NOT NULL DEFAULT '',
	default_visibility TEXT NOT NULL DEFAULT 'private',
	allowed_domains TEXT NOT NULL DEFAULT '[]',
	origin_urls TEXT NOT NULL DEFAULT '[]',
	vector_search_enabled BOOLEAN NOT NULL DEFAULT FALSE,
	auto_invite_shares BOOLEAN NOT NULL DEFAULT FALSE,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS records (
	id TEXT PRIMARY KEY,
	collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	content TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL DEFAULT '{}',
	created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	access_policy TEXT NOT NULL DEFAULT '{"visibility":"private"}',
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS oauth_clients (
	id TEXT PRIMARY KEY,
	client_secret TEXT NOT NULL,
	client_name TEXT NOT NULL,
	redirect_uris TEXT NOT NULL DEFAULT '[]',
	grant_types TEXT NOT NULL DEFAULT '[]',
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS auth_codes (
	code TEXT PRIMARY KEY,
	client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	code_challenge TEXT NOT NULL,
	code_challenge_method TEXT NOT NULL,
	expires_at TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS refresh_tokens (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	client_id TEXT NOT NULL,
	token_hash TEXT UNIQUE NOT NULL,
	token_version TEXT NOT NULL DEFAULT '',
	expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
	revoked_at TIMESTAMP WITH TIME ZONE,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS tenant_billing (
	tenant_id TEXT PRIMARY KEY,
	plan TEXT NOT NULL DEFAULT 'starter',
	billing_status TEXT NOT NULL DEFAULT 'trialing',
	max_collections INTEGER NOT NULL DEFAULT 10,
	trial_ends_at TIMESTAMP WITH TIME ZONE NOT NULL,
	voucher_code TEXT NOT NULL DEFAULT '',
	gc_customer_id TEXT NOT NULL DEFAULT '',
	gc_mandate_id TEXT NOT NULL DEFAULT '',
	gc_subscription_id TEXT NOT NULL DEFAULT '',
	gc_billing_request_id TEXT NOT NULL DEFAULT '',
	pending_plan TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS vouchers (
	code TEXT PRIMARY KEY,
	plan TEXT NOT NULL DEFAULT 'pro',
	max_uses INTEGER NOT NULL DEFAULT 1,
	used_count INTEGER NOT NULL DEFAULT 0,
	expires_at TIMESTAMP WITH TIME ZONE,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_records_collection ON records(collection_id);
CREATE INDEX IF NOT EXISTS idx_records_created_by ON records(created_by);
CREATE INDEX IF NOT EXISTS idx_users_manager ON users(manager_id);
CREATE INDEX IF NOT EXISTS idx_user_tenants_user ON user_tenants(user_id);
CREATE INDEX IF NOT EXISTS idx_user_tenants_tenant ON user_tenants(tenant_id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_hash ON refresh_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user ON refresh_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_tenant_billing_br ON tenant_billing(gc_billing_request_id);
CREATE INDEX IF NOT EXISTS idx_tenant_billing_mandate ON tenant_billing(gc_mandate_id);
CREATE INDEX IF NOT EXISTS idx_tenant_billing_sub ON tenant_billing(gc_subscription_id);

-- +goose Down
DROP TABLE IF EXISTS vouchers;
DROP TABLE IF EXISTS tenant_billing;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS auth_codes;
DROP TABLE IF EXISTS oauth_clients;
DROP TABLE IF EXISTS records;
DROP TABLE IF EXISTS collections;
DROP TABLE IF EXISTS user_tenants;
DROP TABLE IF EXISTS invites;
DROP TABLE IF EXISTS users;
