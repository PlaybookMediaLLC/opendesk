-- One Ollie run per conversation at a time, across every API process: a run claims the
-- conversation with a lease (run_token, run_until) and renews it as it goes. A lease left
-- by a process that stopped lapses at run_until.
ALTER TABLE assistant_sessions ADD COLUMN run_token uuid, ADD COLUMN run_until timestamptz;
