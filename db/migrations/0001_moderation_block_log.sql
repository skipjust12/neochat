CREATE TABLE IF NOT EXISTS moderation_block_log (
    id         BIGSERIAL PRIMARY KEY,
    user_id    TEXT NOT NULL,
    categories JSONB NOT NULL,
    reason     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_moderation_block_log_user_id ON moderation_block_log (user_id);
