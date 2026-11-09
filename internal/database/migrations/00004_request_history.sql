-- +goose Up
CREATE TABLE request_history (
    request_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    endpoint TEXT NOT NULL CHECK (endpoint IN ('/v1/chat/completions','/v1/completions','/v1/responses','/v1/messages','/v1/embeddings')),
    requested_model TEXT,
    enqueued_at_ms INTEGER NOT NULL,
    started_at_ms INTEGER,
    completed_at_ms INTEGER,
    outcome TEXT NOT NULL CHECK (outcome IN ('queued','active','completed','rejected','timed_out','cancelled','upstream_error','interrupted')),
    response_status INTEGER CHECK (response_status BETWEEN 100 AND 599),
    input_tokens INTEGER CHECK (input_tokens >= 0),
    output_tokens INTEGER CHECK (output_tokens >= 0),
    total_tokens INTEGER CHECK (total_tokens >= 0),
    usage_status TEXT NOT NULL CHECK (usage_status IN ('reported','partial','unavailable'))
);
CREATE INDEX request_history_user_recent_idx ON request_history(user_id,enqueued_at_ms DESC,request_id DESC);
CREATE INDEX request_history_user_outcome_idx ON request_history(user_id,outcome,enqueued_at_ms DESC);
-- +goose Down
DROP TABLE request_history;
