# Event-gated scheduling (migration 0014)

Jobs in Kronwrk are triggered by cron time. Event gating adds a second, opt-in
requirement in the Control-M/Autosys "condition" style: the schedule still
decides *when a cycle opens*, but a gated run executes only once a matching
**event** has also occurred (a file landed, an upstream batch finished). Time
alone is no longer sufficient for those jobs — and time-only jobs are
completely unaffected.

## Semantics

A job is gated by adding one or more **conditions** (`job condition add <id>
--event <name> [--wait <duration>]`). At the job's scheduled time the run is
enqueued as `waiting` instead of `queued`. Three timing cases, all handled:

| Case | Outcome |
|---|---|
| Event arrives **before** the scheduled time | Run is promoted in the same scheduler tick that enqueues it — fires right on schedule. |
| Event arrives **after** the scheduled time | Run is promoted within one poll interval of the event, as long as its wait deadline hasn't passed. |
| Event **never** arrives | Run becomes `skipped` (terminal, `finished_at` set) once `wait_deadline` passes. |

Rules that make this predictable:

- **Opt-in per job.** No `job_conditions` rows ⇒ runs are enqueued `queued`
  exactly as before. Removing a job's last condition un-gates its
  already-waiting runs on the next tick (a run with zero conditions is
  vacuously satisfied).
- **Consume-on-match.** Each event satisfies **at most one** run
  (`events.consumed_by_run_id`), oldest unconsumed event first — yesterday's
  file cannot also satisfy tomorrow's run. `event list` shows consumed events
  dimmed, with the run that took them.
- **Multi-condition = AND.** A run promotes only when *every* condition has an
  unconsumed matching event; all of them are consumed atomically.
- **Deadline.** `wait_deadline = scheduled_for + max(wait_seconds)` across the
  job's conditions; any condition with `--wait` omitted (`wait_seconds = 0`,
  wait forever) leaves the deadline NULL — the run waits indefinitely.
- **Promotion beats expiry.** Each tick runs promotion before expiry, so an
  event that is present at tick time wins over a deadline that lapsed within
  the same interval (e.g. after the scheduler was down or the machine slept).
- **Skipped is permanent.** `UNIQUE (job_id, scheduled_for)` (invariant 1)
  means a skipped occurrence cannot be re-run; the next cycle is the retry.

## How it works

Everything coordinates through Postgres, per the core model. **Workers are
untouched**: `ClaimRun` only ever selects `queued` rows, so `waiting` and
`skipped` runs are invisible to them.

- **Enqueue** (`Store.EnqueueRun`): inside the existing transaction, load the
  job's `wait_seconds` values and let `waitPlan`
  ([internal/db/conditions.go](../internal/db/conditions.go)) pick the initial
  status (`queued` or `waiting`) and deadline. The deterministic
  `scheduled_for` and `ON CONFLICT DO NOTHING` idempotency are unchanged.
- **Promotion** (`Store.PromoteWaitingRuns`, called each scheduler tick after
  the enqueue loop): snapshot waiting runs oldest-first, then per run in one
  transaction — re-lock the run and one unconsumed event per condition with
  `FOR UPDATE SKIP LOCKED` (the `ClaimRun` discipline, invariant 2), stamp
  `consumed_by_run_id`, flip the run to `queued`. Any unmet condition rolls
  back; concurrent schedulers can never double-consume an event or
  double-promote a run.
- **Expiry** (`Store.ExpireWaitingRuns`): one UPDATE flipping deadline-passed
  waiting runs to `skipped` with `finished_at = now()`.
- **Latency** is bounded by `POLL_INTERVAL` (default 5s). LISTEN/NOTIFY is
  deferred; if added later, NOTIFY must remain a lossy wake-up signal only —
  the `events` table stays the durable source of truth.

## Emitting events

Phase 1 has no watcher daemon: whatever produces the awaited thing emits the
event, via `event emit <name> [--payload '<json>']` in the shell (admin or
operator) or a plain `INSERT INTO events (name, payload)` by any login with
INSERT on `events`. For "file landed" this is usually *more* reliable than
filesystem watching — the producer knows when the file is complete.

A future `kronwrk watcher` daemon would translate fsnotify into event rows
(watching for completeness sentinels, scanning on startup for files that
arrived while it was down), following the same daemon conventions as
scheduler/worker. See the deferred list in [CLAUDE.md](../CLAUDE.md).

## RBAC

Same model as everything else — Postgres GRANTs enforce, `cmdPerms` mirrors
for shell UX:

- `events` / `job_conditions` owned by `kronwrk_admin`; SELECT to
  user/support/operator.
- `event emit`: admin + operator (`INSERT ON events`). Emitting is an
  operational act — it releases waiting runs.
- `job condition add/remove`: admin only (it changes scheduling behavior,
  like `job add`).
- `kronwrk_scheduler` gains SELECT on `job_conditions`, SELECT+UPDATE on
  `events`, table-level UPDATE on `job_runs` (its promotion
  `SELECT ... FOR UPDATE` needs table-level, the 0006/0008 lesson) — but its
  `job_runs` SELECT stays column-scoped (`id, status, wait_deadline` plus
  0008's `job_id, scheduled_for`), so the scheduler role still cannot read
  run results.

## Known limitations (phase 2 candidates)

- **No event GC.** Unconsumed events live forever and match with no lower
  time bound — an event emitted today can satisfy a run scheduled far later.
  Candidates: `event prune`, or a max-age on matching.
- **No watcher daemon** (see above).
- **No manual re-run** of a skipped occurrence — that would be a `job trigger`
  feature.
- **No NOTIFY wake-up** — promotion latency is one poll interval.
