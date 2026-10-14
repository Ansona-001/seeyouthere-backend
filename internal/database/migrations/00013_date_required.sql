-- +goose Up
-- The date and time is now required when creating an event (the venue became required in 00012).
UPDATE occasions
SET setup_questions = (
    SELECT coalesce(jsonb_agg(
        CASE WHEN t.q ->> 'key' = 'date' THEN jsonb_set(t.q, '{required}', 'true') ELSE t.q END
        ORDER BY t.ord), '[]'::jsonb)
    FROM jsonb_array_elements(setup_questions) WITH ORDINALITY AS t(q, ord)
)
WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(setup_questions) AS q WHERE q ->> 'key' = 'date');

-- +goose Down
UPDATE occasions
SET setup_questions = (
    SELECT coalesce(jsonb_agg(
        CASE WHEN t.q ->> 'key' = 'date' THEN jsonb_set(t.q, '{required}', 'false') ELSE t.q END
        ORDER BY t.ord), '[]'::jsonb)
    FROM jsonb_array_elements(setup_questions) WITH ORDINALITY AS t(q, ord)
)
WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(setup_questions) AS q WHERE q ->> 'key' = 'date');
