#!/usr/bin/env bash
# Live-PostgreSQL test runner.
#
# The postgres-backed tests self-skip unless SPECULA_TEST_POSTGRES_DSN is set, which is why
# they cost nothing in the hermetic loop. This script supplies that DSN:
#
#   - SPECULA_TEST_POSTGRES_DSN already set → use it as-is, provision nothing.
#   - otherwise                            → start a throwaway PostgreSQL container on a
#                                            FREE port and drop it (and its volume) on exit.
#
# Usage: scripts/test-postgres.sh
set -euo pipefail
cd "$(dirname "$0")/.."

PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"

# Free port, never a fixed one: a fixed port collides with any local PostgreSQL (or a
# previous run that leaked) and we would then either fail to bind or — far worse — run the
# suite against somebody's real database and truncate its tables.
pick_free_port() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}

if [[ -n "${SPECULA_TEST_POSTGRES_DSN:-}" ]]; then
  echo "==> using SPECULA_TEST_POSTGRES_DSN from the environment"
else
  command -v docker >/dev/null 2>&1 || {
    echo "test-postgres: docker not found and SPECULA_TEST_POSTGRES_DSN unset." >&2
    echo "               Install docker, or point SPECULA_TEST_POSTGRES_DSN at a database." >&2
    exit 1
  }
  PORT="$(pick_free_port)"
  CNAME="specula-test-pg-$$"
  echo "==> starting throwaway $PG_IMAGE as $CNAME on free port :$PORT"
  docker run -d --rm --name "$CNAME" \
    -e POSTGRES_PASSWORD=specula -e POSTGRES_USER=specula -e POSTGRES_DB=specula_test \
    -p "127.0.0.1:$PORT:5432" "$PG_IMAGE" >/dev/null
  # --rm frees the anonymous volume with the container, so no state survives the run.
  trap 'echo "==> removing $CNAME"; docker rm -f "$CNAME" >/dev/null 2>&1 || true' EXIT

  export SPECULA_TEST_POSTGRES_DSN="postgres://specula:specula@127.0.0.1:$PORT/specula_test?sslmode=disable"

  # Wait for readiness, and prove the container is still alive while doing so — a container
  # that died on startup would otherwise just look like a slow one until the timeout.
  echo "==> waiting for postgres to accept connections"
  for _ in $(seq 1 60); do
    if ! docker ps --format '{{.Names}}' | grep -qx "$CNAME"; then
      echo "==> FATAL: $CNAME exited during startup. Log:" >&2
      docker logs "$CNAME" 2>&1 | tail -30 >&2 || true
      exit 1
    fi
    if docker exec "$CNAME" pg_isready -U specula -d specula_test >/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done
  docker exec "$CNAME" pg_isready -U specula -d specula_test >/dev/null 2>&1 || {
    echo "==> FATAL: postgres never became ready. Log:" >&2
    docker logs "$CNAME" 2>&1 | tail -30 >&2 || true
    exit 1
  }
  echo "==> postgres ready on :$PORT"
fi

# -tags=postgres is forward-compat only; no file carries that tag today (see the Makefile
# comment on test-postgres). The real gate is the DSN env var above.
# -count=1: never serve these from the test cache.
echo "==> running live-postgres tests"
CGO_ENABLED=1 go test -tags=postgres -count=1 \
  ./internal/store/postgres/... ./internal/repo/...
