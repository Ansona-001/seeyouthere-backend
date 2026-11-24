-- +goose Up

-- Two wedding templates opening on a photograph, drawn the same way as Lotus Velvet and Ganesha Mandapam (00052):
-- the photo fills the first screen with the hero set in its open space, and every other section follows below on
-- the theme's paper. On a wide screen the photo's sides fade into that paper. No opening cover.
--   Noor:   for Muslim weddings; a lit Ottoman dome against a black night sky, the names in the sky above it.
--   Grace:  for Christian weddings; a church aisle strewn with rose petals leading to a candlelit altar.
-- Both photographs are from Pexels (free for commercial use under the Pexels licence, no attribution required).
-- DRAFT templates, each with its first version (v1, published). Data only. Each manifest is the canonical form of
-- internal/content/testdata/themes_v2/<slug>.json (what content.ValidateManifest accepts and the admin API would
-- store). Same shape as 00052. Free (is_premium defaults to false); an admin can flag one premium later with the
-- existing PATCH.
--
-- Rollout gate: status 'draft' keeps them out of the host picker and the public catalog. Publishing is an admin
-- PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order: Noor 23 and Grace 24 (after Ganesha Mandapam 22, before Lovebirds 25).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000026', 'noor', 'Noor', '{"occasions":["wedding","engagement"]}', 'draft', 23),
    ('01926a00-0000-7000-8000-000000000027', 'grace', 'Grace', '{"occasions":["wedding","engagement"]}', 'draft', 24);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000026', 1, $manifest${"schema":2,"layout":"centered","hero_style":"invitation","heading_scale":"display","palettes":[{"id":"gold","name":"Gold","colors":{"background":"#0A0806","surface":"#17120C","text":"#F3E6CC","muted":"#CDB892","accent":"#D9A54A","accent_text":"#1A1206"},"accent_ink":"#D9A54A","art":["#D9A54A","#FFF3D6","#8A6A3A","#F3D9A0"],"foil":["#8A6A3A","#D9A54A","#F3D9A0"]},{"id":"pearl","name":"Pearl","colors":{"background":"#0A0806","surface":"#17120C","text":"#F3E6CC","muted":"#CDB892","accent":"#EDE3CF","accent_text":"#1A1206"},"accent_ink":"#EDE3CF","art":["#D9A54A","#FFF3D6","#8A6A3A","#FBF4E6"],"foil":["#8A6A3A","#D9A54A","#F3D9A0"]}],"fonts":[{"id":"cormorant","name":"Cormorant and Cinzel","heading":"cormorant_garamond","body":"cormorant_garamond","accent":"cinzel"}],"defaults":{"palette":"gold","font":"cormorant"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art3","glow_opacity":0.12,"vignette":"art1","vignette_opacity":0.05},{"kind":"art","region":"page","art":"noor","placement":"page","opacity":1},{"kind":"texture","region":"page","texture":"grain","blend":"soft-light","opacity":0.12}],"ornament":{"hero":"none","divider":"line","badge":"none","ampersand":"script","hero_ink":"art4","label_ink":"art1","date":"bars"},"card":{"style":"open","border":"none","radius":10,"fields":"underline","buttons":"accent"},"motion":"sway","music":"music_box"}$manifest$::jsonb, '', NULL, now()),
    ('01926a00-0000-7000-8000-000000000027', 1, $manifest${"schema":2,"layout":"centered","hero_style":"invitation","heading_scale":"display","palettes":[{"id":"ivory","name":"Ivory","colors":{"background":"#F4EEE6","surface":"#FBF8F3","text":"#3A2A1E","muted":"#6B5646","accent":"#8C6A3E","accent_text":"#FFFFFF"},"accent_ink":"#7A5A30","art":["#7A5A30","#FFFFFF","#E8DCCB","#4A3322"],"foil":["#8A6A3A","#C9A04A","#EBD49A"]},{"id":"rose","name":"Rose","colors":{"background":"#F4EEE6","surface":"#FBF8F3","text":"#3A2A1E","muted":"#6B5646","accent":"#7B3F4E","accent_text":"#FFFFFF"},"accent_ink":"#7B3F4E","art":["#7B3F4E","#FFFFFF","#E8DCCB","#4A3322"],"foil":["#8A6A3A","#C9A04A","#EBD49A"]}],"fonts":[{"id":"cormorant","name":"Cormorant and Great Vibes","heading":"cormorant_garamond","body":"cormorant_garamond","accent":"great_vibes"}],"defaults":{"palette":"ivory","font":"cormorant"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art2","glow_opacity":0.5,"vignette":"art3","vignette_opacity":0.2},{"kind":"art","region":"page","art":"grace","placement":"page","opacity":1},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.15}],"ornament":{"hero":"none","divider":"line","badge":"none","ampersand":"script","hero_ink":"art4","label_ink":"art1","date":"bars"},"card":{"style":"open","border":"none","radius":10,"fields":"underline","buttons":"accent"},"motion":"sway","music":"music_box"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00052: published versions are immutable, so the trigger is disabled for the delete.
-- Fails (and the whole Down rolls back) if an event pins a version or a template has a later version; repin or
-- delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id IN ('01926a00-0000-7000-8000-000000000026', '01926a00-0000-7000-8000-000000000027');
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id IN ('01926a00-0000-7000-8000-000000000026', '01926a00-0000-7000-8000-000000000027');
