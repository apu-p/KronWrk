-- Revert 0007: support no longer inherits the daemon service groups. The
-- escalation (job_runs writes + next_run_at advance via kronwrk_scheduler/
-- kronwrk_worker membership) proved more confusing than useful — support goes
-- back to its base grants: SELECT everywhere + column UPDATE (enabled,
-- updated_at) on jobs. Daemon control becomes admin-only; support logins now
-- fail both daemons' startup preflights.
REVOKE kronwrk_scheduler, kronwrk_worker FROM kronwrk_support;
