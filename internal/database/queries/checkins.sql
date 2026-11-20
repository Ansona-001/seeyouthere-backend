-- Door check-in for hosted events. Any member (owner, editor or viewer) may check guests in; access
-- is checked in each query's WHERE clause, and no row means "not found or no access" (404).
-- A guest's entry pass is a token derived from (guest id, token_version) under its own signing key
-- (token.PassToken), not their invite token; the caller verifies its MAC against token_version
-- before CheckInGuest, which re-checks the version so a pass rotated in between is refused.
-- A registration (open RSVP on a hosted event) has an RSVP pass token signed over (id,
-- edit_token_version), so rotating the edit link also rotates the pass; CheckInRSVP re-checks that
-- version the same way. Guest RSVPs have no pass of their own.

-- name: GetPassGuest :one
-- Scanned pass: the guest by id (from the token) for a member of their published hosted event.
-- The RSVP join is at most one row (rsvps_event_guest_key).
SELECT g.id, g.event_id, g.name, g.household_size, g.token_version,
       g.checked_in_at, g.checked_in_count, g.checked_in_by, u.name AS checked_in_by_name,
       e.title AS event_title,
       r.attending AS rsvp_attending, r.count AS rsvp_count
FROM guests g
JOIN events e ON e.id = g.event_id
LEFT JOIN rsvps r ON r.event_id = g.event_id AND r.guest_id = g.id
LEFT JOIN users u ON u.id = g.checked_in_by AND u.deleted_at IS NULL
WHERE g.id = @guest_id::uuid
  AND e.kind = 'hosted'
  AND e.status = 'published'
  AND e.deleted_at IS NULL
  AND event_role(g.event_id, @user_id::uuid) IS NOT NULL;

-- name: CheckInGuest :one
-- First admission of a scanned pass. No row when the guest is already checked in, the pass version
-- is stale, count is outside 1..household_size, the event is not a published hosted event, or the
-- caller has no access; the caller tells these apart with GetCheckIn. Concurrent scans of the same
-- pass serialise on the row lock and the loser re-checks checked_in_at, so exactly one succeeds.
WITH c AS (
    UPDATE guests g
    SET checked_in_at = now(), checked_in_count = @count::int, checked_in_by = @user_id::uuid, updated_at = now()
    WHERE g.id = @guest_id::uuid
      AND g.event_id = @event_id::uuid
      AND g.token_version = @token_version::int
      AND g.checked_in_at IS NULL
      AND @count::int BETWEEN 1 AND g.household_size
      AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
      AND EXISTS (SELECT 1 FROM events e
                  WHERE e.id = @event_id::uuid AND e.kind = 'hosted' AND e.status = 'published')
    RETURNING g.id, g.household_size, g.checked_in_at, g.checked_in_count, g.checked_in_by
)
SELECT c.id, c.household_size, c.checked_in_at::timestamptz AS checked_in_at, c.checked_in_count,
       c.checked_in_by, u.name AS checked_in_by_name
FROM c
LEFT JOIN users u ON u.id = c.checked_in_by AND u.deleted_at IS NULL;

-- name: GetCheckIn :one
-- Current check-in state of one guest of a hosted event, for any member (also explains a CheckInGuest
-- miss: compare token_version, checked_in_at, household_size and event_status).
SELECT g.id, g.name, g.household_size, g.token_version,
       g.checked_in_at, g.checked_in_count, g.checked_in_by, u.name AS checked_in_by_name,
       e.status AS event_status
FROM guests g
JOIN events e ON e.id = g.event_id
LEFT JOIN users u ON u.id = g.checked_in_by AND u.deleted_at IS NULL
WHERE g.id = @guest_id::uuid
  AND g.event_id = @event_id::uuid
  AND e.kind = 'hosted'
  AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL;

-- name: SetGuestCheckIn :one
-- Manual correction from the guest list: sets the admitted count absolutely; 0 undoes the check-in.
-- The first admission time and the member who made it are kept while the guest stays checked in.
-- Any member may admit (count > 0); only an owner or editor may undo (count = 0), since viewers are
-- door staff. No row when count is outside 0..household_size, the event is not hosted, or the caller
-- has no access (or, for an undo, is only a viewer).
WITH c AS (
    UPDATE guests g
    SET checked_in_at = CASE WHEN @count::int = 0 THEN NULL ELSE coalesce(g.checked_in_at, now()) END,
        checked_in_by = CASE WHEN @count::int = 0 THEN NULL
                             WHEN g.checked_in_at IS NULL THEN @user_id::uuid
                             ELSE g.checked_in_by END,
        checked_in_count = @count::int,
        updated_at = now()
    WHERE g.id = @guest_id::uuid
      AND g.event_id = @event_id::uuid
      AND @count::int BETWEEN 0 AND g.household_size
      AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
      AND (@count::int > 0 OR event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor'))
      AND EXISTS (SELECT 1 FROM events e WHERE e.id = @event_id::uuid AND e.kind = 'hosted')
    RETURNING g.id, g.name, g.household_size, g.checked_in_at, g.checked_in_count, g.checked_in_by
)
SELECT c.id, c.name, c.household_size, c.checked_in_at, c.checked_in_count, c.checked_in_by,
       u.name AS checked_in_by_name
FROM c
LEFT JOIN users u ON u.id = c.checked_in_by AND u.deleted_at IS NULL;

-- name: GetPassRSVP :one
-- Scanned registration pass: the RSVP by id (from the token) for a member of its published hosted
-- event.
SELECT r.id, r.event_id, r.name, r.count, r.attending, r.edit_token_version,
       r.checked_in_at, r.checked_in_count, r.checked_in_by, u.name AS checked_in_by_name,
       e.title AS event_title
FROM rsvps r
JOIN events e ON e.id = r.event_id
LEFT JOIN users u ON u.id = r.checked_in_by AND u.deleted_at IS NULL
WHERE r.id = @rsvp_id::uuid
  AND r.registration
  AND e.kind = 'hosted'
  AND e.status = 'published'
  AND e.deleted_at IS NULL
  AND event_role(r.event_id, @user_id::uuid) IS NOT NULL;

-- name: CheckInRSVP :one
-- First admission of a scanned registration pass. No row when the registration is already checked
-- in, the pass version is stale, the answer is not 'yes', count is outside 1..count, the event is not
-- a published hosted event, or the caller has no access; the caller tells these apart with
-- GetRSVPCheckIn. Concurrent scans serialise on the row lock and the loser re-checks checked_in_at.
-- updated_at is left alone: a check-in is not an answer change for the RSVP digest.
WITH c AS (
    UPDATE rsvps r
    SET checked_in_at = now(), checked_in_count = @count::int, checked_in_by = @user_id::uuid
    WHERE r.id = @rsvp_id::uuid
      AND r.event_id = @event_id::uuid
      AND r.registration
      AND r.edit_token_version = @edit_token_version::int
      AND r.attending = 'yes'
      AND r.checked_in_at IS NULL
      AND @count::int BETWEEN 1 AND r.count
      AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
      AND EXISTS (SELECT 1 FROM events e
                  WHERE e.id = @event_id::uuid AND e.kind = 'hosted' AND e.status = 'published')
    RETURNING r.id, r.count, r.checked_in_at, r.checked_in_count, r.checked_in_by
)
SELECT c.id, c.count, c.checked_in_at::timestamptz AS checked_in_at, c.checked_in_count,
       c.checked_in_by, u.name AS checked_in_by_name
FROM c
LEFT JOIN users u ON u.id = c.checked_in_by AND u.deleted_at IS NULL;

-- name: GetRSVPCheckIn :one
-- Current check-in state of one registration of a hosted event, for any member (also explains a
-- CheckInRSVP miss: compare edit_token_version, attending, checked_in_at, count and event_status).
SELECT r.id, r.name, r.count, r.attending, r.edit_token_version,
       r.checked_in_at, r.checked_in_count, r.checked_in_by, u.name AS checked_in_by_name,
       e.status AS event_status
FROM rsvps r
JOIN events e ON e.id = r.event_id
LEFT JOIN users u ON u.id = r.checked_in_by AND u.deleted_at IS NULL
WHERE r.id = @rsvp_id::uuid
  AND r.event_id = @event_id::uuid
  AND r.registration
  AND e.kind = 'hosted'
  AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL;

-- name: SetRSVPCheckIn :one
-- Manual correction from the registration list: sets the admitted count absolutely; 0 undoes the
-- check-in. The first admission time and the member who made it are kept while the registration
-- stays checked in. Any member may admit (count > 0, only on a 'yes' answer); only an owner or editor
-- may undo (count = 0), since viewers are door staff. No row when count is outside 0..count, the
-- answer is not 'yes' for an admission, the event is not hosted, or the caller has no access (or,
-- for an undo, is only a viewer). updated_at is left alone, as in CheckInRSVP.
WITH c AS (
    UPDATE rsvps r
    SET checked_in_at = CASE WHEN @count::int = 0 THEN NULL ELSE coalesce(r.checked_in_at, now()) END,
        checked_in_by = CASE WHEN @count::int = 0 THEN NULL
                             WHEN r.checked_in_at IS NULL THEN @user_id::uuid
                             ELSE r.checked_in_by END,
        checked_in_count = @count::int
    WHERE r.id = @rsvp_id::uuid
      AND r.event_id = @event_id::uuid
      AND r.registration
      AND @count::int BETWEEN 0 AND r.count
      AND (@count::int = 0 OR r.attending = 'yes')
      AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
      AND (@count::int > 0 OR event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor'))
      AND EXISTS (SELECT 1 FROM events e WHERE e.id = @event_id::uuid AND e.kind = 'hosted')
    RETURNING r.id, r.name, r.count, r.checked_in_at, r.checked_in_count, r.checked_in_by
)
SELECT c.id, c.name, c.count, c.checked_in_at, c.checked_in_count, c.checked_in_by,
       u.name AS checked_in_by_name
FROM c
LEFT JOIN users u ON u.id = c.checked_in_by AND u.deleted_at IS NULL;

-- name: ListRegistrations :many
-- Any member. Oldest first, keyset on rsvps_event_registration_created_idx. pattern is NULL or an
-- ILIKE pattern built in Go with %, _ and \ escaped. match_email is false for viewers, who never see contact details, so a search cannot be
-- used to probe which emails registered.
SELECT r.id, r.name, r.email, r.attending, r.count, r.checked_in_at, r.checked_in_count, r.created_at
FROM rsvps r
WHERE r.event_id = @event_id::uuid
  AND r.registration
  AND event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
  AND (sqlc.narg(pattern)::text IS NULL
       OR r.name ILIKE sqlc.narg(pattern)::text
       OR (@match_email::bool AND r.email ILIKE sqlc.narg(pattern)::text))
  AND (r.created_at, r.id) > (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY r.created_at, r.id
LIMIT sqlc.arg(lim)::int;

-- name: CheckInSummary :one
-- Any member of a hosted event. No row when the caller has no access (the cross join with the access
-- CTE guarantees it). Reads the event's guests (capped at 2000 in Go) with their RSVPs, and the
-- event's registrations (rsvps_event_registration_email_key).
-- Guests: invited_people is the sum of household sizes, the most that can be admitted on guest
-- passes; their expected heads come from 'yes' and 'maybe' RSVPs (both may turn up at the door; a
-- "no" or a missing RSVP adds nothing). Registrations count only with a 'yes' answer, the only one
-- that can be admitted, and add their heads to expected_people. checked_in_passes and
-- checked_in_people cover both kinds of pass.
WITH ev AS (
    SELECT x.id FROM events x
    WHERE x.id = @event_id::uuid AND x.kind = 'hosted' AND event_role(x.id, @user_id::uuid) IS NOT NULL
), g AS (
    SELECT count(*) AS passes,
           coalesce(sum(g.household_size), 0) AS invited_people,
           coalesce(sum(r.count) FILTER (WHERE r.attending IN ('yes', 'maybe')), 0) AS expected_people,
           count(g.checked_in_at) AS checked_in_passes,
           coalesce(sum(g.checked_in_count), 0) AS checked_in_people
    FROM ev
    JOIN guests g ON g.event_id = ev.id
    LEFT JOIN rsvps r ON r.event_id = g.event_id AND r.guest_id = g.id
), reg AS (
    SELECT count(*) FILTER (WHERE r.attending = 'yes') AS passes,
           coalesce(sum(r.count) FILTER (WHERE r.attending = 'yes'), 0) AS expected_people,
           count(r.checked_in_at) AS checked_in_passes,
           coalesce(sum(r.checked_in_count), 0) AS checked_in_people
    FROM ev
    JOIN rsvps r ON r.event_id = ev.id AND r.registration
)
SELECT g.passes::bigint AS guests,
       g.invited_people::bigint AS invited_people,
       reg.passes::bigint AS registrations,
       (g.expected_people + reg.expected_people)::bigint AS expected_people,
       (g.checked_in_passes + reg.checked_in_passes)::bigint AS checked_in_passes,
       (g.checked_in_people + reg.checked_in_people)::bigint AS checked_in_people
FROM ev, g, reg;
