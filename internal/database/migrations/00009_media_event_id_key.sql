-- 00009_media_event_id_key.sql
-- Target of events_share_media_fkey (00010): an event's share image must be one of its own media
-- rows, which a composite foreign key can only check against a unique (event_id, id).
-- +goose NO TRANSACTION
-- +goose Up
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS media_event_id_id_key ON media (event_id, id);
-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS media_event_id_id_key;
