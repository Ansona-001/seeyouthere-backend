-- +goose Up

-- Mimosa wedding theme: a DRAFT template with its first version (v1, published). Data only. The
-- manifest is the canonical form of internal/content/testdata/themes_v2/mimosa.json (what
-- content.ValidateManifest accepts and the admin API would store); a Go test runs the stored row
-- back through the validator. Same shape as the 12 launch themes in 00007.
--
-- Rollout gate: status 'draft' keeps it out of the host picker and the public catalog
-- (ListPublishedTemplates only returns status 'published'). Publishing is an admin PATCH of the
-- template status; its v1 is already published, so that needs no further step, and setting it back
-- to 'draft' is the rollback (events pinned to the version keep rendering).
--
-- sort_order 15 lists it between Olive Grove (10) and Letterpress (20), with the other wedding themes.

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000013', 'mimosa', 'Mimosa', '{"occasions":["wedding"]}', 'draft', 15);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000013', 1, $manifest${"schema":2,"layout":"centered","hero_style":"invitation","heading_scale":"display","palettes":[{"id":"sage","name":"Sage","colors":{"background":"#ECF2E8","surface":"#F8FAF3","text":"#52564F","muted":"#5F6360","accent":"#55703F","accent_text":"#FFFFFF"},"accent_ink":"#55703F","art":["#F2D95C","#E2B93B","#A7BC8C","#6F8A5A","#6C8853","#FBFCF7","#C3D2B2"]}],"fonts":[{"id":"vibes","name":"Great Vibes and Lora","heading":"great_vibes","body":"lora","accent":"dancing_script"}],"defaults":{"palette":"sage","font":"vibes"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art6","glow_opacity":0.8,"vignette":"art7","vignette_opacity":0.35},{"kind":"texture","region":"page","texture":"watercolour","colors":["art3","art7","art6"],"blend":"multiply","opacity":0.22},{"kind":"art","region":"page","art":"mimosa","placement":"top","opacity":1},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.3}],"ornament":{"hero":"none","divider":"none","badge":"none","ampersand":"word","hero_ink":"art5","date":"bars"},"card":{"style":"soft","border":"hairline","radius":16,"fields":"underline","buttons":"accent"},"motion":"sway"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00003, 00004 and 00007: published versions are immutable, so the
-- trigger is disabled for the delete. Fails (and the whole Down rolls back, trigger state included)
-- if an event pins the version (events -> template_versions is NO ACTION) or if the template has a
-- later version (template_versions -> templates is RESTRICT); that is intended. Repin or delete
-- those events first. Nothing is cascaded.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id = '01926a00-0000-7000-8000-000000000013';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id = '01926a00-0000-7000-8000-000000000013';
