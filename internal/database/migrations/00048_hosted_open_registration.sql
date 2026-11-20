-- +goose Up

-- Open registration on hosted events: a hosted event may now take open RSVPs. Each one is a
-- registration (one per email per event) that gets its own entry pass (a token signed over the RSVP
-- id and edit_token_version, rendered as a QR code) and is checked in at the door like a guest. RSVPs on invite events and
-- guest RSVPs never have registration set. Whether an event is hosted lives on events, so "a
-- registration belongs to a hosted event" is enforced by the insert path in Go, not here.
--
-- The ALTER only adds a nullable column or NOT NULL columns with constant defaults (metadata-only on
-- PG 11+). The CHECK validation scans and the three index builds read rsvps once each; every
-- existing row holds the defaults, so all three partial indexes start empty and the scans are cheap
-- at this table's size.

ALTER TABLE events DROP CONSTRAINT events_hosted_rsvp_check;

ALTER TABLE rsvps
    -- Self-registration on a hosted event. Set at insert only; no update query writes it.
    ADD COLUMN registration boolean NOT NULL DEFAULT false,
    -- First admission; NULL means not checked in.
    ADD COLUMN checked_in_at timestamptz,
    -- People admitted on this pass (at most count, enforced by the check-in queries so a later edit
    -- of the RSVP is never refused).
    ADD COLUMN checked_in_count int NOT NULL DEFAULT 0
        CONSTRAINT rsvps_checked_in_count_check CHECK (checked_in_count BETWEEN 0 AND 50),
    -- The member who admitted the registration. SET NULL: deleting that account must neither delete
    -- the RSVP nor be blocked by door history; the check-in itself stays, only the attribution is lost.
    ADD COLUMN checked_in_by uuid REFERENCES users (id) ON DELETE SET NULL,
    -- A registration is an open RSVP keyed by its (lower-cased) email.
    ADD CONSTRAINT rsvps_registration_check CHECK (NOT registration OR (guest_id IS NULL AND email IS NOT NULL)),
    -- A check-in always admits at least one person, and "not checked in" admits nobody.
    ADD CONSTRAINT rsvps_checked_in_check CHECK ((checked_in_at IS NULL) = (checked_in_count = 0)),
    -- Only one direction: checked_in_by may become NULL on a checked-in RSVP via the FK action.
    ADD CONSTRAINT rsvps_checked_in_by_check CHECK (checked_in_at IS NOT NULL OR checked_in_by IS NULL),
    -- Only registrations hold a pass of their own; guest RSVPs are checked in on the guest row.
    ADD CONSTRAINT rsvps_checked_in_registration_check CHECK (registration OR checked_in_at IS NULL);

-- One registration per email per event (registration_exists on 23505). Also serves the
-- registration-by-email lookup and the per-event registration scan of the check-in summary.
CREATE UNIQUE INDEX rsvps_event_registration_email_key ON rsvps (event_id, email) WHERE registration;
-- Registration list (keyset, oldest first). rsvps_event_created_idx would have to skip the event's
-- guest RSVPs, and the planner then prefers reading every registration of the event and sorting;
-- this reads one page.
CREATE INDEX rsvps_event_registration_created_idx ON rsvps (event_id, created_at, id) WHERE registration;

-- FK index for the SET NULL action on user delete. Partial: most RSVPs are never checked in, and
-- the FK action's "checked_in_by = $1" implies the predicate.
CREATE INDEX rsvps_checked_in_by_idx ON rsvps (checked_in_by) WHERE checked_in_by IS NOT NULL;

-- +goose Down

DROP INDEX rsvps_checked_in_by_idx;
DROP INDEX rsvps_event_registration_created_idx;
DROP INDEX rsvps_event_registration_email_key;
ALTER TABLE rsvps
    DROP CONSTRAINT rsvps_checked_in_registration_check,
    DROP CONSTRAINT rsvps_checked_in_by_check,
    DROP CONSTRAINT rsvps_checked_in_check,
    DROP CONSTRAINT rsvps_registration_check,
    DROP COLUMN checked_in_by,
    DROP COLUMN checked_in_count,
    DROP COLUMN checked_in_at,
    DROP COLUMN registration;

-- Lossy: hosted events that took open registrations go back to invite-only (their registrations stay
-- as plain open RSVPs, minus check-in state), since the old invariant forbids open hosted events.
UPDATE events SET rsvp_mode = 'invite_only' WHERE kind = 'hosted' AND rsvp_mode <> 'invite_only';
ALTER TABLE events
    ADD CONSTRAINT events_hosted_rsvp_check CHECK (kind <> 'hosted' OR rsvp_mode = 'invite_only');
