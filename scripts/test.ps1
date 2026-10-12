# Runs the full Go test suite against a throwaway Postgres database so the
# dev database never collects test fixture rows. Needs `docker compose up -d --wait`.
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
$db = "syt_test_" + (Get-Random)
$pg = "seeyouthere-dev-postgres-1"

docker exec $pg psql -U seeyouthere -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE $db" | Out-Null
try {
    $env:DATABASE_URL = "postgres://seeyouthere:seeyouthere@localhost:5433/$db`?sslmode=disable"
    $env:VALKEY_URL = "redis://localhost:6380"
    go run ./cmd/migrate | Out-Null
    $pkgs = if ($args.Count) { $args } else { @("./...") }
    go test -count=1 @pkgs
    $code = $LASTEXITCODE
} finally {
    docker exec $pg psql -U seeyouthere -d postgres -c "DROP DATABASE IF EXISTS $db WITH (FORCE)" | Out-Null
}
exit $code
