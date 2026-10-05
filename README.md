# See You There â€” API

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
internal/store/          sqlc-generated code â€” do not edit
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
| GET | `/healthz` | â€“ | Liveness |
| GET | `/readyz` | â€“ | Checks Postgres and Valkey |
| POST | `/v1/auth/code` | â€“ | `{email}` â†’ emails a 6-digit code. Always 202. 5/hour per email, 20/hour per IP |
| POST | `/v1/auth/verify` | â€“ | `{email, code}` â†’ creates the user on first login, sets `syt_session` cookie |
| POST | `/v1/auth/logout` | cookie | Revokes the session |
| GET | `/v1/me` | cookie | Current user and admin roles |

Errors look like `{"error": {"code": "invalid_code", "message": "..."}}`.

### Security notes

- Login codes: 10-minute expiry, 5 attempts, stored as an HMAC keyed by `AUTH_SECRET`.
- Session cookie holds a random token; only its SHA-256 is stored. HttpOnly, SameSite=Lax, Secure in production.
- CSRF: Go's `http.CrossOriginProtection` rejects cross-origin writes except from `APP_ORIGINS`, and bodies must be `application/json`.
- Behind Caddy set `TRUST_PROXY=true` so rate limits use the real client IP.

## Deploying

Pushing to `main` in this repo (API) or `seeyouthere-frontend` (web) runs tests, builds a `linux/amd64` image on a GitHub runner, pushes it to GHCR (`ghcr.io/ansona-001/seeyouthere-api` / `-web`, tags `<full sha>` and `latest`) and SSHes to the VM. The VM never builds; it only pulls and runs. Workflows: `.github/workflows/deploy.yml` in each repo. Actions are pinned to major version tags (not commit SHAs); Dependabot proposes updates weekly.

On the VM the deploy key's forced command is a root-owned copy of `deploy/ssh-entry.sh`. It accepts only `api|web <40-hex sha>`, takes a lock (`~/.syt-deploy.lock`, so deploys never overlap), runs `git pull --ff-only` in this repo, then runs the repo's `deploy/deploy.sh`, which:

1. starts postgres, valkey and caddy if they are not running,
2. pulls `ghcr.io/ansona-001/seeyouthere-<svc>:<sha>`, keeps the running image as `:previous`, tags the new one `:deployed` and recreates only that service,
3. health-checks it from the caddy container for up to 60 s (`/readyz` for api, `/robots.txt` for web),
4. on success reloads Caddy (warns if that fails) and prunes dangling images only,
5. on failure (start or health check) saves the last 200 log lines to `~/deploy-fail-<svc>-<time>.log` on the VM (logs are never printed, because the SSH output lands in public Actions logs), retags `:previous` as `:deployed`, restarts the service, re-checks it and exits 1 (the Actions run goes red). On a first deploy there is nothing to roll back to.

Disk: per service only the running image and `:previous` stay; the per-SHA tag is removed and the older generation is pruned.

Security model, honestly: the entry script is root-owned and cannot be changed by a git push, but `deploy.sh` is pulled from this repo, so anyone who can push to `main` here can run code on the VM as the deploy user, and the docker group is root-equivalent. Protect `main` accordingly (Hardening below). A leaked deploy key can redeploy any SHA that exists in GHCR and force restarts, but cannot run arbitrary commands.

### One-time setup

**1. Deploy key and forced command (on the VM, as `ansonarose`)**

```sh
ssh-keygen -t ed25519 -f ~/.ssh/syt_deploy -N ""
sudo install -o root -g root -m 0755 ~/seeyouthere/seeyouthere-backend/deploy/ssh-entry.sh /usr/local/sbin/syt-ssh-entry
echo "command=\"/usr/local/sbin/syt-ssh-entry\",restrict $(cat ~/.ssh/syt_deploy.pub)" >> ~/.ssh/authorized_keys
```

Re-run the `install` line whenever `ssh-entry.sh` changes (git pull does not update it). If the repo is not at `/home/ansonarose/seeyouthere/seeyouthere-backend`, edit `deploy_dir` at the top of the script first. `command=` makes the key run only that script whatever the client asks for. `restrict` disables port/agent/X11 forwarding, PTY allocation and `~/.ssh/rc`. The user must be in the `docker` group.

Windows does not keep the executable bit. Record it in git before the first commit, after `git add`, so the VM tree is not dirtied by a mode change on pull:

```sh
git add deploy/deploy.sh deploy/ssh-entry.sh
git update-index --chmod=+x deploy/deploy.sh deploy/ssh-entry.sh
```

**2. `DEPLOY_KNOWN_HOSTS`** (pins the VM's host key; the workflow never uses `ssh-keyscan` or disables host-key checking). Run on the VM, with its public IP (the one stored in `DEPLOY_HOST`):

```sh
echo "<vm-ip> $(cut -d' ' -f1,2 /etc/ssh/ssh_host_ed25519_key.pub)"
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub     # fingerprint, for comparison
```

Do not use `ssh-keyscan localhost` on the VM; it records `localhost`, not the IP. Optionally run `ssh-keyscan -t ed25519 <vm-ip> | ssh-keygen -lf -` from your own machine and compare with the fingerprint above. The secret is the single `<vm-ip> ssh-ed25519 AAAA...` line. If you connect by hostname, use that name instead.

**3. GitHub secrets.** In *both* repos: Settings, Environments, create `production`, set Deployment branches to `main` only, and add these as environment secrets (not repository secrets):

| Secret | Value |
|---|---|
| `DEPLOY_HOST` | VM public IP or DNS name |
| `DEPLOY_USER` | `ansonarose` |
| `DEPLOY_SSH_KEY` | full contents of `~/.ssh/syt_deploy` on the VM (including BEGIN/END lines) |
| `DEPLOY_KNOWN_HOSTS` | the line from step 2 |

After copying the private key into GitHub, delete it from the VM (`shred -u ~/.ssh/syt_deploy`). To rotate, generate a new pair and replace the `authorized_keys` line. You can add required reviewers to `production` later. In each repo also set Settings, Actions, General, "Fork pull request workflows" to "Require approval for all external contributors" (the workflows only trigger on `push` to `main`).

**4. First start**

1. On the VM: `cd ~/seeyouthere/seeyouthere-backend/deploy && cp .env.example .env`, fill it in, `chmod 600 .env`.
2. Make sure the GCP firewall allows tcp 80 and 443 and udp 443 (HTTP/3).
3. If any images were built on the VM earlier, tag them so the first CI deploy can roll back: `docker tag <image> ghcr.io/ansona-001/seeyouthere-<svc>:deployed`. On a truly first deploy there is nothing to roll back to.
4. The frontend `public/.gitkeep` must be committed (the Dockerfile copies `public/`).
5. Push to `main` in both repos (or re-run the workflow) and watch the Actions tab. The first run fails at the image pull while the packages are private; do step 5, then re-run.

**5. Make the GHCR packages public.** After the first successful push: GitHub, your profile, Packages, `seeyouthere-api` (and `seeyouthere-web`), Package settings, Change visibility, Public. In the same page connect each package to its repository (the `org.opencontainers.image.source` label usually does this already). Re-run the failed deploy jobs.

### Rollback

From `deploy/` on the VM. The previous build is kept as `:previous`; for an older one pull its SHA first:

```sh
docker tag ghcr.io/ansona-001/seeyouthere-<svc>:previous ghcr.io/ansona-001/seeyouthere-<svc>:deployed
# or: docker pull ghcr.io/ansona-001/seeyouthere-<svc>:<old sha> and tag that instead
docker compose -f compose.prod.yaml --env-file .env up -d --no-deps <svc>
```

The next push to `main` deploys the new code again, so revert or fix in git as well. Rollback does not undo database migrations (they run on API startup), so migrations must stay backward compatible: expand first, contract in a later release.

### Hardening

- Branch ruleset on `main` in both repos: block force-push and deletion, require signed commits.
- 2FA or passkeys on the GitHub account; fine-grained PATs only; audit GitHub Apps and tokens with write access.
- sshd: `PasswordAuthentication no`, `PermitRootLogin no`.
- Optional: a dedicated `deploy` user in the docker group, without sudo, owning the repo checkout.
- Optional: `git pull --ff-only --verify-signatures` in the entry script, with an SSH `allowedSignersFile` configured for git (`gpg.format ssh`, `gpg.ssh.allowedSignersFile`) so only your signed commits deploy.

## Production

Production runs from `deploy/compose.prod.yaml` with `deploy/.env` (copy `deploy/.env.example`; never commit it). Always pass `-f compose.prod.yaml --env-file .env` from `deploy/`. Do not use `up --build`: `api` and `web` are GHCR images (`pull_policy: never`; only `deploy.sh` pulls). Email is sent through Zoho Mail SMTP; set its credentials in `deploy/.env` and add Zoho's SPF, DKIM and DMARC records for the domain.
