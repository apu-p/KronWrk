# Kronwrk

A PostgreSQL-backed job scheduler, worker, and monitor with a CLI admin surface, written in Go.

Kronwrk runs shell commands and scripts on a schedule, records every execution, and survives restarts without double-running jobs. It is built as a small **control-plane service**, not a one-shot CLI: the same binary runs as long-lived `scheduler` and `worker` processes plus an interactive operator shell.

> 🤖 This project was built end-to-end with **agentic coding** using [Claude Code](https://claude.com/claude-code) — design discussions, implementation, security review, and hardening were all driven through AI pair-programming sessions, with [CLAUDE.md](CLAUDE.md) serving as the living architecture brief that keeps the agent aligned with the project's correctness invariants.

## How it works

The system has three roles that coordinate **only through PostgreSQL** — there is no direct RPC between processes. The database is both the source of truth and the coordination layer.

- **Scheduler** — decides *when* a job runs. Each tick it finds due jobs (`enabled AND next_run_at <= now()`), inserts a `queued` run, and advances `next_run_at`. It never executes anything. Jobs can be **event-gated**: a run is born `waiting` and is only promoted to `queued` once a matching event is emitted (`event emit`), with an optional wait deadline after which it is `skipped`.
- **Worker** — owns execution. A fixed-size pool claims `queued` runs (`FOR UPDATE SKIP LOCKED`), runs the command with a timeout, heartbeats a lease, and writes the terminal status.
- **CLI** — the operator surface for creating jobs and inspecting runs.

Two correctness guarantees are baked into the schema:

- **Idempotent scheduling** — `UNIQUE (job_id, scheduled_for)` + `INSERT ... ON CONFLICT DO NOTHING` means a scheduler restart (or brief overlap) can never create duplicate runs.
- **Safe claiming** — `FOR UPDATE SKIP LOCKED` lets multiple workers poll concurrently without ever double-executing a run.

Each scheduler and worker process also records its own **start** and graceful **stop** to a `service_events` audit table (tagged with a `hostname-pid` instance id), so daemon uptime is reconstructable from the database.

See [docs/stack_explanation.md](docs/stack_explanation.md) for the full design rationale and [CLAUDE.md](CLAUDE.md) for architecture notes.

## Prerequisites

- **Go** (1.26+)
- **PostgreSQL** (17 recommended)

On macOS with Homebrew, Postgres tools are keg-only and not on the default PATH:

```bash
brew install postgresql@17
brew services start postgresql@17
export PATH="/opt/homebrew/opt/postgresql@17/bin:$PATH"   # for psql / pg_isready
createdb kronwrk
```

The compiled binary talks to Postgres via the `pgx` library and does **not** need `psql` to run.

### Managing the Postgres server

The repo ships [`pg.sh`](pg.sh), a convenience wrapper around `brew services` for the keg-only `postgresql@17` (whose tools aren't on the default PATH). On start it blocks until the server actually accepts connections (`pg_isready`):

```bash
./pg.sh start      # start, wait until ready
./pg.sh stop       # stop
./pg.sh restart    # stop then start, wait until ready
./pg.sh status     # brew services state
```

It's a manual helper, not used by the app. You can also drive `brew services` directly (`brew services start|stop postgresql@17`, `brew services list`).

On a TTY, `kronwrk shell` also detects when Postgres isn't reachable and offers to run `brew services start postgresql@17` for you before prompting for login.

## Build

```bash
go build -o kronwrk ./cmd/kronwrk
```

## Quick start

```bash
# 1. Apply the database schema (run after any new migration; superuser over the local socket)
./kronwrk migrate

# 2. Open the interactive shell (bare ./kronwrk also works) and log in
./kronwrk shell
```

Everything else happens inside the shell (operator commands are shell-only — direct invocation errors out):

```
# 3. Start the daemons — they run detached with your session's credentials,
#    and service_events records that *you* started them (admin or operator role)
daemon start scheduler
daemon start worker

# 4. Create a job
job add --name hello --command /bin/echo --args "hello,world" --schedule "* * * * *"

# 5. Inspect
job list
run status 1
monitor          # interactive jobs + last-run table; Enter drills into a job's run history

# 6. Stop the daemons gracefully when done
daemon stop scheduler
daemon stop worker
```

First-time setup needs one bootstrap step as the Postgres superuser (creating the user-admin login) — see [Access control](#access-control) below.

For unattended (e.g. systemd) deployments, the direct long-running commands still exist and take credentials via `DATABASE_URL` — use a least-privilege service login, never a superuser or admin:

```bash
DATABASE_URL="postgres://<svc-login>:<password>@localhost:5432/kronwrk?sslmode=disable" ./kronwrk scheduler start
DATABASE_URL="postgres://<svc-login>:<password>@localhost:5432/kronwrk?sslmode=disable" ./kronwrk worker start
```

## Interactive use

Run `./kronwrk` with no arguments (or `./kronwrk shell`) to enter an interactive session with a login prompt, tab-completion, persistent history (`~/.kronwrk_history`, chmod 0600 with passwords redacted), and one shared database connection:

```
$ ./kronwrk
Kronwrk shell — type a command ('job list', 'help'), tab to complete, 'exit' to leave
scheduler ● up   worker ○ down
alice(admin) ▸ job list
alice(admin) ▸ job disable 4
alice(admin) ▸ exit
```

The prompt shows who you are connected as and your role, and the line above it always shows the current daemon state on this machine. `help` renders commands your role cannot use in faint text, annotated with what they require, and the shell refuses them before they ever reach the database.

If Postgres isn't running when the shell starts, it offers to start it for you (`brew services start postgresql@17`) before prompting for login.

Several commands also become guided wizards when run on a terminal without their flags:

- `job add` — form with validation and a **live preview of the next 3 run times** as you type the cron expression. Move between fields with ↑/↓ or Tab/Shift+Tab to edit any input before saving; Enter on the last field saves.
- `user add` / `user set-role` — arrow-key role picker, masked password entry
- `user remove` — asks for confirmation

Any wizard can be cancelled with **Esc** (or Ctrl+C) — nothing is committed, and you return to the shell prompt (or, at the login prompt, exit).

Type `logout` to end the current session and return to the login screen — you can then sign in as a different user or exit. `exit` (or `quit`, or Ctrl-D) leaves the shell.

With piped stdin (scripts, cron), all of this is disabled: missing flags are an error and nothing ever blocks on a prompt.

## Commands

Standalone (run directly):

| Command | Description |
|---------|-------------|
| `kronwrk shell` | Interactive session (also: run `kronwrk` with no arguments) |
| `kronwrk migrate` | Apply database schema migrations |
| `kronwrk scheduler start` | Start the scheduling loop (long-running; for unattended deployments) |
| `kronwrk worker start` | Start a worker that executes queued runs (long-running; for unattended deployments) |

Inside the shell (direct invocation errors out):

| Command | Description |
|---------|-------------|
| `job add` | Create a job definition (see flags below) |
| `job list` | List all job definitions |
| `job disable <id>` / `enable <id>` | Pause / resume a job |
| `job condition add <id> --event <name> [--wait 30m]` | Event-gate the job's runs; `--wait` bounds the wait (omitted = forever) |
| `job condition list <id>` / `remove <id> --event <name>` | Inspect / remove a job's event conditions |
| `event emit <name> [--payload '{"k":1}']` | Emit an event; satisfies waiting runs on the scheduler's next tick |
| `event list [--limit N]` | List events, newest first; consumed events show the run that took them |
| `run status <run-id>` | Show status, timestamps, and exit details for a run |
| `monitor` | Interactive jobs + last-run table; Enter drills into a job's run history |
| `daemon status` | Check whether the scheduler/worker are running on this machine |
| `daemon start <scheduler\|worker>` | Spawn a daemon detached with the session's credentials (audited) |
| `daemon stop <scheduler\|worker>` | Graceful SIGTERM (audited) |
| `user add <name> --role <role>` | Create a database user with one role (prompts for password) |
| `user list` / `set-role <name> --role <role>` / `remove <name>` | Manage users |
| `whoami` | Show the connected database user and Kronwrk role |
| `logout` | End the session; log in as a different user or exit |

### `job add` flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--name` | yes | — | Job name |
| `--command` | yes | — | Command/script to execute (use an absolute path) |
| `--schedule` | yes | — | 5-field cron expression (`min hour dom mon dow`) |
| `--args` | no | none | Comma-separated arguments |
| `--timezone` | no | system timezone | IANA timezone the schedule is evaluated in (e.g. `Asia/Kolkata`) |
| `--timeout` | no | `0` (use default) | Per-run timeout in seconds |
| `--comment` | no | none | Free-form purpose / change-request note (e.g. `CHG-1234: why/what`) |

A job is uniquely identified by `(name, command, schedule_expr)`; adding a duplicate is rejected.

## Configuration

All configuration is via environment variables — there is no config file. Defaults work for local development.

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://localhost:5432/kronwrk?sslmode=disable` | PostgreSQL connection string |
| `POLL_INTERVAL` | `5s` | How often the scheduler and worker poll |
| `WORKER_CONCURRENCY` | `5` | Number of concurrent execution slots per worker |
| `HEARTBEAT_INTERVAL` | `10s` | How often a running job renews its lease |
| `JOB_TIMEOUT_DEFAULT` | `1h` | Timeout for jobs that set `--timeout 0` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |

Prefix any command to override:

```bash
POLL_INTERVAL=2s WORKER_CONCURRENCY=10 LOG_LEVEL=debug ./kronwrk worker start
```

## Access control

Access is role-based and enforced by **PostgreSQL itself**, not by the app: Postgres group roles carry the GRANTs, every login role gets exactly **one** of them, and the database rejects anything outside that role's permissions — even from a modified binary or raw `psql`.

There are four assignable people roles (`admin`, `operator`, `support`, `user`) plus two narrow **service** roles (`scheduler`, `worker`) for unattended daemon logins:

| Capability | `admin` | `operator` | `support` | `user` | `scheduler` | `worker` |
|------------|---------|------------|-----------|--------|-------------|----------|
| `job list`, `run status`, `monitor`, `event list` | yes | yes | yes | yes | — | — |
| `job disable` / `enable` | yes | yes | yes | — | — | — |
| `job add`, `job condition add/remove` | yes | — | — | — | — | — |
| `event emit` | yes | yes | — | — | — | — |
| `daemon start/stop` (shell) | yes | yes | — | — | — | — |
| `scheduler start` (direct) | yes | yes | — | — | yes | — |
| `worker start` (direct) | yes | yes | — | — | — | yes |
| `migrate` | superuser | — | — | — | — | — |
| `user add/remove/set-role` | user-admin login only (see note) | — | — | — | — | — |

Users are created **inside the shell**, connected as the *user-admin* login — a person-named `admin` role that also holds `CREATEROLE` (bootstrapped once as the superuser; the exact SQL is in [CLAUDE.md](CLAUDE.md)):

```
alice(admin) ▸ user add bob --role user      # prompts for a password (hidden)
alice(admin) ▸ user add carol --role operator
alice(admin) ▸ user list
```

Each person then logs into the shell with their own username and password. Denied operations return a friendly error naming the connected role; `whoami` shows who you are connected as.

Notes:

- **The shell refuses superuser logins.** A superuser bypasses every GRANT, so it is confined to `migrate`, `psql`, and admin tooling — never interactive operation.
- **User management needs `CREATEROLE`** (role attributes are not inherited through group membership), so `user add/remove/set-role` require the user-admin login, not a plain admin member's.
- **Daemons are started from the interactive shell** (`daemon start <scheduler|worker>`, admin or operator role): the daemon connects as the person's own login, so `service_events` records who started or stopped it. The direct `scheduler|worker start` commands remain for unattended (systemd) deployments using a least-privilege login created with `user add <name> --role scheduler|worker` (the names `scheduler`/`worker` themselves are reserved as usernames, as are all six role names).
- **Passwords never reach the server in plaintext**: `user add` hashes the password client-side into a SCRAM-SHA-256 verifier before it is embedded in the `CREATE ROLE` DDL, so role statements are safe to appear in server logs.
- **Job commands are immutable after creation**: a `BEFORE UPDATE` trigger rejects any change to `jobs.command`/`jobs.args` from non-admin logins — so a compromised scheduler or operator credential (which legitimately holds `UPDATE` on jobs) cannot rewrite what a worker executes.
- **`pg_hba.conf` requires `scram-sha-256` for TCP connections** (`127.0.0.1`/`::1`), so passwords are real — only the local unix socket stays `trust` (the bootstrap superuser, for `migrate`/`psql`). Authorization (what a role may do) is enforced regardless.
- The `support` role's write access is a column-level grant on `jobs.enabled`/`updated_at` — it can pause and resume jobs during an incident but cannot alter what a job executes or touch the daemons. Around-the-clock daemon operation belongs to the `operator` role, whose grants cover exactly what both daemons need.

## Development

```bash
go test ./...        # run unit tests (DB-free)
go vet ./...         # static checks
gofmt -l .           # list unformatted files
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).

## Status

This is the **v1 walking skeleton** — fully working: scheduler, worker, the role-aware interactive shell (login, wizards, daemon control), the `monitor` TUI with run-history drill-down, event-gated scheduling, database-enforced RBAC across six roles, and a `service_events` audit log attributing every daemon start/stop to a person. Intentionally deferred (schema columns already exist): stale-lease reclaim, retries, overlap and misfire policy, `LISTEN/NOTIFY` low-latency wake-ups, per-run output capture, notification delivery, event GC, and a file-watcher daemon that emits events.
