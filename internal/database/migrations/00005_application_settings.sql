-- +goose Up
CREATE TABLE application_settings (id INTEGER PRIMARY KEY CHECK(id=1), value_json TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
-- +goose Down
DROP TABLE application_settings;
