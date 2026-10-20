-- +goose Up
-- The 30-day hold on a retired slug (00011) protects printed links from strangers, but it also kept the
-- owner of the deleted event from reusing their own link. The tombstone now remembers who owned the
-- event so SlugTaken can exempt that user; everyone else, and anyone not logged in, is still held off.
--
-- owner_id is NULL when no one is exempt, and NULL blocks everyone: rows written before this
-- migration, events deleted under a moderation hold or whose owner was no longer an active account
-- (see the function below), and events an admin deleted while they were still live (the admin delete
-- handler clears it). Deleting, suspending or banning a user clears the owner_id of their tombstones
-- in the same transaction, and the cleanup job deletes tombstones once their 30 days are over, so no
-- stale user id is kept. Deliberately no foreign key: the tombstone has to outlive whatever later
-- happens to the user row (users are soft-deleted, and an FK would either block removing a user or,
-- with CASCADE, lift the hold), and the value is only ever compared with a verified session's user id.
ALTER TABLE retired_slugs ADD COLUMN owner_id uuid;

-- Clearing a deleted, suspended or banned user's tombstones looks them up by owner; most rows have no
-- owner, so the index leaves those out.
CREATE INDEX retired_slugs_owner_id_idx ON retired_slugs (owner_id) WHERE owner_id IS NOT NULL;

-- +goose StatementBegin
-- Re-deleting a slug its owner re-claimed refreshes both retired_at and owner_id. An event that was
-- taken down, or that still has an open report, gives its owner no exemption: the link must not go
-- straight back to whoever was moderated off it. The owner is recorded only while their account is
-- live and active. The users row is read without a lock on purpose: purges lock the event first and
-- user delete / ban lock the user first and then that user's events, so a lock here could deadlock.
-- A delete or ban that commits while this delete is in flight can therefore leave that user's id on
-- one tombstone; a deleted or non-active account has no session to match it, and the row expires.
CREATE OR REPLACE FUNCTION events_retire_slug() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
    claimant uuid;
BEGIN
    IF OLD.published_at IS NOT NULL AND OLD.slug IS NOT NULL THEN
        IF OLD.status <> 'taken_down' AND NOT EXISTS (
            SELECT 1 FROM reports r WHERE r.event_id = OLD.id AND r.status IN ('open', 'reviewing')
        ) THEN
            SELECT u.id INTO claimant FROM users u
            WHERE u.id = OLD.owner_id AND u.deleted_at IS NULL AND u.status = 'active';
        END IF;
        INSERT INTO retired_slugs (slug, owner_id) VALUES (OLD.slug, claimant)
        ON CONFLICT (slug) DO UPDATE SET retired_at = now(), owner_id = EXCLUDED.owner_id;
    END IF;
    RETURN OLD;
END;
$fn$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION events_retire_slug() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF OLD.published_at IS NOT NULL AND OLD.slug IS NOT NULL THEN
        INSERT INTO retired_slugs (slug) VALUES (OLD.slug)
        ON CONFLICT (slug) DO UPDATE SET retired_at = now();
    END IF;
    RETURN OLD;
END;
$fn$;
-- +goose StatementEnd

DROP INDEX IF EXISTS retired_slugs_owner_id_idx;
ALTER TABLE retired_slugs DROP COLUMN owner_id;
