# See You There — API

Go API for seeyouthere.at: chi + sqlc + pgx on PostgreSQL 16, Valkey for sessions and rate limits, River for background jobs.

## Run locally

```sh
docker compose up -d --wait        # Postgres :5433, Valkey :6380
cp .env.example .env                # fill in SMTP_* with your Zoho credentials
set -a; . ./.env; set +a           # PowerShell: Get-Content .env | % { if ($_ -match '^([^#=]+)=(.*)$') { Set-Item "env:$($matches[1])" $matches[2] } }
go run ./cmd/api                   # http://localhost:8080, migrations run on startup
```

The web app runs from `../seeyouthere-frontend` on http://localhost:3100.

## Layout

```
cmd/api/                 entry point: config, migrations, River workers, HTTP server
internal/config/         environment variables
internal/database/       pgx pool, goose migrations (embedded), River migrations
internal/database/queries/  SQL for sqlc
internal/store/          sqlc-generated code — do not edit
internal/auth/           email login codes (Valkey) and sessions (Postgres + Valkey mirror)
internal/httpapi/        chi routes, middleware, handlers
internal/jobs/           River jobs: email delivery, hourly cleanup
internal/mail/           SMTP sender (Zoho Mail)
internal/ratelimit/      fixed-window limiter in Valkey
deploy/                  production compose file and Caddyfile
```

## Regenerate sqlc code

sqlc needs cgo for the Postgres parser, so run it from Docker:

```sh
docker run --rm -v "$(pwd):/src" -w /src sqlc/sqlc generate
```

## API (v1)

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/healthz` | – | Liveness |
| GET | `/readyz` | – | Checks Postgres and Valkey |
| POST | `/v1/auth/code` | – | `{email}` → emails a 6-digit code. Always 202. 5/hour per email, 20/hour per IP |
| POST | `/v1/auth/verify` | – | `{email, code}` → creates the user on first login, sets `syt_session` cookie |
| POST | `/v1/auth/logout` | cookie | Revokes the session |
| GET | `/v1/me` | cookie | Current user and admin roles |

Errors look like `{"error": {"code": "invalid_code", "message": "..."}}`.

### Security notes

- Login codes: 10-minute expiry, 5 attempts, stored as an HMAC keyed by `AUTH_SECRET`.
- Session cookie holds a random token; only its SHA-256 is stored. HttpOnly, SameSite=Lax, Secure in production.
- CSRF: Go's `http.CrossOriginProtection` rejects cross-origin writes except from `APP_ORIGINS`, and bodies must be `application/json`.
- Behind Caddy set `TRUST_PROXY=true` so rate limits use the real client IP.

## Production

See `deploy/`: copy `deploy/.env.example` to `deploy/.env`, then

```sh
docker compose -f deploy/compose.prod.yaml up -d --build
```

Email is sent through Zoho Mail SMTP; set its credentials in `deploy/.env` and add Zoho's SPF, DKIM and DMARC records for the domain.
