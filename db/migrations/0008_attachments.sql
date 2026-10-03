CREATE TABLE IF NOT EXISTS attachments (
    user_id         TEXT NOT NULL,
    attachment_id   TEXT NOT NULL,
    conversation_id TEXT NOT NULL DEFAULT '',
    name            TEXT NOT NULL,
    mime            TEXT NOT NULL,
    kind            TEXT NOT NULL,
    size            BIGINT NOT NULL,
    data            BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, attachment_id)
);

CREATE INDEX IF NOT EXISTS attachments_user_conversation_idx
    ON attachments (user_id, conversation_id, created_at);

ALTER TABLE conversation_messages
    ADD COLUMN IF NOT EXISTS attachments JSONB NOT NULL DEFAULT '[]'::jsonb;
