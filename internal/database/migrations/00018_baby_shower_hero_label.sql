-- +goose Up

-- Baby shower invites show "Baby shower of" as the hero's small label and only the
-- honoree's name as the title. Existing events keep the hero already saved in their content.
UPDATE occasions
SET default_blocks = replace(default_blocks::text,
        '"title": "Baby shower for Sam"',
        '"kicker": "Baby shower of", "title": "Sam"')::jsonb,
    copy = jsonb_set(copy, '{title_template}', '"{name}"')
WHERE slug = 'baby-shower';

-- +goose Down
UPDATE occasions
SET default_blocks = replace(default_blocks::text,
        '"kicker": "Baby shower of", "title": "Sam"',
        '"title": "Baby shower for Sam"')::jsonb,
    copy = jsonb_set(copy, '{title_template}', '"Baby shower for {name}"')
WHERE slug = 'baby-shower';
