-- The relay reads a workspace's pending events in seq order (contracts/events.md "Relay").
-- Built CONCURRENTLY, alone in its file, so writes to events continue during the build.
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_unpublished_seq_idx ON events(workspace_id, seq) WHERE published_at IS NULL;
