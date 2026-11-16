ALTER TABLE contacts DROP COLUMN IF EXISTS email_unconfirmed;
ALTER TABLE tickets DROP COLUMN IF EXISTS assign_pending_since;
ALTER TABLE tickets DROP COLUMN IF EXISTS sla_paused_minutes;
ALTER TABLE tickets DROP COLUMN IF EXISTS sla_paused_at;
ALTER TABLE tickets DROP COLUMN IF EXISTS sla_responded_at;
ALTER TABLE tickets DROP COLUMN IF EXISTS sla_started_at;
