-- +goose Up
-- Sessions created with "keep me logged in": 1 when the session lasts a fixed
-- period from login (persistent cookie), 0 for an idle-timeout session.

-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN remember INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN remember;
-- +goose StatementEnd
