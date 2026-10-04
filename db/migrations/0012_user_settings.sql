-- Each user's preferences (package settings), so every device they sign
-- in on gets the same tone, instructions, model and theme. API keys are
-- never stored here.
CREATE TABLE IF NOT EXISTS user_settings (
    user_id    TEXT PRIMARY KEY,
    settings   JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
