-- +goose Up

-- Harvest autumn theme (full-screen text hero under a bundled raster leaf band, borderless reply cards): a DRAFT template with its first version (v1, published). Data only. The
-- manifest is the canonical form of internal/content/testdata/themes_v2/harvest.json (what
-- content.ValidateManifest accepts and the admin API would store). Same shape as 00025.
--
-- Rollout gate: status 'draft' keeps it out of the host picker and the public catalog. Publishing is
-- an admin PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order 17 lists it after Mimosa (15), before Letterpress (20).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000014', 'harvest', 'Harvest', '{"occasions":["wedding","engagement","birthday","party"]}', 'draft', 17);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000014', 1, $manifest${"schema":2,"layout":"centered","hero_style":"text_only","heading_scale":"display","palettes":[{"id":"ivory","name":"Ivory","colors":{"background":"#FAF3E8","surface":"#FFFAF2","text":"#46281A","muted":"#6E4B35","accent":"#A8481F","accent_text":"#FFFFFF"},"accent_ink":"#A8481F","art":["#B5651D","#8A4B24","#C4622B","#F6E3C6","#A8481F","#E9CDA4","#8A6E4B"]},{"id":"maple","name":"Maple","colors":{"background":"#F6E6D0","surface":"#FDF4E6","text":"#46281A","muted":"#6A4630","accent":"#9C3F18","accent_text":"#FFFFFF"},"accent_ink":"#9C3F18","art":["#B5651D","#8A4B24","#C4622B","#F3D8B2","#9C3F18","#E2BC8A","#8A6E4B"]}],"fonts":[{"id":"fraunces","name":"Fraunces and Lora","heading":"fraunces","body":"lora","accent":"pinyon_script"}],"defaults":{"palette":"ivory","font":"fraunces"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art4","glow_opacity":0.7,"vignette":"art6","vignette_opacity":0.3},{"kind":"texture","region":"page","texture":"watercolour","colors":["art3","art6","art4"],"blend":"multiply","opacity":0.1},{"kind":"art","region":"page","art":"autumn_page","placement":"page","opacity":1},{"kind":"art","region":"page","art":"autumn","placement":"top","opacity":1},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.3}],"ornament":{"hero":"none","divider":"none","badge":"none","ampersand":"script","hero_ink":"","label_ink":""},"card":{"style":"reply_card","border":"none","radius":4,"fields":"underline","buttons":"accent"},"motion":"sway"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00025: published versions are immutable, so the trigger is disabled for
-- the delete. Fails (and the whole Down rolls back) if an event pins the version or the template has
-- a later version; repin or delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id = '01926a00-0000-7000-8000-000000000014';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id = '01926a00-0000-7000-8000-000000000014';
