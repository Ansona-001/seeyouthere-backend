-- +goose Up

-- Regional pricing for premium templates. Regions are a closed set: 'IN' (INR), 'AE' (AED) and
-- 'INT' (USD, everywhere else). Amounts are minor units (paise / fils / cents). The India price stays
-- in templates.price_paise (migration 00036); the other regions live in template_prices.

-- A row means "this region is offered for this template"; no row means it is not. Whether the template
-- is premium is checked in Go (a price row on a free template is inert: grants require is_premium).
-- ON DELETE CASCADE: a price list has no meaning without its template. The PK (template_id, region)
-- also serves as the template_id FK index.
CREATE TABLE template_prices (
    template_id  uuid NOT NULL REFERENCES templates (id) ON DELETE CASCADE,
    region       text NOT NULL CHECK (region IN ('AE', 'INT')),
    currency     text NOT NULL,
    amount_minor integer NOT NULL CHECK (amount_minor BETWEEN 100 AND 1000000),
    PRIMARY KEY (template_id, region),
    CONSTRAINT template_prices_region_currency CHECK (
        (region = 'AE' AND currency = 'AED') OR (region = 'INT' AND currency = 'USD'))
);

-- Existing premium templates are offered in both regions at the launch prices (AED 29, USD 9).
INSERT INTO template_prices (template_id, region, currency, amount_minor)
SELECT id, 'AE', 'AED', 2900 FROM templates WHERE is_premium
UNION ALL
SELECT id, 'INT', 'USD', 900 FROM templates WHERE is_premium;

-- event_payments: currency-neutral amount, the region it was sold in, and soft revoke.
-- All of these are metadata-only changes on PG 11+ (constant defaults, nullable columns); the CHECKs
-- scan a table that holds one row per manual sale.
ALTER TABLE event_payments RENAME COLUMN amount_paise TO amount_minor;
ALTER TABLE event_payments RENAME CONSTRAINT event_payments_amount_paise_check TO event_payments_amount_minor_check;

-- Existing rows are all India sales in INR, which is exactly what the 'IN' default records.
ALTER TABLE event_payments
    ADD COLUMN region text NOT NULL DEFAULT 'IN' CONSTRAINT event_payments_region_check CHECK (region IN ('IN', 'AE', 'INT'));

ALTER TABLE event_payments DROP CONSTRAINT event_payments_currency_check;
ALTER TABLE event_payments
    ADD CONSTRAINT event_payments_currency_check CHECK (currency IN ('INR', 'AED', 'USD')),
    ADD CONSTRAINT event_payments_region_currency CHECK (
        (region = 'IN' AND currency = 'INR') OR (region = 'AE' AND currency = 'AED') OR (region = 'INT' AND currency = 'USD'));

-- A revoked payment stays as an audit record instead of being deleted. revoked_by SET NULL: deleting
-- the admin's account must not delete or un-revoke the record, hence it can be NULL on a revoked row,
-- but a revoker without a revocation time is invalid.
ALTER TABLE event_payments
    ADD COLUMN revoked_at timestamptz,
    ADD COLUMN revoked_by uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD CONSTRAINT event_payments_revoked_check CHECK (revoked_by IS NULL OR revoked_at IS NOT NULL);

-- Partial index: nearly every row is NULL here, and it only serves the SET NULL lookup on user delete.
CREATE INDEX event_payments_revoked_by_idx ON event_payments (revoked_by) WHERE revoked_by IS NOT NULL;

-- At most one live (non-revoked) grant per (event, template); a revoked row does not block a re-grant.
-- The publish gate and ON CONFLICT in GrantEventPayment use this index. The plain index replaces the
-- old unique one for the event_id FK (SET NULL on event purge) and for history scans that include
-- revoked rows; a partial index cannot serve those.
DROP INDEX event_payments_event_template_key;
CREATE UNIQUE INDEX event_payments_live_key ON event_payments (event_id, template_id) WHERE revoked_at IS NULL;
CREATE INDEX event_payments_event_template_idx ON event_payments (event_id, template_id);

-- +goose Down
-- Non-India payments cannot be represented by the old schema, and the old schema hard-deleted revoked
-- rows. Rolling back would destroy financial records, so refuse instead of dropping them.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM event_payments WHERE region <> 'IN' OR revoked_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 00037: event_payments holds non-IN or revoked rows, and a rollback would destroy those financial records';
    END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX event_payments_event_template_idx;
DROP INDEX event_payments_live_key;
CREATE UNIQUE INDEX event_payments_event_template_key ON event_payments (event_id, template_id);

DROP INDEX event_payments_revoked_by_idx;
ALTER TABLE event_payments
    DROP CONSTRAINT event_payments_revoked_check,
    DROP COLUMN revoked_by,
    DROP COLUMN revoked_at;

ALTER TABLE event_payments
    DROP CONSTRAINT event_payments_region_currency,
    DROP CONSTRAINT event_payments_currency_check,
    DROP COLUMN region;
ALTER TABLE event_payments ADD CONSTRAINT event_payments_currency_check CHECK (currency = 'INR');

ALTER TABLE event_payments RENAME CONSTRAINT event_payments_amount_minor_check TO event_payments_amount_paise_check;
ALTER TABLE event_payments RENAME COLUMN amount_minor TO amount_paise;

DROP TABLE template_prices;
