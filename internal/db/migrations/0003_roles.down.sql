-- Statement order is load-bearing: supercron_admin OWNS jobs/job_runs after
-- 0003.up, and DROP OWNED drops owned objects — transferring ownership back
-- FIRST is what keeps the tables alive.
ALTER TABLE jobs OWNER TO CURRENT_USER;
ALTER TABLE job_runs OWNER TO CURRENT_USER;

ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT ON TABLES FROM supercron_user, supercron_support;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM supercron_admin;

-- Now the groups own nothing here, so this only revokes their remaining
-- privileges in this database.
DROP OWNED BY supercron_admin, supercron_user, supercron_support;

-- Dropping a group silently removes memberships; login roles created by
-- `supercron user add` survive but lose all SuperCron access. This still fails
-- loudly if another database in the cluster granted these roles privileges —
-- intentional: resolve that database first.
DROP ROLE IF EXISTS supercron_admin, supercron_user, supercron_support;
