-- +goose Up

-- When the invitation itself last changed (editor save, settings, publish/unpublish), shown to admins.
-- Unlike updated_at it ignores moderation, soft-delete, ownership and background bookkeeping writes.
-- Metadata-only ADD COLUMN (constant-time default); existing rows are backfilled from updated_at.
ALTER TABLE events ADD COLUMN content_updated_at timestamptz NOT NULL DEFAULT now();
UPDATE events SET content_updated_at = updated_at;

-- +goose Down

ALTER TABLE events DROP COLUMN content_updated_at;
