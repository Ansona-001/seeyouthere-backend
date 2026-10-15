-- +goose Up

-- Published template versions are immutable, so the lighter heading style ships as Heirloom v2:
-- v1's manifest with heading_scale "classic" (regular-weight serif under the script label, no
-- display glow). New events use v2; events pinned to v1 are offered the update in the editor.
INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at)
SELECT v.template_id, 2, jsonb_set(v.manifest, '{heading_scale}', '"classic"'), v.assets_path, NULL, now()
FROM template_versions v
JOIN templates t ON t.id = v.template_id
WHERE t.slug = 'heirloom' AND v.version = 1;

-- +goose Down
-- Fails while any event is still pinned to v2 (events -> template_versions foreign key).
DELETE FROM template_versions v
USING templates t
WHERE t.id = v.template_id AND t.slug = 'heirloom' AND v.version = 2;
