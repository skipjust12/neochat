CREATE TABLE IF NOT EXISTS projects (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, project_id)
);

ALTER TABLE conversation_metadata
    ADD COLUMN IF NOT EXISTS project_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS conversation_metadata_user_project_idx
    ON conversation_metadata (user_id, project_id, pinned DESC);
