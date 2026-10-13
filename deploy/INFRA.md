# Production infrastructure — current state

Reference for how See You There is hosted today. **No secrets here**: values live in `deploy/.env` on the server and in GitHub Actions secrets. Update this file whenever the setup changes.

Last updated: 2026-10-13.

## Goal / constraints

- Run everything **free** (no surprise charges) until the move to Contabo (planned, date not set — a paid VPS with a flat monthly price and large bandwidth allowance; check current pricing before switching).
- One small VM shared by Caddy, API, web, Postgres and Valkey. Photos live in Cloudflare R2, not on the VM.

## Hosting (Google Cloud, Always Free)

> **Deadline: the Google Cloud account is a *free trial* ($300 credit) that ends 2027-01-01 (checked 2026-10-13, $1.83 spent, billed $0.00).** When the trial ends all resources are stopped (site goes offline) unless the account is upgraded. Do NOT click Upgrade if zero charges are wanted. The server move (Contabo, or Oracle Cloud Always Free as a free alternative) must be done before then, ideally 2+ weeks earlier. The trial spend so far is most likely the public IPv4 address (~$0.005/h, ~$3.5-4/month), so expect that cost on any upgraded Google account.

| Item | Value |
|---|---|
| Project | `seeyouthere-510409` |
| VM | `instance-20261002-093741`, `e2-micro` (1 vCPU shared, ~1 GB RAM) |
| Zone | `us-central1-a` (Always Free regions: us-west1, us-central1, us-east1) |
| OS | Ubuntu minimal 22.04 LTS |
| Boot disk | `pd-standard`, **30 GB** (grown from 10 GB on 2026-10-13; Always Free covers 30 GB-months of *standard* PD only — never switch to balanced/SSD) |
| Root partition | `/dev/sda1` (ext4), grown with `growpart /dev/sda 1` + `resize2fs /dev/sda1` |
| Network | public ephemeral IPv4, tags `http-server`, `https-server` |
| Agents | Google Cloud Ops agent (~500 MB on disk), optional |

### Things that can still cost money (check Billing → Reports → group by SKU)

- **Snapshots**: NOT in Always Free. The automatic schedule `default-schedule-1` was detached and deleted, and all 11 snapshots were deleted on 2026-10-13, so there are **no automatic backups**. Take a manual database dump instead (see Backups) and keep it off the VM. Do not re-enable a snapshot schedule if zero cost is wanted.
- **Public IPv4**: billed by Google (~$3.5-4/month); currently covered by the trial credit, which is why the $1.83 spend exists.
- **Network egress**: only 1 GB/month outbound is free. Event images are served from the VPS with `Cache-Control: public, max-age=3600`. Putting Cloudflare (free plan) in front would cut this, but needs DNS moved to Cloudflare plus real-client-IP handling for rate limiting — **not done**; skipped because of the Contabo move.
- Set a **$1 budget alert** (Billing → Budgets & alerts) as a safety net.

## Services (Docker Compose)

Folder on the VM: `/home/ansonarose/seeyouthere/seeyouthere-backend/deploy`
Compose file: `compose.prod.yaml`; env file: `.env` (same folder, never committed). Always run compose as:

```sh
cd ~/seeyouthere/seeyouthere-backend/deploy
docker compose -f compose.prod.yaml --env-file .env <command>
```

| Service | Image | Memory limit | Notes |
|---|---|---|---|
| caddy | `caddy:2-alpine` | 48 MB | ports 80/443; TLS via ACME; routes `seeyouthere.at` → web (and `/media/*.jpg` → api), `api.seeyouthere.at` → api |
| api | `ghcr.io/ansona-001/seeyouthere-api:deployed` | 200 MB | Go API :8080 |
| web | `ghcr.io/ansona-001/seeyouthere-web:deployed` | 220 MB | Next.js :3000 |
| postgres | `postgres:16-alpine` | 256 MB | volume `pgdata` |
| valkey | `valkey/valkey:8-alpine` | 48 MB | volume `valkeydata` |

Domains: `seeyouthere.at` (site), `api.seeyouthere.at` (API).

## Deploys

- GitHub Actions in each repo build images and push to GHCR (`ghcr.io/ansona-001/seeyouthere-{api,web}`), then SSH to the VM and run `deploy/deploy.sh <api|web> <sha>`.
- `deploy.sh` pulls the image, tags it `:deployed`, keeps the old one as `:previous`, health-checks, and **rolls back automatically** on failure, saving the last 200 log lines to `~/deploy-fail-<svc>-<timestamp>.log` on the VM.
- GitHub secrets used: `DEPLOY_SSH_KEY`, `DEPLOY_KNOWN_HOSTS`, `DEPLOY_HOST`, `DEPLOY_USER` (change `DEPLOY_HOST`/`DEPLOY_KNOWN_HOSTS` when moving servers).
- **Frontend build arg `NEXT_PUBLIC_SITE_URL=https://seeyouthere.at`** is required (baked into the bundle, makes `og:image` absolute). The Docker build fails without it. `NEXT_PUBLIC_API_URL=https://api.seeyouthere.at`.
- Deploy backend **before** frontend when a release adds API fields (new frontend vs old API gets 400 on unknown fields).
- Open item: `~/deploy-fail-api-1791362244.log` exists on the VM from an earlier failed API deploy; not yet reviewed.

## Media storage (Cloudflare R2) — active

- Verified 2026-10-13: API start-up log prints `media storage backend=r2 bucket=syt-media`.
- Bucket `syt-media` (private; API serves every image itself). Env vars (names only): `R2_ENDPOINT`, `R2_BUCKET`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY` — all four must be set or the API refuses to start.
- Free tier: 10 GB storage, 1M class-A, 10M class-B ops per month. Quotas in code keep us inside it: `MEDIA_USER_QUOTA_MB=200`, `MEDIA_TOTAL_QUOTA_MB=9000`.
- Uploads are still **staged on local disk** before going to R2, so the API refuses uploads ("Storage is nearly full", HTTP 507) when free space on the `MEDIA_ROOT` disk is below `MEDIA_MIN_FREE_MB` (default 2048). History: on 2026-10-13 the 10 GB disk had 570 MB free, which blocked uploads; fixed by growing the disk to 30 GB (21 GB free).
- Check which backend is active: `docker compose -f compose.prod.yaml logs api 2>&1 | grep "media storage"`.

## Link previews (og:image)

- Site default image: `seeyouthere-frontend/src/app/opengraph-image.png` (1200×630).
- Events have a `share_image` setting (`hero` | `site` | `custom`, migrations 00009/00010), edited in the editor's "Link preview" section.
- Messengers cache previews; use Facebook's Sharing Debugger or a `?v=2` URL to refresh.
- Verify after deploy: `curl -s https://seeyouthere.at/ | grep og:image` must show `https://seeyouthere.at/...` (not localhost).

## Environment variables (names only; values in `deploy/.env`)

`SITE_DOMAIN`, `ACME_EMAIL`, `POSTGRES_PASSWORD`, `AUTH_SECRET`, `SMTP_ADDR`, `SMTP_USER`, `SMTP_PASS`, `MAIL_FROM`, `TOTP_KEY`, `ADMIN_ALERT_EMAIL`, `MAIL_DAILY_INVITE_CAP`, `MEDIA_USER_QUOTA_MB`, `MEDIA_TOTAL_QUOTA_MB`, `MEDIA_MIN_FREE_MB`, `R2_*` (four). Email goes through Zoho Mail SMTP. See `deploy/.env.example` for the full list and comments.

## Backups (manual, free)

Postgres user and database are both `seeyouthere`. A first dump was taken and downloaded on 2026-10-13 (81 KB). To take another:

```sh
cd ~/seeyouthere/seeyouthere-backend/deploy
docker compose -f compose.prod.yaml exec -T postgres pg_dump -U seeyouthere -d seeyouthere --format=custom > ~/db-backup.dump
docker compose -f compose.prod.yaml exec -T postgres pg_restore --list < ~/db-backup.dump | head   # sanity check
```
Download it from Cloud Shell (`gcloud compute scp instance-20261002-093741:~/db-backup.dump ~/ --zone us-central1-a`, then Cloud Shell menu → Download), keep it private (it holds user emails; never commit it), and delete the copies on the VM and in Cloud Shell afterwards. Photos are in R2, the code is in GitHub, and `deploy/.env` must be backed up separately.

## Useful commands (on the VM)

```sh
df -h /                                   # disk space (keep well above MEDIA_MIN_FREE_MB)
docker system df                          # Docker disk usage
docker image prune -a -f                  # remove unused images
docker compose -f compose.prod.yaml ps    # service status
docker compose -f compose.prod.yaml logs --tail 100 api
```

## Planned: move to Contabo

1. Dump Postgres (`pg_dump`) on the old VM, restore on the new one.
2. Copy `deploy/.env` unchanged (R2, SMTP, secrets all portable); photos stay in R2.
3. Update GitHub secrets `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_KNOWN_HOSTS`, `DEPLOY_SSH_KEY`.
4. Lower DNS TTL a day ahead, then point the A records for `seeyouthere.at` and `api.seeyouthere.at` at the new IP; Caddy gets certificates automatically.
5. Decommission the Google Cloud VM and delete leftover snapshots/disks to avoid charges.
