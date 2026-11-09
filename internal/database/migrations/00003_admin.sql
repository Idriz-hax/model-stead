-- +goose Up
ALTER TABLE users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1));
CREATE UNIQUE INDEX users_username_lower_idx ON users(lower(username));

CREATE TABLE scheduler_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    policy TEXT NOT NULL CHECK (policy = 'weighted_round_robin'),
    global_capacity INTEGER NOT NULL CHECK (global_capacity BETWEEN 1 AND 64),
    default_weight REAL NOT NULL CHECK (default_weight BETWEEN 0.1 AND 100),
    max_queued INTEGER NOT NULL CHECK (max_queued BETWEEN 1 AND 10000),
    max_active_contended INTEGER NOT NULL CHECK (max_active_contended BETWEEN 1 AND 64),
    global_max_queued INTEGER NOT NULL CHECK (global_max_queued BETWEEN 1 AND 10000),
    max_wait_seconds INTEGER NOT NULL CHECK (max_wait_seconds BETWEEN 1 AND 86400),
    work_conserving INTEGER NOT NULL CHECK (work_conserving IN (0, 1)),
    cancellation_grace_seconds INTEGER NOT NULL CHECK (cancellation_grace_seconds BETWEEN 1 AND 300),
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    before_json TEXT NOT NULL DEFAULT '{}',
    after_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX audit_events_created_at_idx ON audit_events(created_at DESC, id DESC);
CREATE INDEX audit_events_actor_idx ON audit_events(actor_id, id DESC);
CREATE INDEX audit_events_target_idx ON audit_events(target_type, target_id, id DESC);
-- +goose StatementBegin
CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER audit_events_no_delete;
DROP TRIGGER audit_events_no_update;
DROP TABLE audit_events;
DROP TABLE scheduler_settings;
DROP INDEX users_username_lower_idx;
ALTER TABLE users DROP COLUMN must_change_password;
