CREATE TABLE IF NOT EXISTS cost_log (
    id            BIGSERIAL PRIMARY KEY,
    user_id       TEXT NOT NULL,
    request_id    TEXT NOT NULL,
    model_id      TEXT NOT NULL,
    mode          TEXT NOT NULL,
    input_tokens  INT NOT NULL,
    output_tokens INT NOT NULL,
    cost_usd      DOUBLE PRECISION NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cost_log_user_id ON cost_log (user_id);
