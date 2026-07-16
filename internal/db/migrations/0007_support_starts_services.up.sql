-- Let the support role start/stop the scheduler and worker services, alongside
-- admin (which can already, as table owner). Granting the two narrow service
-- groups TO kronwrk_support makes its members inherit — transitively — exactly
-- the privileges the daemons' startup preflight and loops need (jobs
-- SELECT/UPDATE, job_runs SELECT/INSERT/UPDATE, service_events INSERT), without
-- widening the base support role's own column grants on jobs.
--
-- Note the escalation this implies: a support login can now write job_runs and
-- advance next_run_at, not just read + toggle jobs.enabled.
GRANT kronwrk_scheduler, kronwrk_worker TO kronwrk_support;
