-- name: CreateSession :one
INSERT INTO sessions (id, user_id, token_hash, expires_at, ip, user_agent)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetActiveSession :one
-- Only sessions that are unexpired and belong to an active, undeleted user.
SELECT s.id, s.user_id, s.expires_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1
  AND s.expires_at > now()
  AND u.status = 'active'
  AND u.deleted_at IS NULL;

-- name: DeleteSessionByTokenHash :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: DeleteExpiredSessions :execrows
-- Batched so one run never holds many row locks or bloats WAL; the job loops while rows > 0.
DELETE FROM sessions
WHERE id IN (SELECT id FROM sessions WHERE expires_at <= now() LIMIT 5000);

-- name: TouchSession :exec
-- Writes at most once per 5 minutes per session.
UPDATE sessions
SET last_seen_at = now()
WHERE id = @id AND last_seen_at < now() - interval '5 minutes';

-- name: ListUserSessions :many
SELECT id, created_at, last_seen_at, user_agent, ip
FROM sessions
WHERE user_id = @user_id AND expires_at > now()
ORDER BY last_seen_at DESC, id DESC
LIMIT 50;

-- name: DeleteUserSession :one
-- Returns the hash so the caller can drop the Valkey mirror entry.
DELETE FROM sessions
WHERE id = @id AND user_id = @user_id
RETURNING token_hash;

-- name: DeleteOtherUserSessions :many
DELETE FROM sessions
WHERE user_id = @user_id AND id <> @current_id
RETURNING token_hash;

-- name: DeleteAllUserSessions :many
DELETE FROM sessions
WHERE user_id = @user_id
RETURNING token_hash;

-- name: SetSessionMFAVerified :exec
UPDATE sessions
SET mfa_verified_at = now()
WHERE id = @session_id AND user_id = @user_id;

-- name: ClearUserMFAVerified :exec
UPDATE sessions
SET mfa_verified_at = NULL
WHERE user_id = @user_id AND mfa_verified_at IS NOT NULL;

-- name: GetAdminContext :one
-- Read on every admin request. No row: session gone/expired or the user is not active.
SELECT u.id AS user_id, u.email, u.totp_confirmed_at, s.mfa_verified_at,
       coalesce((SELECT array_agg(r.role ORDER BY r.role) FROM user_roles r WHERE r.user_id = u.id), '{}')::text[] AS roles
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = @session_id
  AND s.user_id = @user_id
  AND s.expires_at > now()
  AND u.status = 'active'
  AND u.deleted_at IS NULL;
