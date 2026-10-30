-- +goose Up
-- +goose StatementBegin
CREATE TABLE personal_tokens (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    prefix TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    last_used_at DATETIME,
    revoked_at DATETIME
);
-- +goose StatementEnd

-- +goose Down
DROP TABLE personal_tokens;
