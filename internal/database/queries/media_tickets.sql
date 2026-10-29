-- name: InsertMediaTicket :exec
INSERT INTO media_tickets
    (token_hash, lease_id, ref_digest, representation, session, epoch, entry, target, subject, issued_at, expires_at, absolute_expires_at, revoked)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0);

-- name: GetMediaTicketByHash :one
SELECT id, token_hash, lease_id, ref_digest, representation, session, epoch, entry, target, subject, issued_at, expires_at, absolute_expires_at, revoked
FROM media_tickets
WHERE token_hash = ?;

-- name: RevokeMediaTicketByHash :exec
UPDATE media_tickets SET revoked = 1 WHERE token_hash = ?;

-- name: RevokeMediaTicketsByLease :exec
UPDATE media_tickets SET revoked = 1 WHERE lease_id = ?;

-- name: RevokeMediaTicketsByEntry :exec
UPDATE media_tickets SET revoked = 1
WHERE session = ? AND entry = ? AND epoch = ?;

-- name: RenewMediaTicket :exec
UPDATE media_tickets SET expires_at = ? WHERE token_hash = ? AND revoked = 0;

-- name: ListActiveMediaTicketsByLease :many
SELECT id, token_hash, lease_id, ref_digest, representation, session, epoch, entry, target, subject, issued_at, expires_at, absolute_expires_at, revoked
FROM media_tickets
WHERE lease_id = ? AND revoked = 0 AND expires_at > ?;
