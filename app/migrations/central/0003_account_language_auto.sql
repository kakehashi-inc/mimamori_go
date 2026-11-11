-- +goose Up
-- Allow 'auto' (follow the browser language) as the display language and
-- make it the default. Existing values are kept.

-- +goose StatementBegin
ALTER TABLE accounts ADD COLUMN language_new TEXT NOT NULL DEFAULT 'auto' CHECK (language_new IN ('auto', 'en', 'ja'));
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE accounts SET language_new = language;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE accounts DROP COLUMN language;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE accounts RENAME COLUMN language_new TO language;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE accounts ADD COLUMN language_old TEXT NOT NULL DEFAULT 'en' CHECK (language_old IN ('en', 'ja'));
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE accounts SET language_old = CASE language WHEN 'ja' THEN 'ja' ELSE 'en' END;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE accounts DROP COLUMN language;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE accounts RENAME COLUMN language_old TO language;
-- +goose StatementEnd
