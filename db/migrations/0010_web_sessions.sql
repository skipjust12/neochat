-- Browser sessions (package auth, session.go): signing in to the web UI
-- exchanges an API key for one of these, carried in an HttpOnly cookie.
-- Only hashes are stored, like api_keys. No foreign key on key_hash: a
-- session is checked against api_keys on every use, so deleting a key
-- signs its sessions out at once, and server.RunCleanup deletes the rows
-- it left behind along with expired ones.
CREATE TABLE IF NOT EXISTS web_sessions (
    token_hash TEXT PRIMARY KEY,
    key_hash   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS web_sessions_key_hash_idx ON web_sessions (key_hash, created_at);
CREATE INDEX IF NOT EXISTS web_sessions_expires_at_idx ON web_sessions (expires_at);
