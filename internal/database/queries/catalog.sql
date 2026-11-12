-- name: ListActiveOccasions :many
-- Tiny table; the full row feeds content.ParseOccasion.
SELECT * FROM occasions
WHERE is_active
ORDER BY sort_order, slug
LIMIT 100;

-- name: GetOccasion :one
-- For picking an occasion when creating a new event; deactivated occasions aren't offered.
SELECT * FROM occasions
WHERE slug = @slug AND is_active;

-- name: GetOccasionForEvent :one
-- For loading an existing event's occasion (e.g. rendering/editing it). No is_active filter:
-- an occasion can be deactivated for new events after events already reference it, and those
-- existing events must keep loading it instead of 500ing.
SELECT * FROM occasions
WHERE slug = @slug;

-- name: ListPublishedTemplates :many
-- Host picker: published templates (premium included, flagged with price) with their latest published
-- version. Premium templates can be picked and edited; publishing the event needs a payment grant.
-- occasion NULL lists every template. Picker order is templates.sort_order, then name.
SELECT t.id, t.slug, t.name, t.tags, t.is_premium, t.price_paise, v.version, v.manifest, v.assets_path
FROM templates t
JOIN LATERAL (
    SELECT tv.version, tv.manifest, tv.assets_path
    FROM template_versions tv
    WHERE tv.template_id = t.id AND tv.published_at IS NOT NULL
    ORDER BY tv.version DESC
    LIMIT 1
) v ON true
WHERE t.status = 'published'
  AND (sqlc.narg(occasion)::text IS NULL
       OR t.tags @> jsonb_build_object('occasions', jsonb_build_array(sqlc.narg(occasion)::text)))
ORDER BY t.sort_order, t.name, t.id
LIMIT 100;

-- name: GetPublishedTemplateBySlug :one
-- allow_premium is true wherever a premium template is acceptable (host editing, admin flags); false
-- keeps premium out of public/unauthenticated lookups.
SELECT t.id, t.slug, t.name, t.is_premium, t.price_paise, v.version, v.manifest, v.assets_path
FROM templates t
JOIN LATERAL (
    SELECT tv.version, tv.manifest, tv.assets_path
    FROM template_versions tv
    WHERE tv.template_id = t.id AND tv.published_at IS NOT NULL
    ORDER BY tv.version DESC
    LIMIT 1
) v ON true
WHERE t.slug = @slug
  AND t.status = 'published'
  AND (@allow_premium::bool OR NOT t.is_premium);

-- name: GetTemplateVersion :one
SELECT * FROM template_versions
WHERE template_id = @template_id AND version = @version;

-- name: GetTemplateAssetsPath :one
-- /media/templates/... serving gate (any version, published or not, so the admin preview works).
-- No row: unknown version or no asset uploaded yet. Narrow on purpose: GetTemplateVersion also
-- reads the manifest jsonb.
SELECT assets_path
FROM template_versions
WHERE template_id = @template_id AND version = @version AND assets_path <> '';
