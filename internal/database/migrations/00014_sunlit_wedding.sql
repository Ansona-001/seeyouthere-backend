-- +goose Up
-- Offer the Sunlit Watercolour template for weddings as well as engagements.
UPDATE templates
SET tags = jsonb_set(tags, '{occasions}', (tags -> 'occasions') || '["wedding"]'::jsonb)
WHERE slug = 'sunlit'
  AND NOT (tags -> 'occasions') ? 'wedding';

-- +goose Down
UPDATE templates
SET tags = jsonb_set(tags, '{occasions}', (tags -> 'occasions') - 'wedding')
WHERE slug = 'sunlit';
