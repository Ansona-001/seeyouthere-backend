-- +goose Up

-- Two invitation templates (wedding and engagement) drawn the same way as Temple Pond (00051): each opens on one
-- painting as tall as the screen with the hero set in its open space, a landscape copy of the painting fills a
-- wide screen, and every other section follows below on the theme's paper. No opening cover.
--   Lotus Velvet:      gold-outlined pink lotuses climbing the left edge of deep maroon velvet.
--   Ganesha Mandapam:  a pooja backdrop, jasmine garlands and brass lamps on a cream wall with Ganesha above.
-- DRAFT templates, each with its first version (v1, published). Data only. Each manifest is the canonical form of
-- internal/content/testdata/themes_v2/<slug>.json (what content.ValidateManifest accepts and the admin API would
-- store). Same shape as 00051. Free (is_premium defaults to false); an admin can flag one premium later with the
-- existing PATCH.
--
-- Rollout gate: status 'draft' keeps them out of the host picker and the public catalog. Publishing is an admin
-- PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order: Lotus Velvet 21 and Ganesha Mandapam 22 (after Letterpress 20, before Lovebirds 25).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000024', 'lotus-velvet', 'Lotus Velvet', '{"occasions":["wedding","engagement"]}', 'draft', 21),
    ('01926a00-0000-7000-8000-000000000025', 'ganesha-mandapam', 'Ganesha Mandapam', '{"occasions":["wedding","engagement"]}', 'draft', 22);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000024', 1, $manifest${"schema":2,"layout":"centered","hero_style":"invitation","heading_scale":"display","palettes":[{"id":"maroon","name":"Maroon","colors":{"background":"#5A1019","surface":"#6A1A26","text":"#F8E9DA","muted":"#E6C3B8","accent":"#E2B661","accent_text":"#3A0A12"},"accent_ink":"#E2B661","art":["#E2B661","#FBEFE4","#E9A6B9","#F6E3C8"],"foil":["#9C7A2E","#E2B661","#F6E3B0"]},{"id":"rose","name":"Rose","colors":{"background":"#5A1019","surface":"#6A1A26","text":"#F8E9DA","muted":"#E6C3B8","accent":"#E9A6B9","accent_text":"#3A0A12"},"accent_ink":"#E9A6B9","art":["#E9A6B9","#FBEFE4","#E2B661","#F6E3C8"],"foil":["#9C7A2E","#E2B661","#F6E3B0"]}],"fonts":[{"id":"cormorant","name":"Cormorant and Great Vibes","heading":"cormorant_garamond","body":"cormorant_garamond","accent":"great_vibes"}],"defaults":{"palette":"maroon","font":"cormorant"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art3","glow_opacity":0.15,"vignette":"art1","vignette_opacity":0.1},{"kind":"art","region":"page","art":"lotus_velvet","placement":"page","opacity":1},{"kind":"texture","region":"page","texture":"velvet","blend":"soft-light","opacity":0.3}],"ornament":{"hero":"none","divider":"line","badge":"none","ampersand":"script","hero_ink":"art4","label_ink":"art1","date":"bars"},"card":{"style":"open","border":"none","radius":10,"fields":"underline","buttons":"accent"},"motion":"sway","music":"music_box"}$manifest$::jsonb, '', NULL, now()),
    ('01926a00-0000-7000-8000-000000000025', 1, $manifest${"schema":2,"layout":"centered","hero_style":"invitation","heading_scale":"display","palettes":[{"id":"lotus","name":"Lotus","colors":{"background":"#EFE4DC","surface":"#F8F1EB","text":"#3E2416","muted":"#6B4A3A","accent":"#9C2156","accent_text":"#FFFFFF"},"accent_ink":"#9C2156","art":["#7E5A1C","#FFF8EE","#E7CFC2","#5A2A14"],"foil":["#8A6420","#C9A04A","#EBD49A"]},{"id":"kumkum","name":"Kumkum","colors":{"background":"#EFE4DC","surface":"#F8F1EB","text":"#3E2416","muted":"#6B4A3A","accent":"#8E2A1C","accent_text":"#FFFFFF"},"accent_ink":"#8E2A1C","art":["#7E5A1C","#FFF8EE","#E7CFC2","#6B1E12"],"foil":["#8A6420","#C9A04A","#EBD49A"]}],"fonts":[{"id":"cormorant","name":"Cormorant and Cinzel","heading":"cormorant_garamond","body":"cormorant_garamond","accent":"cinzel"}],"defaults":{"palette":"lotus","font":"cormorant"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art2","glow_opacity":0.6,"vignette":"art3","vignette_opacity":0.3},{"kind":"art","region":"page","art":"ganesha_mandapam","placement":"page","opacity":1},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.2}],"ornament":{"hero":"none","divider":"line","badge":"none","ampersand":"script","hero_ink":"art4","label_ink":"art1","date":"bars"},"card":{"style":"open","border":"none","radius":10,"fields":"underline","buttons":"accent"},"motion":"sway","music":"music_box"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00051: published versions are immutable, so the trigger is disabled for the delete.
-- Fails (and the whole Down rolls back) if an event pins a version or a template has a later version; repin or
-- delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id IN ('01926a00-0000-7000-8000-000000000024', '01926a00-0000-7000-8000-000000000025');
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id IN ('01926a00-0000-7000-8000-000000000024', '01926a00-0000-7000-8000-000000000025');
