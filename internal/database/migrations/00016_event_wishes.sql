-- +goose Up
-- Guest-submitted wishes shown on the invitation once a host approves them. Guests are not
-- accounts, so there is no author FK; the name is whatever the guest typed.
-- ON DELETE CASCADE: wishes are content of the event and have no meaning without it; the hard
-- purge of an expired event must not be blocked by (or leave behind) its wishes.
CREATE TABLE event_wishes (
    id         uuid PRIMARY KEY,
    event_id   uuid NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    author     text NOT NULL CHECK (char_length(author) BETWEEN 1 AND 80),
    message    text NOT NULL CHECK (char_length(message) BETWEEN 1 AND 500),
    status     text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'hidden')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Serves the public approved list, the host list per status (both keyset on created_at, id), the
-- event_id FK, and the per-event count behind the submission cap.
CREATE INDEX event_wishes_event_status_created_idx ON event_wishes (event_id, status, created_at DESC, id DESC);

-- +goose Down
-- Dropping the table deletes every stored wish. Binaries from before this migration cannot parse a
-- stored wishes block whose mode is "open", so roll the application back before (or together
-- with) this migration, and clear or rewrite such blocks if you keep the data.
DROP TABLE event_wishes;
