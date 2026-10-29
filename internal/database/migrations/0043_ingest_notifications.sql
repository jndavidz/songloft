-- S-08：WebGUI completed outbox → Songloft 增量扫描的有界状态。
-- 通知按幂等键去重（重复投递只产生一次逻辑入库）；pending 行持久化，
-- 失败重试与重启补偿复用同一状态；不建立新常驻服务。
-- +goose Up
-- +goose StatementBegin
CREATE TABLE ingest_notifications (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    notif_id TEXT NOT NULL UNIQUE,
    file_path TEXT NOT NULL,
    identity TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_ingest_notifications_pending ON ingest_notifications (state, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS ingest_notifications;
-- +goose StatementEnd
