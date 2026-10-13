-- +goose Up

-- Link-preview ("share") image per event: 'hero' uses the hero image (else the site default),
-- 'site' always uses the site default, 'custom' uses share_media_id, an image of this event.
--
-- The column default is a constant, so ADD COLUMN is metadata-only; the CHECK and FK validation
-- scans are cheap on the events table.

ALTER TABLE events
    ADD COLUMN share_image text NOT NULL DEFAULT 'hero'
        CONSTRAINT events_share_image_check CHECK (share_image IN ('hero', 'site', 'custom')),
    ADD COLUMN share_media_id uuid,
    ADD CONSTRAINT events_share_media_check CHECK (share_image = 'custom' OR share_media_id IS NULL),
    -- Composite key so the image must belong to this same event (media_event_id_id_key, 00009).
    -- Deleting the media row (host delete, rejected/detached cleanup) only clears share_media_id;
    -- share_image stays 'custom' and the public page falls back to the site image. The referencing
    -- side needs no extra index: the delete action looks up events by id, which is the primary key.
    ADD CONSTRAINT events_share_media_fkey FOREIGN KEY (id, share_media_id)
        REFERENCES media (event_id, id) ON DELETE SET NULL (share_media_id);

-- +goose Down

ALTER TABLE events
    DROP CONSTRAINT events_share_media_fkey,
    DROP CONSTRAINT events_share_media_check,
    DROP COLUMN share_media_id,
    DROP COLUMN share_image;
