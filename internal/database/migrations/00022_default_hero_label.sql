-- +goose Up

-- Give each occasion's hero a small label above the title. Heroes that already have one (the
-- baby shower's "Baby shower for") keep it, and existing events keep their saved content.
UPDATE occasions o
SET default_blocks = (
    SELECT jsonb_agg(
        CASE
            WHEN b->>'type' = 'hero' AND coalesce(b->>'kicker', '') = '' THEN
                b || jsonb_build_object('kicker',
                    CASE WHEN o.slug IN ('wedding', 'engagement') THEN 'Please join us' ELSE 'You''re invited' END)
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
            WHEN b->>'type' = 'hero' AND b->>'kicker' IN ('Please join us', 'You''re invited') THEN b - 'kicker'
            ELSE b
        END
        ORDER BY ord)
    FROM jsonb_array_elements(o.default_blocks) WITH ORDINALITY AS t(b, ord)
);
