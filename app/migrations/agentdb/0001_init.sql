-- +goose Up
-- Schema of a per-target database (one file per monitored target, i.e. per
-- token-identified agent and one for the built-in agent). Times are INTEGER
-- Unix seconds (UTC); 0 means "not set".

-- +goose StatementBegin
CREATE TABLE detections (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint     TEXT    NOT NULL,
    type            TEXT    NOT NULL,
    category        TEXT    NOT NULL,
    os_user         TEXT    NOT NULL DEFAULT '',
    exe             TEXT    NOT NULL DEFAULT '',
    target          TEXT    NOT NULL DEFAULT '',
    count           INTEGER NOT NULL DEFAULT 1,
    first_at        INTEGER NOT NULL,
    last_at         INTEGER NOT NULL,
    file_ctime      INTEGER NOT NULL DEFAULT 0,
    file_owner      TEXT    NOT NULL DEFAULT '',
    file_mode       TEXT    NOT NULL DEFAULT '',
    suppressed      INTEGER NOT NULL DEFAULT 0,
    suppression_id  INTEGER NOT NULL DEFAULT 0,
    acknowledged_at INTEGER NOT NULL DEFAULT 0,
    acknowledged_by TEXT    NOT NULL DEFAULT '',
    notify_state    TEXT    NOT NULL DEFAULT 'pending',
    notified_at     INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_detections_fingerprint ON detections(fingerprint, acknowledged_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_detections_last_at ON detections(last_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_detections_notify ON detections(notify_state);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE host_reports (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    received_at INTEGER NOT NULL,
    host_info   TEXT    NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE host_reports;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE detections;
-- +goose StatementEnd
