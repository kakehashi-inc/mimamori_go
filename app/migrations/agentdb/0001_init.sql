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

-- +goose StatementBegin
CREATE TABLE db_instance (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    value      TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO db_instance (id, value, created_at) VALUES (1, lower(hex(randomblob(16))), CAST(strftime('%s', 'now') AS INTEGER));
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE command_sets (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT    NOT NULL,
    description       TEXT    NOT NULL DEFAULT '',
    purpose           TEXT    NOT NULL CHECK (purpose IN ('collect_output', 'run_only')),
    os_user           TEXT    NOT NULL,
    work_dir          TEXT    NOT NULL DEFAULT '',
    run_mode          TEXT    NOT NULL CHECK (run_mode IN ('manual', 'schedule')),
    schedule_kind     TEXT    NOT NULL DEFAULT '',
    schedule_at       INTEGER NOT NULL DEFAULT 0,
    schedule_weekdays INTEGER NOT NULL DEFAULT 0,
    schedule_time     INTEGER NOT NULL DEFAULT 0,
    schedule_every    INTEGER NOT NULL DEFAULT 0,
    schedule_minute   INTEGER NOT NULL DEFAULT 0,
    keep_runs         INTEGER NOT NULL DEFAULT 100,
    joiner            TEXT    NOT NULL DEFAULT 'comma' CHECK (joiner IN ('comma', 'newline')),
    revision          INTEGER NOT NULL DEFAULT 1,
    imported_from     TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    created_by        TEXT    NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL,
    updated_by        TEXT    NOT NULL DEFAULT '',
    deleted_at        INTEGER NOT NULL DEFAULT 0,
    deleted_by        TEXT    NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_command_sets_name ON command_sets(name) WHERE deleted_at = 0;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE command_items (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    set_id       INTEGER NOT NULL REFERENCES command_sets(id) ON DELETE CASCADE,
    position     INTEGER NOT NULL,
    command_line TEXT    NOT NULL,
    note         TEXT    NOT NULL DEFAULT '',
    success_kind TEXT    NOT NULL DEFAULT 'single',
    success_a    INTEGER NOT NULL DEFAULT 0,
    success_b    INTEGER NOT NULL DEFAULT 0,
    failure_kind TEXT    NOT NULL DEFAULT 'min',
    failure_a    INTEGER NOT NULL DEFAULT 1,
    failure_b    INTEGER NOT NULL DEFAULT 0,
    on_problems  TEXT    NOT NULL DEFAULT 'stop' CHECK (on_problems IN ('stop', 'continue_on_failure', 'continue_always')),
    timeout_sec  INTEGER NOT NULL DEFAULT 300,
    line_limit   INTEGER NOT NULL DEFAULT 20,
    patterns     TEXT    NOT NULL DEFAULT '[]',
    UNIQUE (set_id, position)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE command_runs (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    run_uid           TEXT    NOT NULL UNIQUE,
    set_id            INTEGER NOT NULL REFERENCES command_sets(id) ON DELETE CASCADE,
    set_revision      INTEGER NOT NULL DEFAULT 0,
    set_name          TEXT    NOT NULL,
    purpose           TEXT    NOT NULL,
    joiner            TEXT    NOT NULL DEFAULT 'comma',
    os_user           TEXT    NOT NULL DEFAULT '',
    work_dir          TEXT    NOT NULL DEFAULT '',
    run_trigger       TEXT    NOT NULL CHECK (run_trigger IN ('manual', 'schedule')),
    request_id        INTEGER NOT NULL DEFAULT 0,
    scheduled_for     INTEGER NOT NULL DEFAULT 0,
    commands_total    INTEGER NOT NULL DEFAULT 0,
    started_at        INTEGER NOT NULL,
    finished_at       INTEGER NOT NULL DEFAULT 0,
    state             TEXT    NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'success', 'failure', 'error')),
    reason            TEXT    NOT NULL DEFAULT '',
    display_rows      TEXT    NOT NULL DEFAULT '',
    display_truncated INTEGER NOT NULL DEFAULT 0,
    received_at       INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_command_runs_set ON command_runs(set_id, id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_command_runs_received ON command_runs(received_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE command_results (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id           INTEGER NOT NULL REFERENCES command_runs(id) ON DELETE CASCADE,
    position         INTEGER NOT NULL,
    command_line     TEXT    NOT NULL DEFAULT '',
    note             TEXT    NOT NULL DEFAULT '',
    kind             TEXT    NOT NULL CHECK (kind IN ('success', 'failure', 'error')),
    reason           TEXT    NOT NULL DEFAULT '',
    exit_code        INTEGER,
    started_at       INTEGER NOT NULL DEFAULT 0,
    finished_at      INTEGER NOT NULL DEFAULT 0,
    output           BLOB    NOT NULL DEFAULT x'',
    output_bytes     INTEGER NOT NULL DEFAULT 0,
    output_truncated INTEGER NOT NULL DEFAULT 0,
    values_json      TEXT    NOT NULL DEFAULT '',
    UNIQUE (run_id, position)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE command_requests (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    set_id       INTEGER NOT NULL REFERENCES command_sets(id) ON DELETE CASCADE,
    state        TEXT    NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'delivered', 'done', 'expired', 'discarded')),
    requested_at INTEGER NOT NULL,
    requested_by TEXT    NOT NULL DEFAULT '',
    expires_at   INTEGER NOT NULL,
    delivered_at INTEGER NOT NULL DEFAULT 0,
    done_at      INTEGER NOT NULL DEFAULT 0,
    run_uid      TEXT    NOT NULL DEFAULT '',
    discarded_at INTEGER NOT NULL DEFAULT 0,
    discarded_by TEXT    NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_command_requests_active ON command_requests(set_id) WHERE state IN ('pending', 'delivered');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE command_requests;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE command_results;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE command_runs;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE command_items;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE command_sets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE db_instance;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE host_reports;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE detections;
-- +goose StatementEnd
