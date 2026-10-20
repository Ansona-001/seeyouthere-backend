-- +goose Up

-- Mimosa plays a soft music-box loop on the public page (with a mute button): the new optional
-- schema-2 knob music = "music_box". Published template versions are immutable, so the latest
-- published Mimosa version is copied into a new version with only that field added; palette and
-- font-pair ids are unchanged, so an event's choices carry over. Events pinned to the old version
-- are offered the update in the editor. A no-op when there is no published Mimosa version.
INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at)
SELECT v.template_id,
       (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = v.template_id) + 1,
       jsonb_set(v.manifest, '{music}', '"music_box"'),
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
-- Dev only, same pattern as 00027 (published versions are immutable, so the trigger is disabled for
-- the delete). Removes the latest Mimosa version when it is the one this migration added (music is
-- music_box and the previous version has no music). Fails while any event is still pinned to it
-- (events -> template_versions foreign key).
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions nv
USING template_versions ov, templates t
WHERE t.slug = 'mimosa'
  AND nv.template_id = t.id
  AND ov.template_id = t.id
  AND ov.version = nv.version - 1
  AND nv.version = (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = t.id)
  AND nv.manifest->>'music' = 'music_box'
  AND coalesce(ov.manifest->>'music', '') = '';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
