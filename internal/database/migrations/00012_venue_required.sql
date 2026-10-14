-- +goose Up
-- The venue (or, for housewarmings, the address) is now required when creating an event.
UPDATE occasions
SET setup_questions = (
    SELECT coalesce(jsonb_agg(
        CASE WHEN t.q ->> 'key' IN ('venue', 'address') THEN jsonb_set(t.q, '{required}', 'true') ELSE t.q END
        ORDER BY t.ord), '[]'::jsonb)
    FROM jsonb_array_elements(setup_questions) WITH ORDINALITY AS t(q, ord)
)
WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(setup_questions) AS q WHERE q ->> 'key' IN ('venue', 'address'));

-- +goose Down
UPDATE occasions
SET setup_questions = (
    SELECT coalesce(jsonb_agg(
        CASE WHEN t.q ->> 'key' IN ('venue', 'address') THEN jsonb_set(t.q, '{required}', 'false') ELSE t.q END
        ORDER BY t.ord), '[]'::jsonb)
    FROM jsonb_array_elements(setup_questions) WITH ORDINALITY AS t(q, ord)
)
WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(setup_questions) AS q WHERE q ->> 'key' IN ('venue', 'address'));
