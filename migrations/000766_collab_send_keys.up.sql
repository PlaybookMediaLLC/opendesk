-- A send's idempotency key (contracts/collab.md "POST /v1/collab/messages"): the same key from
-- the same person answers the message it already sent instead of sending it again. message_id
-- is set once the sent message is stored; a key whose send the provider refused is removed.
CREATE TABLE collab_send_keys (
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key          text NOT NULL,
    account_id   uuid NOT NULL REFERENCES collab_accounts(id) ON DELETE CASCADE,
    message_id   uuid REFERENCES collab_messages(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id, key)
);
CREATE INDEX collab_send_keys_created_idx ON collab_send_keys(created_at);
