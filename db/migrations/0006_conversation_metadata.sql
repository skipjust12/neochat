CREATE TABLE IF NOT EXISTS conversation_metadata (
    user_id         TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    title           TEXT NOT NULL DEFAULT '',
    pinned          BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (user_id, conversation_id)
);

CREATE INDEX IF NOT EXISTS conversation_metadata_user_pinned_idx
    ON conversation_metadata (user_id, pinned DESC);
