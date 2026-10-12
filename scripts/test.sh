#!/usr/bin/env bash
# Runs the full Go test suite against a throwaway Postgres database so the
# dev database never collects test fixture rows. Needs `docker compose up -d --wait`.
set -euo pipefail

cd "$(dirname "$0")/.."
db="syt_test_$$"
pg="seeyouthere-dev-postgres-1"

docker exec "$pg" psql -U seeyouthere -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE $db" >/dev/null
trap 'docker exec "$pg" psql -U seeyouthere -d postgres -c "DROP DATABASE IF EXISTS $db WITH (FORCE)" >/dev/null' EXIT

export DATABASE_URL="postgres://seeyouthere:seeyouthere@localhost:5433/$db?sslmode=disable"
export VALKEY_URL="redis://localhost:6380"
go run ./cmd/migrate >/dev/null
go test -count=1 "${@:-./...}"
