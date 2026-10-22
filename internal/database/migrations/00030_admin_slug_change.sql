-- +goose Up
-- +goose StatementBegin
-- A published slug is immutable (it may be printed on invitations), except when an admin changes it through
-- the API. That path sets the transaction-local setting syt.slug_change = 'admin' just before the UPDATE;
-- set_config(..., true) lasts only until the transaction ends, so nothing else can ride on it.
CREATE OR REPLACE FUNCTION events_slug_immutable() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF OLD.published_at IS NOT NULL AND NEW.slug IS DISTINCT FROM OLD.slug
       AND coalesce(current_setting('syt.slug_change', true), '') <> 'admin' THEN
        RAISE EXCEPTION 'event slug cannot change after first publish'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'events_slug_immutable';
    END IF;
    RETURN NEW;
END;
$fn$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION events_slug_immutable() RETURNS trigger
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
