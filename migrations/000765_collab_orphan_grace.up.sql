-- A synced message that no folder read in a pass still shows is kept until several later
-- passes still miss it (contracts/collab.md "Sync"): orphaned_at is when it was first missed,
-- orphan_passes how many passes in a row have missed it.
ALTER TABLE collab_messages
    ADD COLUMN orphaned_at timestamptz,
    ADD COLUMN orphan_passes smallint NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS collab_messages_orphaned_idx ON collab_messages(account_id) WHERE orphaned_at IS NOT NULL;

-- A conversation whose mail is all gone keeps its comments, assignees and shares: it is set
-- aside (emptied_at) instead of deleted, with the Message-IDs it held, so the same mail
-- coming back joins it again ("Conversations").
ALTER TABLE collab_threads
    ADD COLUMN emptied_at timestamptz,
    ADD COLUMN gone_message_ids text[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS collab_threads_emptied_idx ON collab_threads(account_id) WHERE emptied_at IS NOT NULL;
