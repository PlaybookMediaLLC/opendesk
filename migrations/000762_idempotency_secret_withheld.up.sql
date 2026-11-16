-- A response that carried a secret shown once is never stored: the key keeps only that
-- the request succeeded, and a retry is refused (contracts/api.md "Idempotency").
ALTER TABLE idempotency_keys ADD COLUMN IF NOT EXISTS secret boolean NOT NULL DEFAULT false;
