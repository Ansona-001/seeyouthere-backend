-- +goose Up

-- Theme engine v2: an explicit picker order for templates and a size cap on manifests.
--
-- Both tables are tiny (a handful of curated templates plus their versions), so the plain ALTERs
-- below take their ACCESS EXCLUSIVE locks for milliseconds and no NOT VALID / CONCURRENTLY dance
-- is needed.

-- Host-picker order: ListPublishedTemplates sorts by (sort_order, name, id). Seeded templates use
-- spaced values (1010, 1020, ...) so later curated ones can be slotted in between without a
-- renumber; the default 0 lists a newly created template first until an admin places it.
ALTER TABLE templates ADD COLUMN sort_order int NOT NULL DEFAULT 0;

UPDATE templates
SET sort_order = CASE slug
        WHEN 'classic'  THEN 1010
        WHEN 'garden'   THEN 1020
        WHEN 'heirloom' THEN 1030
        WHEN 'modern'   THEN 1040
        WHEN 'confetti' THEN 1050
        WHEN 'minimal'  THEN 1060
    END,
    updated_at = now()
WHERE slug IN ('classic', 'garden', 'heirloom', 'modern', 'confetti', 'minimal');

-- Backstop for the manifest size cap that the API also enforces (Go is the primary check and gives
-- the friendly error). pg_column_size measures the jsonb datum, which is the binary form, not the
-- JSON text: a little larger than the compact text for manifests with many small values. While a
-- row is being written the datum is not yet compressed or moved out of line, so the check sees the
-- full size; on an UPDATE that leaves manifest untouched it may see the smaller compressed size,
-- which can only let an already-stored value through, never reject one. Existing rows are far below
-- the cap (the largest is about 1.2 KB). Adding a CHECK validates by scanning and does not fire
-- the template_versions_immutable trigger, so published versions are unaffected.
ALTER TABLE template_versions
    ADD CONSTRAINT template_versions_manifest_size_check CHECK (pg_column_size(manifest) <= 32768);

-- +goose Down

ALTER TABLE template_versions DROP CONSTRAINT template_versions_manifest_size_check;
ALTER TABLE templates DROP COLUMN sort_order;
