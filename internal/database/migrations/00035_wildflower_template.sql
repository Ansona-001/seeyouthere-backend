-- +goose Up

-- Wildflower wedding/engagement/baby-shower theme: a DRAFT template with its first version (v1, published).
-- Data only. The manifest is the canonical form of internal/content/testdata/themes_v2/wildflower.json
-- (what content.ValidateManifest accepts and the admin API would store). Same shape as 00034.
--
-- Rollout gate: status 'draft' keeps it out of the host picker and the public catalog. Publishing is an
-- admin PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order 19 lists it after Marigold (18), before Letterpress (20).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000016', 'wildflower', 'Wildflower', '{"occasions":["wedding","engagement","baby-shower"]}', 'draft', 19);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000016', 1, $manifest${"schema":2,"layout":"centered","hero_style":"text_only","heading_scale":"display","palettes":[{"id":"poppy","name":"Poppy","colors":{"background":"#FBF7F0","surface":"#FFFDF9","text":"#3A2B25","muted":"#64483C","accent":"#B13F17","accent_text":"#FFFFFF"},"accent_ink":"#B13F17","art":["#E8541C","#B13F17","#6F8A6A","#F6EBDD","#C4501F","#E9DCC8","#8FA7B0"]},{"id":"sage","name":"Sage","colors":{"background":"#F7F8F1","surface":"#FDFEF9","text":"#2C3A33","muted":"#4D5F55","accent":"#2F5D50","accent_text":"#FFFFFF"},"accent_ink":"#2F5D50","art":["#E8541C","#2F5D50","#6F8A6A","#EEF1E4","#3C6E5F","#DDE4D2","#8FA7B0"]}],"fonts":[{"id":"cormorant","name":"Cormorant and Lora","heading":"cormorant_garamond","body":"lora","accent":"pinyon_script"}],"defaults":{"palette":"poppy","font":"cormorant"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art4","glow_opacity":0.5,"vignette":"art6","vignette_opacity":0.2},{"kind":"art","region":"page","art":"wildflower_page","placement":"page","opacity":1},{"kind":"art","region":"page","art":"wildflower","placement":"top","opacity":1},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.3}],"ornament":{"hero":"none","divider":"none","badge":"none","ampersand":"script","hero_ink":"","label_ink":""},"card":{"style":"reply_card","border":"none","radius":4,"fields":"underline","buttons":"accent"},"motion":"sway","music":"music_box"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00034: published versions are immutable, so the trigger is disabled for the
-- delete. Fails (and the whole Down rolls back) if an event pins the version or the template has a later
-- version; repin or delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id = '01926a00-0000-7000-8000-000000000016';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id = '01926a00-0000-7000-8000-000000000016';
