#!/usr/bin/env bash
# Nightly Postgres backup: custom-format pg_dump, kept locally (7 newest) and uploaded to R2 (14 days).
# Runs on the VM from the ubuntu user's crontab. Needs deploy/.env (R2_* values) and rclone.
# Never prints secrets. Exit status is non-zero if either the dump or the upload failed.
set -euo pipefail
umask 077

cd "${DEPLOY_DIR:-$(dirname "$(readlink -f "$0")")}"
[[ -f .env ]] || { echo "backup: deploy/.env is missing" >&2; exit 1; }

backup_dir=${BACKUP_DIR:-$HOME/backups}
local_keep=7
remote_days=14
remote_prefix=backups/db
mkdir -p "$backup_dir"

# Read single values from .env without sourcing it (values may contain shell metacharacters).
envval() { grep -m1 "^$1=" .env | cut -d= -f2- | sed -e 's/^"//' -e 's/"$//'; }

stamp=$(date -u +%Y%m%dT%H%M%SZ)
dump=$backup_dir/db-$stamp.dump
tmp=$dump.tmp
trap 'rm -f "$tmp"' EXIT

dc() { docker compose -f compose.prod.yaml --env-file .env "$@"; }

dc exec -T postgres pg_dump -U seeyouthere -d seeyouthere --format=custom >"$tmp"
# A dump that cannot be listed is not a backup.
dc exec -T postgres pg_restore --list <"$tmp" >/dev/null
mv "$tmp" "$dump"
echo "backup: wrote $(basename "$dump") ($(stat -c %s "$dump") bytes)"

# Keep the newest $local_keep dumps.
mapfile -t old < <(ls -1t "$backup_dir"/db-*.dump 2>/dev/null | tail -n +$((local_keep + 1)))
((${#old[@]} == 0)) || rm -f -- "${old[@]}"

endpoint=$(envval R2_ENDPOINT)
bucket=$(envval R2_BUCKET)
if [[ -z $endpoint || -z $bucket ]]; then
  echo "backup: R2 not configured, local copy only" >&2
  exit 0
fi

export RCLONE_CONFIG_R2_TYPE=s3
export RCLONE_CONFIG_R2_PROVIDER=Cloudflare
export RCLONE_CONFIG_R2_ENDPOINT=$endpoint
export RCLONE_CONFIG_R2_ACCESS_KEY_ID=$(envval R2_ACCESS_KEY_ID)
export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY=$(envval R2_SECRET_ACCESS_KEY)
# Tokens scoped to object read/write cannot create buckets; skip rclone's bucket check.
export RCLONE_S3_NO_CHECK_BUCKET=true

rclone copyto --s3-upload-cutoff 1G "$dump" "R2:$bucket/$remote_prefix/$(basename "$dump")"
rclone delete --min-age "${remote_days}d" "R2:$bucket/$remote_prefix/"
echo "backup: uploaded to R2 (kept ${remote_days} days)"
