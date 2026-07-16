# Kronwrk — recommended stack

A job scheduler and monitor with a CLI admin surface, built as a small service rather than a one-shot CLI.

## Framing

Treat this as a control-plane process, not just a CLI. Command parsing is easy; the hard parts are:

- scheduling correctly
- claiming work safely (no duplicate execution)
- recovering from crashes
- tracking job state over time

Architecture:

- a **scheduler** service that finds due jobs and creates run records
- one or more **worker** processes that execute runs
- a **monitor** that detects stuck/orphaned runs
- a **CLI** for operations and administration
- **PostgreSQL** as both system of record and coordination layer

## Why Go

Go is close to the textbook-correct choice here:

- **Concurrency** — scheduler loop, worker pool, monitor, and heartbeats map directly onto goroutines and channels.
- **Single static binary** — trivial deployment under systemd; no venvs, JVM, or Node runtime on the server.
- **Postgres tooling** — pgx and `FOR UPDATE SKIP LOCKED` patterns are idiomatic.
- **Low, predictable footprint** for a 24/7 process.

Most comparable tools (Kubernetes, Nomad, Temporal workers) are written in Go for these reasons.

Go is right **unless**: your team won't invest in Go; the jobs are tightly coupled to an existing Python/JS codebase; or you need durable multi-step workflows, DAGs, or approval steps (then use a workflow engine like Temporal, not a different language). Rust is the only serious alternative but is overkill at this scale — Go gives the better productivity-to-reliability balance.

## Stack

- **Go** — language
- **Cobra** — CLI
- **goroutines + channels** — concurrency
- **cron library** (`robfig/cron` or `adhocore/gronx`) — schedule evaluation
- **pgx** — PostgreSQL access, with hand-written SQL
- **in-app migrator** (`goose`, `tern`, or `golang-migrate` as a library) — schema migrations, compiled into the binary
- **env-based config** — Viper only later, if config grows
- **slog or zerolog** — structured logging

### Core approach

For v1, use PostgreSQL for both persistent state and coordination — deciding which jobs are due, which runs are queued, which worker claimed a run, and which runs are stale. One system to operate, simpler deployment, easier debugging.

Consider a dedicated queue/workflow engine (Temporal, RabbitMQ, Redis, Kafka) only if you need very high dispatch throughput, DAGs, large-scale event fan-out, or long-running orchestration with compensation. For running commands/scripts and recording history, Postgres-backed coordination is a solid start.

## Components

### Cobra (CLI)

The entry point is still a CLI — the operator interface. Cobra gives clean subcommands, flag parsing (`--db-url`, `--worker-id`), and auto-generated help. Example commands:

```
kronwrk scheduler start      # run the scheduling loop
kronwrk worker start         # run a worker
kronwrk job add              # create a job definition
kronwrk job disable          # mark a job inactive
kronwrk run status <run-id>  # show status, timestamps, exit details
```

### goroutines + channels (concurrency)

Pieces that shouldn't block each other:

- **Scheduler loop** — wakes periodically, finds due jobs, inserts queued runs.
- **Worker pool** — claims queued runs, executes, captures output, updates status.
- **Monitor** — flags runs with stale heartbeats as failed/timed-out.
- **Retry handler** — re-queues failed runs per policy.

Keep v1 simple: one scheduler loop, a fixed-size worker pool, one monitor loop. No heavy orchestration framework.

### pgx (PostgreSQL)

pgx is a fast, reliable, production-grade driver. It will connect, run transactions, fetch due jobs, claim queued runs, and update run state/timestamps/heartbeats.

**Use pgx with hand-written SQL.** It gives full control over the queries that matter most — `FOR UPDATE SKIP LOCKED` for claiming, `ON CONFLICT DO NOTHING` for idempotent scheduling. Keep them in a thin, well-named data-access layer. This avoids the overhead and hidden behavior of an ORM, at the cost of maintaining the query strings yourself.

#### Safe claiming

One of the easiest places to introduce duplicate-execution bugs. If multiple workers poll at once, only one should claim a queued run.

Recommended:

- `SELECT ... FOR UPDATE SKIP LOCKED` inside a transaction
- mark the row `running`, record `worker_id`, set heartbeat/lease fields

Alternative: `UPDATE ... WHERE status = 'queued' ... RETURNING *` so only the successful worker proceeds.

#### Polling vs. LISTEN/NOTIFY

v1 can poll on an interval — simple and correct. Once latency or DB load matters, add `LISTEN/NOTIFY`: the scheduler issues `NOTIFY` on enqueue and idle workers wake immediately. Keep a slow poll as a safety net so a missed notification can't strand a run.

### Schema migrations (in-app)

The schema will evolve (retry settings, lease data, notifications, per-run metadata). Migrations make changes versioned, repeatable, and consistent across dev/staging/prod. Postgres has transactional DDL but no built-in migration tracking — recording which migrations have run is what a migrator adds; git only versions the `.sql` files.

Use an **in-app migrator** rather than a separately installed CLI: embed the migration files in the binary and apply them on startup or via a `kronwrk migrate` subcommand. Libraries such as `goose`, `tern` (pgx family), or `golang-migrate`-as-a-library give proper applied-version tracking with no extra system binary to install. Use it for initial tables, indexes, constraints, and new columns.

### Configuration

Likely settings: `DATABASE_URL`, `POLL_INTERVAL`, `WORKER_CONCURRENCY`, `HEARTBEAT_INTERVAL`, `JOB_TIMEOUT_DEFAULT`, `LOG_LEVEL`.

For v1, **do not use Viper** — environment variables (parsed with stdlib or `kelseyhightower/envconfig`), optionally overridable by CLI flags. Add Viper only if you later need config files, profiles, or layered precedence.

The operational settings that matter most: poll interval, worker concurrency, heartbeat interval, lease timeout, default job timeout, graceful-shutdown timeout. Make these explicit and tunable.

### Logging

Structured logs carry fields like `job_id`, `run_id`, `worker_id`, `status`, `duration_ms`, which makes filtering and shipping to observability systems easy.

- **slog** — stdlib, simple, good default.
- **zerolog** — faster, strong JSON output; use if performance/machine-readable logs are a priority from the start.

## Suggested architecture

1. **CLI (Cobra)** — entry points: start scheduler/workers, create/inspect jobs, query runs.
2. **Scheduler** — decide *when* a job runs (not execute it); create queued runs; prevent duplicate scheduling.
3. **Worker** — claim runs, execute, capture logs, renew heartbeats/leases, update state. Owns execution while the run is active.
4. **Monitor** — detect stuck jobs and dead workers; mark runs timed-out/orphaned.
5. **PostgreSQL** — store definitions and history; back retries, heartbeats, leases, timeouts, status.

### How "due" is computed (core of the scheduler)

Don't parse cron yourself — use a maintained library. The scalable pattern stores a precomputed `next_run_at` per job instead of re-evaluating every expression each tick:

- on create/edit, compute `next_run_at` from `schedule_expr` + `timezone`
- the loop runs `SELECT jobs WHERE enabled AND next_run_at <= now()`
- for each due job, insert a `job_runs` row with `scheduled_for = next_run_at`
- advance `next_run_at` to the next occurrence in the same transaction

An index on `(enabled, next_run_at)` keeps the hot path to a single cheap query.

**Timezone/DST:** evaluate cron in the job's timezone and decide deliberately how to handle skipped (spring-forward) and duplicated (fall-back) wall-clock times. A good library handles most of this; the policy should still be a conscious choice.

### End-to-end flow

1. Operator creates a job (name, command, args, schedule, timeout, retry policy, enabled).
2. Scheduler finds it due → inserts a `queued` row in `job_runs`.
3. A worker claims the run → marks it `running`, records `worker_id`, `started_at`, heartbeat/lease.
4. Worker executes, writes logs, updates heartbeat, tracks exit code, enforces timeout.
5. Worker finishes → `succeeded` / `failed` / `timed_out` / `cancelled`.
6. Monitor catches abnormal cases: no heartbeat, crashed worker, stuck-queued, expired lease.

## PostgreSQL design

### `jobs` — what should run and when

Columns: `id`, `name`, `command`, `args`, `schedule_expr`, `timezone`, `next_run_at`, `enabled`, `allow_overlap`, `max_concurrent_runs`, `misfire_policy`, `timeout_seconds`, `max_retries`, `created_at`, `updated_at`.

- `next_run_at` — precomputed next occurrence; queried each tick and advanced transactionally. Index `(enabled, next_run_at)`.
- `allow_overlap` / `max_concurrent_runs` — control parallelism when a prior run is still active.
- `misfire_policy` — what to do for missed windows: `skip`, `run_once`, or `catch_up`.

### `job_runs` — one row per attempt (the operational truth)

Columns: `id`, `job_id`, `scheduled_for`, `status`, `worker_id`, `attempt`, `started_at`, `finished_at`, `last_heartbeat_at`, `lease_expires_at`, `exit_code`, `error_message`, `created_at`, `updated_at`.

Add `UNIQUE (job_id, scheduled_for)`. Combined with `INSERT ... ON CONFLICT DO NOTHING`, it makes scheduling idempotent and the scheduler safe to restart.

### `run_logs` — or external log storage

Option A: store lines in Postgres (`run_id`, `log_line`, `logged_at`) — simple but grows fast.
Option B: store only summaries (`status`, `exit_code`, `error_message`, short preview) and ship full logs elsewhere.

For v1, avoid unbounded raw logs in Postgres. Fine for small setups; expensive at volume.

### `workers` — optional, useful with multiple workers

Columns: `id`, `hostname`, `pid`, `started_at`, `last_seen_at`, `status`. Helps know which workers are alive and trace which executed a run.

### `notifications` — optional, pairs with the monitor

Alerts need a home and a delivery mechanism. Columns: `id`, `run_id`, `job_id`, `kind` (`failed`, `timed_out`, `orphaned`, `stale_lease`), `message`, `created_at`, `delivered_at`. Decouples detection (monitor writes a row) from delivery (a sender pushes to email/Slack/webhook), so alerts aren't lost if a channel is briefly down.

## Key design concerns

1. **Idempotency** — `UNIQUE (job_id, scheduled_for)` + `ON CONFLICT DO NOTHING`, with a deterministic `scheduled_for`. This single constraint is what makes restarts and brief scheduler overlap safe. State it explicitly.
2. **Job claiming** — `FOR UPDATE SKIP LOCKED` in a transaction; mark `running` and record `worker_id` immediately.
3. **Heartbeats + stale reclaim** — a run with `lease_expires_at < now()` is reclaimable; the monitor re-queues a new attempt or marks it failed, respecting `max_retries`. The monitor only changes DB state — it can't kill a remote process — so the worker enforces its own timeouts; the monitor is the backstop for a dead worker.
4. **Timeouts** — every job has a max runtime so a stuck process can't run forever.
5. **Retries** — explicit policy, tracked in the DB.
6. **Overlap policy** — when the next time arrives mid-run: allow, skip, or queue one delayed run. Set per job.
7. **Misfire handling** — if the scheduler was down: skip, one catch-up, or backfill. Affects correctness and load — be explicit.
8. **Graceful shutdown** — stop claiming new work, finish/hand off active work, flush final status, exit without corrupting state.
9. **Observability** — logs, timestamps, status transitions, worker ownership.
10. **Security / trust boundary** — executing DB-stored commands is RCE by design. Treat the database as the trust boundary (control who can create/edit jobs); run commands as a restricted OS user; exec the command and args directly instead of through a string-interpolated shell; keep secrets out of plain job rows and logs.

### Worker process management

Where execution bugs live:

- start each command in its own **process group** so a timeout can kill the whole group, not just the parent
- drive cancellation with a `context.Context` wired to the job timeout and graceful shutdown
- capture stdout/stderr into **bounded buffers** — unbounded capture of a chatty job exhausts memory
- distinguish normal exit codes from termination by signal, and record both

## Package layout

```
cmd/                Cobra commands and entry points
internal/config/    configuration loading
internal/db/        PostgreSQL connection and queries
internal/scheduler/ due-job detection and run creation
internal/worker/    job claiming and execution
internal/monitor/   heartbeat and timeout checks
internal/models/    domain structs (Job, JobRun)
migrations/         SQL migration files (embedded in the binary)
```

## When another approach fits

This stack suits jobs that are shell commands, scripts, or recurring batch tasks. Prefer a workflow engine (e.g. Temporal) for complex DAGs, cross-service orchestration, durable business workflows, human-approval steps, or replay/compensation logic.

Language alternatives: **Python** if your team is deeply invested or prototyping fast; **Rust** for strict performance/memory control; **Java/Kotlin** in a heavily JVM org. For most teams building this, Go wins on deployment, concurrency, and operational simplicity.

## Bottom line

Build a Go service with a CLI admin surface, using PostgreSQL as source of truth and coordination layer, pgx for access, an in-app migrator for schema, goroutines for workers/monitoring, and structured logging. Be explicit from day one about claiming strategy, heartbeat/lease renewal, the `UNIQUE (job_id, scheduled_for)` idempotency constraint, overlap policy, misfire handling, and graceful shutdown.
