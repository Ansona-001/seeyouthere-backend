-- +goose Up

-- Great Vibes draws its capital A as a looped swash that reads as "Cl" ("Alan" looks like "Clan"), which
-- is wrong for names. Published template versions are immutable, so every template whose latest
-- published version has a font pair with Great Vibes as the HEADING ships a new version that uses
-- Birthstone there instead (same ids, so an event's chosen font pair carries over; only the label
-- changes). Great Vibes as an accent font is left alone: it only sets short labels. New events use the
-- new version; events pinned to the old one are offered the update in the editor.
INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at)
SELECT v.template_id,
       v.version + 1,
       jsonb_set(
           v.manifest,
           '{fonts}',
           (SELECT jsonb_agg(
                       CASE WHEN f->>'heading' = 'great_vibes'
                            THEN jsonb_set(
                                     jsonb_set(f, '{heading}', '"birthstone"'),
                                     '{name}',
                                     to_jsonb(replace(f->>'name', 'Great Vibes', 'Birthstone')))
                            ELSE f END
                       ORDER BY ord)
            FROM jsonb_array_elements(v.manifest->'fonts') WITH ORDINALITY AS e(f, ord))),
       v.assets_path,
       NULL,
       now()
FROM template_versions v
WHERE v.published_at IS NOT NULL
  AND v.version = (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = v.template_id)
  AND EXISTS (SELECT 1 FROM jsonb_array_elements(v.manifest->'fonts') f WHERE f->>'heading' = 'great_vibes');

-- +goose Down
-- Dev only, same pattern as 00025 (published versions are immutable, so the trigger is disabled for the
-- delete). Removes the versions this migration added: the latest version of each template whose manifest now
-- uses Birthstone as a heading where the previous version used Great Vibes. Fails while any event is
-- still pinned to one of them (events -> template_versions foreign key).
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions nv
USING template_versions ov
WHERE ov.template_id = nv.template_id
  AND ov.version = nv.version - 1
  AND nv.version = (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = nv.template_id)
  AND EXISTS (SELECT 1 FROM jsonb_array_elements(ov.manifest->'fonts') f WHERE f->>'heading' = 'great_vibes')
  AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(nv.manifest->'fonts') f WHERE f->>'heading' = 'great_vibes');
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
