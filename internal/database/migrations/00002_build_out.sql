-- +goose Up

-- Every table is still empty in every environment when this runs, so plain CREATE INDEX is fine here.

-- users ----------------------------------------------------------------------

ALTER TABLE users
    -- A secret with totp_confirmed_at NULL means enrolment is in progress.
    ADD COLUMN totp_confirmed_at timestamptz,
    -- Last accepted TOTP time step; a code is accepted only for a later step (replay protection).
    ADD COLUMN totp_last_step bigint NOT NULL DEFAULT 0 CONSTRAINT users_totp_last_step_check CHECK (totp_last_step >= 0),
    ADD CONSTRAINT users_name_len_check CHECK (char_length(name) <= 80),
    ADD CONSTRAINT users_email_len_check CHECK (char_length(email) <= 254),
    ADD CONSTRAINT users_totp_confirmed_check CHECK (totp_confirmed_at IS NULL OR totp_secret IS NOT NULL);

-- Admin user list, newest first (keyset), unfiltered and filtered by status.
CREATE INDEX users_created_idx ON users (created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX users_status_created_idx ON users (status, created_at DESC, id DESC) WHERE deleted_at IS NULL;

-- sessions -------------------------------------------------------------------

ALTER TABLE sessions
    -- Admin step-up: set when the session passes a TOTP check.
    ADD COLUMN mfa_verified_at timestamptz,
    ADD CONSTRAINT sessions_user_agent_len_check CHECK (char_length(user_agent) <= 512);

-- events ---------------------------------------------------------------------

ALTER TABLE events
    -- Optimistic concurrency for editor saves and settings changes.
    ADD COLUMN version int NOT NULL DEFAULT 1 CONSTRAINT events_version_check CHECK (version > 0),
    ADD COLUMN notify_rsvps boolean NOT NULL DEFAULT true,
    -- RSVP digest watermark; NULL means the host has never been notified.
    ADD COLUMN rsvp_notified_at timestamptz,
    ADD CONSTRAINT events_published_at_check CHECK (status <> 'published' OR published_at IS NOT NULL),
    -- Together with the 00001 check: a password hash exists exactly when visibility is 'password'.
    ADD CONSTRAINT events_password_hash_check CHECK (visibility = 'password' OR password_hash IS NULL),
    ADD CONSTRAINT events_title_len_check CHECK (char_length(title) <= 120),
    ADD CONSTRAINT events_content_size_check CHECK (pg_column_size(content) <= 262144),
    ADD CONSTRAINT events_overrides_size_check CHECK (pg_column_size(overrides) <= 4096);

DROP INDEX events_owner_id_idx;
-- Host dashboard (keyset, newest first) and per-owner counts. Users are never hard-deleted, so the
-- RESTRICT FK check that a partial index can't serve never runs in practice.
CREATE INDEX events_owner_created_idx ON events (owner_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
-- FK indexes missing in 00001.
CREATE INDEX events_template_idx ON events (template_id, template_version);
CREATE INDEX events_occasion_idx ON events (occasion_slug);
-- Purge of soft-deleted events.
CREATE INDEX events_purge_idx ON events (deleted_at) WHERE deleted_at IS NOT NULL;
-- Admin event list, newest first (includes deleted events), unfiltered and filtered by status.
-- A status filter over the first index would scan most of the table for rare statuses (taken_down).
CREATE INDEX events_admin_created_idx ON events (created_at DESC, id DESC);
CREATE INDEX events_admin_status_idx ON events (status, created_at DESC, id DESC);
-- Admin overview: events published in the last 7 days.
CREATE INDEX events_published_at_idx ON events (published_at) WHERE published_at IS NOT NULL;

-- +goose StatementBegin
-- A published slug may be printed on physical invitations; freeing it would let someone else take the URL.
CREATE FUNCTION events_slug_immutable() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF OLD.published_at IS NOT NULL AND NEW.slug IS DISTINCT FROM OLD.slug THEN
        RAISE EXCEPTION 'event slug cannot change after first publish'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'events_slug_immutable';
    END IF;
    RETURN NEW;
END;
$fn$;
-- +goose StatementEnd

CREATE TRIGGER events_slug_immutable
    BEFORE UPDATE OF slug ON events
    FOR EACH ROW EXECUTE FUNCTION events_slug_immutable();

-- +goose StatementBegin
-- event_role returns the caller's access to a live event ('owner', 'editor', 'viewer'), or NULL.
-- Used in the WHERE clause of every event-scoped query. With constant arguments it is a
-- pseudo-constant qual, so the planner evaluates it once (One-Time Filter), not per row.
CREATE FUNCTION event_role(p_event uuid, p_user uuid) RETURNS text
LANGUAGE sql STABLE PARALLEL SAFE AS $fn$
    SELECT CASE WHEN e.owner_id = p_user THEN 'owner' ELSE m.role END
    FROM events e
    LEFT JOIN event_members m ON m.event_id = e.id AND m.user_id = p_user
    WHERE e.id = p_event AND e.deleted_at IS NULL AND p_user IS NOT NULL
$fn$;
-- +goose StatementEnd

-- template_versions ----------------------------------------------------------

CREATE INDEX template_versions_created_by_idx ON template_versions (created_by);

-- +goose StatementBegin
-- Events pin a published version, so its content must never change or disappear.
-- created_by stays mutable so the ON DELETE SET NULL from users keeps working.
CREATE FUNCTION template_versions_immutable() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF OLD.published_at IS NOT NULL THEN
        IF TG_OP = 'DELETE'
           OR NEW.template_id IS DISTINCT FROM OLD.template_id
           OR NEW.version IS DISTINCT FROM OLD.version
           OR NEW.manifest IS DISTINCT FROM OLD.manifest
           OR NEW.assets_path IS DISTINCT FROM OLD.assets_path
           OR NEW.published_at IS DISTINCT FROM OLD.published_at THEN
            RAISE EXCEPTION 'published template versions are immutable'
                USING ERRCODE = 'check_violation', CONSTRAINT = 'template_versions_immutable';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$fn$;
-- +goose StatementEnd

CREATE TRIGGER template_versions_immutable
    BEFORE UPDATE OR DELETE ON template_versions
    FOR EACH ROW EXECUTE FUNCTION template_versions_immutable();

-- guests ---------------------------------------------------------------------

-- Invite tokens are derived from (id, token_version) with an HMAC and never stored.
ALTER TABLE guests
    DROP COLUMN invite_token,
    ADD COLUMN token_version int NOT NULL DEFAULT 1 CONSTRAINT guests_token_version_check CHECK (token_version > 0),
    -- Last time an invite email was queued.
    ADD COLUMN invited_at timestamptz,
    ADD CONSTRAINT guests_email_check CHECK (email IS NULL OR (email = lower(email) AND char_length(email) <= 254)),
    ADD CONSTRAINT guests_name_len_check CHECK (char_length(name) BETWEEN 1 AND 120),
    ADD CONSTRAINT guests_phone_check CHECK (phone IS NULL OR phone ~ '^\+?[0-9]{6,15}$');

-- One invite per address per event.
CREATE UNIQUE INDEX guests_event_email_key ON guests (event_id, email) WHERE email IS NOT NULL;
DROP INDEX guests_event_id_idx;
CREATE INDEX guests_event_created_idx ON guests (event_id, created_at, id);

-- rsvps ----------------------------------------------------------------------

-- Edit tokens are an HMAC over (id, edit_token_version) and never stored; bumping the version
-- revokes every issued link.
ALTER TABLE rsvps
    DROP COLUMN edit_token_hash,
    ADD COLUMN edit_token_version int NOT NULL DEFAULT 1 CONSTRAINT rsvps_edit_token_version_check CHECK (edit_token_version > 0),
    -- "no" means nobody comes; "yes"/"maybe" mean at least one person.
    ADD CONSTRAINT rsvps_attending_count_check CHECK ((attending = 'no') = (count = 0)),
    ADD CONSTRAINT rsvps_name_len_check CHECK (char_length(name) BETWEEN 1 AND 120),
    ADD CONSTRAINT rsvps_email_check CHECK (email IS NULL OR (email = lower(email) AND char_length(email) <= 254)),
    ADD CONSTRAINT rsvps_answers_size_check CHECK (pg_column_size(answers) <= 16384);

DROP INDEX rsvps_event_id_idx;
-- Host RSVP list and CSV export (keyset).
CREATE INDEX rsvps_event_created_idx ON rsvps (event_id, created_at, id);
-- FK index for the cascade from guests.
CREATE INDEX rsvps_guest_id_idx ON rsvps (guest_id) WHERE guest_id IS NOT NULL;
-- RSVP digest: rows changed since the watermark.
CREATE INDEX rsvps_event_updated_idx ON rsvps (event_id, updated_at);

-- media ----------------------------------------------------------------------

ALTER TABLE media
    -- Host media no longer referenced by the event content; purged after 7 days.
    ADD COLUMN detached_at timestamptz,
    -- Every media row belongs to an event; template assets are files only.
    ALTER COLUMN event_id SET NOT NULL,
    -- Rows are only written after our own renditions exist, so dimensions are always known.
    ALTER COLUMN width SET NOT NULL,
    ALTER COLUMN height SET NOT NULL,
    ADD CONSTRAINT media_dimensions_check CHECK (width > 0 AND height > 0),
    ADD CONSTRAINT media_guest_check CHECK (uploaded_by = 'guest' OR guest_id IS NULL),
    -- Only our re-encoded JPEG renditions are stored.
    ADD CONSTRAINT media_content_type_check CHECK (content_type = 'image/jpeg');

DROP INDEX media_event_id_idx;
CREATE INDEX media_event_created_idx ON media (event_id, created_at, id);
CREATE INDEX media_owner_id_idx ON media (owner_id);
CREATE INDEX media_guest_id_idx ON media (guest_id) WHERE guest_id IS NOT NULL;
CREATE INDEX media_detached_idx ON media (detached_at) WHERE detached_at IS NOT NULL;
-- Admin moderation queue by status, newest first; also serves the pending count, so it replaces
-- media_pending_idx (only guest uploads are ever pending).
DROP INDEX media_pending_idx;
CREATE INDEX media_guest_status_created_idx ON media (moderation_status, created_at DESC, id DESC) WHERE uploaded_by = 'guest';
-- Public guest photo wall (keyset, newest first).
CREATE INDEX media_event_approved_guest_idx ON media (event_id, created_at DESC, id DESC)
    WHERE uploaded_by = 'guest' AND moderation_status = 'approved';
-- Cleanup of rejected rows.
CREATE INDEX media_rejected_idx ON media (created_at) WHERE moderation_status = 'rejected';

-- reports --------------------------------------------------------------------

ALTER TABLE reports
    ADD CONSTRAINT reports_details_len_check CHECK (char_length(details) <= 1000);

-- One open report per event per reporter; duplicates are dropped silently.
CREATE UNIQUE INDEX reports_open_dedupe_key ON reports (event_id, reporter_ip_hash) WHERE status IN ('open', 'reviewing');
CREATE INDEX reports_handled_by_idx ON reports (handled_by);
-- Admin report queue by status (keyset, newest first) and the overview count.
DROP INDEX reports_open_idx;
CREATE INDEX reports_status_created_idx ON reports (status, created_at DESC, id DESC);

-- audit_log ------------------------------------------------------------------

-- Keyset views, newest first: global, by actor, by target. The last two replace the 00001 indexes,
-- which lacked id and needed a sort step.
CREATE INDEX audit_log_created_idx ON audit_log (created_at DESC, id DESC);
DROP INDEX audit_log_actor_idx;
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, created_at DESC, id DESC);
DROP INDEX audit_log_target_idx;
CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id, created_at DESC, id DESC);

-- slug_blocklist -------------------------------------------------------------

INSERT INTO slug_blocklist (term, reason) VALUES
    ('create', 'reserved'), ('edit', 'reserved'), ('new', 'reserved'), ('media', 'reserved'),
    ('invite', 'reserved'), ('rsvp', 'reserved'), ('events', 'reserved'), ('event', 'reserved'),
    ('dashboard', 'reserved'), ('robots', 'reserved'), ('sitemap', 'reserved'), ('favicon', 'reserved'),
    ('manifest', 'reserved'), ('www', 'reserved'), ('mail', 'reserved'), ('root', 'reserved'),
    ('null', 'reserved'), ('undefined', 'reserved'), ('moderator', 'reserved'), ('official', 'reserved'),
    ('auth', 'reserved'), ('oauth', 'reserved'),
    ('seeyouthere', 'brand'), ('seeuthere', 'brand');

-- +goose Down

DELETE FROM slug_blocklist WHERE term IN (
    'create', 'edit', 'new', 'media', 'invite', 'rsvp', 'events', 'event', 'dashboard', 'robots',
    'sitemap', 'favicon', 'manifest', 'www', 'mail', 'root', 'null', 'undefined', 'moderator',
    'official', 'auth', 'oauth', 'seeyouthere', 'seeuthere');

DROP INDEX audit_log_target_idx;
CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id, created_at);
DROP INDEX audit_log_actor_idx;
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, created_at);
DROP INDEX audit_log_created_idx;

DROP INDEX reports_status_created_idx;
CREATE INDEX reports_open_idx ON reports (created_at) WHERE status IN ('open', 'reviewing');
DROP INDEX reports_handled_by_idx;
DROP INDEX reports_open_dedupe_key;
ALTER TABLE reports DROP CONSTRAINT reports_details_len_check;

DROP INDEX media_rejected_idx;
DROP INDEX media_event_approved_guest_idx;
DROP INDEX media_guest_status_created_idx;
CREATE INDEX media_pending_idx ON media (created_at) WHERE moderation_status = 'pending';
DROP INDEX media_detached_idx;
DROP INDEX media_guest_id_idx;
DROP INDEX media_owner_id_idx;
DROP INDEX media_event_created_idx;
CREATE INDEX media_event_id_idx ON media (event_id);
ALTER TABLE media
    DROP CONSTRAINT media_content_type_check,
    DROP CONSTRAINT media_guest_check,
    DROP CONSTRAINT media_dimensions_check,
    ALTER COLUMN height DROP NOT NULL,
    ALTER COLUMN width DROP NOT NULL,
    ALTER COLUMN event_id DROP NOT NULL,
    DROP COLUMN detached_at;

-- Dev only: the dropped token columns come back nullable because their values can't be recreated.
DROP INDEX rsvps_event_updated_idx;
DROP INDEX rsvps_guest_id_idx;
DROP INDEX rsvps_event_created_idx;
CREATE INDEX rsvps_event_id_idx ON rsvps (event_id);
ALTER TABLE rsvps
    DROP CONSTRAINT rsvps_answers_size_check,
    DROP CONSTRAINT rsvps_email_check,
    DROP CONSTRAINT rsvps_name_len_check,
    DROP CONSTRAINT rsvps_attending_count_check,
    DROP COLUMN edit_token_version,
    ADD COLUMN edit_token_hash bytea UNIQUE;

DROP INDEX guests_event_created_idx;
CREATE INDEX guests_event_id_idx ON guests (event_id);
DROP INDEX guests_event_email_key;
ALTER TABLE guests
    DROP CONSTRAINT guests_phone_check,
    DROP CONSTRAINT guests_name_len_check,
    DROP CONSTRAINT guests_email_check,
    DROP COLUMN invited_at,
    DROP COLUMN token_version,
    ADD COLUMN invite_token text UNIQUE;

DROP TRIGGER template_versions_immutable ON template_versions;
DROP FUNCTION template_versions_immutable();
DROP INDEX template_versions_created_by_idx;

DROP FUNCTION event_role(uuid, uuid);
DROP TRIGGER events_slug_immutable ON events;
DROP FUNCTION events_slug_immutable();
DROP INDEX events_published_at_idx;
DROP INDEX events_admin_status_idx;
DROP INDEX events_admin_created_idx;
DROP INDEX events_purge_idx;
DROP INDEX events_occasion_idx;
DROP INDEX events_template_idx;
DROP INDEX events_owner_created_idx;
CREATE INDEX events_owner_id_idx ON events (owner_id) WHERE deleted_at IS NULL;
ALTER TABLE events
    DROP CONSTRAINT events_overrides_size_check,
    DROP CONSTRAINT events_content_size_check,
    DROP CONSTRAINT events_title_len_check,
    DROP CONSTRAINT events_password_hash_check,
    DROP CONSTRAINT events_published_at_check,
    DROP COLUMN rsvp_notified_at,
    DROP COLUMN notify_rsvps,
    DROP COLUMN version;

ALTER TABLE sessions
    DROP CONSTRAINT sessions_user_agent_len_check,
    DROP COLUMN mfa_verified_at;

DROP INDEX users_status_created_idx;
DROP INDEX users_created_idx;
ALTER TABLE users
    DROP CONSTRAINT users_totp_confirmed_check,
    DROP CONSTRAINT users_email_len_check,
    DROP CONSTRAINT users_name_len_check,
    DROP COLUMN totp_last_step,
    DROP COLUMN totp_confirmed_at;
