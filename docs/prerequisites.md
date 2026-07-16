# Prerequisites

Prerequisites for the stack described in [stack_explanation.md](stack_explanation.md).

## Must install

- **Go** — builds the scheduler, worker, and CLI.
- **PostgreSQL** — database and coordination layer.

## Usually needed

- **git** — source control and dependency pulls.
- **Job runtimes** — whatever the scheduled jobs invoke (`sh`, `bash`, `python`, `node`, `java`, …). The server must have these.

## Added via Go modules (not system installs)

- **Cobra** — CLI framework.
- **pgx** — PostgreSQL driver.
- **Cron library** (`robfig/cron` or `adhocore/gronx`) — schedule evaluation.
- **Migrator** (`goose`, `tern`, or `golang-migrate` as a library) — schema migrations, embedded in the binary; no separate CLI to install.
- **zerolog** — only if chosen over stdlib `slog`.
- **Viper** — not needed for v1; use env vars.

`slog`, goroutines, and channels are part of Go itself — no separate install.

## Optional but recommended

- **make** — build/test/migration shortcuts.
- **psql** — manual database inspection.
- **systemd** (or another process manager) — run scheduler/workers reliably on Linux.
- **Docker** — local PostgreSQL and reproducible dev environments.

## By environment

**Development:** Go, PostgreSQL or Docker, git; optionally psql and make.

**Production:** the compiled binary, a running PostgreSQL instance, job runtimes; optionally a process manager.

## Short version

At minimum: **Go and PostgreSQL**. Cobra, pgx, a cron library, and an in-app migrator come through Go modules — no migration CLI to install. Viper is not needed for v1.
