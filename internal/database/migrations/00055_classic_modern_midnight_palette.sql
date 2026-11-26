-- +goose Up

-- A dark colour choice for Classic and Modern, so a host can pick a night-time invite (made with the On stage
-- occasion in mind). Minimal already ships a dark "Charcoal" palette.
--   Classic -> "Midnight gold": warm near-black with gold accents.
--   Modern  -> "Midnight":      cool near-black with a stage-light cyan accent.
-- Every text colour clears the manifest contrast checks (text 4.5:1 on background and surface, accent text 4.5:1
-- on accent, muted 3:1 on background).
--
-- Published template versions are immutable, so each template's latest published version is copied into a new
-- version with one palette appended; existing palette ids, defaults and fonts are unchanged, so events' choices
-- carry over and events pinned to the old version are offered the update in the editor. A no-op for a template
-- that has no published version or whose latest version already has the palette.
INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at)
SELECT v.template_id,
       (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = v.template_id) + 1,
       jsonb_set(v.manifest, '{palettes}', (v.manifest -> 'palettes') || CASE t.slug
         WHEN 'classic' THEN $p${"id":"midnight-gold","name":"Midnight gold","colors":{"background":"#14110D","surface":"#201A13","text":"#F4EBDD","muted":"#B5A894","accent":"#C9A15B","accent_text":"#14110D"}}$p$::jsonb
         ELSE $p${"id":"midnight","name":"Midnight","colors":{"background":"#0E1116","surface":"#181C23","text":"#F2F4F7","muted":"#9BA4B1","accent":"#5CC8D7","accent_text":"#0A1A1E"}}$p$::jsonb
       END),
       v.assets_path,
       NULL,
       now()
FROM template_versions v
JOIN templates t ON t.id = v.template_id
WHERE t.slug IN ('classic', 'modern')
  AND v.published_at IS NOT NULL
  AND v.version = (SELECT max(v3.version) FROM template_versions v3
                   WHERE v3.template_id = v.template_id AND v3.published_at IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(v.manifest -> 'palettes') AS p
                  WHERE p ->> 'id' IN ('midnight-gold', 'midnight'));

-- +goose Down

-- Dev only, same pattern as 00042: published versions are immutable, so the trigger is disabled for the delete.
-- Removes the latest version of Classic and Modern when it is the one this migration added (its last palette is
-- the midnight one). Fails while any event is still pinned to it (events -> template_versions foreign key).
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions nv
USING templates t
WHERE t.slug IN ('classic', 'modern')
  AND nv.template_id = t.id
  AND nv.version = (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = t.id)
  AND nv.manifest #>> '{palettes,-1,id}' IN ('midnight-gold', 'midnight');
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
