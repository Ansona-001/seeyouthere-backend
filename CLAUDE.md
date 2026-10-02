# Backend — Go API

Shared standards (security, DB, logging, model routing) live in `../CLAUDE.md`. Layout, API table and run instructions: `README.md`.

## Commands

```sh
docker compose up -d --wait                         # Postgres :5433, Valkey :6380
go run ./cmd/api                                    # needs env from .env; migrations run on startup
docker run --rm -v "$(pwd):/src" -w /src sqlc/sqlc generate   # after editing queries/ or migrations/
```

Verify before calling anything done (all must pass, fix don't suppress):

```sh
gofmt -l .                                          # must print nothing
go vet ./...
go build ./...
go test ./...                                       # add -race in Docker/CI (needs cgo)
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # after any dependency change
```

## Architecture

- `cmd/api/main.go` wires everything; `run()` returns errors, only `main` exits. Keep it that way.
- `internal/httpapi`: one `Server` struct holds dependencies; handlers are `func (s *Server) handleX(w, r)`. Routes registered in `server.go` — public under `/v1`, authenticated inside the `requireUser` group.
- `internal/store` is **sqlc-generated — never edit**. Change SQL in `internal/database/queries/*.sql` and regenerate.
- `internal/auth`, `internal/ratelimit`, `internal/mail`, `internal/jobs` are small focused packages. New domain areas get their own package under `internal/`; no `utils`/`common`/`helpers` packages.
- Background/slow work (email, cleanup, anything > ~100 ms not needed for the response) goes to River jobs, enqueued in the same transaction as the write.

## Go conventions

- Idiomatic Go per Effective Go + Go Code Review Comments. Package doc comment on every package; doc comment on exported identifiers.
- Errors: return, don't panic. Wrap with context `fmt.Errorf("load session: %w", err)`. Use `errors.Is/As`; map `pgx.ErrNoRows` to 404/`false` at the handler/service boundary.
- `context.Context` is the first param of anything doing I/O; never store it in a struct; always respect cancellation.
- Accept interfaces where a test needs a fake (see `mail.Sender`), otherwise concrete types. Define interfaces at the consumer.
- Handler shape: `decodeJSON` → validate/normalise → authorise → DB/tx → `writeJSON`/`writeError`. Stable snake_case error codes (`invalid_code`, `not_found`), human messages.
- New request/response types: explicit structs with `json` tags; never serialise sqlc models containing secrets/hashes directly.
- No global mutable state; config comes from `config.Config`.

## Performance (runs on a tiny VPS)

- One shared `pgxpool` and one Valkey client — never create per request. Keep pool size small (≈ 2–4 × vCPU).
- Every outbound call has a timeout (`context.WithTimeout`); every goroutine has an owner and an exit path.
- Bound all reads: `http.MaxBytesReader`, `LIMIT` on queries, max page size (≤ 100).
- Avoid allocations in hot paths (preallocate slices with known length, reuse buffers); don't use reflection-heavy libraries.
- Cache in Valkey only with explicit TTLs and a clear invalidation point; the session mirror is the pattern to follow.
- Measure before optimising: `go test -bench`, `pprof`, `EXPLAIN (ANALYZE, BUFFERS)`.

## Testing

- Table-driven tests with `t.Run`, standard `testing` only (see `internal/httpapi/httpapi_test.go`). `httptest` for handlers and middleware.
- Test every validation branch, auth failure (401/403), and error path — not just success.
- DB-dependent tests hit the real local Postgres (compose), wrapped in a tx that rolls back; skip with `t.Skip` when `DATABASE_URL` is unset.

## Gotchas

- Local toolchain is Go 1.24 while `go.mod` says 1.26 — `GOTOOLCHAIN=auto` downloads it on first run; don't downgrade `go.mod`.
- sqlc needs cgo, so run it via Docker (command above).
- Don't read `.env` / `deploy/.env` — use `.env.example` for variable names.
- `TRUST_PROXY=true` only behind Caddy; otherwise `X-Forwarded-For` is attacker-controlled.
