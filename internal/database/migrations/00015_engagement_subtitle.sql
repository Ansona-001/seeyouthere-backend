-- +goose Up
-- New engagement invitations read "are getting engaged" instead of "are engaged". Existing events
-- keep the content they were created with.
UPDATE occasions
SET copy = jsonb_set(copy, '{tagline}', '"are getting engaged"'),
    default_blocks = (
        SELECT coalesce(jsonb_agg(
            CASE WHEN t.b ->> 'type' = 'hero' AND t.b ->> 'subtitle' = 'are engaged'
                 THEN jsonb_set(t.b, '{subtitle}', '"are getting engaged"') ELSE t.b END
            ORDER BY t.ord), '[]'::jsonb)
        FROM jsonb_array_elements(default_blocks) WITH ORDINALITY AS t(b, ord)
    )
WHERE slug = 'engagement';

-- +goose Down
UPDATE occasions
SET copy = jsonb_set(copy, '{tagline}', '"are engaged"'),
    default_blocks = (
        SELECT coalesce(jsonb_agg(
            CASE WHEN t.b ->> 'type' = 'hero' AND t.b ->> 'subtitle' = 'are getting engaged'
                 THEN jsonb_set(t.b, '{subtitle}', '"are engaged"') ELSE t.b END
            ORDER BY t.ord), '[]'::jsonb)
        FROM jsonb_array_elements(default_blocks) WITH ORDINALITY AS t(b, ord)
    )
WHERE slug = 'engagement';
