#!/usr/bin/env bash
#
# pg.sh — manually start/stop the local Postgres used by Kronwrk.
#
# Not used by the app; a convenience wrapper for `brew services` around the
# keg-only postgresql@17 (whose tools aren't on the default PATH). On start it
# waits until the server actually accepts connections via pg_isready.
#
#   ./pg.sh start     start the server, block until it is ready
#   ./pg.sh stop      stop the server
#   ./pg.sh restart   stop then start
#   ./pg.sh status    show brew services state

set -euo pipefail

SERVICE="postgresql@17"
DB_URL="${DATABASE_URL:-postgres://localhost:5432/kronwrk?sslmode=disable}"

# postgresql@17 is keg-only; put its bin (pg_isready) on PATH for this script.
export PATH="/opt/homebrew/opt/${SERVICE}/bin:$PATH"

wait_ready() {
  echo "Waiting for Postgres to accept connections..."
  for _ in $(seq 1 20); do
    if pg_isready -q -d "$DB_URL"; then
      echo "Postgres is ready."
      return 0
    fi
    sleep 0.5
  done
  echo "Timed out waiting for Postgres to become ready." >&2
  return 1
}

case "${1:-}" in
  start)
    brew services start "$SERVICE"
    wait_ready
    ;;
  stop)
    brew services stop "$SERVICE"
    ;;
  restart)
    brew services stop "$SERVICE"
    brew services start "$SERVICE"
    wait_ready
    ;;
  status)
    brew services list | grep -E "Name|$SERVICE" || brew services list
    ;;
  *)
    echo "usage: $0 {start|stop|restart|status}" >&2
    exit 2
    ;;
esac
