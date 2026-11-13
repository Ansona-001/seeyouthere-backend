-- +goose Up

-- Four illustrated invitation templates: Clothesline (baby shower), Welcome Home (housewarming),
-- Lovebirds (engagement) and Fizz (party/birthday). Each is a DRAFT template with its first version
-- (v1, published). Data only. The manifests are the canonical form of
-- internal/content/testdata/themes_v2/<slug>.json (what content.ValidateManifest accepts and the admin
-- API would store). Same shape as 00035. All four are free (is_premium defaults to false); an admin can
-- flag one premium later with the existing PATCH.
--
-- Rollout gate: status 'draft' keeps them out of the host picker and the public catalog. Publishing is
-- an admin PATCH of the template status; setting it back to 'draft' is the rollback.
--
-- sort_order: Clothesline 65 (before Cloud Nine 70), Welcome Home 85 (before Open Door 90), Lovebirds 25
-- (between Letterpress 20 and Velvet Foil 30), Fizz 105 (before Gilt Noir 110).

INSERT INTO templates (id, slug, name, tags, status, sort_order) VALUES
    ('01926a00-0000-7000-8000-000000000017', 'clothesline', 'Clothesline', '{"occasions":["baby-shower"]}', 'draft', 65),
    ('01926a00-0000-7000-8000-000000000018', 'welcome-home', 'Welcome Home', '{"occasions":["housewarming"]}', 'draft', 85),
    ('01926a00-0000-7000-8000-000000000019', 'lovebirds', 'Lovebirds', '{"occasions":["engagement"]}', 'draft', 25),
    ('01926a00-0000-7000-8000-00000000001a', 'fizz', 'Fizz', '{"occasions":["party","birthday"]}', 'draft', 105);

INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000017', 1, $manifest${"schema":2,"layout":"centered","hero_style":"text_only","heading_scale":"display","palettes":[{"id":"sorbet","name":"Sorbet","colors":{"background":"#FFF6EC","surface":"#FFFFFF","text":"#2E2340","muted":"#5C5170","accent":"#C2365F","accent_text":"#FFFFFF"},"accent_ink":"#B42F57","art":["#4A3B5C","#FF8A7A","#3FBF9B","#FFC23D","#7DB3F2","#FFE1CC"]},{"id":"bluebell","name":"Bluebell","colors":{"background":"#F2F6FF","surface":"#FFFFFF","text":"#1F2A4A","muted":"#4A5577","accent":"#3554C4","accent_text":"#FFFFFF"},"accent_ink":"#3554C4","art":["#34406B","#FF9A7B","#4CC3A5","#FFCB4A","#F59BB8","#DDE8FF"]}],"fonts":[{"id":"fraunces","name":"Fraunces and Figtree","heading":"fraunces","body":"figtree","accent":"dancing_script"}],"defaults":{"palette":"sorbet","font":"fraunces"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art6","glow_opacity":0.55},{"kind":"art","region":"page","art":"clothesline","colors":["art1","art2","art3","art4","art5"],"placement":"top","opacity":1},{"kind":"art","region":"page","art":"doodles","colors":["art2","art4","art5","art3"],"placement":"scatter","density":"sparse","opacity":0.9},{"kind":"texture","region":"page","texture":"grain","blend":"multiply","opacity":0.08}],"ornament":{"hero":"none","divider":"dots","badge":"none","ampersand":"none","hero_ink":"","label_ink":""},"card":{"style":"soft","border":"none","radius":24,"fields":"boxed","buttons":"accent"},"motion":"pop_and_settle"}$manifest$::jsonb, '', NULL, now()),
    ('01926a00-0000-7000-8000-000000000018', 1, $manifest${"schema":2,"layout":"centered","hero_style":"text_only","heading_scale":"display","palettes":[{"id":"terracotta","name":"Terracotta","colors":{"background":"#FFF3E3","surface":"#FFFCF6","text":"#2B2019","muted":"#5E4E42","accent":"#0E6B66","accent_text":"#FFFFFF"},"accent_ink":"#0E6B66","art":["#2B2019","#E8693F","#1B8A84","#FFC447","#5DA35B","#F7A6B9","#FFE6C7"]},{"id":"blue-door","name":"Blue Door","colors":{"background":"#F0F5F8","surface":"#FFFFFF","text":"#1C2833","muted":"#4A5866","accent":"#2C5AA0","accent_text":"#FFFFFF"},"accent_ink":"#2C5AA0","art":["#1C2833","#F4B39B","#2C5AA0","#FFD15C","#4E9A6A","#E85D4A","#DCE9F2"]}],"fonts":[{"id":"bricolage","name":"Bricolage and Figtree","heading":"bricolage_grotesque","body":"figtree","accent":"dancing_script"}],"defaults":{"palette":"terracotta","font":"bricolage"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art7","glow_opacity":0.6},{"kind":"art","region":"hero","art":"little_house","colors":["art1","art2","art3","art4","art5","art6"],"placement":"hero_top","opacity":1},{"kind":"art","region":"page","art":"doodles","colors":["art4","art6","art3","art5"],"placement":"edges","density":"sparse","opacity":0.9},{"kind":"texture","region":"page","texture":"linen","blend":"multiply","opacity":0.1}],"ornament":{"hero":"none","divider":"squiggle","badge":"none","ampersand":"none","hero_ink":"","label_ink":""},"card":{"style":"reply_card","border":"ink","radius":16,"fields":"underline","buttons":"accent"},"motion":"pop_and_settle"}$manifest$::jsonb, '', NULL, now()),
    ('01926a00-0000-7000-8000-000000000019', 1, $manifest${"schema":2,"layout":"centered","hero_style":"text_only","heading_scale":"display","palettes":[{"id":"rosy","name":"Rosy","colors":{"background":"#FFF2EE","surface":"#FFFFFF","text":"#3A1A24","muted":"#6A4450","accent":"#BE2A4F","accent_text":"#FFFFFF"},"accent_ink":"#B0274A","art":["#F2788E","#3A1A24","#E8433F","#FFB938","#4F9A6B","#FFDCD3"]},{"id":"lilac","name":"Lilac","colors":{"background":"#F6F1FF","surface":"#FFFFFF","text":"#2A2042","muted":"#574B70","accent":"#6B3FB3","accent_text":"#FFFFFF"},"accent_ink":"#6B3FB3","art":["#9C86E8","#2A2042","#F0607E","#FFC24A","#4E9A7A","#E6DCFF"]}],"fonts":[{"id":"dm-serif","name":"DM Serif and Figtree","heading":"dm_serif_display","body":"figtree","accent":"pinyon_script"}],"defaults":{"palette":"rosy","font":"dm-serif"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art6","glow_opacity":0.6},{"kind":"art","region":"page","art":"lovebirds","colors":["art1","art2","art3","art4","art5"],"placement":"top_corners","opacity":1},{"kind":"art","region":"page","art":"doodles","colors":["art3","art4","art1","art5"],"placement":"scatter","density":"sparse","opacity":0.85},{"kind":"texture","region":"page","texture":"cold_press","blend":"multiply","opacity":0.25}],"ornament":{"hero":"none","divider":"heart","badge":"none","ampersand":"script","hero_ink":"","label_ink":""},"card":{"style":"soft","border":"hairline","radius":28,"fields":"boxed","buttons":"accent"},"motion":"sway"}$manifest$::jsonb, '', NULL, now()),
    ('01926a00-0000-7000-8000-00000000001a', 1, $manifest${"schema":2,"layout":"centered","hero_style":"color_block","heading_scale":"display","palettes":[{"id":"magenta","name":"Magenta","colors":{"background":"#FFF3F6","surface":"#FFFFFF","text":"#2A0F24","muted":"#5E3A55","accent":"#A8155C","accent_text":"#FFFFFF"},"accent_ink":"#A8155C","art":["#FFD23F","#FF7A2F","#43D1E0","#FFFFFF","#FF9ACB","#1E7A55","#FFE0EC"]},{"id":"emerald","name":"Emerald","colors":{"background":"#EFFAF4","surface":"#FFFFFF","text":"#0F2E22","muted":"#3D5C50","accent":"#0B6B4E","accent_text":"#FFFFFF"},"accent_ink":"#0B6B4E","art":["#FFD23F","#FF7A59","#9EE7FF","#FFFFFF","#FF9ACB","#1B2A4A","#D9F3E6"]}],"fonts":[{"id":"bagel","name":"Bagel Fat One and Figtree","heading":"bagel_fat_one","body":"figtree","accent":"figtree"}],"defaults":{"palette":"magenta","font":"bagel"},"background":null,"layers":[{"kind":"paper","region":"page","tone":"background","glow":"art7","glow_opacity":0.5},{"kind":"block","region":"hero","edge":"wave"},{"kind":"art","region":"hero","art":"fizz","colors":["art1","art2","art3","art4","art5","art6"],"placement":"hero_top","opacity":1},{"kind":"art","region":"page","art":"doodles","colors":["art2","art1","art5","art3"],"placement":"scatter","density":"normal","opacity":0.9},{"kind":"texture","region":"page","texture":"grain","blend":"multiply","opacity":0.1}],"ornament":{"hero":"none","divider":"squiggle","badge":"none","ampersand":"none","hero_ink":"","label_ink":""},"card":{"style":"sticker","border":"ink","radius":24,"fields":"boxed","buttons":"accent"},"motion":"pop_and_settle"}$manifest$::jsonb, '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00034/00035: published versions are immutable, so the trigger is disabled for
-- the delete. Fails (and the whole Down rolls back) if an event pins a version or a template has a later
-- version; repin or delete those events first.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE version = 1
  AND template_id IN (
    '01926a00-0000-7000-8000-000000000017',
    '01926a00-0000-7000-8000-000000000018',
    '01926a00-0000-7000-8000-000000000019',
    '01926a00-0000-7000-8000-00000000001a');
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates
WHERE id IN (
    '01926a00-0000-7000-8000-000000000017',
    '01926a00-0000-7000-8000-000000000018',
    '01926a00-0000-7000-8000-000000000019',
    '01926a00-0000-7000-8000-00000000001a');
