-- S-09：非破坏性 remote 隔离（GC 首版只做可恢复隔离，绝不物理删除）。
-- tombstone = 隔离标记：幂等（重复运行不重复隔离）、可恢复（restored_at）。
-- +goose Up
-- +goose StatementBegin
CREATE TABLE gc_tombstones (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    song_id INTEGER NOT NULL UNIQUE,
    ref_digest TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL,
    quarantined_at INTEGER NOT NULL,
    restored_at INTEGER
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS gc_tombstones;
-- +goose StatementEnd
