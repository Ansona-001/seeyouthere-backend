-- name: CreateReport :execrows
-- Zero rows: this reporter already has an open report for the event (silently ignored).
INSERT INTO reports (id, event_id, reporter_ip_hash, reason, details)
VALUES (@id, @event_id, @reporter_ip_hash, @reason, @details)
ON CONFLICT (event_id, reporter_ip_hash) WHERE status IN ('open', 'reviewing') DO NOTHING;

-- name: ListReportsAdmin :many
-- Moderation queue for one status, newest first, with the event, its owner and the number of
-- open reports against that event (counted only for the rows on this page).
WITH page AS (
    SELECT p.id, p.event_id, p.reason, p.details, p.status, p.handled_by, p.handled_at, p.created_at
    FROM reports p
    WHERE p.status = @status::text
      AND (p.created_at, p.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
    ORDER BY p.created_at DESC, p.id DESC
    LIMIT sqlc.arg(lim)::int
)
SELECT page.id, page.event_id, page.reason, page.details, page.status, page.handled_by, page.handled_at, page.created_at,
       e.slug AS event_slug, e.title AS event_title, e.status AS event_status, u.email AS owner_email,
       oc.open_reports_for_event
FROM page
JOIN events e ON e.id = page.event_id
LEFT JOIN users u ON u.id = e.owner_id
CROSS JOIN LATERAL (
    SELECT count(*) AS open_reports_for_event FROM reports o
    WHERE o.event_id = page.event_id AND o.status IN ('open', 'reviewing')
) oc
ORDER BY page.created_at DESC, page.id DESC;

-- name: ListReportsForEvent :many
-- All reports against one event, newest first, for the admin event detail page.
-- Caller already has the event/owner in hand, so no join is needed here.
SELECT id, event_id, reason, details, status, handled_by, handled_at, created_at
FROM reports
WHERE event_id = @event_id
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;

-- name: DismissReport :one
WITH prev AS (
    SELECT x.id, x.status FROM reports x
    WHERE x.id = @report_id AND x.status IN ('open', 'reviewing')
    FOR UPDATE
)
UPDATE reports p
SET status = 'dismissed', handled_by = @handled_by::uuid, handled_at = now()
FROM prev
WHERE p.id = prev.id
RETURNING p.id, p.event_id, prev.status AS old_status, p.status AS new_status;

-- name: ResolveOpenReportsForEvent :execrows
-- On takedown: every open report against the event is resolved as taken_down.
UPDATE reports
SET status = 'taken_down', handled_by = @handled_by::uuid, handled_at = now()
WHERE event_id = @event_id AND status IN ('open', 'reviewing');

-- name: RestoreReportsForEvent :execrows
-- On restore: reports that led to the takedown are marked restored.
UPDATE reports
SET status = 'restored', handled_by = @handled_by::uuid, handled_at = now()
WHERE event_id = @event_id AND status = 'taken_down';

-- name: CountOpenReports :one
SELECT count(*) FROM reports WHERE status IN ('open', 'reviewing');
