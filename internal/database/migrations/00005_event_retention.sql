-- +goose Up

-- Event retention: an event is deleted 30 days after it ends, with a reminder to the owner a few
-- days before. The cutoffs live in Go; the database stores the effective end, the retention anchor
-- and the reminder stamps.
--
-- The events table is small enough that the in-transaction backfill and plain CREATE INDEX are
-- fine; the backfill writes every dated row anyway, so a CONCURRENTLY index would not avoid the lock.

ALTER TABLE events
    ADD COLUMN ends_at timestamptz,
    -- The retention clock: greatest(ends_at, now()) at the moment ends_at was set or last changed.
    -- Every retention cutoff compares this, not ends_at, so an editor cannot back-date an event to
    -- get it deleted early: it always lives a full retention window from the edit.
    ADD COLUMN retention_from timestamptz,
    -- Set when the retention reminder is claimed; expiry only deletes reminded events. Cleared by an
    -- editor save that moves ends_at, so a re-dated event gets a fresh reminder.
    ADD COLUMN retention_reminded_at timestamptz,
    -- Set when the reminder email was handed to the mail server. Expiry waits on this rather than the
    -- claim, so an owner is never deleted on a reminder that was never sent (with a fallback in the
    -- expiry query so an undeliverable address cannot keep an event forever).
    ADD COLUMN retention_reminder_sent_at timestamptz;

COMMENT ON COLUMN events.ends_at IS
    'Effective end: the datetime block end, else its start. NULL exactly when starts_at is NULL.';
COMMENT ON COLUMN events.retention_from IS
    'Retention anchor: greatest(ends_at, now()) when ends_at was set or last changed. NULL exactly when ends_at is NULL.';

-- Wall-clock gap between two "YYYY-MM-DDTHH:MM" strings, NULL when either is not a real date
-- (the shape regex lets impossible dates such as 2026-02-31 through, and one such row must not
-- abort the migration). Dropped again at the end of Up.
-- +goose StatementBegin
CREATE FUNCTION events_backfill_gap(start_local text, end_local text) RETURNS interval
LANGUAGE plpgsql AS $$
BEGIN
    RETURN make_interval(secs => extract(epoch FROM end_local::timestamp - start_local::timestamp));
EXCEPTION WHEN data_exception THEN
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- Backfill from the stored datetime block (internal/content: start_local/end_local are wall-clock
-- "YYYY-MM-DDTHH:MM" in the block timezone). Adding the wall-clock gap to starts_at matches Go's
-- zone-aware end except across a DST change inside the event (off by at most the DST shift).
-- Content was validated by Go on save; the shape guards and the function only stop a malformed row
-- from aborting the migration (such a row falls back to ends_at = starts_at).
UPDATE events e
SET ends_at = e.starts_at + greatest(interval '0', coalesce(
        CASE WHEN jsonb_typeof(e.content) = 'array' THEN (
            SELECT events_backfill_gap(b->>'start_local', b->>'end_local')
            FROM jsonb_array_elements(e.content) b
            WHERE b->>'type' = 'datetime'
              AND b->>'start_local' ~ '^[1-9][0-9]{3}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]$'
              AND b->>'end_local' ~ '^[1-9][0-9]{3}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]$'
            LIMIT 1)
        END,
        interval '0'))
WHERE e.starts_at IS NOT NULL;

DROP FUNCTION events_backfill_gap(text, text);

-- Legacy events get the full retention window from deploy time instead of being deleted at once.
-- The random 14-day spread keeps every existing event from becoming due at the same instant, which
-- would flood the shared SMTP account (it also sends login codes).
UPDATE events SET retention_from = greatest(ends_at, now()) + random() * interval '14 days'
WHERE ends_at IS NOT NULL;

-- The IS NOT NULL terms are spelled out: without them a NULL on a dated event makes the CHECK
-- evaluate to NULL, which passes.
ALTER TABLE events
    ADD CONSTRAINT events_ends_at_check CHECK (
        (starts_at IS NULL AND ends_at IS NULL AND retention_from IS NULL)
        OR (starts_at IS NOT NULL AND ends_at IS NOT NULL AND ends_at >= starts_at
            AND retention_from IS NOT NULL AND retention_from >= ends_at)),
    ADD CONSTRAINT events_retention_sent_check CHECK (
        retention_reminder_sent_at IS NULL OR retention_reminded_at IS NOT NULL);

-- Retention scans in anchor order, one partial index per stage so each scan only walks rows it can
-- act on: a row leaves the reminder index when claimed and the expiry index when soft-deleted.
-- A single index on retention_from would make every run re-read (and discard) all reminded-but-not-
-- yet-expired and ownerless rows. Undated events (retention_from NULL) never match the cutoffs.
CREATE INDEX events_retention_remind_idx ON events (retention_from)
    WHERE deleted_at IS NULL AND status <> 'taken_down' AND retention_reminded_at IS NULL
      AND owner_id IS NOT NULL;
CREATE INDEX events_retention_expire_idx ON events (retention_from)
    WHERE deleted_at IS NULL AND status <> 'taken_down' AND retention_reminded_at IS NOT NULL;

-- +goose Down

DROP INDEX events_retention_expire_idx;
DROP INDEX events_retention_remind_idx;
ALTER TABLE events
    DROP CONSTRAINT events_retention_sent_check,
    DROP CONSTRAINT events_ends_at_check,
    DROP COLUMN retention_reminder_sent_at,
    DROP COLUMN retention_reminded_at,
    DROP COLUMN retention_from,
    DROP COLUMN ends_at;
