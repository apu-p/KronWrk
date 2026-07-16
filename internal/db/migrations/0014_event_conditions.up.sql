-- Event-gated runs: a job with rows in job_conditions has its runs enqueued as
-- 'waiting' and promoted to 'queued' only once every condition has a matching
-- unconsumed event (consume-on-match: each event satisfies at most one run).
-- Jobs without conditions are unaffected — their runs are enqueued 'queued'
-- exactly as before.

-- events: emitted facts ("file landed", "upstream finished") that gate waiting
-- runs. consumed_by_run_id records which run an event satisfied; NULL = still
-- available. ON DELETE SET NULL: deleting a job (cascading its runs) returns
-- its consumed events to the pool — audit retention over strictness; job
-- deletion isn't exposed in the CLI today.
CREATE TABLE events (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name               TEXT        NOT NULL,
    payload            JSONB,
    emitted_by         TEXT        NOT NULL DEFAULT current_user,
    emitted_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumed_by_run_id BIGINT      REFERENCES job_runs (id) ON DELETE SET NULL
);

-- Promotion hot path: "oldest unconsumed event with this name".
CREATE INDEX idx_events_unconsumed ON events (name, emitted_at, id)
    WHERE consumed_by_run_id IS NULL;

-- job_conditions: which events a job's runs wait for. wait_seconds = 0 means
-- wait forever; otherwise the run is skipped wait_seconds after scheduled_for.
CREATE TABLE job_conditions (
    job_id       BIGINT      NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    event_name   TEXT        NOT NULL,
    wait_seconds INTEGER     NOT NULL DEFAULT 0 CHECK (wait_seconds >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (job_id, event_name)
);

-- When a conditioned run must give up waiting (NULL = wait forever). Computed
-- at enqueue as scheduled_for + max(wait_seconds) over the job's conditions;
-- any wait-forever condition (wait_seconds = 0) leaves it NULL.
ALTER TABLE job_runs ADD COLUMN wait_deadline TIMESTAMPTZ;

-- RBAC: explicit owner + grants per convention (0003's default privileges only
-- cover tables created by the role that ran 0003).
ALTER TABLE events OWNER TO kronwrk_admin;
ALTER TABLE job_conditions OWNER TO kronwrk_admin;

-- Observability: read-only everywhere.
GRANT SELECT ON events, job_conditions TO kronwrk_user, kronwrk_support, kronwrk_operator;

-- Emitting events: admin (owner) + operator. Operator also runs the scheduler
-- daemon, so it needs the consume side (UPDATE) too.
GRANT INSERT, UPDATE ON events TO kronwrk_operator;

-- Scheduler promotion/expiry: read conditions; lock + consume events; read the
-- gating columns of waiting runs and flip their status. UPDATE on job_runs is
-- table-level because the promotion's SELECT ... FOR UPDATE needs it (same
-- lesson as the jobs grant in 0006); SELECT stays column-scoped so the
-- scheduler still cannot read run results.
GRANT SELECT ON job_conditions TO kronwrk_scheduler;
GRANT SELECT, UPDATE ON events TO kronwrk_scheduler;
GRANT UPDATE ON job_runs TO kronwrk_scheduler;
GRANT SELECT (id, status, wait_deadline) ON job_runs TO kronwrk_scheduler;
-- (Column SELECT on job_runs (job_id, scheduled_for) already granted in 0008.)
