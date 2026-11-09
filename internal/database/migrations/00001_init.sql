-- +goose Up
CREATE TABLE IF NOT EXISTS schema_migrations_placeholder (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    initialized_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE IF EXISTS schema_migrations_placeholder;
