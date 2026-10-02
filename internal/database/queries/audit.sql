-- name: InsertAuditLog :exec
INSERT INTO audit_log (id, actor_id, action, target_type, target_id, before, after, ip)
VALUES (@id, sqlc.narg(actor_id), @action, @target_type, @target_id, sqlc.narg(before), sqlc.narg(after), sqlc.narg(ip));

-- name: ListAuditLog :many
-- Global view, newest first (audit_log_created_idx).
SELECT id, actor_id, action, target_type, target_id, before, after, ip, created_at
FROM audit_log
WHERE (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;

-- name: ListAuditByActor :many
-- Entries by one admin, newest first (audit_log_actor_idx).
SELECT id, actor_id, action, target_type, target_id, before, after, ip, created_at
FROM audit_log
WHERE actor_id = @actor_id::uuid
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;

-- name: ListAuditForTarget :many
-- Entries about one object, newest first (audit_log_target_idx). The user detail page passes lim 20.
SELECT id, actor_id, action, target_type, target_id, before, after, ip, created_at
FROM audit_log
WHERE target_type = @target_type
  AND target_id = @target_id
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;
