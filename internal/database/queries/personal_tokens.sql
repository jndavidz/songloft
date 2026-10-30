-- name: CreatePersonalToken :exec
INSERT INTO personal_tokens (id, name, token_hash, prefix, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: GetPersonalTokenByHash :one
SELECT id, name, prefix, created_at, last_used_at, revoked_at
FROM personal_tokens WHERE token_hash = ?;

-- name: ListPersonalTokens :many
SELECT id, name, prefix, created_at, last_used_at, revoked_at
FROM personal_tokens ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?;

-- name: CountPersonalTokens :one
SELECT COUNT(*) FROM personal_tokens;

-- name: RevokePersonalToken :execrows
UPDATE personal_tokens SET revoked_at = COALESCE(revoked_at, ?)
WHERE id = ?;

-- name: TouchPersonalToken :exec
UPDATE personal_tokens SET last_used_at = ?
WHERE id = ? AND revoked_at IS NULL
AND (last_used_at IS NULL OR last_used_at < ?);
