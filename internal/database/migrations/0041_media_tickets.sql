-- S-04：短期可撤销媒体票据与独立 lease。
-- 高熵 opaque token 只存 SHA-256 摘要；票据绑定 TrackRef/表示/会话/代次/entry/target
-- 与授权主体；续期延长授权但 URI 与表示不变且有绝对上限；stop／换代只撤销对应 lease。
-- +goose Up
-- +goose StatementBegin
CREATE TABLE media_tickets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL UNIQUE,
    lease_id TEXT NOT NULL,
    ref_digest TEXT NOT NULL,
    representation TEXT NOT NULL,
    session TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    entry TEXT NOT NULL,
    target TEXT NOT NULL,
    subject TEXT NOT NULL,
    issued_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    absolute_expires_at INTEGER NOT NULL,
    revoked INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_media_tickets_lease ON media_tickets (lease_id);
CREATE INDEX idx_media_tickets_entry ON media_tickets (session, epoch, entry);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS media_tickets;
-- +goose StatementEnd
