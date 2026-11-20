-- +goose Up

-- Whether a hosted event issues entry passes (the QR code shown on the page and promised in emails).
-- Hosts who only want a registration page can switch it off. Metadata-only ADD COLUMN (constant default);
-- every existing event keeps issuing passes. Invites never show passes, so the flag is ignored for them.
ALTER TABLE events ADD COLUMN entry_passes boolean NOT NULL DEFAULT true;

-- +goose Down

ALTER TABLE events DROP COLUMN entry_passes;
