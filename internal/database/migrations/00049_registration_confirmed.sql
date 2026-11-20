-- +goose Up

-- A registration on a hosted event is made by whoever types an email into the public form, so until
-- the pass link mailed to that address has been opened nothing proves the address is real or wanted.
-- confirmed_at is set when the link is first accepted (handleAcceptRSVPLink). An unconfirmed
-- registration counts toward capacity and the registration bound only for its first 24 hours, and is
-- deleted after 48 (cleanup job), so junk sign-ups cannot hold an event's spots indefinitely.
-- Only registrations are ever confirmed; every other RSVP leaves it NULL.
--
-- Metadata-only ADD COLUMN. Existing registrations are backfilled as confirmed (at their creation
-- time): they predate the purge, and deleting people who may already hold a pass would be worse than
-- leaving the few old junk rows until their event is deleted. The CHECK and the index scan rsvps
-- once; the table is small and the backfill leaves the partial index empty, so they run inline.

ALTER TABLE rsvps
    ADD COLUMN confirmed_at timestamptz,
    ADD CONSTRAINT rsvps_confirmed_registration_check CHECK (registration OR confirmed_at IS NULL);

UPDATE rsvps SET confirmed_at = created_at WHERE registration;

-- The cleanup job's purge scan: oldest unconfirmed, never-used registrations first. Partial, so it
-- holds only rows still waiting for their link to be opened and stays tiny.
CREATE INDEX rsvps_unconfirmed_registration_idx ON rsvps (created_at)
    WHERE registration AND confirmed_at IS NULL AND checked_in_at IS NULL;

-- +goose Down

DROP INDEX rsvps_unconfirmed_registration_idx;
ALTER TABLE rsvps
    DROP CONSTRAINT rsvps_confirmed_registration_check,
    DROP COLUMN confirmed_at;
