-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1 AND deleted_at IS NULL;

-- name: UpsertUserByEmail :one
-- Creates the user on first login; returns the existing row otherwise.
INSERT INTO users (id, email)
VALUES ($1, $2)
ON CONFLICT (email) WHERE deleted_at IS NULL
DO UPDATE SET updated_at = users.updated_at
RETURNING *;

-- name: ListUserRoles :many
SELECT role FROM user_roles
WHERE user_id = $1
ORDER BY role;

-- name: UpdateUserName :one
UPDATE users
SET name = @name, updated_at = now()
WHERE id = @user_id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteUser :exec
-- Scrubs PII immediately. The placeholder address keeps the lower-case check and frees the real
-- address for a future sign-up (users_email_key only covers live rows anyway).
UPDATE users
SET email = 'deleted+' || id::text || '@deleted.invalid',
    name = '',
    totp_secret = NULL,
    totp_confirmed_at = NULL,
    deleted_at = now(),
    updated_at = now()
WHERE id = @user_id AND deleted_at IS NULL;

-- name: GetUserTOTP :one
SELECT id, email, totp_secret, totp_confirmed_at, totp_last_step
FROM users
WHERE id = @user_id AND deleted_at IS NULL;

-- name: SetUserTOTPSecret :execrows
-- Starts (or restarts) enrolment. Only role holders may enrol, and a confirmed secret is never
-- replaced here (an admin reset clears it first).
UPDATE users u
SET totp_secret = @totp_secret, updated_at = now()
WHERE u.id = @user_id
  AND u.deleted_at IS NULL
  AND u.totp_confirmed_at IS NULL
  AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id);

-- name: ConfirmUserTOTPStep :execrows
-- Records an accepted code. The step must be newer than the last one, so a code can't be replayed,
-- even by two concurrent requests (the row lock serialises them and the second sees the new step).
-- expected_secret must equal the secret that was actually verified: if a new enrolment
-- (SetUserTOTPSecret) replaced totp_secret between verify and confirm, this no-ops instead of
-- confirming a different, unverified secret.
UPDATE users
SET totp_last_step = @step,
    totp_confirmed_at = coalesce(totp_confirmed_at, now()),
    updated_at = now()
WHERE id = @user_id
  AND deleted_at IS NULL
  AND totp_secret IS NOT NULL
  AND totp_secret = @expected_secret::text
  AND totp_last_step < @step;

-- name: ResetUserTOTP :execrows
-- Also drops the MFA step-up on the user's sessions. Rows affected counts users only.
WITH cleared AS (
    UPDATE sessions SET mfa_verified_at = NULL
    WHERE user_id = @user_id::uuid AND mfa_verified_at IS NOT NULL
)
UPDATE users u
SET totp_secret = NULL, totp_confirmed_at = NULL, updated_at = now()
WHERE u.id = @user_id::uuid AND u.deleted_at IS NULL;

-- name: SearchUsersAdmin :many
-- Admin browse, newest first (users_created_idx). First page: cursor = (far future, max uuid).
-- Roles are aggregated only for the rows on the page.
WITH page AS (
    SELECT u.id, u.email, u.name, u.status, u.created_at
    FROM users u
    WHERE u.deleted_at IS NULL
      AND (u.created_at, u.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
    ORDER BY u.created_at DESC, u.id DESC
    LIMIT sqlc.arg(lim)::int
)
SELECT p.id, p.email, p.name, p.status, p.created_at,
       coalesce((SELECT array_agg(r.role ORDER BY r.role) FROM user_roles r WHERE r.user_id = p.id), '{}')::text[] AS roles
FROM page p
ORDER BY p.created_at DESC, p.id DESC;

-- name: SearchUsersAdminByStatus :many
-- As SearchUsersAdmin, one status only (users_status_created_idx).
WITH page AS (
    SELECT u.id, u.email, u.name, u.status, u.created_at
    FROM users u
    WHERE u.status = @status::text
      AND u.deleted_at IS NULL
      AND (u.created_at, u.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
    ORDER BY u.created_at DESC, u.id DESC
    LIMIT sqlc.arg(lim)::int
)
SELECT p.id, p.email, p.name, p.status, p.created_at,
       coalesce((SELECT array_agg(r.role ORDER BY r.role) FROM user_roles r WHERE r.user_id = p.id), '{}')::text[] AS roles
FROM page p
ORDER BY p.created_at DESC, p.id DESC;

-- name: SearchUsersAdminByEmail :many
-- Exact-address fast path (users_email_key). Returns at most one row.
SELECT u.id, u.email, u.name, u.status, u.created_at,
       coalesce((SELECT array_agg(r.role ORDER BY r.role) FROM user_roles r WHERE r.user_id = u.id), '{}')::text[] AS roles
FROM users u
WHERE u.email = @email AND u.deleted_at IS NULL
  AND (sqlc.narg(status)::text IS NULL OR u.status = sqlc.narg(status)::text);

-- name: SearchUsersAdminText :many
-- Prefix search over email and name (users_search_idx). prefix_query is built in Go from sanitised
-- tokens, e.g. 'ali:* & example:*'. Keyset as SearchUsersAdmin; status NULL = any (a filter on
-- the bitmap result, which the tsquery already narrows).
WITH page AS (
    SELECT u.id, u.email, u.name, u.status, u.created_at
    FROM users u
    WHERE to_tsvector('simple', u.email || ' ' || u.name) @@ to_tsquery('simple', @prefix_query)
      AND u.deleted_at IS NULL
      AND (sqlc.narg(status)::text IS NULL OR u.status = sqlc.narg(status)::text)
      AND (u.created_at, u.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
    ORDER BY u.created_at DESC, u.id DESC
    LIMIT sqlc.arg(lim)::int
)
SELECT p.id, p.email, p.name, p.status, p.created_at,
       coalesce((SELECT array_agg(r.role ORDER BY r.role) FROM user_roles r WHERE r.user_id = p.id), '{}')::text[] AS roles
FROM page p
ORDER BY p.created_at DESC, p.id DESC;

-- name: GetUserAdmin :one
-- Includes deleted (scrubbed) users so audit trails stay navigable.
SELECT u.id, u.email, u.name, u.status, u.totp_confirmed_at, u.created_at, u.updated_at, u.deleted_at,
       coalesce((SELECT array_agg(r.role ORDER BY r.role) FROM user_roles r WHERE r.user_id = u.id), '{}')::text[] AS roles,
       (SELECT count(*) FROM events e WHERE e.owner_id = u.id AND e.deleted_at IS NULL) AS live_events,
       (SELECT count(*) FROM sessions s WHERE s.user_id = u.id AND s.expires_at > now()) AS active_sessions
FROM users u
WHERE u.id = @user_id;

-- name: SetUserStatus :one
-- Returns the status before and after for the audit row. No row: user missing or deleted.
WITH prev AS (
    SELECT x.id, x.status FROM users x
    WHERE x.id = @target_id AND x.deleted_at IS NULL
    FOR NO KEY UPDATE
)
UPDATE users u
SET status = @status, updated_at = now()
FROM prev
WHERE u.id = prev.id
RETURNING prev.status AS old_status, u.status AS new_status;

-- name: GrantRole :execrows
-- Zero rows: the role was already held or the user is missing/deleted.
INSERT INTO user_roles (user_id, role)
SELECT u.id, @role::text
FROM users u
WHERE u.id = @user_id AND u.deleted_at IS NULL
ON CONFLICT (user_id, role) DO NOTHING;

-- name: RevokeRole :execrows
DELETE FROM user_roles WHERE user_id = @user_id AND role = @role;

-- name: CountSuperAdmins :one
SELECT count(*)
FROM user_roles r
JOIN users u ON u.id = r.user_id
WHERE r.role = 'super_admin' AND u.deleted_at IS NULL AND u.status = 'active';

-- name: LockRoles :exec
-- Serialises role changes so the "last super_admin" check can't race. Transaction-scoped.
SELECT pg_advisory_xact_lock(hashtextextended('syt:user_roles', 0));
