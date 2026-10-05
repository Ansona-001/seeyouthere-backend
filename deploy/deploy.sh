#!/usr/bin/env bash
# Deploy one prebuilt service (api or web) from GHCR. Usage: deploy.sh <api|web> <40-hex-sha>
# Runs on the VM, normally via ssh-entry.sh. Rolls back to the previous image if the new one is unhealthy.
# Output may end up in public CI logs: never print container logs or env values here.
set -euo pipefail

usage() { echo "usage: deploy.sh <api|web> <40-hex-sha>" >&2; exit 2; }

[[ $# -eq 2 ]] || usage
svc=$1
sha=$2
[[ $svc =~ ^(api|web)$ ]] || usage
[[ $sha =~ ^[0-9a-f]{40}$ ]] || usage

cd "$(dirname "$(readlink -f "$0")")"
[[ -f .env ]] || { echo "deploy: deploy/.env is missing" >&2; exit 1; }

# compose.prod.yaml is not a default file name; never fall back to the dev ../compose.yaml.
dc() { docker compose -f compose.prod.yaml --env-file .env "$@"; }

image=ghcr.io/ansona-001/seeyouthere-$svc
case $svc in
  api) health_url=http://api:8080/readyz ;;
  web) health_url=http://web:3000/robots.txt ;;
esac

wait_healthy() {
  local i
  for i in $(seq 1 30); do
    if dc exec -T caddy wget -q -T 5 -O /dev/null "$health_url" 2>/dev/null; then
      return 0
    fi
    echo "deploy: waiting for $svc to become healthy ($i/30)"
    sleep 2
  done
  return 1
}

# Always ends the script with exit 1. Every step is guarded so a failure does not abort it midway.
fail() {
  set +e
  echo "deploy: $1" >&2
  local logfile="$HOME/deploy-fail-$svc-$(date +%s).log"
  ( umask 077; dc logs --tail 200 "$svc" >"$logfile" 2>&1 )
  echo "deploy: service logs saved to $logfile on the VM" >&2
  if [[ -z $had_current ]]; then
    echo "deploy: nothing to roll back to (first deploy of $svc)" >&2
    exit 1
  fi
  echo "deploy: rolling back $svc to the previous image" >&2
  docker tag "$image:previous" "$image:deployed" || echo "deploy: rollback retag failed" >&2
  dc up -d --no-deps "$svc" || echo "deploy: rollback restart failed" >&2
  if wait_healthy; then
    echo "deploy: rolled back; $svc is running the previous image" >&2
  else
    echo "deploy: rollback unhealthy; $svc needs manual attention" >&2
  fi
  exit 1
}

had_current=
echo "deploy: ensuring postgres, valkey and caddy are up"
dc up -d --wait --wait-timeout 120 --no-deps postgres valkey caddy

echo "deploy: pulling $image:$sha"
docker pull "$image:$sha" >/dev/null || { echo "deploy: pull failed (is the package public and the build finished?)" >&2; exit 1; }

# Keep the running image as :previous (a tag, so prune never removes it). The old :previous
# becomes untagged and is pruned after a successful deploy: at most one extra image per service.
if docker image inspect "$image:deployed" >/dev/null 2>&1; then
  docker tag "$image:deployed" "$image:previous"
  had_current=1
fi
docker tag "$image:$sha" "$image:deployed"
# Drop the per-SHA tag so superseded images can be pruned; the disk is tiny.
docker rmi "$image:$sha" >/dev/null || true

echo "deploy: starting $svc"
if ! dc up -d --no-deps "$svc"; then
  fail "starting $svc failed"
fi
if ! wait_healthy; then
  fail "$svc did not become healthy within 60s"
fi

echo "deploy: $svc is healthy"
dc exec -T caddy caddy reload --config /etc/caddy/Caddyfile >/dev/null 2>&1 \
  || echo "deploy: warning: caddy reload failed (config unchanged or invalid)" >&2
docker image prune -f >/dev/null || true
echo "deploy: done ($svc @ $sha)"
