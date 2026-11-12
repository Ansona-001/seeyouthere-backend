-- +goose Up

-- Premium templates are unlocked per event by an admin recording an off-platform payment (bank
-- transfer arranged over WhatsApp). No payment provider, no webhooks. A premium template can be
-- picked and edited freely; an event using it can only be published once a grant exists for the
-- (event, template) pair. Rows are enforced in PublishEvent / UpdateEventContent.

-- Current list price in paise (INR minor units; 59900 = INR 599). Only meaningful when is_premium.
-- Adding a NOT NULL column with a constant default is metadata-only on PG 11+ (no table rewrite).
ALTER TABLE templates
    ADD COLUMN price_paise integer NOT NULL DEFAULT 59900
    CONSTRAINT templates_price_paise_range CHECK (price_paise BETWEEN 100 AND 1000000);

-- One row per recorded payment. amount_paise snapshots the template price at grant time so later
-- price changes never rewrite history.
-- event_id SET NULL: this is a financial record and must outlive a purged event (retention purge or
-- admin delete). Orphan rows (event_id NULL) are exempt from the unique key below (NULLs are distinct).
-- template_id RESTRICT: a template with recorded payments must not be deletable.
-- granted_by SET NULL: deleting the admin's account must not delete the payment record.
CREATE TABLE event_payments (
    id           uuid PRIMARY KEY,
    event_id     uuid REFERENCES events (id) ON DELETE SET NULL,
    template_id  uuid NOT NULL REFERENCES templates (id) ON DELETE RESTRICT,
    granted_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    amount_paise integer NOT NULL CHECK (amount_paise BETWEEN 100 AND 1000000),
    currency     text NOT NULL DEFAULT 'INR' CHECK (currency = 'INR'),
    method       text NOT NULL DEFAULT 'manual' CHECK (method IN ('manual')),
    note         text NOT NULL DEFAULT '' CHECK (length(note) <= 200),
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- One grant per (event, template). Its leading column also serves the event_id FK (SET NULL) and
-- the publish-gate lookup (event_id, template_id).
CREATE UNIQUE INDEX event_payments_event_template_key ON event_payments (event_id, template_id);

-- FK indexes (RESTRICT check on template delete; SET NULL on user delete).
CREATE INDEX event_payments_template_id_idx ON event_payments (template_id);
CREATE INDEX event_payments_granted_by_idx ON event_payments (granted_by);

-- +goose Down
DROP INDEX event_payments_granted_by_idx;
DROP INDEX event_payments_template_id_idx;
DROP INDEX event_payments_event_template_key;
DROP TABLE event_payments;
ALTER TABLE templates DROP COLUMN price_paise;
