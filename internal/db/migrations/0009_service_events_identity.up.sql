-- Record who started/stopped a daemon: the connected login (username) and its
-- kronwrk group role (role_name, e.g. kronwrk_scheduler). Both are captured
-- server-side at insert time by LogServiceEvent (current_user + a pg_auth_members
-- lookup), so they reflect the actual connection rather than anything the app
-- claims. Nullable: pre-existing rows have no identity, and a login outside the
-- role model (e.g. a superuser-run daemon) has no group role.
ALTER TABLE service_events
    ADD COLUMN username  TEXT,
    ADD COLUMN role_name TEXT;
