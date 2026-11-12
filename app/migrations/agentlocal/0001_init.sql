-- +goose Up
-- Schema of the agent's local database on each monitored host
-- (data/agent.db next to the executable, or data/builtin-agent.db on the central
-- host).
-- Times are INTEGER Unix seconds (UTC).

-- +goose StatementBegin
CREATE TABLE state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE outbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at   INTEGER NOT NULL,
    payload      TEXT    NOT NULL,
    alert_count  INTEGER NOT NULL DEFAULT 0,
    normal_count INTEGER NOT NULL DEFAULT 0
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE findings (
    fingerprint TEXT    PRIMARY KEY,
    seen_at     INTEGER NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE scan_roots (
    path TEXT PRIMARY KEY
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE command_executions (
    run_uid       TEXT    PRIMARY KEY,
    set_id        INTEGER NOT NULL,
    set_revision  INTEGER NOT NULL DEFAULT 0,
    request_id    INTEGER NOT NULL DEFAULT 0,
    run_trigger   TEXT    NOT NULL,
    scheduled_for INTEGER NOT NULL DEFAULT 0,
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER NOT NULL DEFAULT 0
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_command_executions_request ON command_executions(request_id) WHERE request_id > 0;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE command_schedules (
    set_id       INTEGER PRIMARY KEY,
    schedule_sig TEXT    NOT NULL,
    armed_at     INTEGER NOT NULL,
    last_due     INTEGER NOT NULL DEFAULT 0
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE command_schedules;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE command_executions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE scan_roots;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE findings;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE outbox;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE state;
-- +goose StatementEnd
