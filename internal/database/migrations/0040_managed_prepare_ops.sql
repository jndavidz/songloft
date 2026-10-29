-- Managed prepare 操作的持久化（S-03）：remote 生命周期首版只覆盖准备状态与恢复。
-- 重启后 preparing 行在恢复时显式转 interrupted，绝不自动续跑、不自动出声。
-- +goose Up
-- +goose StatementBegin
CREATE TABLE managed_prepare_ops (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issuer TEXT NOT NULL,
    session TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    entry TEXT NOT NULL,
    ref_digest TEXT NOT NULL,
    state TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    resolved_source TEXT NOT NULL DEFAULT '',
    fallback_reason TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_managed_prepare_ops_active
    ON managed_prepare_ops (state) WHERE state = 'preparing';
CREATE INDEX idx_managed_prepare_ops_session_entry
    ON managed_prepare_ops (session, entry, epoch);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS managed_prepare_ops;
-- +goose StatementEnd
