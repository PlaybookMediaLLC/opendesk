-- A webhook receives each event once: one delivery row per (webhook, event). Earlier copies
-- of the same pair are folded into the oldest before the constraint is added. Deliveries
-- are kept 30 days and written only by the webhook queue, which waits for the build.
DELETE FROM webhook_deliveries d
USING webhook_deliveries keep
WHERE keep.webhook_id = d.webhook_id AND keep.event_id = d.event_id
  AND (keep.created_at, keep.id) < (d.created_at, d.id);

CREATE UNIQUE INDEX IF NOT EXISTS webhook_deliveries_hook_event_key ON webhook_deliveries(webhook_id, event_id);
