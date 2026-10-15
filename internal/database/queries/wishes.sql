-- name: CreateWish :one
-- Guest submission, published immediately (status 'approved'; the host deletes or hides what they
-- don't want). Returns no row once the event holds max_per_event wishes of any status (the host
-- frees room by deleting). The count is not race-free on its own: the caller holds the event row
-- lock (LockEventForRSVP) in the same tx, as for guest media. The inner LIMIT keeps the count at
-- most max_per_event index entries however many rows exist. Event visibility (published, not
-- deleted) and the wishes block being enabled are the caller's checks.
INSERT INTO event_wishes (id, event_id, author, message, status)
SELECT @id::uuid, @event_id::uuid, @author::text, @message::text, 'approved'
WHERE (SELECT count(*) FROM (SELECT 1 FROM event_wishes w WHERE w.event_id = @event_id::uuid
                            LIMIT sqlc.arg(max_per_event)::int) c) < sqlc.arg(max_per_event)::int
RETURNING id;

-- name: ListApprovedWishes :many
-- Public wall, newest first (event_wishes_event_status_created_idx). The caller bounds lim and
-- checks the event is publicly visible.
SELECT id, author, message
FROM event_wishes
WHERE event_id = @event_id::uuid AND status = 'approved'
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;

-- name: ListEventWishes :many
-- Any member, one status at a time, newest first. First page: pass a far-future cursor_created_at
-- and the max uuid as cursor_id.
SELECT id, author, message, status, created_at, updated_at
FROM event_wishes
WHERE event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
  AND status = @status::text
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;

-- name: SetWishStatus :one
-- Owner or editor moderates a wish (pending / approved / hidden). No row: unknown wish, not
-- permitted, or the event is taken down.
UPDATE event_wishes w
SET status = @status::text, updated_at = now()
WHERE w.id = @wish_id::uuid
  AND w.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
  AND EXISTS (SELECT 1 FROM events e WHERE e.id = @event_id::uuid AND e.status <> 'taken_down')
RETURNING w.id, w.author, w.message, w.status, w.created_at, w.updated_at;

-- name: DeleteWish :execrows
-- Owner or editor. 0 rows: unknown wish, not permitted, or the event is taken down.
DELETE FROM event_wishes w
WHERE w.id = @wish_id::uuid
  AND w.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
  AND EXISTS (SELECT 1 FROM events e WHERE e.id = @event_id::uuid AND e.status <> 'taken_down');

-- name: DeleteWishesByStatus :execrows
-- Owner or editor clears every wish in one status. 0 rows: none in that status, not permitted, or
-- the event is taken down.
DELETE FROM event_wishes w
WHERE w.event_id = @event_id::uuid
  AND w.status = @status::text
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
  AND EXISTS (SELECT 1 FROM events e WHERE e.id = @event_id::uuid AND e.status <> 'taken_down');
