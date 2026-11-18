-- +goose Up

-- One "Wisteria" invitation template (wedding and engagement): the first to use the schema-2 `intro` field
-- (a full-screen "tap to open" reveal the public page renders). A DRAFT template with its first version
-- (v1, published). Data only. The manifest is the canonical form of
-- internal/content/testdata/themes_v2/wisteria.json (what content.ValidateManifest accepts and the admin
-- API would store). Same shape as 00040. Free (is_premium defaults to false); an admin can flag it premium
-- later with the existing PATCH.
--
-- Rollout gate: status 'draft' keeps it out of the host picker and the public catalog. Publishing is
-- an admin PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order: Wisteria 17 (after Nocturne 16, before Letterpress 20).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000022', 'wisteria', 'Wisteria', '{"occasions":["wedding","engagement"]}', 'draft', 17);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000022', 1, $manifest${"schema":2,"layout":"centered","hero_style":"invitation","heading_scale":"display","palettes":[{"id":"ivory","name":"Ivory","colors":{"background":"#F5EFE3","surface":"#FBF8F1","text":"#3D3831","muted":"#675E52","accent":"#6E5A2A","accent_text":"#FFFFFF"},"accent_ink":"#6E5A2A","art":["#7A6020","#FFFDF8","#E6DCC8","#5B4A6B"],"foil":["#8C6D22","#D4AF4F","#F7E8B5"]},{"id":"sage","name":"Sage","colors":{"background":"#EEF1E8","surface":"#F8FAF4","text":"#39403A","muted":"#5E665C","accent":"#56693F","accent_text":"#FFFFFF"},"accent_ink":"#56693F","art":["#7A6420","#FBFCF7","#D5DCC8","#4F5A6B"],"foil":["#8C6D22","#D4AF4F","#F7E8B5"]}],"fonts":[{"id":"cormorant","name":"Cormorant and Lora","heading":"cormorant_garamond","body":"lora","accent":"pinyon_script"}],"defaults":{"palette":"ivory","font":"cormorant"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art2","glow_opacity":0.8,"vignette":"art3","vignette_opacity":0.35},{"kind":"frame","region":"page","frame":"single_hairline","paint":"foil","inset":12},{"kind":"art","region":"page","art":"wisteria","placement":"top","opacity":1},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.3}],"ornament":{"hero":"none","divider":"line","badge":"foil_seal","ampersand":"script","hero_ink":"art4","label_ink":"art1","date":"bars"},"card":{"style":"soft","border":"foil_inset","radius":10,"fields":"underline","buttons":"accent"},"motion":"sway","music":"music_box","intro":"wisteria"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00040: published versions are immutable, so the trigger is disabled for
-- the delete. Fails (and the whole Down rolls back) if an event pins the version or the template has a later
-- version; repin or delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id = '01926a00-0000-7000-8000-000000000022';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id = '01926a00-0000-7000-8000-000000000022';
