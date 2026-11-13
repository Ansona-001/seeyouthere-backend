-- +goose Up

-- Glitterball birthday/party theme: a DRAFT template with its first version (v1, published).
-- Data only. The manifest is the canonical form of internal/content/testdata/themes_v2/glitterball.json
-- (what content.ValidateManifest accepts and the admin API would store). Same shape as 00038.
--
-- Rollout gate: status 'draft' keeps it out of the host picker and the public catalog. Publishing is an
-- admin PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order 107 lists it after Fizz (105), before Gilt Noir (110).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-00000000001b', 'glitterball', 'Glitterball', '{"occasions":["birthday","party"]}', 'draft', 107);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-00000000001b', 1, $manifest${"schema":2,"layout":"centered","hero_style":"spotlight","heading_scale":"display","background":null,"palettes":[{"id":"bubblegum","name":"Bubblegum","colors":{"background":"#F6EEE1","surface":"#FFFBF5","text":"#2B2230","muted":"#5F4F5C","accent":"#C2185B","accent_text":"#FFFFFF"},"accent_ink":"#C2185B","art":["#E58AAE","#C2185B","#9A9CA3","#F7C1D6","#F6EEE1","#E6D5C3"]},{"id":"silver","name":"Silver","colors":{"background":"#F0F0F2","surface":"#FFFFFF","text":"#1F1F26","muted":"#53535E","accent":"#3A3A46","accent_text":"#FFFFFF"},"accent_ink":"#3A3A46","art":["#9A9CA3","#C2185B","#E58AAE","#F7C1D6","#F0F0F2","#D9D9DE"]}],"fonts":[{"id":"bodoni","name":"Bodoni Moda and Josefin Sans","heading":"bodoni_moda","body":"josefin_sans","accent":"dancing_script"}],"defaults":{"palette":"bubblegum","font":"bodoni"},"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art4","glow_opacity":0.35,"vignette":"art6","vignette_opacity":0.3},{"kind":"art","region":"page","art":"glitterball_page","placement":"page"},{"kind":"art","region":"page","art":"glitterball","placement":"top"},{"kind":"texture","region":"page","texture":"cold_press","opacity":0.3,"blend":"multiply"}],"ornament":{"hero":"none","divider":"sparkle_rule","badge":"starburst_sticker","ampersand":"none","hero_ink":"","label_ink":""},"card":{"style":"soft","border":"hairline","radius":20,"fields":"boxed","buttons":"accent"},"motion":"pop_and_settle"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00038: published versions are immutable, so the trigger is disabled for the
-- delete. Fails (and the whole Down rolls back) if an event pins the version or the template has a later
-- version; repin or delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id = '01926a00-0000-7000-8000-00000000001b';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id = '01926a00-0000-7000-8000-00000000001b';
