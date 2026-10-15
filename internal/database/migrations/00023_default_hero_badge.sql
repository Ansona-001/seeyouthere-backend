-- +goose Up

-- Themes with a monogram or numeral hero ornament (Olive Grove, Letterpress, Sprinkles, Gilt Noir,
-- Open Door) print the hero badge text and show nothing without one. Give each occasion a starter
-- badge: initials for couples, a numeral for birthdays and parties, an initial for housewarmings.
-- Couples' and hosts' initials are filled in from the setup answers when the event is created.
-- Themes without a badge ornament ignore it, and existing events keep their saved content.
UPDATE occasions o
SET default_blocks = (
    SELECT jsonb_agg(
        CASE
            WHEN b->>'type' = 'hero' AND coalesce(b->>'badge', '') = '' THEN
                b || jsonb_build_object('badge',
                    CASE
                        WHEN o.slug IN ('wedding', 'engagement') THEN 'A&S'
                        WHEN o.slug IN ('birthday', 'party') THEN '30'
                        WHEN o.slug = 'housewarming' THEN 'S'
                    END)
            ELSE b
        END
        ORDER BY ord)
    FROM jsonb_array_elements(o.default_blocks) WITH ORDINALITY AS t(b, ord)
)
WHERE o.slug IN ('wedding', 'engagement', 'birthday', 'party', 'housewarming');

-- +goose Down
UPDATE occasions o
SET default_blocks = (
    SELECT jsonb_agg(
        CASE
            WHEN b->>'type' = 'hero' AND b->>'badge' IN ('A&S', '30', 'S') THEN b - 'badge'
            ELSE b
        END
        ORDER BY ord)
    FROM jsonb_array_elements(o.default_blocks) WITH ORDINALITY AS t(b, ord)
)
WHERE o.slug IN ('wedding', 'engagement', 'birthday', 'party', 'housewarming');
