# Production infrastructure — current state

Reference for how See You There is hosted today. **No secrets here**: values live in `deploy/.env` on the server and in GitHub Actions secrets. Update this file whenever the setup changes.

Last updated: 2026-10-19. Moved from Google Cloud to Contabo on 2026-10-16/19.

## Hosting (Contabo VPS)

| Item | Value |
|---|---|
| Host | `185.182.185.48` (DNS A records for `seeyouthere.at` and `api.seeyouthere.at`, TTL 600 s, at GoDaddy) |
| OS | Ubuntu 26.04 LTS, x86_64 |
| Size | 6 vCPU, ~12 GB RAM, ~96 GB disk |
| Login | `ubuntu` (sudo, `docker` group), SSH key only |
| Firewall | ufw: 22, 80, 443; fail2ban running |

**The server is shared with another app, VetoTrade** (`vetotrade` + `vetotrade-db` containers behind the host nginx; default nginx vhost, Basic Auth). Do not touch its nginx files, ports (127.0.0.1:8747) or containers. Host nginx owns ports 80/443, so there is **no Caddy** here.

### Reverse proxy (host nginx + certbot)

- Config: `deploy/nginx-seeyouthere.conf` → `/etc/nginx/sites-available/seeyouthere` (symlinked into `sites-enabled`); shared proxy headers `deploy/nginx-seeyouthere-proxy.conf` → `/etc/nginx/snippets/seeyouthere-proxy.conf`. Certbot edited the site file in place (443 listeners, redirect), so **do not blindly overwrite it** from the repo; diff first.
- `seeyouthere.at`: `/media/<uuid>/<uuid>/{480,1080,1920,og}.jpg`, `/media/<uuid>/<uuid>/audio.mp3` (event music; own `limit_req` zone `sytaudio`, 1 r/s burst 5, with `proxy_buffering on`) and template backgrounds → API (`127.0.0.1:8080`), other `/media/*` → 404, everything else → web (`127.0.0.1:3000`).
- `api.seeyouthere.at` → API, body limit 12 MB. Both vhosts: HSTS and security headers, gzip, `limit_req` 20 r/s (burst 60).
- The API trusts the rightmost `X-Forwarded-For` entry (`TRUST_PROXY=true`); nginx appends the real client address with `$proxy_add_x_forwarded_for`.
- HTTP/2 is enabled on both 443 server blocks (`http2 on;`, added by hand after certbot; certbot does not add it). Re-add it if the vhost is ever regenerated.
- TLS: Let's Encrypt cert `seeyouthere.at` + `api.seeyouthere.at`, issued 2026-10-16, renewed by the `snap.certbot.renew.timer` systemd timer. Check with `sudo certbot certificates`.

## Services (Docker Compose)

Folder on the VM: `/home/ubuntu/seeyouthere/seeyouthere-backend/deploy` (a git clone of the backend repo, branch `main`). Compose file `compose.prod.yaml`; env file `.env` (same folder, never committed). Always run compose as:

```sh
cd ~/seeyouthere/seeyouthere-backend/deploy
docker compose -f compose.prod.yaml --env-file .env <command>
```

| Service | Image | Memory limit | Notes |
|---|---|---|---|
| api | `ghcr.io/ansona-001/seeyouthere-api:deployed` | 512 MB (`GOMEMLIMIT` 400 MiB) | Go API, published on `127.0.0.1:8080` |
| web | `ghcr.io/ansona-001/seeyouthere-web:deployed` | 384 MB (Node heap 300 MB) | Next.js, published on `127.0.0.1:3000` |
| postgres | `postgres:16-alpine` | 512 MB | volume `pgdata`, internal only; `shared_buffers=128MB` |
| valkey | `valkey/valkey:8-alpine` | 48 MB | volume `valkeydata`, internal only |

Domains: `seeyouthere.at` (site), `api.seeyouthere.at` (API). Limits were raised on 2026-10-19 for the 12 GB host; the server is in Germany (Frankfurt), so users far away see ~140 ms round trips. The api/web containers are recreated by the next deploy; postgres picks up its new settings the next time `deploy.sh` runs `up`.

## Deploys

- GitHub Actions in each repo build images, push to GHCR (`ghcr.io/ansona-001/seeyouthere-{api,web}`), then SSH to the VM. The deploy key is restricted in `~ubuntu/.ssh/authorized_keys` to the forced command `/usr/local/sbin/syt-ssh-entry` (installed root-owned from `deploy/ssh-entry.sh`), which accepts only `<api|web> <40-hex-sha>`, runs `git pull --ff-only` and then `deploy/deploy.sh`.
- `deploy.sh` pulls the image, tags it `:deployed`, keeps the old one as `:previous`, health-checks over `127.0.0.1` and **rolls back automatically** on failure, saving the last 200 log lines to `~/deploy-fail-<svc>-<timestamp>.log` on the VM.
- After editing `deploy/ssh-entry.sh`, re-install it: `sudo install -o root -g root -m 0755 deploy/ssh-entry.sh /usr/local/sbin/syt-ssh-entry`.
- GitHub secrets (both repos): `DEPLOY_SSH_KEY`, `DEPLOY_KNOWN_HOSTS`, `DEPLOY_HOST` (`185.182.185.48`), `DEPLOY_USER` (`ubuntu`).
- **Frontend build arg `NEXT_PUBLIC_SITE_URL=https://seeyouthere.at`** is required (baked into the bundle, makes `og:image` absolute). The Docker build fails without it. `NEXT_PUBLIC_API_URL=https://api.seeyouthere.at`.
- Deploy backend **before** frontend when a release adds API fields (new frontend vs old API gets 400 on unknown fields).
- Keep shell scripts LF-only (`.gitattributes` has `*.sh text eol=lf`); a CRLF shebang breaks the forced command with `env: 'bash\r'`.

## Media storage (Cloudflare R2) — active

- API start-up log prints `media storage backend=r2 bucket=syt-media`. Check: `docker compose -f compose.prod.yaml logs api 2>&1 | grep "media storage"`.
- Bucket `syt-media` (private; API serves every image itself). Env vars (names only): `R2_ENDPOINT`, `R2_BUCKET`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY` — all four must be set or the API refuses to start. In production the API only accepts endpoints ending in `.r2.cloudflarestorage.com`.
- Free tier: 10 GB storage, 1M class-A, 10M class-B ops per month. Quotas in code keep us inside it: `MEDIA_USER_QUOTA_MB=200`, `MEDIA_TOTAL_QUOTA_MB=9000`.
- Uploads are staged on local disk before going to R2; the API refuses uploads (HTTP 507) when free space on the `MEDIA_ROOT` disk is below `MEDIA_MIN_FREE_MB` (default 2048). The 96 GB disk makes this a non-issue.
- Event deletion removes the event's objects from this bucket right away (owner delete queues a purge job within moments; admin delete is synchronous).

## Link previews (og:image)

- Site default image: `seeyouthere-frontend/src/app/opengraph-image.png` (1200×630).
- Events have a `share_image` setting (`hero` | `site` | `custom`), edited in the editor's "Link preview" section.
- Messengers cache previews; use Facebook's Sharing Debugger or a `?v=2` URL to refresh.
- Verify after deploy: `curl -s https://seeyouthere.at/ | grep og:image` must show `https://seeyouthere.at/...` (not localhost).

## Environment variables (names only; values in `deploy/.env`)

`SITE_DOMAIN`, `ACME_EMAIL`, `POSTGRES_PASSWORD`, `AUTH_SECRET`, `SMTP_ADDR`, `SMTP_USER`, `SMTP_PASS`, `MAIL_FROM`, `TOTP_KEY`, `ADMIN_ALERT_EMAIL`, `MAIL_DAILY_INVITE_CAP`, `MEDIA_USER_QUOTA_MB`, `MEDIA_TOTAL_QUOTA_MB`, `MEDIA_MIN_FREE_MB`, `R2_*` (four). Email goes through Zoho Mail SMTP. See `deploy/.env.example` for the full list and comments. **Back up `deploy/.env` separately** (password manager); it is not in git or in the DB dumps.

## Backups — none (deliberate)

There are **no database backups**, by decision on 2026-10-19: deleted events and accounts must not survive anywhere after the purge job runs (see event deletion in `internal/jobs/admin_purge.go`). The nightly cron job, the local dumps and the `backups/db/` copies in R2 were removed.

- Consequence: a lost disk, a bad migration or an accidental delete means the data is gone. Revisit this once real users exist.
- `deploy/backup.sh` is kept in the repo but **not scheduled**. To take a one-off dump by hand, run it (`bash deploy/backup.sh`); it writes `~/backups/db-<stamp>.dump` (mode 600) and uploads to `R2:<bucket>/backups/db/`. Delete both copies afterwards.
- Restore from a dump into a fresh database:
  ```sh
  cd ~/seeyouthere/seeyouthere-backend/deploy
  dc() { docker compose -f compose.prod.yaml --env-file .env "$@"; }
  dc stop api web
  dc exec -T postgres pg_restore -U seeyouthere -d seeyouthere --clean --if-exists --no-owner --exit-on-error < db-<stamp>.dump
  dc exec -T valkey valkey-cli flushall   # drop cached sessions/rate limits
  dc up -d api web
  ```
- `deploy/.env` is not in git or in any dump; keep a copy in a password manager.

## Useful commands (on the VM)

```sh
df -h /                                   # disk space
docker system df                          # Docker disk usage
docker image prune -a -f                  # remove unused images
docker compose -f compose.prod.yaml ps    # service status
docker compose -f compose.prod.yaml logs --tail 100 api
sudo nginx -t && sudo systemctl reload nginx
```

## History

- 2026-10-02 → 2026-10-16: ran on a Google Cloud `e2-micro` (Always Free / trial credit, Caddy for TLS).
- 2026-10-16: moved to Contabo (this server): Postgres restored from `pg_dump`, `.env` copied unchanged, DNS A records switched, Caddy replaced by the host nginx because VetoTrade already owns ports 80/443.
- The Google Cloud VM (project `seeyouthere-510409`, instance `instance-20261002-093741`, zone `us-central1-a`) is retired; delete the VM, its disk and its static IP so nothing is billed after the trial.
