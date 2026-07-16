-- kronwrk_operator: an assignable "runs the daemons" role, for on-duty staff
-- who start/stop the scheduler and worker around the clock without being
-- admins. Daemons started by an operator connect as that person's login, so
-- service_events attributes start/stop to them — tracking via the DB
-- connection identity, no app-side bookkeeping.
--
-- Direct grants (deliberately not group-to-group membership like the reverted
-- 0007, so the privilege story stays flat and readable): the union of what
-- both daemons' preflights and loops need, plus read access to observe them.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kronwrk_operator') THEN
        CREATE ROLE kronwrk_operator NOLOGIN;
    END IF;
END
$$;

-- Scheduler needs: SELECT + table-level UPDATE on jobs (DueJobs, next_run_at
-- advance, preflight's FOR UPDATE), INSERT on job_runs, and SELECT for the
-- ON CONFLICT arbiter columns. Worker needs: SELECT on jobs, SELECT + UPDATE
-- on job_runs (claim/heartbeat/finalize). Both log to service_events.
GRANT SELECT, UPDATE ON jobs TO kronwrk_operator;
GRANT SELECT, INSERT, UPDATE ON job_runs TO kronwrk_operator;
GRANT SELECT, INSERT ON service_events TO kronwrk_operator;
