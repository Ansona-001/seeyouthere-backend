-- name: SlugBlocked :one
-- Blocked when a term equals the slug or one of its '-' tokens, or (for abuse/brand terms of 5+
-- chars) appears anywhere in the slug with hyphens removed, either as-is or with common leetspeak
-- digit substitutions undone (0/1/3/4/5/7/8 -> o/l/e/a/s/t/b), so "s33youthere"/"paym3nt"/"veri7y"
-- are still caught. The blocklist is small, so this is a single scan of it.
SELECT EXISTS (
    SELECT 1 FROM slug_blocklist b
    WHERE b.term = @slug::text
       OR b.term = ANY(string_to_array(@slug::text, '-'))
       OR (length(b.term) >= 5 AND b.reason <> 'reserved'
           AND (strpos(replace(@slug::text, '-', ''), b.term) > 0
                OR strpos(translate(replace(@slug::text, '-', ''), '0134578', 'oleastb'), b.term) > 0))
) AS blocked;

-- name: SlugTaken :one
-- exclude_event_id lets an event keep its own slug. Soft-deleted events keep their slug reserved, and
-- so does a hard-deleted published event's slug for 30 days (retired_slugs, written by a trigger).
-- user_id is the caller's verified session user (NULL when anonymous): the hold does not apply to the
-- user who owned the deleted event. A tombstone with an unknown owner (NULL) holds against everyone.
SELECT (
    EXISTS (
        SELECT 1 FROM events e
        WHERE e.slug = @slug::text AND e.id IS DISTINCT FROM sqlc.narg(exclude_event_id)::uuid
    )
    OR EXISTS (
        SELECT 1 FROM retired_slugs r
        WHERE r.slug = @slug::text AND r.retired_at > now() - interval '30 days'
          AND (r.owner_id = sqlc.narg(user_id)::uuid) IS NOT TRUE
    )
)::boolean AS taken;

-- name: ClearRetiredSlugOwner :exec
-- Admin delete of a live event: its owner gets no exemption from the slug's 30-day hold.
UPDATE retired_slugs SET owner_id = NULL WHERE slug = @slug;

-- name: ClearRetiredSlugOwnerForUser :exec
-- A user being deleted, suspended or banned loses every exemption (retired_slugs_owner_id_idx). Run it
-- after the users row update, in the same transaction.
UPDATE retired_slugs SET owner_id = NULL WHERE owner_id = @owner_id::uuid;

-- name: DeleteExpiredRetiredSlugs :execrows
-- Cleanup job: a tombstone older than 30 days no longer holds its slug (SlugTaken), so it is dropped.
-- The table only holds about 30 days of deletes, so this scan stays small.
DELETE FROM retired_slugs WHERE retired_at <= now() - interval '30 days';

-- name: ListBlocklist :many
-- Keyset on term; first page passes after_term = ''.
SELECT term, reason, created_at
FROM slug_blocklist
WHERE term > @after_term::text
ORDER BY term
LIMIT sqlc.arg(lim)::int;

-- name: AddBlockTerm :execrows
-- Zero rows: the term already exists.
INSERT INTO slug_blocklist (term, reason)
VALUES (@term, @reason)
ON CONFLICT (term) DO NOTHING;

-- name: DeleteBlockTerm :one
DELETE FROM slug_blocklist
WHERE term = @term
RETURNING term, reason;
