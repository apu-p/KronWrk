-- EnqueueRun's INSERT ... ON CONFLICT (job_id, scheduled_for) DO NOTHING needs
-- SELECT on the arbiter-index columns to detect conflicts, but 0006 granted
-- the scheduler INSERT only — so every real enqueue failed with 42501 even
-- though the startup preflight's plain INSERT passed. Column-scoped so the
-- role stays narrow: the scheduler can match the unique key, not read run
-- results.
GRANT SELECT (job_id, scheduled_for) ON job_runs TO kronwrk_scheduler;
