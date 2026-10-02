-- name: ListTemplatesAdmin :many
-- Every template with its newest version and newest published version (0 = none).
SELECT t.id, t.slug, t.name, t.tags, t.status, t.is_premium, t.created_at, t.updated_at,
       v.latest_version, v.latest_published_version
FROM templates t
CROSS JOIN LATERAL (
    SELECT coalesce(max(tv.version), 0)::int AS latest_version,
           coalesce(max(tv.version) FILTER (WHERE tv.published_at IS NOT NULL), 0)::int AS latest_published_version
    FROM template_versions tv
    WHERE tv.template_id = t.id
) v
ORDER BY t.name, t.id
LIMIT 200;

-- name: GetTemplateAdmin :one
SELECT * FROM templates WHERE id = @template_id;

-- name: CreateTemplate :one
-- A duplicate slug raises 23505 on templates_slug_key.
INSERT INTO templates (id, slug, name, tags, is_premium)
VALUES (@id, @slug, @name, @tags, @is_premium)
RETURNING *;

-- name: UpdateTemplate :one
-- NULL leaves a field unchanged. Setting status 'published' requires a published version; no row
-- then (or when the template is missing), which the caller maps with CountPublishedVersions.
WITH prev AS (
    SELECT x.id, x.name, x.tags, x.status, x.is_premium FROM templates x
    WHERE x.id = @template_id
    FOR NO KEY UPDATE
)
UPDATE templates t
SET name = coalesce(sqlc.narg(name), t.name),
    tags = coalesce(sqlc.narg(tags), t.tags),
    is_premium = coalesce(sqlc.narg(is_premium), t.is_premium),
    status = coalesce(sqlc.narg(status), t.status),
    updated_at = now()
FROM prev
WHERE t.id = prev.id
  AND (coalesce(sqlc.narg(status)::text, t.status) <> 'published'
       OR EXISTS (SELECT 1 FROM template_versions tv
                  WHERE tv.template_id = t.id AND tv.published_at IS NOT NULL))
RETURNING prev.name AS old_name, prev.tags AS old_tags, prev.status AS old_status, prev.is_premium AS old_is_premium,
          t.id, t.slug, t.name, t.tags, t.status, t.is_premium, t.created_at, t.updated_at;

-- name: ListTemplateVersions :many
-- Without manifests (wide); fetch one with GetTemplateVersion.
SELECT template_id, version, assets_path, created_by, created_at, published_at
FROM template_versions
WHERE template_id = @template_id
ORDER BY version DESC
LIMIT 100;

-- name: CreateTemplateVersion :one
-- New draft version n+1. Two concurrent creates collide on the primary key (23505) → 409 retry.
INSERT INTO template_versions (template_id, version, manifest, created_by)
SELECT @template_id::uuid, coalesce(max(tv.version), 0) + 1, @manifest::jsonb, sqlc.narg(created_by)::uuid
FROM template_versions tv
WHERE tv.template_id = @template_id::uuid
RETURNING template_id, version, assets_path, created_by, created_at, published_at;

-- name: UpdateTemplateVersionManifest :execrows
-- Zero rows: missing or already published (version_published).
UPDATE template_versions
SET manifest = @manifest
WHERE template_id = @template_id AND version = @version AND published_at IS NULL;

-- name: SetTemplateVersionAssets :execrows
UPDATE template_versions
SET assets_path = @assets_path
WHERE template_id = @template_id AND version = @version AND published_at IS NULL;

-- name: PublishTemplateVersion :one
UPDATE template_versions
SET published_at = now()
WHERE template_id = @template_id AND version = @version AND published_at IS NULL
RETURNING published_at::timestamptz AS published_at;

-- name: CountPublishedVersions :one
SELECT count(*) FROM template_versions
WHERE template_id = @template_id AND published_at IS NOT NULL;
