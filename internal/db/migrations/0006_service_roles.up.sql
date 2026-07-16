-- Two narrow group roles so the scheduler and worker daemons run with least
-- privilege instead of the full kronwrk_admin they used to carry. The dedicated
-- `scheduler`/`worker` admin *login* roles go away; a daemon login is now a
-- member of exactly one of these narrow groups (via `kronwrk user add <name>
-- --role scheduler|worker`). kronwrk_admin still owns every table, so it keeps
-- full access to everything these roles touch.
--
-- Created idempotently — roles are cluster-wide, like the 0003 groups.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kronwrk_scheduler') THEN
        CREATE ROLE kronwrk_scheduler NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kronwrk_worker') THEN
        CREATE ROLE kronwrk_worker NOLOGIN;
    END IF;
END
$$;

-- scheduler: read + lock jobs and advance next_run_at (DueJobs SELECT;
-- EnqueueRun UPDATE jobs; the startup preflight's SELECT ... FOR UPDATE needs
-- table-level UPDATE on jobs), enqueue runs (INSERT job_runs), and log its own
-- lifecycle (INSERT service_events). No SELECT on job_runs — it only inserts.
GRANT SELECT, UPDATE ON jobs TO kronwrk_scheduler;
GRANT INSERT ON job_runs TO kronwrk_scheduler;
GRANT INSERT ON service_events TO kronwrk_scheduler;

-- worker: read jobs (GetJob), claim/heartbeat/finalize runs (ClaimRun's
-- SELECT ... FOR UPDATE SKIP LOCKED needs table-level UPDATE on job_runs, plus
-- the UPDATEs in Claim/Heartbeat/Finalize), and log its own lifecycle.
GRANT SELECT ON jobs TO kronwrk_worker;
GRANT SELECT, UPDATE ON job_runs TO kronwrk_worker;
GRANT INSERT ON service_events TO kronwrk_worker;
