-- Manual premium-template unlocks: an admin records an off-platform payment for one event+template.
-- The publish gate itself lives in PublishEvent / UpdateEventContent / RestoreEvent (events.sql).
-- Regions: 'IN' (INR, templates.price_paise), 'AE' (AED) and 'INT' (USD) via template_prices.
-- Amounts are minor units. A revoked payment is kept (revoked_at set) and no longer counts.

-- name: GrantEventPayment :one
-- Records a payment at the template's current price in the given region; amount and currency come from
-- the DB, never the caller. No row: the template is missing or not premium, the region is unknown or not
-- offered for it, the event already has a live grant for it, or expected_amount_minor is not the live
-- price for the region (the caller tells these apart with EventTemplatePaid and ListTemplatePrices).
-- expected_amount_minor is the price the admin confirmed, so a price edited meanwhile is never recorded.
-- A missing event raises 23503 on event_payments_event_id_fkey.
INSERT INTO event_payments (id, event_id, template_id, granted_by, region, currency, amount_minor, note)
SELECT @id::uuid, @event_id::uuid, t.id, sqlc.narg(granted_by)::uuid, @region::text,
       CASE WHEN @region::text = 'IN' THEN 'INR' ELSE p.currency END,
       CASE WHEN @region::text = 'IN' THEN t.price_paise ELSE p.amount_minor END,
       @note::text
FROM templates t
LEFT JOIN template_prices p ON p.template_id = t.id AND p.region = @region::text
WHERE t.id = @template_id::uuid
  AND t.is_premium
  AND (@region::text = 'IN' OR p.template_id IS NOT NULL)
  AND @expected_amount_minor::int = CASE WHEN @region::text = 'IN' THEN t.price_paise ELSE p.amount_minor END
ON CONFLICT (event_id, template_id) WHERE revoked_at IS NULL DO NOTHING
RETURNING *;

-- name: LockEventForPayment :one
-- Share-locks a live, owned event and returns its current template. A concurrent template switch
-- (SetEventFlags takes FOR NO KEY UPDATE) waits for the grant tx, so a grant cannot land on a template
-- the event has just left. No row: missing, deleted or an ownerless anonymous draft.
SELECT template_id FROM events
WHERE id = @event_id::uuid AND deleted_at IS NULL AND owner_id IS NOT NULL
FOR SHARE;

-- name: RevokeEventPayment :one
-- Soft-revokes the live grant (recorded by mistake, refunded). No row: there was no live grant. A
-- published event stays published; the gate applies to the next publish or template switch.
UPDATE event_payments
SET revoked_at = now(), revoked_by = @revoked_by::uuid
WHERE event_id = @event_id::uuid AND template_id = @template_id::uuid AND revoked_at IS NULL
RETURNING *;

-- name: EventTemplatePaid :one
SELECT EXISTS (SELECT 1 FROM event_payments p
               WHERE p.event_id = @event_id::uuid AND p.template_id = @template_id::uuid
                 AND p.revoked_at IS NULL) AS paid;

-- name: ListEventPayments :many
-- Live payments only, newest first; an event has at most a handful (one per template it has used).
SELECT id, event_id, template_id, granted_by, amount_minor, currency, method, note, created_at,
       region, revoked_at, revoked_by
FROM event_payments
WHERE event_id = @event_id::uuid AND revoked_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 50;

-- name: ListEventPaymentHistory :many
-- Same as ListEventPayments but includes revoked rows (audit view).
SELECT id, event_id, template_id, granted_by, amount_minor, currency, method, note, created_at,
       region, revoked_at, revoked_by
FROM event_payments
WHERE event_id = @event_id::uuid
ORDER BY created_at DESC, id DESC
LIMIT 50;

-- name: ListTemplatePrices :many
-- Non-India prices for a batch of templates (one round-trip for a whole list). The India price is
-- templates.price_paise. Callers pass a bounded id list.
SELECT template_id, region, currency, amount_minor
FROM template_prices
WHERE template_id = ANY(@template_ids::uuid[])
ORDER BY template_id, region;

-- name: UpsertTemplatePrice :one
-- Offers the template in a region at a price (admin). The currency is derived from the region; an
-- unknown region raises 23502/23514. Whether the template is premium is the caller's check.
INSERT INTO template_prices (template_id, region, currency, amount_minor)
VALUES (@template_id::uuid, @region::text,
        CASE @region::text WHEN 'AE' THEN 'AED' WHEN 'INT' THEN 'USD' END,
        @amount_minor::int)
ON CONFLICT (template_id, region) DO UPDATE SET amount_minor = EXCLUDED.amount_minor
RETURNING *;

-- name: DeleteTemplatePrice :one
-- Stops offering the template in a region (admin). No row: it was not offered. Recorded payments are
-- unaffected (they snapshot the amount).
DELETE FROM template_prices
WHERE template_id = @template_id::uuid AND region = @region::text
RETURNING *;
