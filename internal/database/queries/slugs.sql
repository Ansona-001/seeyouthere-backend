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
-- exclude_event_id lets an event keep its own slug. Deleted events keep their slug reserved.
SELECT EXISTS (
    SELECT 1 FROM events e
    WHERE e.slug = @slug::text AND e.id IS DISTINCT FROM sqlc.narg(exclude_event_id)::uuid
) AS taken;

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
