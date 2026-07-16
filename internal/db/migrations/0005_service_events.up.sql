-- service_events: audit log of long-lived service lifecycle. Each scheduler and
-- worker process appends a 'start' row when it comes up and a 'stop' row when it
-- shuts down gracefully, so operators can reconstruct daemon uptime from the DB.
CREATE TABLE service_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    service     TEXT        NOT NULL,   -- 'scheduler' | 'worker'
    instance_id TEXT        NOT NULL,   -- hostname-pid of the process
    event       TEXT        NOT NULL,   -- 'start' | 'stop'
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- History lookups are "events for this service, newest first".
CREATE INDEX idx_service_events_service ON service_events (service, created_at DESC);

-- Explicit ownership + grants per the RBAC convention: 0003's ALTER DEFAULT
-- PRIVILEGES only covers tables created by kronwrk_admin, but `migrate` may run
-- as the bootstrap superuser, so spell out the matrix. Owner (kronwrk_admin)
-- gets full DML implicitly; readers get SELECT. INSERT for the scheduler/worker
-- service roles is granted in 0006, once those roles exist.
ALTER TABLE service_events OWNER TO kronwrk_admin;
GRANT SELECT ON service_events TO kronwrk_user, kronwrk_support;
