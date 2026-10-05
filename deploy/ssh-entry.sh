#!/usr/bin/env bash
# Forced command for the GitHub Actions deploy key (see README "Deploying").
# Install root-owned: sudo install -o root -g root -m 0755 deploy/ssh-entry.sh /usr/local/sbin/syt-ssh-entry
# Accepts only "<api|web> <40-hex-sha>" in SSH_ORIGINAL_COMMAND.
set -euo pipefail
export LC_ALL=C

deploy_dir=/home/ansonarose/seeyouthere/seeyouthere-backend/deploy

cmd=${SSH_ORIGINAL_COMMAND:-}
if [[ ! $cmd =~ ^(api|web)\ [0-9a-f]{40}$ ]]; then
  echo "ssh-entry: command rejected" >&2
  exit 1
fi
read -r svc sha <<<"$cmd"

# Serialise deploys of both services; the lock is inherited by the exec'd script.
exec 9>"$HOME/.syt-deploy.lock"
flock -w 900 9 || { echo "ssh-entry: deploy lock busy" >&2; exit 1; }

git -C "$deploy_dir" pull --ff-only
exec bash "$deploy_dir/deploy.sh" "$svc" "$sha"
