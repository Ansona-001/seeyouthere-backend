-- name: ListEventMembers :many
-- Visible to any member. Owner first, then co-hosts by join time; at most 1 owner + 10 members.
SELECT x.user_id, x.email, x.name, x.role
FROM (
    SELECT u.id AS user_id, u.email, u.name, 'owner'::text AS role, 0 AS ord, e.created_at
    FROM events e
    JOIN users u ON u.id = e.owner_id
    WHERE e.id = @event_id::uuid AND e.deleted_at IS NULL
    UNION ALL
    SELECT u.id, u.email, u.name, m.role, 1, m.created_at
    FROM event_members m
    JOIN users u ON u.id = m.user_id
    WHERE m.event_id = @event_id::uuid
) x
WHERE event_role(@event_id::uuid, @user_id::uuid) IS NOT NULL
ORDER BY x.ord, x.created_at
LIMIT 11;

-- name: CountEventMembers :one
SELECT count(*) FROM event_members WHERE event_id = @event_id;

-- name: UpsertEventMember :one
-- Owner only; the owner can't be added as a member of their own event. No row: not the owner,
-- event gone, or member_id is the owner (pgx.ErrNoRows at the call site). inserted is false for a
-- re-add or role change of an existing member, so the caller only notifies on a fresh add.
INSERT INTO event_members (event_id, user_id, role)
SELECT e.id, @member_id::uuid, @role::text
FROM events e
WHERE e.id = @event_id::uuid
  AND e.owner_id = @owner_id::uuid
  AND e.deleted_at IS NULL
  AND e.owner_id <> @member_id::uuid
ON CONFLICT (event_id, user_id) DO UPDATE SET role = EXCLUDED.role
RETURNING (xmax = 0) AS inserted;

-- name: UpdateEventMemberRole :execrows
UPDATE event_members m
SET role = @role
WHERE m.event_id = @event_id::uuid
  AND m.user_id = @member_id::uuid
  AND EXISTS (SELECT 1 FROM events e
              WHERE e.id = @event_id::uuid AND e.owner_id = @owner_id::uuid AND e.deleted_at IS NULL);

-- name: DeleteEventMember :execrows
-- The owner removes anyone; a member may remove themself (leave).
DELETE FROM event_members m
WHERE m.event_id = @event_id::uuid
  AND m.user_id = @member_id::uuid
  AND (m.user_id = @user_id::uuid
       OR EXISTS (SELECT 1 FROM events e
                  WHERE e.id = @event_id::uuid AND e.owner_id = @user_id::uuid AND e.deleted_at IS NULL));

-- name: DeleteMembershipsByUser :exec
DELETE FROM event_members WHERE user_id = @user_id;

-- name: GetCohostNotification :one
-- notify_cohost_added job. A missing row (membership removed, user deleted,
-- or the event gone before the job runs) is treated by the worker as done,
-- not an error.
SELECT u.email, e.title AS event_title
FROM event_members m
JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
JOIN events e ON e.id = m.event_id AND e.deleted_at IS NULL
WHERE m.event_id = @event_id::uuid AND m.user_id = @user_id::uuid;
