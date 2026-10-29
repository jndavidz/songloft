-- name: InsertManagedPrepareOp :exec
INSERT INTO managed_prepare_ops
    (issuer, session, epoch, entry, ref_digest, state, error, resolved_source, fallback_reason, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateManagedPrepareOpState :exec
UPDATE managed_prepare_ops
SET state = ?, error = ?, resolved_source = ?, fallback_reason = ?, updated_at = ?
WHERE issuer = ? AND session = ? AND epoch = ? AND entry = ?;

-- name: ListActiveManagedPrepareOps :many
SELECT id, issuer, session, epoch, entry, ref_digest, state, error, resolved_source, fallback_reason, created_at, updated_at
FROM managed_prepare_ops
WHERE state = 'preparing';

-- name: ListManagedPrepareOpsByEntry :many
SELECT id, issuer, session, epoch, entry, ref_digest, state, error, resolved_source, fallback_reason, created_at, updated_at
FROM managed_prepare_ops
WHERE session = ? AND entry = ?
ORDER BY epoch DESC
LIMIT 50;
