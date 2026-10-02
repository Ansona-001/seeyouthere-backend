-- Public RSVP writes run after LockEventForRSVP in the same tx; the caller has already resolved
-- the guest or RSVP identity from a verified token.

-- name: UpsertGuestRSVP :one
-- One RSVP per invited guest. inserted is true for a new row.
INSERT INTO rsvps (id, event_id, guest_id, name, email, attending, count, answers)
VALUES (@id, @event_id, @guest_id::uuid, @name, sqlc.narg(email), @attending, @count, @answers)
ON CONFLICT (event_id, guest_id) WHERE guest_id IS NOT NULL DO UPDATE
SET name = EXCLUDED.name,
    email = EXCLUDED.email,
    attending = EXCLUDED.attending,
    count = EXCLUDED.count,
    answers = EXCLUDED.answers,
    updated_at = now()
RETURNING id, event_id, guest_id, name, email, attending, count, answers, created_at, updated_at,
          edit_token_version, (xmax = 0)::bool AS inserted;

-- name: CreateOpenRSVP :one
INSERT INTO rsvps (id, event_id, name, email, attending, count, answers)
VALUES (@id, @event_id, @name, sqlc.narg(email), @attending, @count, @answers)
RETURNING id, event_id, guest_id, name, email, attending, count, answers, edit_token_version, created_at, updated_at;

-- name: UpdateOpenRSVP :one
-- edit_token_version is the one the caller verified the token against; a concurrent rotation
-- makes this update no row.
UPDATE rsvps
SET name = @name, email = sqlc.narg(email), attending = @attending, count = @count, answers = @answers,
    updated_at = now()
WHERE id = @id AND event_id = @event_id AND guest_id IS NULL AND edit_token_version = @edit_token_version
RETURNING id, event_id, guest_id, name, email, attending, count, answers, edit_token_version, created_at, updated_at;

-- name: GetRSVPForEvent :one
SELECT id, event_id, guest_id, name, email, attending, count, answers, edit_token_version, created_at, updated_at
FROM rsvps
WHERE id = @id AND event_id = @event_id;

-- name: GetGuestRSVP :one
SELECT id, event_id, guest_id, name, email, attending, count, answers, created_at, updated_at
FROM rsvps
WHERE event_id = @event_id AND guest_id = @guest_id::uuid;

-- name: GetRSVPForToken :one
-- Public. Load by the token's RSVP id, then verify the MAC against (id, edit_token_version).
SELECT r.id, r.event_id, r.guest_id, r.edit_token_version, e.slug AS event_slug, e.status AS event_status
FROM rsvps r
JOIN events e ON e.id = r.event_id
WHERE r.id = @id AND e.deleted_at IS NULL;

-- name: SumYesHeads :one
-- Capacity check under the event lock. exclude_id leaves out the RSVP being edited.
SELECT coalesce(sum(count), 0)::int AS heads
FROM rsvps
WHERE event_id = @event_id AND attending = 'yes' AND id IS DISTINCT FROM sqlc.narg(exclude_id)::uuid;

-- name: CountRSVPs :one
SELECT count(*) FROM rsvps WHERE event_id = @event_id;

-- name: ListRSVPs :many
-- Any member. Newest first; attending NULL = all.
SELECT r.id, r.guest_id, r.name, r.email, r.attending, r.count, r.answers, r.created_at, r.updated_at
FROM rsvps r
WHERE r.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
  AND (sqlc.narg(attending)::text IS NULL OR r.attending = sqlc.narg(attending)::text)
  AND (r.created_at, r.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY r.created_at DESC, r.id DESC
LIMIT sqlc.arg(lim)::int;

-- name: RSVPSummary :one
-- Any member. No row when the caller has no access (the GROUP BY on the access CTE guarantees it).
WITH ev AS (
    SELECT x.id FROM events x
    WHERE x.id = @event_id::uuid AND event_role(x.id, @user_id::uuid) IS NOT NULL
)
SELECT count(r.id) FILTER (WHERE r.attending = 'yes') AS yes,
       count(r.id) FILTER (WHERE r.attending = 'no') AS no,
       count(r.id) FILTER (WHERE r.attending = 'maybe') AS maybe,
       coalesce(sum(r.count) FILTER (WHERE r.attending = 'yes'), 0)::bigint AS yes_heads,
       coalesce(sum(r.count) FILTER (WHERE r.attending = 'maybe'), 0)::bigint AS maybe_heads,
       count(r.id) AS total,
       count(r.guest_id) AS guests_responded,
       (SELECT count(*) FROM guests g WHERE g.event_id = ev.id) AS guests_total
FROM ev
LEFT JOIN rsvps r ON r.event_id = ev.id
GROUP BY ev.id;

-- name: ExportRSVPsBatch :many
-- CSV export, oldest first, called in a loop with the last row as the cursor (lim ≤ 500).
SELECT r.id, r.guest_id, r.name, r.email, r.attending, r.count, r.answers, r.created_at, r.updated_at
FROM rsvps r
WHERE r.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
  AND (r.created_at, r.id) > (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY r.created_at, r.id
LIMIT sqlc.arg(lim)::int;

-- name: RotateRSVPEditToken :one
-- Owner or editor. Invalidates every edit link issued for the RSVP; the caller mints a new one from
-- the returned version.
UPDATE rsvps r
SET edit_token_version = r.edit_token_version + 1 -- updated_at untouched: not an answer change for the digest
WHERE r.id = @id::uuid
  AND r.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING r.id, r.edit_token_version;

-- name: DeleteRSVP :execrows
DELETE FROM rsvps r
WHERE r.id = @id::uuid
  AND r.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor');

-- name: ListRSVPsUpdatedSince :many
-- Digest job: RSVPs changed in (since, until], newest first, at most 50, plus the total in range.
-- since NULL = from the beginning.
SELECT r.id, r.name, r.attending, r.count, r.created_at, r.updated_at, count(*) OVER () AS total
FROM rsvps r
WHERE r.event_id = @event_id
  AND r.updated_at > coalesce(sqlc.narg(since)::timestamptz, '-infinity')
  AND r.updated_at <= @until::timestamptz
ORDER BY r.updated_at DESC, r.id DESC
LIMIT 50;

-- name: GetRSVPForConfirmation :one
-- send_rsvp_confirmation job. guest_token_version is set for guest RSVPs (the link is the invite link).
SELECT r.id, r.name, r.email, r.attending, r.count, r.guest_id, r.edit_token_version,
       g.token_version AS guest_token_version,
       e.id AS event_id, e.slug AS event_slug, e.title AS event_title, e.status AS event_status,
       e.starts_at AS event_starts_at, e.content AS event_content
FROM rsvps r
JOIN events e ON e.id = r.event_id
LEFT JOIN guests g ON g.id = r.guest_id
WHERE r.id = @id AND e.deleted_at IS NULL;
