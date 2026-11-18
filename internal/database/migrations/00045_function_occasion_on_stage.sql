-- +goose Up

-- Final catalog label for the function occasion (the slug stays "function").
UPDATE occasions SET name = 'On stage' WHERE slug = 'function';

-- +goose Down

UPDATE occasions SET name = 'Spotlight' WHERE slug = 'function';
