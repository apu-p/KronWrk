-- Guard the job command surface against privilege escalation. The scheduler and
-- operator roles hold table-level UPDATE on jobs — required because their
-- SELECT ... FOR UPDATE row locks (preflight, enqueue) need table-level UPDATE
-- and column-scoping it breaks the lock. But table-level UPDATE also lets those
-- comparatively low-trust logins rewrite jobs.command / jobs.args, and a worker
-- then executes the rewritten command under the worker's OS user — turning
-- "runs the scheduler" or "on-duty operator" into arbitrary code execution on
-- any worker host.
--
-- No application path ever legitimately updates command or args (jobs are
-- immutable after `job add`; there is no `job edit`), so a BEFORE UPDATE trigger
-- that rejects changes to those columns from anyone outside kronwrk_admin closes
-- the hole without touching the grants the locking depends on. next_run_at /
-- enabled / updated_at updates (the scheduler's and support's real work) leave
-- command and args unchanged and pass through untouched.
CREATE OR REPLACE FUNCTION kronwrk_forbid_job_command_rewrite()
    RETURNS trigger
    LANGUAGE plpgsql
AS $$
BEGIN
    IF (NEW.command IS DISTINCT FROM OLD.command
        OR NEW.args IS DISTINCT FROM OLD.args)
       AND NOT pg_has_role(current_user, 'kronwrk_admin', 'MEMBER') THEN
        RAISE EXCEPTION 'permission denied: role % may not modify job command or args',
            current_user
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

ALTER FUNCTION kronwrk_forbid_job_command_rewrite() OWNER TO kronwrk_admin;

CREATE TRIGGER forbid_job_command_rewrite
    BEFORE UPDATE ON jobs
    FOR EACH ROW
    EXECUTE FUNCTION kronwrk_forbid_job_command_rewrite();
