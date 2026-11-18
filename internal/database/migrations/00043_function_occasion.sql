-- +goose Up

-- "Function / show" occasion: organisational functions, concerts, stage programmes, award nights and
-- similar ticketless public events. It differs from the personal occasions in three ways: the host
-- sets a from AND to date-time up front (datetime.end_local setup target; content validation already
-- allows ranges of up to 14 days), the default blocks include a programme, and speakers/performers
-- (people), a video and FAQs are offered as optional blocks.
-- The JSON follows the formats validated by internal/content (ParseOccasion).
INSERT INTO occasions (slug, name, sort_order, default_blocks, optional_blocks, rsvp_fields, copy, setup_questions) VALUES
    ('function', 'Function or show', 70,
     '[{"id":"hero","type":"hero","title":"Annual awards night","subtitle":"You''re invited","image":null},{"id":"when","type":"datetime","heading":"When","start_local":"2027-06-12T18:00","end_local":"","timezone":"UTC","all_day":false},{"id":"where","type":"location","heading":"Where","name":"The venue","address":"","map_url":"","notes":""},{"id":"welcome","type":"text","heading":"","body":"Join us for an evening of recognition, music and celebration."},{"id":"programme","type":"schedule","heading":"Programme","items":[{"time":"18:00","title":"Doors open and registration","location":"","description":""},{"time":"19:00","title":"Welcome address","location":"","description":""},{"time":"19:30","title":"Main programme","location":"","description":""}]},{"id":"rsvp","type":"rsvp","heading":"RSVP","body":"Please let us know if you will be attending.","deadline_local":"","capacity":null,"max_party_size":4,"fields":[{"key":"organisation","required":false},{"key":"dietary","required":false}],"questions":[]}]',
     '["text","image","gallery","schedule","location","dress_code","links","faq","countdown","people","video","guest_photos","rsvp"]',
     '[{"key":"organisation","label":"Organisation","type":"text","required":false,"max_length":120,"options":[],"min":null,"max":null,"help":""},{"key":"dietary","label":"Dietary requirements","type":"text","required":false,"max_length":200,"options":[],"min":null,"max":null,"help":""},{"key":"accessibility","label":"Accessibility needs","type":"textarea","required":false,"max_length":300,"options":[],"min":null,"max":null,"help":""},{"key":"message","label":"Message for the organisers","type":"textarea","required":false,"max_length":500,"options":[],"min":null,"max":null,"help":""}]',
     '{"title_template":"{title}","tagline":"You''re invited","rsvp_heading":"RSVP","rsvp_body":"Please let us know if you will be attending.","share_message":"You''re invited! Event details and RSVP:"}',
     '[{"key":"title","label":"Event name","type":"text","required":true,"max_length":60,"target":"title_var"},{"key":"tagline","label":"Tagline","type":"text","required":false,"max_length":120,"target":"hero.subtitle"},{"key":"from","label":"From","type":"date_time","required":true,"max_length":16,"target":"datetime.start_local"},{"key":"to","label":"To","type":"date_time","required":true,"max_length":16,"target":"datetime.end_local"},{"key":"timezone","label":"Time zone","type":"timezone","required":false,"max_length":64,"target":"datetime.timezone"},{"key":"venue","label":"Venue","type":"place","required":false,"max_length":120,"target":"location.name"}]');

-- Offer the neutral published templates for the new occasion; without a tag an occasion has no
-- templates to pick from.
UPDATE templates
SET tags = jsonb_set(tags, '{occasions}', (tags -> 'occasions') || '["function"]'::jsonb)
WHERE slug IN ('classic', 'modern', 'minimal')
  AND NOT (tags -> 'occasions') ? 'function';

-- +goose Down

UPDATE templates
SET tags = jsonb_set(tags, '{occasions}', (tags -> 'occasions') - 'function')
WHERE slug IN ('classic', 'modern', 'minimal');

-- Fails if events reference the occasion (FK), which is intended.
DELETE FROM occasions WHERE slug = 'function';
