-- An AI call holds its estimate of the workspace's AI balance from before the model is
-- called until it is charged, so concurrent calls cannot spend the same balance twice. A
-- hold left by a call that never finished lapses at expires_at.
CREATE TABLE ai_balance_holds (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    micro_usd    bigint NOT NULL CHECK (micro_usd >= 0),
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_balance_holds_workspace_idx ON ai_balance_holds (workspace_id, expires_at);
