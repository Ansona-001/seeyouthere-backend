-- +goose Up

-- The function occasion's dates are optional: a host can leave them for the editor, give a single date and
-- time, or switch on an end ("To") to make it a range. The wizard shows the first question as "From" once
-- the end is switched on; the API only ever sees the label below.
UPDATE occasions SET setup_questions =
    '[{"key":"title","label":"Event name","type":"text","required":true,"max_length":60,"target":"title_var"},{"key":"tagline","label":"Tagline","type":"text","required":false,"max_length":120,"target":"hero.subtitle"},{"key":"from","label":"Date and time","type":"date_time","required":false,"max_length":16,"target":"datetime.start_local"},{"key":"to","label":"To","type":"date_time","required":false,"max_length":16,"target":"datetime.end_local"},{"key":"timezone","label":"Time zone","type":"timezone","required":false,"max_length":64,"target":"datetime.timezone"},{"key":"venue","label":"Venue","type":"place","required":false,"max_length":120,"target":"location.name"}]'
WHERE slug = 'function';

-- +goose Down

UPDATE occasions SET setup_questions =
    '[{"key":"title","label":"Event name","type":"text","required":true,"max_length":60,"target":"title_var"},{"key":"tagline","label":"Tagline","type":"text","required":false,"max_length":120,"target":"hero.subtitle"},{"key":"from","label":"From","type":"date_time","required":true,"max_length":16,"target":"datetime.start_local"},{"key":"to","label":"To","type":"date_time","required":true,"max_length":16,"target":"datetime.end_local"},{"key":"timezone","label":"Time zone","type":"timezone","required":false,"max_length":64,"target":"datetime.timezone"},{"key":"venue","label":"Venue","type":"place","required":false,"max_length":120,"target":"location.name"}]'
WHERE slug = 'function';
