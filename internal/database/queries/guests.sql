-- name: ListGuests :many
-- Any member. Oldest first (the order hosts entered them). pattern is NULL or an ILIKE pattern
-- built in Go with %, _ and \ escaped, e.g. '%ann%'. The RSVP join is at most one row per guest
-- (rsvps_event_guest_key).
-- The page is cut before the join so only its rows are joined.
SELECT g.id, g.name, g.email, g.phone, g.household_size, g.token_version, g.invited_at, g.created_at,
       r.attending AS rsvp_attending, r.count AS rsvp_count, r.updated_at AS rsvp_updated_at
FROM (
    SELECT x.id, x.event_id, x.name, x.email, x.phone, x.household_size, x.token_version, x.invited_at, x.created_at
    FROM guests x
    WHERE x.event_id = @event_id::uuid
      AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
      AND (sqlc.narg(pattern)::text IS NULL
           OR x.name ILIKE sqlc.narg(pattern)::text
           OR x.email ILIKE sqlc.narg(pattern)::text)
      AND (x.created_at, x.id) > (@cursor_created_at::timestamptz, @cursor_id::uuid)
    ORDER BY x.created_at, x.id
    LIMIT sqlc.arg(lim)::int
) g
LEFT JOIN rsvps r ON r.event_id = g.event_id AND r.guest_id = g.id
ORDER BY g.created_at, g.id;

-- name: CountGuests :one
SELECT count(*) FROM guests WHERE event_id = @event_id;

-- name: CreateGuest :one
-- Owner or editor. A duplicate email raises 23505 on guests_event_email_key (guest_exists).
INSERT INTO guests (id, event_id, name, email, phone, household_size)
SELECT @id::uuid, @event_id::uuid, @name::text, sqlc.narg(email)::text, sqlc.narg(phone)::text, @household_size::int
WHERE event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING id, name, email, phone, household_size, token_version, invited_at, created_at;

-- name: InsertGuestsBatch :many
-- CSV import, after LockEventForEditor in the same tx. Arrays are parallel; an empty string in
-- emails/phones means none. Rows whose email is already invited (or repeated in the batch) are
-- skipped; the returned ids are the inserted rows.
INSERT INTO guests (id, event_id, name, email, phone, household_size)
SELECT b.id, @event_id::uuid, b.name, nullif(b.email, ''), nullif(b.phone, ''), b.household_size
FROM (SELECT unnest(@ids::uuid[]) AS id, unnest(@names::text[]) AS name, unnest(@emails::text[]) AS email,
             unnest(@phones::text[]) AS phone, unnest(@household_sizes::int[]) AS household_size) b
WHERE event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
ON CONFLICT (event_id, email) WHERE email IS NOT NULL DO NOTHING
RETURNING id;

-- name: UpdateGuest :one
-- Owner or editor. NULL name/household_size leave them unchanged; email and phone change only when
-- their set_ flag is true (so they can be cleared with NULL).
UPDATE guests g
SET name = coalesce(sqlc.narg(name)::text, g.name),
    household_size = coalesce(sqlc.narg(household_size)::int, g.household_size),
    email = CASE WHEN @set_email::bool THEN sqlc.narg(email)::text ELSE g.email END,
    phone = CASE WHEN @set_phone::bool THEN sqlc.narg(phone)::text ELSE g.phone END,
    updated_at = now()
WHERE g.id = @guest_id::uuid
  AND g.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING g.id, g.name, g.email, g.phone, g.household_size, g.token_version, g.invited_at, g.created_at;

-- name: DeleteGuest :execrows
-- Their RSVP cascades; their photos keep the row with guest_id NULL.
DELETE FROM guests g
WHERE g.id = @guest_id::uuid
  AND g.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor');

-- name: RotateGuestToken :one
-- Invalidates the guest's current invite link.
UPDATE guests g
SET token_version = g.token_version + 1, updated_at = now()
WHERE g.id = @guest_id::uuid
  AND g.event_id = @event_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING g.id, g.name, g.email, g.phone, g.household_size, g.token_version, g.invited_at, g.created_at;

-- name: MarkGuestsInvited :many
-- Owner or editor. Stamps up to 500 guests with an email, either the given ids or every guest not
-- yet invited; the caller enqueues one send_invite job per returned id in the same tx. Explicit ids
-- are re-sent at most once per 24 hours, so repeated calls can't be used to spam an inbox.
UPDATE guests g
SET invited_at = now(), updated_at = now()
WHERE g.id IN (
        SELECT x.id FROM guests x
        WHERE x.event_id = @event_id::uuid
          AND x.email IS NOT NULL
          AND ((x.id = ANY(@guest_ids::uuid[])
                AND (x.invited_at IS NULL OR x.invited_at < now() - interval '24 hours'))
               OR (@all_uninvited::bool AND x.invited_at IS NULL))
        ORDER BY x.created_at, x.id
        LIMIT 500)
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING g.id;

-- name: GetGuestForToken :one
-- Public; the caller has already verified the token MAC against token_version.
SELECT g.id, g.event_id, g.name, g.household_size, g.token_version, e.slug AS event_slug, e.status AS event_status
FROM guests g
JOIN events e ON e.id = g.event_id
WHERE g.id = @guest_id AND e.deleted_at IS NULL;

-- name: GetGuestForInviteEmail :one
-- send_invite job. The job no-ops unless the event is published and the guest still has an email.
SELECT g.id, g.name, g.email, g.token_version,
       e.id AS event_id, e.slug AS event_slug, e.title AS event_title, e.status AS event_status,
       e.starts_at AS event_starts_at, u.name AS owner_name
FROM guests g
JOIN events e ON e.id = g.event_id
LEFT JOIN users u ON u.id = e.owner_id AND u.deleted_at IS NULL
WHERE g.id = @guest_id AND e.deleted_at IS NULL;
