-- +goose Up

-- Hosted events: the host invites a guest list, every guest gets a personal entry pass (the signed
-- invite token rendered as a QR code), and members check guests in at the door. An 'invite' event is
-- today's behaviour. Hosted events are always invite-only for RSVPs: an open RSVP has no guest row,
-- so it could never be checked in.
--
-- Both ALTERs only add a nullable column or a NOT NULL column with a constant default
-- (metadata-only on PG 11+). The CHECK validation scans and the partial index build are cheap: the
-- events and guests tables are small (guests are capped at 2000 per event) and every existing row
-- holds the defaults.

ALTER TABLE events
    ADD COLUMN kind text NOT NULL DEFAULT 'invite'
        CONSTRAINT events_kind_check CHECK (kind IN ('invite', 'hosted')),
    ADD CONSTRAINT events_hosted_rsvp_check CHECK (kind <> 'hosted' OR rsvp_mode = 'invite_only');

ALTER TABLE guests
    -- First admission; NULL means not checked in.
    ADD COLUMN checked_in_at timestamptz,
    -- People admitted on this pass (at most household_size, enforced by the check-in queries so a
    -- later household_size edit is never refused).
    ADD COLUMN checked_in_count int NOT NULL DEFAULT 0
        CONSTRAINT guests_checked_in_count_check CHECK (checked_in_count BETWEEN 0 AND 50),
    -- The member who admitted the guest. SET NULL: deleting that account must neither delete the
    -- guest nor be blocked by door history; the check-in itself stays, only the attribution is lost.
    ADD COLUMN checked_in_by uuid REFERENCES users (id) ON DELETE SET NULL,
    -- A check-in always admits at least one person, and "not checked in" admits nobody.
    ADD CONSTRAINT guests_checked_in_check CHECK ((checked_in_at IS NULL) = (checked_in_count = 0)),
    -- Only one direction: checked_in_by may become NULL on a checked-in guest via the FK action.
    ADD CONSTRAINT guests_checked_in_by_check CHECK (checked_in_at IS NOT NULL OR checked_in_by IS NULL);

-- FK index for the SET NULL action on user delete. Partial: most guests are never checked in, and
-- the FK action's "checked_in_by = $1" implies the predicate.
CREATE INDEX guests_checked_in_by_idx ON guests (checked_in_by) WHERE checked_in_by IS NOT NULL;

-- +goose Down

DROP INDEX guests_checked_in_by_idx;
ALTER TABLE guests
    DROP CONSTRAINT guests_checked_in_by_check,
    DROP CONSTRAINT guests_checked_in_check,
    DROP COLUMN checked_in_by,
    DROP COLUMN checked_in_count,
    DROP COLUMN checked_in_at;

ALTER TABLE events
    DROP CONSTRAINT events_hosted_rsvp_check,
    DROP COLUMN kind;
