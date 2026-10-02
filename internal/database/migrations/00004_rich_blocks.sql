-- +goose Up

-- Rich blocks: the people, video and wishes block types, the Heirloom template, and version 2 of
-- Classic, Garden, Modern and Confetti with the optional theme knobs (surface, texture,
-- heading_scale, fonts[].accent). Data only: occasions.optional_blocks and
-- template_versions.manifest are jsonb, so no DDL is needed.
--
-- Published template versions are immutable (template_versions_immutable), so existing events stay
-- pinned to v1 and are offered the upgrade; new events pin the latest published version. Minimal
-- keeps v1 only. The JSON follows the formats validated by internal/content (ParseOccasion,
-- ValidateManifest); a Go test runs these rows through the validators so they can't drift.

-- The new block types are generic (hosts, guests of honour, a video, curated wishes), so every
-- occasion offers them. Appends only the types an occasion doesn't already list.
UPDATE occasions
SET optional_blocks = optional_blocks || (
        SELECT coalesce(jsonb_agg(t ORDER BY ord), '[]'::jsonb)
        FROM jsonb_array_elements_text('["people","video","wishes"]') WITH ORDINALITY AS n (t, ord)
        WHERE NOT optional_blocks ? t),
    updated_at = now()
WHERE slug IN ('wedding', 'engagement', 'birthday', 'baby-shower', 'housewarming', 'party');

INSERT INTO templates (id, slug, name, tags, status) VALUES
    ('01926a00-0000-7000-8000-000000000006', 'heirloom', 'Heirloom', '{"occasions":["wedding","engagement"]}', 'published');

-- Heirloom v1, then v2 of four existing templates: each v2 is its v1 manifest from 00003 verbatim
-- plus surface, texture, heading_scale and a per-font-pair accent ("" = the heading font).
INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at) VALUES
    ('01926a00-0000-7000-8000-000000000006', 1,
     '{"schema":1,"layout":"centered","hero_style":"spotlight","decoration":"heart","surface":"glass","texture":"grid","heading_scale":"display","palettes":[{"id":"crimson","name":"Crimson","colors":{"background":"#F9F4EC","surface":"#FFFFFF","text":"#8B0000","muted":"#B22222","accent":"#8A6E3C","accent_text":"#FFFFFF"}},{"id":"emerald","name":"Emerald","colors":{"background":"#F3F5EE","surface":"#FFFFFF","text":"#0F4D3A","muted":"#4A6356","accent":"#8A6E3C","accent_text":"#FFFFFF"}},{"id":"midnight","name":"Midnight","colors":{"background":"#15171F","surface":"#232633","text":"#F4ECDD","muted":"#B9AE98","accent":"#D2B77C","accent_text":"#15171F"}}],"fonts":[{"id":"playfair","name":"Playfair and Montserrat","heading":"playfair_display","body":"montserrat","accent":"birthstone"},{"id":"cormorant","name":"Cormorant and Montserrat","heading":"cormorant_garamond","body":"montserrat","accent":"great_vibes"}],"defaults":{"palette":"crimson","font":"playfair"},"background":null}',
     '', NULL, now()),
    ('01926a00-0000-7000-8000-000000000001', 2,
     '{"schema":1,"layout":"centered","hero_style":"framed","decoration":"line","surface":"card","texture":"none","heading_scale":"regular","palettes":[{"id":"ivory","name":"Ivory","colors":{"background":"#FBF8F3","surface":"#FFFFFF","text":"#1F1B16","muted":"#6B645C","accent":"#8C6A3F","accent_text":"#FFFFFF"}},{"id":"navy","name":"Navy","colors":{"background":"#F4F6FA","surface":"#FFFFFF","text":"#14213D","muted":"#56607A","accent":"#1F3A68","accent_text":"#FFFFFF"}},{"id":"sage","name":"Sage","colors":{"background":"#F3F6F1","surface":"#FFFFFF","text":"#1E2A1F","muted":"#5C6B5D","accent":"#4A6B4E","accent_text":"#FFFFFF"}}],"fonts":[{"id":"playfair","name":"Playfair and Lora","heading":"playfair_display","body":"lora","accent":"great_vibes"},{"id":"cormorant","name":"Cormorant and Figtree","heading":"cormorant_garamond","body":"figtree","accent":""}],"defaults":{"palette":"ivory","font":"playfair"},"background":null}',
     '', NULL, now()),
    ('01926a00-0000-7000-8000-000000000002', 2,
     '{"schema":1,"layout":"centered","hero_style":"full_bleed","decoration":"floral","surface":"glass","texture":"none","heading_scale":"regular","palettes":[{"id":"blush","name":"Blush","colors":{"background":"#FDF4F2","surface":"#FFFFFF","text":"#3A2027","muted":"#7A5A60","accent":"#A64D63","accent_text":"#FFFFFF"}},{"id":"meadow","name":"Meadow","colors":{"background":"#F4F8EE","surface":"#FFFFFF","text":"#1F2B17","muted":"#5A6B4C","accent":"#4F7A2E","accent_text":"#FFFFFF"}},{"id":"lavender","name":"Lavender","colors":{"background":"#F6F3FB","surface":"#FFFFFF","text":"#2A2140","muted":"#675D80","accent":"#6B4FA0","accent_text":"#FFFFFF"}}],"fonts":[{"id":"cormorant","name":"Cormorant and Lora","heading":"cormorant_garamond","body":"lora","accent":"great_vibes"},{"id":"script","name":"Great Vibes and Lora","heading":"great_vibes","body":"lora","accent":""}],"defaults":{"palette":"blush","font":"cormorant"},"background":null}',
     '', NULL, now()),
    ('01926a00-0000-7000-8000-000000000003', 2,
     '{"schema":1,"layout":"split","hero_style":"full_bleed","decoration":"none","surface":"card","texture":"none","heading_scale":"display","palettes":[{"id":"mono","name":"Mono","colors":{"background":"#FFFFFF","surface":"#F4F4F4","text":"#111111","muted":"#666666","accent":"#111111","accent_text":"#FFFFFF"}},{"id":"ocean","name":"Ocean","colors":{"background":"#F1F7FA","surface":"#FFFFFF","text":"#0E2A38","muted":"#4F6B78","accent":"#0F6E8C","accent_text":"#FFFFFF"}},{"id":"terracotta","name":"Terracotta","colors":{"background":"#FAF3EE","surface":"#FFFFFF","text":"#2E1D14","muted":"#7A5F50","accent":"#A8502C","accent_text":"#FFFFFF"}}],"fonts":[{"id":"grotesque","name":"Bricolage and Figtree","heading":"bricolage_grotesque","body":"figtree","accent":""},{"id":"serif","name":"DM Serif and Figtree","heading":"dm_serif_display","body":"figtree","accent":""}],"defaults":{"palette":"mono","font":"grotesque"},"background":null}',
     '', NULL, now()),
    ('01926a00-0000-7000-8000-000000000004', 2,
     '{"schema":1,"layout":"centered","hero_style":"text_only","decoration":"dots","surface":"card","texture":"dots","heading_scale":"regular","palettes":[{"id":"sunshine","name":"Sunshine","colors":{"background":"#FFF9E6","surface":"#FFFFFF","text":"#2B2100","muted":"#6E6340","accent":"#F2C12E","accent_text":"#2B2100"}},{"id":"bubblegum","name":"Bubblegum","colors":{"background":"#FFF1F7","surface":"#FFFFFF","text":"#3A0F24","muted":"#7D5468","accent":"#C2185B","accent_text":"#FFFFFF"}},{"id":"mint","name":"Mint","colors":{"background":"#EFFAF5","surface":"#FFFFFF","text":"#0F2E22","muted":"#4E6E61","accent":"#0E7A55","accent_text":"#FFFFFF"}}],"fonts":[{"id":"fraunces","name":"Fraunces and Figtree","heading":"fraunces","body":"figtree","accent":""},{"id":"grotesque","name":"Bricolage and Figtree","heading":"bricolage_grotesque","body":"figtree","accent":""}],"defaults":{"palette":"sunshine","font":"fraunces"},"background":null}',
     '', NULL, now());

-- +goose Down

-- Dev only, same pattern as 00003. Fails by FK if events pin Heirloom v1 or any v2 (events ->
-- template_versions is NO ACTION), which is intended: repin or delete those events first.
-- Events whose content already uses people, video or wishes blocks fail ParseStored after Down.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions
WHERE (template_id = '01926a00-0000-7000-8000-000000000006' AND version = 1)
   OR (template_id IN ('01926a00-0000-7000-8000-000000000001', '01926a00-0000-7000-8000-000000000002',
                       '01926a00-0000-7000-8000-000000000003', '01926a00-0000-7000-8000-000000000004')
       AND version = 2);
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
DELETE FROM templates WHERE id = '01926a00-0000-7000-8000-000000000006';

UPDATE occasions
SET optional_blocks = (
        SELECT coalesce(jsonb_agg(e.value ORDER BY e.ord), '[]'::jsonb)
        FROM jsonb_array_elements(optional_blocks) WITH ORDINALITY AS e (value, ord)
        WHERE e.value NOT IN ('"people"'::jsonb, '"video"'::jsonb, '"wishes"'::jsonb)),
    updated_at = now()
WHERE optional_blocks ?| ARRAY['people', 'video', 'wishes'];
