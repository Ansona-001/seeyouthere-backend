-- +goose Up
-- A published slug can be printed on physical invitations, so after the event is hard-deleted its slug
-- must not be free for someone else to claim at once. Every delete of a published event (owner delete
-- purge, retention purge, admin delete, cascades) leaves a tombstone here; SlugTaken treats a slug
-- retired within the last 30 days as taken. The row holds no event data.
CREATE TABLE retired_slugs (
    slug       text PRIMARY KEY CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$'),
    retired_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
CREATE FUNCTION events_retire_slug() RETURNS trigger
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

CREATE TRIGGER events_retire_slug
    BEFORE DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION events_retire_slug();

-- +goose Down
DROP TRIGGER IF EXISTS events_retire_slug ON events;
DROP FUNCTION IF EXISTS events_retire_slug();
DROP TABLE IF EXISTS retired_slugs;
