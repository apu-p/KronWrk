-- jobs: what should run and when.
CREATE TABLE jobs (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                TEXT        NOT NULL,
    command             TEXT        NOT NULL,
    args                TEXT[]      NOT NULL DEFAULT '{}',
    schedule_expr       TEXT        NOT NULL,
    timezone            TEXT        NOT NULL DEFAULT 'UTC',
    next_run_at         TIMESTAMPTZ,
    enabled             BOOLEAN     NOT NULL DEFAULT TRUE,
    allow_overlap       BOOLEAN     NOT NULL DEFAULT FALSE,
    max_concurrent_runs INTEGER     NOT NULL DEFAULT 1,
    misfire_policy      TEXT        NOT NULL DEFAULT 'skip',
    timeout_seconds     INTEGER     NOT NULL DEFAULT 0,
    max_retries         INTEGER     NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hot path for the scheduler: "which enabled jobs are due?"
CREATE INDEX idx_jobs_due ON jobs (enabled, next_run_at);

-- job_runs: one row per execution attempt; the operational truth of the system.
CREATE TABLE job_runs (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id            BIGINT      NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    -- Denormalized from jobs.name so run history is self-describing and preserves
    -- the name as it was at run time.
    job_name          TEXT        NOT NULL,
    scheduled_for     TIMESTAMPTZ NOT NULL,
    status            TEXT        NOT NULL DEFAULT 'queued',
    worker_id         TEXT,
    attempt           INTEGER     NOT NULL DEFAULT 1,
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    last_heartbeat_at TIMESTAMPTZ,
    lease_expires_at  TIMESTAMPTZ,
    exit_code         INTEGER,
    error_message     TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Idempotent scheduling: a given job can have at most one run per scheduled
    -- time. Combined with INSERT ... ON CONFLICT DO NOTHING this makes the
    -- scheduler safe to restart and safe to run briefly-overlapping.
    UNIQUE (job_id, scheduled_for)
);

-- Workers poll for queued runs; keep that lookup cheap.
CREATE INDEX idx_job_runs_status ON job_runs (status);
