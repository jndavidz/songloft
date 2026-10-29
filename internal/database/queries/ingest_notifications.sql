-- name: InsertIngestNotification :one
INSERT INTO ingest_notifications
    (notif_id, file_path, identity, state, attempts, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, 0, ?, ?, ?)
ON CONFLICT(notif_id) DO NOTHING
RETURNING id, notif_id, file_path, identity, state, attempts, last_error, created_at, updated_at;

-- name: GetIngestNotificationState :one
SELECT state FROM ingest_notifications WHERE notif_id = ?;

-- name: ListPendingIngestNotifications :many
SELECT id, notif_id, file_path, identity, state, attempts, last_error, created_at, updated_at
FROM ingest_notifications
WHERE state = 'pending'
ORDER BY id
LIMIT ?;

-- name: UpdateIngestNotificationResult :exec
UPDATE ingest_notifications
SET state = ?, attempts = ?, last_error = ?, updated_at = ?
WHERE notif_id = ?;
