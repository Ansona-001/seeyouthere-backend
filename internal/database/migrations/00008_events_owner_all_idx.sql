-- 00008_events_owner_all_idx.sql
-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_owner_id_all_idx ON events (owner_id);
-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS events_owner_id_all_idx;
