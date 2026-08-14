CREATE TABLE IF NOT EXISTS api_keys (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    plan_id    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys (user_id);
