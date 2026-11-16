DROP INDEX IF EXISTS collab_threads_emptied_idx;
ALTER TABLE collab_threads
    DROP COLUMN emptied_at,
    DROP COLUMN gone_message_ids;
DROP INDEX IF EXISTS collab_messages_orphaned_idx;
ALTER TABLE collab_messages
    DROP COLUMN orphaned_at,
    DROP COLUMN orphan_passes;
