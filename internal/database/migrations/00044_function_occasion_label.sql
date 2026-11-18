-- +goose Up

-- Rename the function occasion's catalog label (the slug stays "function" so existing events and
-- template tags keep working).
UPDATE occasions SET name = 'Spotlight' WHERE slug = 'function';

-- +goose Down

UPDATE occasions SET name = 'Function or show' WHERE slug = 'function';
