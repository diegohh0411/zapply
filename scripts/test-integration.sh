#!/usr/bin/env bash
# Runs the full test suite against a throwaway Postgres container.
# Uses podman if available, else docker. The container is removed on exit.
set -euo pipefail

cd "$(dirname "$0")/.."
engine=$(command -v podman || command -v docker) || { echo "need podman or docker" >&2; exit 1; }
name="zapply-test-$$"
port=${ZAPPLY_TEST_PG_PORT:-55432}

trap '"$engine" rm -f "$name" >/dev/null 2>&1 || true' EXIT
"$engine" run -d --rm --name "$name" \
  -e POSTGRES_USER=zapply -e POSTGRES_PASSWORD=zapply -e POSTGRES_DB=zapply \
  -p "127.0.0.1:${port}:5432" \
  -v "$PWD/deploy/postgres/init.sql:/docker-entrypoint-initdb.d/00-roles.sql:ro,Z" \
  docker.io/library/postgres:17-alpine >/dev/null

# The entrypoint restarts Postgres after init scripts; wait for the final server.
for _ in $(seq 1 60); do
  if "$engine" exec "$name" psql -U zapply -d zapply -tAc "SELECT 1 FROM pg_roles WHERE rolname='zapply_app'" 2>/dev/null | grep -q 1 \
     && "$engine" logs "$name" 2>&1 | grep -q "PostgreSQL init process complete"; then
    break
  fi
  sleep 1
done
sleep 1

ZAPPLY_TEST_DATABASE_URL="postgres://zapply:zapply@127.0.0.1:${port}/zapply?sslmode=disable" \
  go test -race -count=1 "${@:-./...}"
