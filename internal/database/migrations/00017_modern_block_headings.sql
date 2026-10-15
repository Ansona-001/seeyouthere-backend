-- +goose Up

-- New events start from occasions.default_blocks; give the date and location blocks
-- friendlier default headings. Existing events keep the headings already saved in their content.
UPDATE occasions
SET default_blocks = replace(replace(default_blocks::text,
        '"heading": "When"', '"heading": "Date & time"'),
        '"heading": "Where"', '"heading": "Venue"')::jsonb
WHERE default_blocks::text LIKE '%"heading": "When"%'
   OR default_blocks::text LIKE '%"heading": "Where"%';

-- +goose Down
UPDATE occasions
SET default_blocks = replace(replace(default_blocks::text,
        '"heading": "Date & time"', '"heading": "When"'),
        '"heading": "Venue"', '"heading": "Where"')::jsonb
WHERE default_blocks::text LIKE '%"heading": "Date & time"%'
   OR default_blocks::text LIKE '%"heading": "Venue"%';
