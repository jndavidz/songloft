-- name: InsertGCTombstone :exec
INSERT INTO gc_tombstones (song_id, ref_digest, reason, quarantined_at, restored_at)
VALUES (?, ?, ?, ?, NULL)
ON CONFLICT(song_id) DO NOTHING;

-- name: ListGCTombstonedSongIDs :many
SELECT song_id FROM gc_tombstones WHERE restored_at IS NULL;

-- name: RestoreGCTombstone :exec
UPDATE gc_tombstones SET restored_at = ? WHERE song_id = ? AND restored_at IS NULL;

-- name: CountGCTombstones :one
SELECT CAST(COUNT(*) AS INTEGER) AS n FROM gc_tombstones WHERE restored_at IS NULL;
