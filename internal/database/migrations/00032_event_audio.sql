-- +goose Up

-- Per-event music choice: the template's own loop (default, keeps today's behaviour), one uploaded
-- MP3, or silence. Text + CHECK instead of an enum, like events.status. Adding a NOT NULL column
-- with a constant default is a metadata-only change on PG 11+ (no table rewrite).
ALTER TABLE events
    ADD COLUMN music_source text NOT NULL DEFAULT 'template'
    CHECK (music_source IN ('template', 'custom', 'none'));

-- The uploaded track of an event. At most one per event (event_audio_event_key). The file lives in
-- object storage under a key derived from id; the row holds only what quota and the player need.
-- No storage_key / content_type column: the file is always the re-encoded MP3 the media package wrote.
-- event_id CASCADE: the track has no meaning without its event (purge removes both; the storage
-- sweep removes the file). uploaded_by SET NULL: deleting the uploader's account must not delete a
-- track that the event, and so the remaining owner/editors, still use.
CREATE TABLE event_audio (
    id          uuid PRIMARY KEY,
    event_id    uuid NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    uploaded_by uuid REFERENCES users (id) ON DELETE SET NULL,
    size_bytes  bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 5242880),
    duration_ms integer NOT NULL CHECK (duration_ms BETWEEN 1000 AND 600000),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT event_audio_event_key UNIQUE (event_id)
);

-- FK index (the UNIQUE constraint above already indexes event_id). Serves the SET NULL action on user delete.
CREATE INDEX event_audio_uploaded_by_idx ON event_audio (uploaded_by);

-- +goose Down
DROP INDEX event_audio_uploaded_by_idx;
DROP TABLE event_audio;
ALTER TABLE events DROP COLUMN music_source;
