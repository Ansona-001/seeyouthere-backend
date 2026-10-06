-- Event-scoped authorisation lives in the WHERE clause of each query (event_role, owner_id, or the
-- anonymous-draft cookie hash). No row means "not found or no access"; callers map it to 404.
-- Keyset cursors: first page of a DESC list passes (far future, max uuid); of an ASC list (zero time, nil uuid).

-- Template pins set by hosts must name a published version of a published, non-premium template.
-- Admin changes go through SetEventFlags instead.

-- name: CreateEvent :one
-- No row when the template pin is not selectable; the caller maps pgx.ErrNoRows to a validation error.
-- retention_from is the retention clock: greatest(ends_at, now()), so a past end starts a full
-- retention window now (greatest ignores NULL, hence the CASE).
INSERT INTO events (id, owner_id, occasion_slug, title, content, template_id, template_version, overrides,
                    starts_at, ends_at, retention_from)
SELECT @id::uuid, sqlc.narg(owner_id)::uuid, @occasion_slug::text, @title::text, @content::jsonb,
       t.id, tv.version, @overrides::jsonb, sqlc.narg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz,
       CASE WHEN sqlc.narg(ends_at)::timestamptz IS NOT NULL THEN greatest(sqlc.narg(ends_at)::timestamptz, now()) END
FROM templates t
JOIN template_versions tv ON tv.template_id = t.id
WHERE t.id = @template_id::uuid
  AND tv.version = @template_version::int
  AND tv.published_at IS NOT NULL
  AND t.status = 'published'
  AND NOT t.is_premium
RETURNING *;

-- name: CreateAnonDraft :exec
INSERT INTO anon_drafts (id, cookie_hash, event_id, expires_at)
VALUES (@id, @cookie_hash, @event_id, @expires_at);

-- name: CountAnonDraftsByCookie :one
SELECT count(*) FROM anon_drafts
WHERE cookie_hash = @cookie_hash AND expires_at > now();

-- name: CountLiveEventsByOwner :one
SELECT count(*) FROM events
WHERE owner_id = @owner_id::uuid AND deleted_at IS NULL;

-- name: GetEventForUser :one
-- Host view for any member. role is 'owner', 'editor' or 'viewer'. Includes the pinned manifest
-- and the template's latest published version (for the "upgrade available" hint).
SELECT sqlc.embed(e), r.role::text AS role,
       t.slug AS template_slug, t.name AS template_name,
       tv.manifest, tv.assets_path,
       coalesce((SELECT max(v.version) FROM template_versions v
                 WHERE v.template_id = e.template_id AND v.published_at IS NOT NULL),
                e.template_version)::int AS latest_version
FROM events e
CROSS JOIN LATERAL (SELECT event_role(e.id, @user_id::uuid) AS role) r
JOIN templates t ON t.id = e.template_id
JOIN template_versions tv ON tv.template_id = e.template_id AND tv.version = e.template_version
WHERE e.id = @event_id AND e.deleted_at IS NULL AND r.role IS NOT NULL;

-- name: GetAnonDraftEvent :one
-- Same shape as GetEventForUser for an unclaimed draft owned by the draft cookie.
SELECT sqlc.embed(e), 'anon'::text AS role,
       t.slug AS template_slug, t.name AS template_name,
       tv.manifest, tv.assets_path,
       coalesce((SELECT max(v.version) FROM template_versions v
                 WHERE v.template_id = e.template_id AND v.published_at IS NOT NULL),
                e.template_version)::int AS latest_version
FROM events e
JOIN templates t ON t.id = e.template_id
JOIN template_versions tv ON tv.template_id = e.template_id AND tv.version = e.template_version
WHERE e.id = @event_id
  AND e.deleted_at IS NULL
  AND e.owner_id IS NULL
  AND EXISTS (SELECT 1 FROM anon_drafts a
              WHERE a.event_id = e.id AND a.cookie_hash = @cookie_hash AND a.expires_at > now());

-- name: ListEventsForUser :many
-- Owned and co-hosted events, newest first. Each branch is limited on its own index before the merge.
SELECT x.id, x.slug, x.title, x.occasion_slug, x.status, x.starts_at, x.created_at, x.role
FROM (
    (SELECT e.id, e.slug, e.title, e.occasion_slug, e.status, e.starts_at, e.created_at, 'owner'::text AS role
     FROM events e
     WHERE e.owner_id = @user_id::uuid
       AND e.deleted_at IS NULL
       AND (e.created_at, e.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
     ORDER BY e.created_at DESC, e.id DESC
     LIMIT sqlc.arg(lim)::int)
    UNION ALL
    (SELECT e.id, e.slug, e.title, e.occasion_slug, e.status, e.starts_at, e.created_at, m.role
     FROM event_members m
     JOIN events e ON e.id = m.event_id
     WHERE m.user_id = @user_id::uuid
       AND e.deleted_at IS NULL
       AND (e.created_at, e.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
     ORDER BY e.created_at DESC, e.id DESC
     LIMIT sqlc.arg(lim)::int)
) x
ORDER BY x.created_at DESC, x.id DESC
LIMIT sqlc.arg(lim)::int;

-- name: RSVPCountsForEvents :many
-- Dashboard counters for one page of events (ids from ListEventsForUser, at most 100).
-- Access is checked once per event, not per RSVP row.
WITH allowed AS (
    SELECT a.id FROM events a
    WHERE a.id = ANY(@event_ids::uuid[]) AND event_role(a.id, @user_id::uuid) IS NOT NULL
)
SELECT r.event_id,
       count(*) FILTER (WHERE r.attending = 'yes') AS yes,
       count(*) FILTER (WHERE r.attending = 'no') AS no,
       count(*) FILTER (WHERE r.attending = 'maybe') AS maybe,
       coalesce(sum(r.count) FILTER (WHERE r.attending = 'yes'), 0)::bigint AS yes_heads
FROM rsvps r
JOIN allowed ON allowed.id = r.event_id
GROUP BY r.event_id;

-- name: UpdateEventContent :one
-- Editor save for owners and editors. NULL content/overrides/template leave that part unchanged;
-- title, starts_at and ends_at are derived from content and only change with it. Moving ends_at
-- restarts retention from greatest(new end, now()) (so back-dating never shortens an event's life)
-- and clears the reminder so the new end gets its own. A template change must
-- resolve (with the unchanged half) to a selectable pin; leaving both NULL keeps an existing pin even
-- if that template has since become premium or retired. An unselectable pin updates no row.
UPDATE events e
SET content = coalesce(sqlc.narg(content), e.content),
    title = CASE WHEN sqlc.narg(content)::jsonb IS NULL THEN e.title ELSE @title::text END,
    starts_at = CASE WHEN sqlc.narg(content)::jsonb IS NULL THEN e.starts_at ELSE sqlc.narg(starts_at)::timestamptz END,
    ends_at = CASE WHEN sqlc.narg(content)::jsonb IS NULL THEN e.ends_at ELSE sqlc.narg(ends_at)::timestamptz END,
    retention_from = CASE
        WHEN sqlc.narg(content)::jsonb IS NULL
          OR sqlc.narg(ends_at)::timestamptz IS NOT DISTINCT FROM e.ends_at THEN e.retention_from
        WHEN sqlc.narg(ends_at)::timestamptz IS NOT NULL THEN greatest(sqlc.narg(ends_at)::timestamptz, now())
    END,
    retention_reminded_at = CASE
        WHEN sqlc.narg(content)::jsonb IS NULL
          OR sqlc.narg(ends_at)::timestamptz IS NOT DISTINCT FROM e.ends_at THEN e.retention_reminded_at
    END,
    retention_reminder_sent_at = CASE
        WHEN sqlc.narg(content)::jsonb IS NULL
          OR sqlc.narg(ends_at)::timestamptz IS NOT DISTINCT FROM e.ends_at THEN e.retention_reminder_sent_at
    END,
    overrides = coalesce(sqlc.narg(overrides), e.overrides),
    template_id = coalesce(sqlc.narg(template_id), e.template_id),
    template_version = coalesce(sqlc.narg(template_version), e.template_version),
    version = e.version + 1,
    updated_at = now()
WHERE e.id = @event_id
  AND e.deleted_at IS NULL
  AND e.version = @version
  AND e.status <> 'taken_down'
  AND event_role(@event_id, @user_id::uuid) IN ('owner', 'editor')
  AND ((sqlc.narg(template_id)::uuid IS NULL AND sqlc.narg(template_version)::int IS NULL)
       OR EXISTS (SELECT 1 FROM templates t
                  JOIN template_versions tv ON tv.template_id = t.id
                  WHERE t.id = coalesce(sqlc.narg(template_id)::uuid, e.template_id)
                    AND tv.version = coalesce(sqlc.narg(template_version)::int, e.template_version)
                    AND tv.published_at IS NOT NULL
                    AND t.status = 'published'
                    AND NOT t.is_premium))
RETURNING e.version, e.updated_at;

-- name: UpdateAnonDraftContent :one
-- Same derived-field and template-pin rules as UpdateEventContent.
UPDATE events e
SET content = coalesce(sqlc.narg(content), e.content),
    title = CASE WHEN sqlc.narg(content)::jsonb IS NULL THEN e.title ELSE @title::text END,
    starts_at = CASE WHEN sqlc.narg(content)::jsonb IS NULL THEN e.starts_at ELSE sqlc.narg(starts_at)::timestamptz END,
    ends_at = CASE WHEN sqlc.narg(content)::jsonb IS NULL THEN e.ends_at ELSE sqlc.narg(ends_at)::timestamptz END,
    retention_from = CASE
        WHEN sqlc.narg(content)::jsonb IS NULL
          OR sqlc.narg(ends_at)::timestamptz IS NOT DISTINCT FROM e.ends_at THEN e.retention_from
        WHEN sqlc.narg(ends_at)::timestamptz IS NOT NULL THEN greatest(sqlc.narg(ends_at)::timestamptz, now())
    END,
    retention_reminded_at = CASE
        WHEN sqlc.narg(content)::jsonb IS NULL
          OR sqlc.narg(ends_at)::timestamptz IS NOT DISTINCT FROM e.ends_at THEN e.retention_reminded_at
    END,
    retention_reminder_sent_at = CASE
        WHEN sqlc.narg(content)::jsonb IS NULL
          OR sqlc.narg(ends_at)::timestamptz IS NOT DISTINCT FROM e.ends_at THEN e.retention_reminder_sent_at
    END,
    overrides = coalesce(sqlc.narg(overrides), e.overrides),
    template_id = coalesce(sqlc.narg(template_id), e.template_id),
    template_version = coalesce(sqlc.narg(template_version), e.template_version),
    version = e.version + 1,
    updated_at = now()
WHERE e.id = @event_id
  AND e.deleted_at IS NULL
  AND e.version = @version
  AND e.owner_id IS NULL
  AND EXISTS (SELECT 1 FROM anon_drafts a
              WHERE a.event_id = e.id AND a.cookie_hash = @cookie_hash AND a.expires_at > now())
  AND ((sqlc.narg(template_id)::uuid IS NULL AND sqlc.narg(template_version)::int IS NULL)
       OR EXISTS (SELECT 1 FROM templates t
                  JOIN template_versions tv ON tv.template_id = t.id
                  WHERE t.id = coalesce(sqlc.narg(template_id)::uuid, e.template_id)
                    AND tv.version = coalesce(sqlc.narg(template_version)::int, e.template_version)
                    AND tv.published_at IS NOT NULL
                    AND t.status = 'published'
                    AND NOT t.is_premium))
RETURNING e.version, e.updated_at;

-- name: GetEventWriteState :one
-- Only to map a zero-row update: no row → 404, viewer → 403, taken_down → 409, else version conflict.
SELECT r.role::text AS role, e.version, e.status
FROM events e
CROSS JOIN LATERAL (SELECT event_role(e.id, @user_id::uuid) AS role) r
WHERE e.id = @event_id AND e.deleted_at IS NULL AND r.role IS NOT NULL;

-- name: GetAnonDraftWriteState :one
SELECT e.version, e.status
FROM events e
WHERE e.id = @event_id
  AND e.deleted_at IS NULL
  AND e.owner_id IS NULL
  AND EXISTS (SELECT 1 FROM anon_drafts a
              WHERE a.event_id = e.id AND a.cookie_hash = @cookie_hash AND a.expires_at > now());

-- name: UpdateEventSettings :one
-- Owner only. NULL leaves a field unchanged. The password hash is kept only while visibility is
-- 'password' (the CHECKs require that); a new hash replaces the old one. Changing the slug of a
-- published event raises check_violation on constraint events_slug_immutable.
UPDATE events e
SET slug = coalesce(sqlc.narg(slug), e.slug),
    visibility = coalesce(sqlc.narg(visibility), e.visibility),
    password_hash = CASE
        WHEN coalesce(sqlc.narg(visibility)::text, e.visibility) <> 'password' THEN NULL
        ELSE coalesce(sqlc.narg(password_hash)::text, e.password_hash)
    END,
    rsvp_mode = coalesce(sqlc.narg(rsvp_mode), e.rsvp_mode),
    notify_rsvps = coalesce(sqlc.narg(notify_rsvps), e.notify_rsvps),
    version = e.version + 1,
    updated_at = now()
WHERE e.id = @event_id
  AND e.owner_id = @user_id::uuid
  AND e.deleted_at IS NULL
  AND e.version = @version
  AND e.status <> 'taken_down'
RETURNING e.version, e.updated_at;

-- name: PublishEvent :one
-- published_at keeps the first publish time (it also locks the slug).
UPDATE events e
SET status = 'published',
    published_at = coalesce(e.published_at, now()),
    version = e.version + 1,
    updated_at = now()
WHERE e.id = @event_id
  AND e.owner_id = @user_id::uuid
  AND e.deleted_at IS NULL
  AND e.version = @version
  AND e.status IN ('draft', 'hidden')
  AND e.slug IS NOT NULL
RETURNING e.version, e.updated_at, e.published_at;

-- name: UnpublishEvent :one
UPDATE events e
SET status = 'hidden', version = e.version + 1, updated_at = now()
WHERE e.id = @event_id
  AND e.owner_id = @user_id::uuid
  AND e.deleted_at IS NULL
  AND e.version = @version
  AND e.status = 'published'
RETURNING e.version, e.updated_at;

-- name: SoftDeleteEvent :one
-- The slug stays reserved until the purge, so a deleted page's URL can't be taken over at once.
-- A never-published draft never showed its slug publicly, so its slug is released immediately
-- instead of squatting on it until the purge job runs (events CHECK requires published/taken-down
-- events to keep a non-null slug, so this only ever applies to drafts).
UPDATE events
SET deleted_at = now(), updated_at = now(),
    slug = CASE WHEN published_at IS NULL AND status = 'draft' THEN NULL ELSE slug END
WHERE id = @event_id AND owner_id = @user_id::uuid AND deleted_at IS NULL
RETURNING id;

-- name: SoftDeleteAnonDraft :one
UPDATE events e
SET deleted_at = now(), updated_at = now()
WHERE e.id = @event_id
  AND e.deleted_at IS NULL
  AND e.owner_id IS NULL
  AND EXISTS (SELECT 1 FROM anon_drafts a
              WHERE a.event_id = e.id AND a.cookie_hash = @cookie_hash AND a.expires_at > now())
RETURNING e.id;

-- name: ClaimAnonDrafts :many
-- At login: hands every unexpired draft of this browser to the user. Soft-deleted drafts are
-- claimed too, so the 30-day purge removes them instead of leaving ownerless rows behind.
-- A past-dated draft claimed late gets a full retention window from now; its reminder state is
-- cleared only when the anchor actually moves.
WITH claimed AS (
    DELETE FROM anon_drafts a
    WHERE a.cookie_hash = @cookie_hash AND a.expires_at > now()
    RETURNING a.event_id
)
UPDATE events e
SET owner_id = @user_id::uuid, updated_at = now(),
    retention_from = CASE WHEN e.retention_from IS NOT NULL THEN greatest(e.retention_from, now()) END,
    retention_reminded_at = CASE WHEN e.retention_from < now() THEN NULL ELSE e.retention_reminded_at END,
    retention_reminder_sent_at = CASE WHEN e.retention_from < now() THEN NULL ELSE e.retention_reminder_sent_at END
FROM claimed c
WHERE e.id = c.event_id AND e.owner_id IS NULL
RETURNING e.id;

-- name: ClaimAnonDraftMedia :exec
UPDATE media
SET owner_id = @user_id::uuid
WHERE event_id = ANY(@event_ids::uuid[]) AND owner_id IS NULL;

-- name: GetPublicEventBySlug :one
-- Public page. password_hash is only for the Go-side gate and must never be serialised.
SELECT e.id, e.slug, e.title, e.occasion_slug, e.content, e.overrides, e.visibility, e.password_hash,
       e.rsvp_mode, e.remove_branding, e.starts_at, e.template_id, e.template_version,
       tv.manifest, tv.assets_path,
       sqlc.embed(o)
FROM events e
JOIN template_versions tv ON tv.template_id = e.template_id AND tv.version = e.template_version
JOIN occasions o ON o.slug = e.occasion_slug
WHERE e.slug = @slug AND e.status = 'published' AND e.deleted_at IS NULL;

-- name: LockEventForRSVP :one
-- First statement of an RSVP or guest-photo write: serialises capacity and quota checks per event.
-- FOR NO KEY UPDATE does not block the FK checks of concurrent inserts into child tables.
SELECT e.id, e.slug, e.title, e.occasion_slug, e.content, e.rsvp_mode, e.visibility, e.notify_rsvps, e.owner_id
FROM events e
WHERE e.id = @event_id AND e.status = 'published' AND e.deleted_at IS NULL
FOR NO KEY UPDATE;

-- name: LockEventForEditor :one
-- Serialises owner/editor writes with per-event caps (guests, media, members).
SELECT e.id, e.slug, e.status, e.owner_id
FROM events e
WHERE e.id = @event_id
  AND e.deleted_at IS NULL
  AND event_role(@event_id, @user_id::uuid) IN ('owner', 'editor')
FOR NO KEY UPDATE;

-- name: LockAnonDraftEvent :one
-- Serialises anonymous draft writes; confirms the cookie still owns a live (unclaimed, unexpired) draft.
SELECT e.id
FROM events e
WHERE e.id = @event_id
  AND e.deleted_at IS NULL
  AND e.owner_id IS NULL
  AND EXISTS (SELECT 1 FROM anon_drafts a
              WHERE a.event_id = e.id AND a.cookie_hash = @cookie_hash AND a.expires_at > now())
FOR NO KEY UPDATE;

-- name: ClaimRSVPWatermark :one
-- Digest job: returns the previous watermark (NULL = never) and moves it to now().
WITH prev AS (
    SELECT x.id, x.rsvp_notified_at FROM events x
    WHERE x.id = @event_id AND x.deleted_at IS NULL
    FOR NO KEY UPDATE
)
UPDATE events e
SET rsvp_notified_at = now()
FROM prev
WHERE e.id = prev.id
RETURNING prev.rsvp_notified_at AS previous_notified_at, e.rsvp_notified_at::timestamptz AS notified_at;

-- name: GetEventForNotify :one
SELECT e.id, e.slug, e.title, e.status, e.notify_rsvps, u.email AS owner_email, u.name AS owner_name
FROM events e
JOIN users u ON u.id = e.owner_id
WHERE e.id = @event_id AND e.deleted_at IS NULL AND u.deleted_at IS NULL;

-- name: SoftDeleteEventsByOwner :many
-- Account deletion. Bounded by the per-user live-event cap. Never-published drafts release their
-- slug immediately (see SoftDeleteEvent); published/taken-down events keep theirs reserved.
UPDATE events
SET deleted_at = now(), updated_at = now(),
    slug = CASE WHEN published_at IS NULL AND status = 'draft' THEN NULL ELSE slug END
WHERE owner_id = @owner_id::uuid AND deleted_at IS NULL
RETURNING id;

-- name: TakeDownEventsByOwner :many
-- Ban. Drafts without a slug were never public and can't be taken down (events CHECK), so they are skipped.
UPDATE events
SET status = 'taken_down', updated_at = now()
WHERE owner_id = @owner_id::uuid
  AND deleted_at IS NULL
  AND status <> 'taken_down'
  AND slug IS NOT NULL
RETURNING id;

-- name: ListExpiredAnonDraftEvents :many
-- Expired, unclaimed anonymous drafts, oldest first (anon_drafts_expires_at_idx). The caller deletes
-- them with DeleteExpiredAnonDraftEvents, then their media files.
SELECT a.event_id
FROM anon_drafts a
JOIN events e ON e.id = a.event_id
WHERE a.expires_at <= now() AND e.owner_id IS NULL
ORDER BY a.expires_at
LIMIT sqlc.arg(lim)::int;

-- name: DeleteExpiredAnonDraftEvents :execrows
-- Hard-deletes listed drafts that are still expired and unclaimed (anon_drafts and media rows
-- cascade). A draft claimed since the list is skipped.
DELETE FROM events e
WHERE e.id = ANY(@event_ids::uuid[])
  AND e.owner_id IS NULL
  AND EXISTS (SELECT 1 FROM anon_drafts a WHERE a.event_id = e.id AND a.expires_at <= now());

-- name: ClaimRetentionReminders :many
-- Retention job: claims owned events whose retention_from is before ended_before and that were not
-- yet reminded, oldest first (events_retention_remind_idx), and returns each claim stamp (the job
-- args carry it so only the current claim sends). Events with an open report are skipped: moderation
-- decides those. SKIP LOCKED lets concurrent runs take disjoint batches and never waits on an
-- editor save. The claim is the dedupe: each id gets one reminder.
-- ANY(ARRAY(...)) runs the batch once as an InitPlan, so the outer update is a pkey lookup even in
-- a generic plan (an IN semi-join assumes LIMIT $n is 10% of the table and seq-scans events).
UPDATE events e
SET retention_reminded_at = now()
WHERE e.id = ANY(ARRAY(
    SELECT x.id FROM events x
    WHERE x.deleted_at IS NULL
      AND x.status <> 'taken_down'
      AND x.retention_from < @ended_before::timestamptz
      AND x.retention_reminded_at IS NULL
      AND x.owner_id IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM reports r WHERE r.event_id = x.id AND r.status IN ('open', 'reviewing'))
    ORDER BY x.retention_from
    LIMIT sqlc.arg(lim)::int
    FOR NO KEY UPDATE SKIP LOCKED))
  AND e.deleted_at IS NULL
  AND e.retention_reminded_at IS NULL
  AND e.retention_from < @ended_before::timestamptz
  AND NOT EXISTS (SELECT 1 FROM reports r WHERE r.event_id = e.id AND r.status IN ('open', 'reviewing'))
RETURNING e.id, e.retention_reminded_at::timestamptz AS claimed_at;

-- name: GetEventForRetentionReminder :one
-- Reminder email data for one claim. No row once the event was deleted, taken down, re-dated
-- (claim cleared or replaced: retention_reminded_at must equal the stamp in the job args), already
-- sent, or its owner removed; the job then sends nothing.
-- timezone is the datetime block's IANA zone, '' if missing (the caller falls back to UTC).
SELECT e.owner_id::uuid AS owner_id, e.title, e.retention_from::timestamptz AS retention_from,
       e.retention_reminded_at::timestamptz AS retention_reminded_at,
       u.email AS owner_email,
       coalesce(CASE WHEN jsonb_typeof(e.content) = 'array' THEN
           (SELECT b->>'timezone' FROM jsonb_array_elements(e.content) b WHERE b->>'type' = 'datetime' LIMIT 1)
       END, '')::text AS timezone
FROM events e
JOIN users u ON u.id = e.owner_id
WHERE e.id = @event_id
  AND e.retention_reminded_at = @claimed_at::timestamptz
  AND e.retention_reminder_sent_at IS NULL
  AND e.deleted_at IS NULL
  AND e.status <> 'taken_down'
  AND e.retention_from IS NOT NULL
  AND u.deleted_at IS NULL;

-- name: MarkRetentionReminderSent :one
-- Stamps the claim as delivered and returns the stamp. No row: the claim was replaced or cleared by
-- a re-date, the reminder was already marked, or the event was deleted or taken down since the
-- caller loaded it; the caller sends nothing.
UPDATE events
SET retention_reminder_sent_at = now()
WHERE id = @event_id
  AND retention_reminded_at = @claimed_at::timestamptz
  AND retention_reminder_sent_at IS NULL
  AND deleted_at IS NULL
  AND status <> 'taken_down'
RETURNING retention_reminder_sent_at::timestamptz AS sent_at;

-- name: UnmarkRetentionReminderSent :execrows
-- Undoes MarkRetentionReminderSent after the send failed, so the job's retry can send again.
UPDATE events
SET retention_reminder_sent_at = NULL
WHERE id = @event_id AND retention_reminded_at = @claimed_at::timestamptz;

-- name: ExpireEndedEvents :many
-- Retention job: soft-deletes events whose retention_from is before ended_before and whose reminder
-- was delivered before reminded_before. Only an event whose reminder was never delivered
-- (retention_reminder_sent_at NULL: undeliverable address, or the send is still retrying) falls back
-- to the claim stamp, claimed before fallback_before; a delivered reminder always gets its full lead
-- time, however late it was sent. Uses events_retention_expire_idx (the redundant IS NOT NULL lets
-- the planner prove that partial index's predicate through the OR). Events with an open report are
-- skipped. Slug release as in SoftDeleteEvent; batch shape as in ClaimRetentionReminders.
UPDATE events e
SET deleted_at = now(), updated_at = now(),
    slug = CASE WHEN e.published_at IS NULL AND e.status = 'draft' THEN NULL ELSE e.slug END
WHERE e.id = ANY(ARRAY(
    SELECT x.id FROM events x
    WHERE x.deleted_at IS NULL
      AND x.status <> 'taken_down'
      AND x.retention_from < @ended_before::timestamptz
      AND x.retention_reminded_at IS NOT NULL
      AND (x.retention_reminder_sent_at < @reminded_before::timestamptz
           OR (x.retention_reminder_sent_at IS NULL AND x.retention_reminded_at < @fallback_before::timestamptz))
      AND NOT EXISTS (SELECT 1 FROM reports r WHERE r.event_id = x.id AND r.status IN ('open', 'reviewing'))
    ORDER BY x.retention_from
    LIMIT sqlc.arg(lim)::int
    FOR NO KEY UPDATE SKIP LOCKED))
  AND e.deleted_at IS NULL
  AND e.status <> 'taken_down'
  AND e.retention_from < @ended_before::timestamptz
  AND (e.retention_reminder_sent_at < @reminded_before::timestamptz
       OR (e.retention_reminder_sent_at IS NULL AND e.retention_reminded_at < @fallback_before::timestamptz))
  AND NOT EXISTS (SELECT 1 FROM reports r WHERE r.event_id = e.id AND r.status IN ('open', 'reviewing'))
RETURNING e.id;

-- name: ListPurgeableEvents :many
-- Soft-deleted events due for hard delete: deleted before deleted_before (the normal grace for an
-- owner's delete), or expired by retention (reminder delivered before reminded_before, or never
-- delivered but claimed before fallback_before, and retention_from before ended_before; those skip
-- the grace because the owner already had the full window).
-- Taken-down events are kept as moderation evidence, and so is any event with an open report
-- (deleting the event cascades its reports).
-- The batch is row-locked FOR UPDATE SKIP LOCKED: run this, the file deletion and PurgeEvents in ONE
-- transaction. A report insert takes a KEY SHARE lock on the event through its foreign key, which
-- conflicts with FOR UPDATE only, so a report filed meanwhile waits for the commit (and then fails
-- its insert once the row is gone) instead of landing on an event whose files are already deleted.
-- A row a report insert holds right now is skipped and picked up on the next run. Concurrent runs
-- take disjoint batches. ANY(ARRAY(...)) as in ClaimRetentionReminders.
SELECT e.id
FROM events e
WHERE e.id = ANY(ARRAY(
    SELECT x.id FROM events x
    WHERE x.deleted_at IS NOT NULL
      AND x.status <> 'taken_down'
      AND (x.deleted_at < @deleted_before::timestamptz
           OR (x.retention_reminded_at IS NOT NULL
               AND (x.retention_reminder_sent_at < @reminded_before::timestamptz
                    OR (x.retention_reminder_sent_at IS NULL AND x.retention_reminded_at < @fallback_before::timestamptz))
               AND x.retention_from < @ended_before::timestamptz))
      AND NOT EXISTS (SELECT 1 FROM reports r WHERE r.event_id = x.id AND r.status IN ('open', 'reviewing'))
    ORDER BY x.deleted_at
    LIMIT sqlc.arg(lim)::int
    FOR UPDATE SKIP LOCKED))
ORDER BY e.deleted_at;

-- name: FilterLockedPurgeable :many
-- Second half of the purge check, run in the same transaction right after ListPurgeableEvents: that
-- query's open-report test used the statement snapshot, taken before its row locks, so a report
-- committed in between would be missed. This statement takes a new snapshot while the locks are
-- held; a report filed from now on waits on the row lock. Returns the listed ids that are still
-- purgeable, mirroring the checks PurgeEvents makes; only these may have their files deleted.
SELECT e.id
FROM events e
WHERE e.id = ANY(@event_ids::uuid[]) AND e.deleted_at IS NOT NULL AND e.status <> 'taken_down'
  AND NOT EXISTS (SELECT 1 FROM reports r WHERE r.event_id = e.id AND r.status IN ('open', 'reviewing'));

-- name: PurgeEvents :execrows
-- Hard-deletes listed soft-deleted events; child rows cascade. Re-checks the state ListPurgeableEvents
-- saw, including that no report is open.
DELETE FROM events e
WHERE e.id = ANY(@event_ids::uuid[]) AND e.deleted_at IS NOT NULL AND e.status <> 'taken_down'
  AND NOT EXISTS (SELECT 1 FROM reports r WHERE r.event_id = e.id AND r.status IN ('open', 'reviewing'));

-- name: ExistingEventIDs :many
-- Media reconcile: which of these directory names still have an event row (deleted or not).
SELECT id FROM events WHERE id = ANY(@event_ids::uuid[]);

-- name: GetEventMediaState :one
-- media_visibility job: decides where the event's files belong.
SELECT id, status, deleted_at FROM events WHERE id = @event_id;

-- name: SearchEventsAdmin :many
-- Admin browse, newest first, deleted events included (events_admin_created_idx).
SELECT e.id, e.slug, e.title, e.occasion_slug, e.status, e.created_at, e.published_at, e.deleted_at,
       e.owner_id, u.email AS owner_email
FROM events e
LEFT JOIN users u ON u.id = e.owner_id
WHERE (e.created_at, e.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg(lim)::int;

-- name: SearchEventsAdminByStatus :many
-- As SearchEventsAdmin, one status only (events_admin_status_idx).
SELECT e.id, e.slug, e.title, e.occasion_slug, e.status, e.created_at, e.published_at, e.deleted_at,
       e.owner_id, u.email AS owner_email
FROM events e
LEFT JOIN users u ON u.id = e.owner_id
WHERE e.status = @status::text
  AND (e.created_at, e.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg(lim)::int;

-- name: SearchEventsAdminText :many
-- Prefix search over slug and title (events_search_idx); prefix_query built in Go as for users.
-- status NULL = any.
SELECT e.id, e.slug, e.title, e.occasion_slug, e.status, e.created_at, e.published_at, e.deleted_at,
       e.owner_id, u.email AS owner_email
FROM events e
LEFT JOIN users u ON u.id = e.owner_id
WHERE to_tsvector('simple', coalesce(e.slug, '') || ' ' || e.title) @@ to_tsquery('simple', @prefix_query)
  AND (sqlc.narg(status)::text IS NULL OR e.status = sqlc.narg(status)::text)
  AND (e.created_at, e.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg(lim)::int;

-- name: GetEventAdmin :one
SELECT sqlc.embed(e),
       u.email AS owner_email,
       t.slug AS template_slug, t.name AS template_name, tv.manifest, tv.assets_path,
       (SELECT count(*) FROM rsvps r WHERE r.event_id = e.id) AS rsvp_count,
       (SELECT count(*) FROM guests g WHERE g.event_id = e.id) AS guest_count,
       (SELECT count(*) FROM media m WHERE m.event_id = e.id) AS media_count,
       (SELECT count(*) FROM reports p WHERE p.event_id = e.id AND p.status IN ('open', 'reviewing')) AS open_reports
FROM events e
LEFT JOIN users u ON u.id = e.owner_id
JOIN templates t ON t.id = e.template_id
JOIN template_versions tv ON tv.template_id = e.template_id AND tv.version = e.template_version
WHERE e.id = @event_id;

-- name: TakeDownEvent :one
-- No row: missing, deleted, already taken down, or a slug-less draft (never public).
WITH prev AS (
    SELECT x.id, x.status FROM events x
    WHERE x.id = @event_id AND x.deleted_at IS NULL AND x.status <> 'taken_down' AND x.slug IS NOT NULL
    FOR NO KEY UPDATE
)
UPDATE events e
SET status = 'taken_down', updated_at = now()
FROM prev
WHERE e.id = prev.id
RETURNING e.id, e.owner_id, prev.status AS old_status, e.status AS new_status;

-- name: RestoreEvent :one
-- Back to 'published' if it had ever been published, else 'draft'.
WITH prev AS (
    SELECT x.id, x.status FROM events x
    WHERE x.id = @event_id AND x.deleted_at IS NULL AND x.status = 'taken_down'
    FOR NO KEY UPDATE
)
UPDATE events e
SET status = CASE WHEN e.published_at IS NOT NULL THEN 'published' ELSE 'draft' END,
    updated_at = now()
FROM prev
WHERE e.id = prev.id
RETURNING e.id, e.owner_id, prev.status AS old_status, e.status AS new_status;

-- name: SetEventFlags :one
-- Super-admin flags. NULL leaves a field unchanged. A template change bumps version so an open
-- editor reloads instead of saving over it.
WITH prev AS (
    SELECT x.id, x.remove_branding, x.template_id, x.template_version, x.overrides FROM events x
    WHERE x.id = @event_id AND x.deleted_at IS NULL
    FOR NO KEY UPDATE
)
UPDATE events e
SET remove_branding = coalesce(sqlc.narg(remove_branding), e.remove_branding),
    template_id = coalesce(sqlc.narg(template_id), e.template_id),
    template_version = coalesce(sqlc.narg(template_version), e.template_version),
    overrides = coalesce(sqlc.narg(overrides), e.overrides),
    version = CASE WHEN sqlc.narg(template_id)::uuid IS NULL THEN e.version ELSE e.version + 1 END,
    updated_at = now()
FROM prev
WHERE e.id = prev.id
RETURNING e.id,
          prev.remove_branding AS old_remove_branding, prev.template_id AS old_template_id,
          prev.template_version AS old_template_version, prev.overrides AS old_overrides,
          e.remove_branding, e.template_id, e.template_version, e.overrides, e.version;
