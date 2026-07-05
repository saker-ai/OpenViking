-- OAuth provider token storage.
--
-- One row per (provider, account_id, user_id) holding the current
-- access_token + refresh_token pair issued by an external OAuth provider
-- (feishu / google / slack / dingtalk). Tokens are encrypted at rest via
-- internal/crypto envelope encryption; only the SHA-256 hash of the
-- refresh_token is stored in plaintext to support lookup-by-token.
--
-- On refresh, the row is updated with the new pair (refresh-token rotation).
-- The previous refresh_token hash is retained for one cycle so a replayed
-- refresh request can be detected and rejected with permanent_error.

CREATE TABLE IF NOT EXISTS oauth_provider_tokens (
    provider TEXT NOT NULL,
    account_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    subject TEXT NOT NULL,
    access_token_ciphertext TEXT NOT NULL,
    refresh_token_hash TEXT NOT NULL,
    refresh_token_ciphertext TEXT NOT NULL,
    previous_refresh_token_hash TEXT,
    scopes TEXT,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (provider, account_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_expires ON oauth_provider_tokens(expires_at);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_user ON oauth_provider_tokens(provider, account_id, user_id);

-- OAuth state JWTs (short-lived authorize-state + PKCE verifiers).
-- One row per state value; deleted on consume or after 10-minute TTL.

CREATE TABLE IF NOT EXISTS oauth_state_cache (
    state TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    code_challenge TEXT,
    code_challenge_method TEXT,
    redirect_uri TEXT,
    account_id TEXT,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_state_expires ON oauth_state_cache(expires_at);
