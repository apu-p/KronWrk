-- Revoke first: DROP ROLE fails while a role still holds privileges. These
-- groups own no objects, so no ownership transfer is needed (unlike 0003.down).
-- If a daemon LOGIN is still a member of one of these groups the DROP will fail
-- loudly — remove the daemon login first (`kronwrk user remove <name>`).
REVOKE ALL PRIVILEGES ON jobs, job_runs, service_events FROM kronwrk_scheduler, kronwrk_worker;
DROP ROLE IF EXISTS kronwrk_scheduler;
DROP ROLE IF EXISTS kronwrk_worker;
