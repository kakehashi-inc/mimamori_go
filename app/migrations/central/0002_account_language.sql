-- +goose Up
-- Display language of the browser screens for each account ('en' or 'ja').

-- +goose StatementBegin
ALTER TABLE accounts ADD COLUMN language TEXT NOT NULL DEFAULT 'en' CHECK (language IN ('en', 'ja'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE accounts DROP COLUMN language;
-- +goose StatementEnd
