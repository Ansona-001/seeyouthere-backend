-- +goose Up

-- Give the date, venue and RSVP blocks of every occasion a small label above their heading, so
-- each template's label styling (script or small capitals) is visible from the start. Blocks
-- that already have a label keep it, and existing events keep their saved content.
UPDATE occasions o
SET default_blocks = (
    SELECT jsonb_agg(
        CASE
            WHEN coalesce(b->>'kicker', '') <> '' THEN b
            WHEN b->>'type' = 'datetime' THEN b || '{"kicker":"Save the date"}'
            WHEN b->>'type' = 'location' THEN b || '{"kicker":"Find us"}'
            WHEN b->>'type' = 'rsvp'     THEN b || '{"kicker":"Kindly reply"}'
            ELSE b
        END
        ORDER BY ord)
    FROM jsonb_array_elements(o.default_blocks) WITH ORDINALITY AS t(b, ord)
);

-- +goose Down
UPDATE occasions o
SET default_blocks = (
    SELECT jsonb_agg(
        CASE
            WHEN b->>'type' = 'datetime' AND b->>'kicker' = 'Save the date' THEN b - 'kicker'
            WHEN b->>'type' = 'location' AND b->>'kicker' = 'Find us'       THEN b - 'kicker'
            WHEN b->>'type' = 'rsvp'     AND b->>'kicker' = 'Kindly reply'  THEN b - 'kicker'
            ELSE b
        END
        ORDER BY ord)
    FROM jsonb_array_elements(o.default_blocks) WITH ORDINALITY AS t(b, ord)
);
