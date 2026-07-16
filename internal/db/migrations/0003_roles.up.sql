-- Access control: three NOLOGIN group roles enforced by Postgres itself.
-- Every human gets a LOGIN role (via `supercron user add`) that is a member of
-- exactly one group. The database — not the app — is the trust boundary.
--
--   supercron_admin   full DML, runs migrations/scheduler/worker
--   supercron_user    read-only (job list, run status)
--   supercron_support read + flip jobs.enabled (incident response)

-- Roles are cluster-wide, so create them idempotently: a second database in the
-- same cluster (or a rerun after a partial failure) must not error.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'supercron_admin') THEN
        CREATE ROLE supercron_admin NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'supercron_user') THEN
        CREATE ROLE supercron_user NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'supercron_support') THEN
        CREATE ROLE supercron_support NOLOGIN;
    END IF;
END
$$;

-- user: read-only.
GRANT SELECT ON jobs, job_runs TO supercron_user;

-- support: read everything, plus enable/disable jobs. DisableJob/EnableJob
-- update exactly (enabled, updated_at); grant exactly those columns.
GRANT SELECT ON jobs, job_runs TO supercron_support;
GRANT UPDATE (enabled, updated_at) ON jobs TO supercron_support;

-- admin: full DML, plus the migrations bookkeeping table (golang-migrate
-- TRUNCATEs + INSERTs schema_migrations on every apply).
GRANT SELECT, INSERT, UPDATE, DELETE ON jobs, job_runs TO supercron_admin;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON schema_migrations TO supercron_admin;
-- Identity columns need no sequence grants, but this is free insurance should a
-- serial or explicit sequence ever appear.
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO supercron_admin;

-- Future DDL migrations (ALTER TABLE ...) require ownership, and ownership
-- checks pass through role membership. Hand the tables to the admin group so
-- admin members can run `supercron migrate`.
ALTER TABLE jobs OWNER TO supercron_admin;
ALTER TABLE job_runs OWNER TO supercron_admin;
GRANT CREATE ON SCHEMA public TO supercron_admin;

-- Tables created by future migrations inherit the matrix — but note this only
-- covers tables created by the role running THIS migration. Convention: every
-- migration that creates a table also adds explicit GRANTs + OWNER TO.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT ON TABLES TO supercron_user, supercron_support;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO supercron_admin;
