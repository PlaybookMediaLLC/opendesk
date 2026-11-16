-- The assignment backlog job reads the tickets waiting for someone eligible, oldest first.
-- Built without blocking writes to tickets; this file holds this one statement only.
CREATE INDEX CONCURRENTLY IF NOT EXISTS tickets_assign_pending_idx ON tickets (assign_pending_since) WHERE assign_pending_since IS NOT NULL;
