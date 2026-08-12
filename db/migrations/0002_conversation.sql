CREATE TABLE IF NOT EXISTS conversation_messages (
    id              BIGSERIAL PRIMARY KEY,
    user_id         TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    role            TEXT NOT NULL,
    content         TEXT NOT NULL,
    model_id        TEXT NOT NULL DEFAULT '',
    is_summary      BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_conv_msgs_user_conv ON conversation_messages (user_id, conversation_id, id);

CREATE TABLE IF NOT EXISTS conversation_summaries (
    user_id         TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    text            TEXT NOT NULL,
    covers_through  INT NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, conversation_id)
);
