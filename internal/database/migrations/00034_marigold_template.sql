-- +goose Up

-- Marigold wedding/engagement theme: a DRAFT template with its first version (v1, published). Data only.
-- The manifest is the canonical form of internal/content/testdata/themes_v2/marigold.json (what
-- content.ValidateManifest accepts and the admin API would store). Same shape as 00031.
--
-- Rollout gate: status 'draft' keeps it out of the host picker and the public catalog. Publishing is an
-- admin PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order 18 lists it after Harvest (17), before Letterpress (20).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000015', 'marigold', 'Marigold', '{"occasions":["wedding","engagement"]}', 'draft', 18);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000015', 1, $manifest${"schema":2,"layout":"centered","hero_style":"text_only","heading_scale":"display","palettes":[{"id":"saffron","name":"Saffron","colors":{"background":"#FAF3E6","surface":"#FFFBF3","text":"#4A2412","muted":"#6B4430","accent":"#B8420A","accent_text":"#FFFFFF"},"accent_ink":"#B8420A","art":["#E8761F","#B8420A","#6F8447","#F6E3C6","#C2560F","#EBCFA3","#8A5A2B"]},{"id":"ivory","name":"Ivory","colors":{"background":"#FBF7EE","surface":"#FFFDF8","text":"#3E2415","muted":"#664432","accent":"#A33D0A","accent_text":"#FFFFFF"},"accent_ink":"#A33D0A","art":["#E8761F","#A33D0A","#6F8447","#F8EBD3","#B54F10","#EBD7B5","#8A5A2B"]}],"fonts":[{"id":"playfair","name":"Playfair and Lora","heading":"playfair_display","body":"lora","accent":"pinyon_script"}],"defaults":{"palette":"saffron","font":"playfair"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art4","glow_opacity":0.6,"vignette":"art6","vignette_opacity":0.25},{"kind":"art","region":"page","art":"marigold_page","placement":"page","opacity":1},{"kind":"art","region":"page","art":"marigold","placement":"top","opacity":1},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.35}],"ornament":{"hero":"none","divider":"none","badge":"none","ampersand":"script","hero_ink":"","label_ink":""},"card":{"style":"reply_card","border":"none","radius":4,"fields":"underline","buttons":"accent"},"motion":"sway","music":"music_box"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00025 and 00031: published versions are immutable, so the trigger is disabled
-- for the delete. Fails (and the whole Down rolls back) if an event pins the version or the template has
-- a later version; repin or delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id = '01926a00-0000-7000-8000-000000000015';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id = '01926a00-0000-7000-8000-000000000015';
