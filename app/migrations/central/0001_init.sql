-- +goose Up
-- Schema of the central management database (central.db).
-- Times are stored as INTEGER Unix seconds (UTC). 0 means "not set".
-- Detections and host reports of each monitored target are stored in a
-- separate per-target database (see app/migrations/agentdb).

-- +goose StatementBegin
CREATE TABLE accounts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    email         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    display_name  TEXT    NOT NULL,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('admin', 'viewer')),
    language      TEXT    NOT NULL DEFAULT 'auto' CHECK (language IN ('auto', 'en', 'ja')),
    disabled      INTEGER NOT NULL DEFAULT 0,
    failed_logins INTEGER NOT NULL DEFAULT 0,
    locked_until  INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE sessions (
    id           TEXT    PRIMARY KEY,
    account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    remember     INTEGER NOT NULL DEFAULT 0,
    step_up_at   INTEGER NOT NULL DEFAULT 0
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE auth_codes (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    purpose        TEXT    NOT NULL CHECK (purpose IN ('login', 'email_change', 'step_up')),
    account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    challenge_hash TEXT    NOT NULL DEFAULT '',
    session_id     TEXT    NOT NULL DEFAULT '',
    new_email      TEXT    NOT NULL DEFAULT '',
    code_hash      TEXT    NOT NULL,
    remember       INTEGER NOT NULL DEFAULT 0,
    attempts       INTEGER NOT NULL DEFAULT 0,
    remote_addr    TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_auth_codes_challenge ON auth_codes(challenge_hash);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_auth_codes_account ON auth_codes(account_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE browser_verifications (
    id          TEXT    PRIMARY KEY,
    account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    trusted     INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    remote_addr TEXT    NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_browser_verifications_account ON browser_verifications(account_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE rules (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT    NOT NULL UNIQUE,
    description    TEXT    NOT NULL DEFAULT '',
    latest_version INTEGER NOT NULL DEFAULT 1,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    created_by     TEXT    NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE rule_versions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id    INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
    version    INTEGER NOT NULL,
    content    TEXT    NOT NULL,
    comment    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    created_by TEXT    NOT NULL DEFAULT '',
    UNIQUE (rule_id, version)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE templates (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    description TEXT    NOT NULL DEFAULT '',
    content     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    created_by  TEXT    NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE tokens (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT    NOT NULL UNIQUE,
    hash               TEXT    NOT NULL UNIQUE,
    value              TEXT    NOT NULL DEFAULT '',
    expires_at         INTEGER NOT NULL,
    pending_hash       TEXT    NOT NULL DEFAULT '',
    pending_expires_at INTEGER NOT NULL DEFAULT 0,
    renew_counter      INTEGER NOT NULL DEFAULT 0,
    instance_id         TEXT    NOT NULL DEFAULT '',
    bound_at           INTEGER NOT NULL DEFAULT 0,
    used_at            INTEGER NOT NULL DEFAULT 0,
    last_seen_at       INTEGER NOT NULL DEFAULT 0,
    revoked_at         INTEGER NOT NULL DEFAULT 0,
    reissued_at        INTEGER NOT NULL DEFAULT 0,
    renewed_at         INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    created_by         TEXT    NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_tokens_pending_hash ON tokens(pending_hash);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE agents (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    kind                  TEXT    NOT NULL CHECK (kind IN ('builtin', 'token')),
    token_id              INTEGER UNIQUE REFERENCES tokens(id) ON DELETE CASCADE,
    db_file               TEXT    NOT NULL,
    hostname              TEXT    NOT NULL DEFAULT '',
    os_name               TEXT    NOT NULL DEFAULT '',
    os_version            TEXT    NOT NULL DEFAULT '',
    agent_version         TEXT    NOT NULL DEFAULT '',
    assigned_rule_id      INTEGER REFERENCES rules(id) ON DELETE RESTRICT,
    assigned_rule_version INTEGER NOT NULL DEFAULT 0,
    assigned_at           INTEGER NOT NULL DEFAULT 0,
    reported_rule_id      INTEGER NOT NULL DEFAULT 0,
    reported_rule_version INTEGER NOT NULL DEFAULT 0,
    apply_status          TEXT    NOT NULL DEFAULT 'none',
    apply_message         TEXT    NOT NULL DEFAULT '',
    apply_warnings        TEXT    NOT NULL DEFAULT '[]',
    apply_version         INTEGER NOT NULL DEFAULT 0,
    apply_at              INTEGER NOT NULL DEFAULT 0,
    ca_fingerprints       TEXT    NOT NULL DEFAULT '[]',
    auditd_json           TEXT    NOT NULL DEFAULT '',
    last_sync_at          INTEGER NOT NULL DEFAULT 0,
    last_report_at        INTEGER NOT NULL DEFAULT 0,
    dropped_total         INTEGER NOT NULL DEFAULT 0,
    host_os_user          TEXT    NOT NULL DEFAULT '',
    time_zone             TEXT    NOT NULL DEFAULT '',
    utc_offset_sec        INTEGER NOT NULL DEFAULT 0,
    command_sig           TEXT    NOT NULL DEFAULT '',
    command_problems      TEXT    NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE suppressions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id   INTEGER REFERENCES agents(id) ON DELETE CASCADE,
    type       TEXT    NOT NULL DEFAULT '',
    os_user    TEXT    NOT NULL DEFAULT '',
    exe        TEXT    NOT NULL DEFAULT '',
    target     TEXT    NOT NULL DEFAULT '',
    comment    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    created_by TEXT    NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE history (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    at           INTEGER NOT NULL,
    last_at      INTEGER NOT NULL,
    account_name TEXT    NOT NULL DEFAULT '',
    category     TEXT    NOT NULL,
    action       TEXT    NOT NULL,
    target       TEXT    NOT NULL DEFAULT '',
    detail       TEXT    NOT NULL DEFAULT '',
    remote_addr  TEXT    NOT NULL DEFAULT '',
    count        INTEGER NOT NULL DEFAULT 1
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_history_category ON history(category, at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE certificates (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT    NOT NULL CHECK (kind IN ('ca', 'server')),
    fingerprint TEXT    NOT NULL UNIQUE,
    subject     TEXT    NOT NULL DEFAULT '',
    issuer_fp   TEXT    NOT NULL DEFAULT '',
    names       TEXT    NOT NULL DEFAULT '[]',
    cert_pem    TEXT    NOT NULL,
    key_pem     TEXT    NOT NULL,
    not_before  INTEGER NOT NULL,
    not_after   INTEGER NOT NULL,
    status      TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE notifications (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at      INTEGER NOT NULL,
    subject         TEXT    NOT NULL,
    body            TEXT    NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',
    last_attempt_at INTEGER NOT NULL DEFAULT 0,
    sent_at         INTEGER NOT NULL DEFAULT 0
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE settings;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE notifications;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE certificates;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE history;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE suppressions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE agents;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE tokens;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE templates;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE rule_versions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE rules;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE browser_verifications;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE auth_codes;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE sessions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE accounts;
-- +goose StatementEnd
