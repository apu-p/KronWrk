-- Revoke the job_runs grants this migration added to pre-existing roles;
-- DROP TABLE below only cleans up grants on the dropped tables. No roles are
-- created here, so no OWNER TO CURRENT_USER dance is needed (see 0003.down).
REVOKE UPDATE ON job_runs FROM kronwrk_scheduler;
REVOKE SELECT (id, status, wait_deadline) ON job_runs FROM kronwrk_scheduler;

-- events references job_runs (not the other way around), so drop order between
-- the two new tables is free.
DROP TABLE IF EXISTS job_conditions;
DROP TABLE IF EXISTS events;
ALTER TABLE job_runs DROP COLUMN IF EXISTS wait_deadline;
