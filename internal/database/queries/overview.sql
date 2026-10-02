-- name: AdminOverviewCounts :one
-- Each count is served by a partial or leading-column index.
SELECT
    (SELECT count(*) FROM reports WHERE status IN ('open', 'reviewing')) AS open_reports,
    (SELECT count(*) FROM media WHERE uploaded_by = 'guest' AND moderation_status = 'pending') AS pending_guest_photos,
    (SELECT count(*) FROM users WHERE deleted_at IS NULL AND created_at >= now() - interval '7 days') AS new_users_7d,
    (SELECT count(*) FROM events WHERE published_at >= now() - interval '7 days') AS events_published_7d;
