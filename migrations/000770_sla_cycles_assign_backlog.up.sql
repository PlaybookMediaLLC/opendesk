-- SLA cycles and pauses (contracts/api.md "SLA"). A ticket's targets run from the start of
-- its current cycle: its creation (sla_started_at null), else the moment a solved ticket was
-- reopened. sla_responded_at is the team's first reply in the cycle. While the ticket is
-- pending or on hold the resolution clock stands still from sla_paused_at; when it runs
-- again the time it stood still is added to sla_paused_minutes and to resolution_due_at.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS sla_started_at timestamptz;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS sla_responded_at timestamptz;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS sla_paused_at timestamptz;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS sla_paused_minutes integer NOT NULL DEFAULT 0;

-- Auto-assignment that found nobody eligible keeps the ticket waiting from this moment, and
-- a minute job assigns it once someone is (contracts/api.md "Assignment").
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS assign_pending_since timestamptz;

-- An address a visitor typed in the widget without a verified identity is the contact's,
-- unconfirmed: chat replies are emailed only to a confirmed address (contracts/teams.md
-- "Chat follow-up"). An agent setting the address, or mail from it, confirms it.
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS email_unconfirmed boolean NOT NULL DEFAULT false;
UPDATE contacts SET email_unconfirmed = true
    WHERE email IS NOT NULL AND external_id IS NULL AND custom->>'source' = 'widget';
