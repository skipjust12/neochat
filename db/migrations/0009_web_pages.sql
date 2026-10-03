-- Pages the model read with web_fetch (package pagestore): extracted text,
-- kept so a later message in the same chat can read further into a page
-- or read it again without going back to the site. Rows go with their
-- chat, expire a day after the fetch, and the oldest are evicted when the
-- table grows past its size budget (server.RunCleanup).
CREATE TABLE IF NOT EXISTS web_pages (
    user_id         TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    url             TEXT NOT NULL,
    final_url       TEXT NOT NULL,
    title           TEXT NOT NULL DEFAULT '',
    content         TEXT NOT NULL,
    content_bytes   INTEGER NOT NULL,
    truncated       BOOLEAN NOT NULL DEFAULT FALSE,
    fetched_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, conversation_id, url)
);

CREATE INDEX IF NOT EXISTS web_pages_fetched_at_idx ON web_pages (fetched_at);
