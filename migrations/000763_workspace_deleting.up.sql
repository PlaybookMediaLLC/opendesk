-- A large workspace whose deletion was asked for is closed at once and removed in the
-- background (core.workspace_purge); deleting_at marks it, and only such a workspace is purged.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS deleting_at timestamptz;
