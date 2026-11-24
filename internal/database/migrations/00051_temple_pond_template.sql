-- +goose Up

-- One "Temple Pond" invitation template (wedding and engagement): a painted Kerala temple scene. The carved
-- arch and brass lamps run across the top of the page, its pillars down both sides and the temple, pond and
-- lotuses along the bottom; no opening cover. A DRAFT template with its first version (v1, published). Data only. The manifest is the canonical form of
-- internal/content/testdata/themes_v2/temple-pond.json (what content.ValidateManifest accepts and the admin
-- API would store). Same shape as 00041. Free (is_premium defaults to false); an admin can flag it premium
-- later with the existing PATCH.
--
-- Rollout gate: status 'draft' keeps it out of the host picker and the public catalog. Publishing is
-- an admin PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order: Temple Pond 19 (after Marigold 18, before Letterpress 20).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000023', 'temple-pond', 'Temple Pond', '{"occasions":["wedding","engagement"]}', 'draft', 19);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000023', 1, $manifest${"schema":2,"layout":"centered","hero_style":"invitation","heading_scale":"display","palettes":[{"id":"sandal","name":"Sandal","colors":{"background":"#F8EBD2","surface":"#FCF5E6","text":"#3B2312","muted":"#66472E","accent":"#7A3B14","accent_text":"#FFFFFF"},"accent_ink":"#7A3B14","art":["#7E5A1C","#FFF7E6","#E3C99B","#4A2310"],"foil":["#7E5A1C","#C39A45","#EBD49A"]},{"id":"kumkum","name":"Kumkum","colors":{"background":"#F8EBD2","surface":"#FCF5E6","text":"#3B2312","muted":"#66472E","accent":"#8E2A1C","accent_text":"#FFFFFF"},"accent_ink":"#8E2A1C","art":["#7E5A1C","#FFF7E6","#E3C99B","#6B1E12"],"foil":["#7E5A1C","#C39A45","#EBD49A"]}],"fonts":[{"id":"cormorant","name":"Cormorant and Cinzel","heading":"cormorant_garamond","body":"cormorant_garamond","accent":"cinzel"}],"defaults":{"palette":"sandal","font":"cormorant"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art2","glow_opacity":0.6,"vignette":"art3","vignette_opacity":0.3},{"kind":"art","region":"page","art":"temple_pond_page","placement":"page","opacity":1},{"kind":"art","region":"page","art":"temple_pond","placement":"top","opacity":1},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.25}],"ornament":{"hero":"none","divider":"line","badge":"none","ampersand":"script","hero_ink":"art4","label_ink":"art1","date":"bars"},"card":{"style":"open","border":"none","radius":10,"fields":"underline","buttons":"accent"},"motion":"sway","music":"music_box"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00041: published versions are immutable, so the trigger is disabled for
-- the delete. Fails (and the whole Down rolls back) if an event pins the version or the template has a later
-- version; repin or delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id = '01926a00-0000-7000-8000-000000000023';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id = '01926a00-0000-7000-8000-000000000023';
