#!/bin/sh
# Publish the publisher's notices (security advisories, releases, events) as
# notices.json at the root of every distribution source (ADR 0054). Servers
# fetch it with their update check and show what applies to them in the
# console's notice bell.
#
#   scripts/publish_notices.sh [build/notices/notices.json]
#
# The input is the unsigned payload kept in the repository; it is validated
# the way servers validate it, signed with the distribution key and uploaded
# with Cache-Control: no-cache, then read back from every public URL.
# Credentials are those of scripts/publish_dist.sh (the environment or
# secrets/dist-publish.env). Needs rclone (RCLONE overrides the binary).
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

input=${1:-build/notices/notices.json}
for env_file in secrets/dist-publish.env ../gpuworks/secrets/dist-publish.env; do
  if [ -z "${GPUWORKS_DIST_SIGNING_KEY:-}" ] && [ -f "$env_file" ]; then
    # shellcheck disable=SC1090
    . "$env_file"
  fi
done
: "${GPUWORKS_DIST_SIGNING_KEY:?set GPUWORKS_DIST_SIGNING_KEY (base64 ed25519 seed)}"
# shellcheck source=scripts/dist_remotes.sh
. ./scripts/dist_remotes.sh
export GPUWORKS_DIST_SIGNING_KEY GPUWORKS_DIST_SIGNING_KEY_ID

rclone=${RCLONE:-rclone}
work=$(mktemp -d "${TMPDIR:-/tmp}/gpuworks-notices.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM
: >"$work/rclone.conf"
export RCLONE_CONFIG="$work/rclone.conf"

go build -o "$work/gpuworks-dist" ./cmd/gpuworks-dist
"$work/gpuworks-dist" notices -in "$input" -out "$work/notices.json"
for remote in $remotes; do
  "$rclone" copyto "$work/notices.json" "$remote/notices.json" --header-upload 'Cache-Control: no-cache'
done
set --
for url in $publics; do
  set -- "$@" -source "$url"
done
"$work/gpuworks-dist" notices -expect "$work/notices.json" "$@"
