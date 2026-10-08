-- Files are written before the row (see internal/media) and deleted before the row on every delete
-- path except detached-media cleanup, which is row-first on purpose: DeleteDetachedMedia re-checks
-- detached_at atomically, and files of a row that was re-attached meanwhile must not be removed.
-- Orphaned files left by a failed step are swept by media reconcile.

-- name: CreateHostMedia :one
-- Owner or editor, after LockEventForEditor and the quota check in the same tx.
INSERT INTO media (id, owner_id, event_id, storage_key, content_type, size_bytes, width, height, uploaded_by, moderation_status)
SELECT @id::uuid, @user_id::uuid, @event_id::uuid, @storage_key::text, 'image/jpeg',
       @size_bytes::bigint, @width::int, @height::int, 'host', 'approved'
WHERE event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING id, event_id, width, height, size_bytes, uploaded_by, moderation_status, created_at;

-- name: CreateAnonDraftMedia :one
-- Unclaimed draft owned by the draft cookie; owner_id is filled in when the draft is claimed.
INSERT INTO media (id, owner_id, event_id, storage_key, content_type, size_bytes, width, height, uploaded_by, moderation_status)
SELECT @id::uuid, NULL, e.id, @storage_key::text, 'image/jpeg',
       @size_bytes::bigint, @width::int, @height::int, 'host', 'approved'
FROM events e
WHERE e.id = @event_id::uuid
  AND e.deleted_at IS NULL
  AND e.owner_id IS NULL
  AND EXISTS (SELECT 1 FROM anon_drafts a
              WHERE a.event_id = e.id AND a.cookie_hash = @cookie_hash AND a.expires_at > now())
RETURNING id, event_id, width, height, size_bytes, uploaded_by, moderation_status, created_at;

-- name: CreateGuestMedia :one
-- After LockEventForRSVP and the photo-limit check in the same tx. Starts pending moderation.
INSERT INTO media (id, owner_id, event_id, storage_key, content_type, size_bytes, width, height, uploaded_by, guest_id, moderation_status)
SELECT @id::uuid, NULL, e.id, @storage_key::text, 'image/jpeg',
       @size_bytes::bigint, @width::int, @height::int, 'guest', sqlc.narg(guest_id)::uuid, 'pending'
FROM events e
WHERE e.id = @event_id::uuid AND e.status = 'published' AND e.deleted_at IS NULL
RETURNING id, moderation_status;

-- name: MediaUsage :one
-- Quota check (files and bytes) per event and uploader kind; rejected rows don't count.
SELECT count(*) AS files, coalesce(sum(size_bytes), 0)::bigint AS bytes
FROM media
WHERE event_id = @event_id AND uploaded_by = @uploaded_by AND moderation_status <> 'rejected';

-- name: MediaStorageUsage :one
-- Upload quota (run after LockMediaStorageQuota). owner_bytes: live events of one owner, rejected
-- rows and pending guest photos excluded (soft-deleted events free the owner's quota at once; a
-- guest photo is charged to the owner only once the host approves it). total_bytes: every event,
-- soft-deleted ones included until purge, pending guest photos too, because their files still
-- occupy the bucket. pending_guest_bytes: pending guest photos of one event, bounded separately.
-- owner_pending_guest_bytes: pending guest photos across all live events of one owner, bounded
-- by a per-owner allowance so many events cannot each park a full per-event allowance.
-- NULL owner (anonymous draft): owner_bytes and owner_pending_guest_bytes are 0 and the caller
-- skips the per-user checks. NULL event: pending_guest_bytes is 0.
SELECT (SELECT coalesce(sum(m.size_bytes), 0)
        FROM media m JOIN events e ON e.id = m.event_id
        WHERE e.owner_id = sqlc.narg(owner_id)::uuid
          AND e.deleted_at IS NULL
          AND m.moderation_status <> 'rejected'
          AND NOT (m.uploaded_by = 'guest' AND m.moderation_status = 'pending'))::bigint AS owner_bytes,
       (SELECT coalesce(sum(size_bytes), 0)
        FROM media
        WHERE moderation_status <> 'rejected')::bigint AS total_bytes,
       (SELECT coalesce(sum(size_bytes), 0)
        FROM media
        WHERE event_id = sqlc.narg(event_id)::uuid
          AND uploaded_by = 'guest'
          AND moderation_status = 'pending')::bigint AS pending_guest_bytes,
       (SELECT coalesce(sum(m.size_bytes), 0)
        FROM media m JOIN events e ON e.id = m.event_id
        WHERE e.owner_id = sqlc.narg(owner_id)::uuid
          AND e.deleted_at IS NULL
          AND m.uploaded_by = 'guest'
          AND m.moderation_status = 'pending')::bigint AS owner_pending_guest_bytes;

-- name: MediaOwnerUsage :one
-- Approval check (run after LockMediaStorageQuota): same owner_bytes rule as MediaStorageUsage,
-- without the global sum, so the global lock is not held across a bucket-wide scan.
SELECT coalesce(sum(m.size_bytes), 0)::bigint AS owner_bytes
FROM media m JOIN events e ON e.id = m.event_id
WHERE e.owner_id = @owner_id::uuid
  AND e.deleted_at IS NULL
  AND m.moderation_status <> 'rejected'
  AND NOT (m.uploaded_by = 'guest' AND m.moderation_status = 'pending');

-- name: LockMediaStorageQuota :exec
-- Serialises the quota check and insert across upload transactions. Take it last, after the event
-- row lock, so lock order is always event -> quota and cannot deadlock.
SELECT pg_advisory_xact_lock(hashtextextended('syt:media_storage_quota', 0));

-- name: GetServableMedia :one
-- Public /media serving gate: approved media of an event that is neither soft-deleted nor taken
-- down. No row = not servable, whatever the reason (the caller answers every case with the same 404).
SELECT m.id
FROM media m
JOIN events e ON e.id = m.event_id
WHERE m.id = @media_id
  AND m.event_id = @event_id
  AND m.moderation_status = 'approved'
  AND e.deleted_at IS NULL
  AND e.status <> 'taken_down';

-- name: LiveMediaIDs :many
-- Reconcile: which of these media ids still have a non-rejected row; files of any other id are
-- orphans. Callers pass at most one listing page (1000 ids).
SELECT id
FROM media
WHERE id = ANY(@ids::uuid[]) AND moderation_status <> 'rejected';

-- name: ListEventMedia :many
-- Any member. Newest first; uploaded_by / status NULL = all. The page is cut before the guest join.
SELECT m.id, m.width, m.height, m.size_bytes, m.uploaded_by, m.moderation_status, m.created_at,
       g.name AS guest_name
FROM (
    SELECT x.id, x.guest_id, x.width, x.height, x.size_bytes, x.uploaded_by, x.moderation_status, x.created_at
    FROM media x
    WHERE x.event_id = @event_id::uuid
      AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
      AND (sqlc.narg(uploaded_by)::text IS NULL OR x.uploaded_by = sqlc.narg(uploaded_by)::text)
      AND (sqlc.narg(status)::text IS NULL OR x.moderation_status = sqlc.narg(status)::text)
      AND (x.created_at, x.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
    ORDER BY x.created_at DESC, x.id DESC
    LIMIT sqlc.arg(lim)::int
) m
LEFT JOIN guests g ON g.id = m.guest_id
ORDER BY m.created_at DESC, m.id DESC;

-- name: GetEventMedia :one
-- Any member.
SELECT m.id, m.event_id, m.width, m.height, m.size_bytes, m.uploaded_by, m.moderation_status, m.created_at,
       g.name AS guest_name
FROM media m
LEFT JOIN guests g ON g.id = m.guest_id
WHERE m.id = @media_id::uuid
  AND m.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL;

-- name: ListRejectedMediaIDsByEvent :many
-- MediaVisibilityWorker: rejected media of one event, whose files must be gone. Unpaginated: an
-- event has at most ~2,150 media rows (per-event caps) and media_event_created_idx covers the lookup.
SELECT id
FROM media
WHERE event_id = @event_id::uuid AND moderation_status = 'rejected';

-- name: SetGuestMediaModeration :one
-- Owner or editor approves/rejects a guest photo; returns the previous status so the caller knows
-- whether files need deleting. Approve only from pending; reject blocked only when already
-- rejected (its files are already deleted, so re-rejecting would re-issue a delete for files that
-- don't exist). Either transition is blocked once the event is taken down.
WITH prev AS (
    SELECT x.id, x.moderation_status FROM media x
    WHERE x.id = @media_id::uuid
      AND x.event_id = @event_id::uuid
      AND x.uploaded_by = 'guest'
      AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
      AND ((@status::text = 'approved' AND x.moderation_status = 'pending')
           OR (@status::text = 'rejected' AND x.moderation_status <> 'rejected'))
      AND EXISTS (SELECT 1 FROM events e WHERE e.id = @event_id::uuid AND e.status <> 'taken_down')
    FOR UPDATE
)
UPDATE media m
SET moderation_status = @status::text
FROM prev
WHERE m.id = prev.id
RETURNING m.id, m.event_id, m.width, m.height, m.size_bytes, m.uploaded_by, m.moderation_status, m.created_at,
          prev.moderation_status AS old_status;

-- name: DeleteEventMedia :one
-- Owner or editor. The caller has already checked the media isn't referenced by content.
DELETE FROM media m
WHERE m.id = @media_id::uuid
  AND m.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING m.id, m.uploaded_by, m.moderation_status;

-- name: ApprovedMediaByIDs :many
-- Content save: which referenced media exist, belong to this event and are approved.
SELECT id, width, height
FROM media
WHERE event_id = @event_id AND id = ANY(@media_ids::uuid[]) AND moderation_status = 'approved';

-- name: SyncMediaDetached :exec
-- After a content save: referenced host media are attached, the rest get a detached_at (kept if
-- already set). Only rows whose state actually changes are written.
UPDATE media
SET detached_at = CASE WHEN id = ANY(@referenced::uuid[]) THEN NULL ELSE now() END
WHERE event_id = @event_id
  AND uploaded_by = 'host'
  AND (id = ANY(@referenced::uuid[])) = (detached_at IS NOT NULL);

-- name: ListApprovedGuestPhotos :many
-- Public photo wall, newest first (media_event_approved_guest_idx).
SELECT id, width, height, created_at
FROM media
WHERE event_id = @event_id
  AND uploaded_by = 'guest'
  AND moderation_status = 'approved'
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;

-- name: DeleteDetachedMedia :many
-- Cleanup: host media detached for 7+ days, in batches. The outer predicate is re-checked on
-- locked rows, so media re-attached concurrently is kept.
DELETE FROM media
WHERE id IN (
    SELECT x.id FROM media x
    WHERE x.detached_at < now() - interval '7 days'
    LIMIT 500)
  AND detached_at < now() - interval '7 days'
RETURNING event_id, id;

-- name: DeleteRejectedMedia :execrows
-- Cleanup: rejected rows (files already deleted) older than 7 days, in batches.
DELETE FROM media
WHERE id IN (
    SELECT x.id FROM media x
    WHERE x.moderation_status = 'rejected' AND x.created_at < now() - interval '7 days'
    LIMIT 500);

-- name: ListGuestMediaAdmin :many
-- Moderation queue: guest uploads with one status, newest first (media_guest_status_created_idx).
-- event_id NULL = all events.
SELECT m.id, m.event_id, m.width, m.height, m.size_bytes, m.moderation_status, m.created_at,
       e.slug AS event_slug, e.title AS event_title, g.name AS guest_name
FROM media m
JOIN events e ON e.id = m.event_id
LEFT JOIN guests g ON g.id = m.guest_id
WHERE m.uploaded_by = 'guest'
  AND m.moderation_status = @status::text
  AND (sqlc.narg(event_id)::uuid IS NULL OR m.event_id = sqlc.narg(event_id)::uuid)
  AND (m.created_at, m.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg(lim)::int;

-- name: GetMediaAdmin :one
SELECT m.id, m.event_id, m.width, m.height, m.size_bytes, m.uploaded_by, m.moderation_status, m.created_at,
       e.status AS event_status, e.deleted_at AS event_deleted_at
FROM media m
JOIN events e ON e.id = m.event_id
WHERE m.id = @media_id;

-- name: RejectMediaAdmin :one
-- Returns the previous status for the audit row. No row: missing or already rejected.
WITH prev AS (
    SELECT x.id, x.moderation_status FROM media x
    WHERE x.id = @media_id AND x.moderation_status <> 'rejected'
    FOR UPDATE
)
UPDATE media m
SET moderation_status = 'rejected'
FROM prev
WHERE m.id = prev.id
RETURNING m.id, m.event_id, m.uploaded_by, prev.moderation_status AS old_status, m.moderation_status AS new_status;
