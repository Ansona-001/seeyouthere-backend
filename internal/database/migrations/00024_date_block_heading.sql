-- +goose Up

-- The date block's default heading reads "Mark your calendar" instead of "Date & time".
-- Existing events keep the heading saved in their content.
UPDATE occasions
SET default_blocks = replace(default_blocks::text, '"heading": "Date & time"', '"heading": "Mark your calendar"')::jsonb
WHERE default_blocks::text LIKE '%"heading": "Date & time"%';

-- +goose Down
UPDATE occasions
SET default_blocks = replace(default_blocks::text, '"heading": "Mark your calendar"', '"heading": "Date & time"')::jsonb
WHERE default_blocks::text LIKE '%"heading": "Mark your calendar"%';
