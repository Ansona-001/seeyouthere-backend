-- +goose Up

-- Mimosa section labels (the small accent-font kickers) get the theme's gold via the new optional
-- schema-2 knob ornament.label_ink. Gold text needs 4.5:1 on the sage page, so the art2 slot (the
-- yellow blossom colour #E2B93B, which no layer references: layers use art3, art6 and art7, and the
-- canopy is a raster) becomes a deep mimosa gold, #85690A (4.58:1 on the background, 4.96:1 on the
-- surface). Published template versions are immutable, so Mimosa's latest published version is copied
-- into a new version with those two changes; palette and font-pair ids are unchanged, so an event's
-- choices carry over. Events pinned to the old version are offered the update in the editor. A no-op
-- when there is no published Mimosa version.
INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at)
SELECT v.template_id,
       (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = v.template_id) + 1,
       jsonb_set(
           jsonb_set(v.manifest, '{palettes,0,art,1}', '"#85690A"'),
           '{ornament,label_ink}',
           '"art2"'),
       v.assets_path,
       NULL,
       now()
FROM template_versions v
JOIN templates t ON t.id = v.template_id
WHERE t.slug = 'mimosa'
  AND v.published_at IS NOT NULL
  AND v.version = (SELECT max(v3.version) FROM template_versions v3
                   WHERE v3.template_id = v.template_id AND v3.published_at IS NOT NULL);

-- +goose Down
-- Dev only, same pattern as 00025 and 00026 (published versions are immutable, so the trigger is
-- disabled for the delete). Removes Mimosa's latest version when it is the one this migration added
-- (it sets ornament.label_ink to art2 and the previous version has no label_ink). Fails while any
-- event is still pinned to it (events -> template_versions foreign key).
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions nv
USING template_versions ov, templates t
WHERE t.slug = 'mimosa'
  AND nv.template_id = t.id
  AND ov.template_id = t.id
  AND ov.version = nv.version - 1
  AND nv.version = (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = t.id)
  AND nv.manifest->'ornament'->>'label_ink' = 'art2'
  AND coalesce(ov.manifest->'ornament'->>'label_ink', '') = '';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
