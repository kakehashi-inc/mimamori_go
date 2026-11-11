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

-- +goose Down
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
