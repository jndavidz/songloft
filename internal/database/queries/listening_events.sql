-- name: InsertListeningEvent :one
INSERT INTO listening_events
    (idempotency_key, type, ref_digest, entry, lease_id, session, payload, created_at, acknowledged)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
ON CONFLICT(idempotency_key) DO NOTHING
RETURNING seq, idempotency_key, type, ref_digest, entry, lease_id, session, payload, created_at, acknowledged;

-- name: GetListeningEventSeq :one
SELECT seq FROM listening_events WHERE idempotency_key = ?;

-- name: ListPendingListeningEvents :many
SELECT seq, idempotency_key, type, ref_digest, entry, lease_id, session, payload, created_at, acknowledged
FROM listening_events
WHERE acknowledged = 0 AND seq > ?
ORDER BY seq
LIMIT ?;

-- name: AckListeningEventsUpTo :exec
UPDATE listening_events SET acknowledged = 1 WHERE seq <= ?;

-- name: MinAckedListeningEventSeq :one
SELECT COALESCE(MAX(seq), 0) - ? FROM listening_events WHERE acknowledged = 1;

-- name: MaxListeningEventSeq :one
SELECT COALESCE(CAST(MAX(seq) AS INTEGER), 0) FROM listening_events;

-- name: DeleteListeningEventsBelow :exec
DELETE FROM listening_events WHERE acknowledged = 1 AND seq <= ?;
