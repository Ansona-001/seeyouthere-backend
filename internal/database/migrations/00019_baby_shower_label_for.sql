-- +goose Up

-- The baby shower hero label reads "Baby shower for", matching the setup question
-- ("Who is the shower for?"). Existing events keep the hero saved in their content.
UPDATE occasions
SET default_blocks = replace(default_blocks::text, '"kicker": "Baby shower of"', '"kicker": "Baby shower for"')::jsonb
WHERE slug = 'baby-shower';

-- +goose Down
UPDATE occasions
SET default_blocks = replace(default_blocks::text, '"kicker": "Baby shower for"', '"kicker": "Baby shower of"')::jsonb
WHERE slug = 'baby-shower';
