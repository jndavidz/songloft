-- S-06：听音事件有界 outbox——幂等去重、replay/快照补偿、确认后清理。
-- prepare/HEAD/Range/prefetch 类事件不入库（不记已听）；重复幂等键不乱记历史。
-- +goose Up
-- +goose StatementBegin
CREATE TABLE listening_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    idempotency_key TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL,
    ref_digest TEXT NOT NULL DEFAULT '',
    entry TEXT NOT NULL DEFAULT '',
    lease_id TEXT NOT NULL DEFAULT '',
    session TEXT NOT NULL DEFAULT '',
    payload TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    acknowledged INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_listening_events_pending ON listening_events (acknowledged, seq);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS listening_events;
-- +goose StatementEnd
