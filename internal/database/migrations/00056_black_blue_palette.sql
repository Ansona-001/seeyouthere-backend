-- +goose Up

-- A "Black and blue" colour choice for Classic, Modern and Minimal: true black paper, deep navy cards, an
-- electric-blue accent. Text colours clear the manifest contrast checks (text 4.5:1 on background and surface,
-- accent text 4.5:1 on accent, muted 3:1 on background).
--
-- Published template versions are immutable, so each template's latest published version is copied into a new
-- version with one palette appended; existing palette ids, defaults and fonts are unchanged. A no-op for a
-- template with no published version or whose latest version already has the palette.
INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at)
SELECT v.template_id,
       (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = v.template_id) + 1,
       jsonb_set(v.manifest, '{palettes}', (v.manifest -> 'palettes') ||
         $p${"id":"black-blue","name":"Black and blue","colors":{"background":"#05070D","surface":"#0E1424","text":"#F1F5FF","muted":"#9DB0D2","accent":"#3B82F6","accent_text":"#04101F"}}$p$::jsonb),
       v.assets_path,
       NULL,
       now()
FROM template_versions v
JOIN templates t ON t.id = v.template_id
WHERE t.slug IN ('classic', 'modern', 'minimal')
  AND v.published_at IS NOT NULL
  AND v.version = (SELECT max(v3.version) FROM template_versions v3
                   WHERE v3.template_id = v.template_id AND v3.published_at IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(v.manifest -> 'palettes') AS p WHERE p ->> 'id' = 'black-blue');

-- +goose Down

-- Dev only, same pattern as 00055: published versions are immutable, so the trigger is disabled for the delete.
-- Removes the latest version when it is the one this migration added. Fails while any event is pinned to it.
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions nv
USING templates t
WHERE t.slug IN ('classic', 'modern', 'minimal')
  AND nv.template_id = t.id
  AND nv.version = (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = t.id)
  AND nv.manifest #>> '{palettes,-1,id}' = 'black-blue';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
