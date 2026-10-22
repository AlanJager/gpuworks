#!/bin/sh
# Publish the system image catalog manifest as manifest.json at the root of
# every distribution source (ADR 0040: Aliyun OSS, Huawei OBS and Cloudflare
# R2). https://image-manifest.gpuworks.ai/manifest.json is served from R2, so
# servers that check for catalog updates there read the same bytes.
#
# Credentials are those of scripts/publish_dist.sh: the environment, or
# secrets/dist-publish.env, falling back to the main checkout's
# ../gpuworks/secrets/dist-publish.env. Needs rclone (RCLONE overrides the
# binary).
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

manifest=${1:-internal/server/imagecatalog/catalog-seed.json}
# shellcheck source=scripts/dist_remotes.sh
. ./scripts/dist_remotes.sh

rclone=${RCLONE:-rclone}

# Refuse to publish something the server would reject or a node could not
# pull: the check parses the manifest as the server does, then pulls every
# reference it names, canonical and mirror alike, down to each layer.
go run ./cmd/gpuworks-image-manifest -verify "$manifest"

work=$(mktemp -d "${TMPDIR:-/tmp}/gpuworks-manifest.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM
: >"$work/rclone.conf"
export RCLONE_CONFIG="$work/rclone.conf"

for remote in $remotes; do
  "$rclone" copyto "$manifest" "$remote/manifest.json" --header-upload 'Cache-Control: no-cache'
done

# Read every public copy back; each must be the published bytes.
want=$(sha256sum "$manifest" | cut -d' ' -f1)
for url in "https://$bucket.oss-cn-shanghai.aliyuncs.com/manifest.json" \
  "https://$bucket.obs.cn-east-3.myhuaweicloud.com/manifest.json" \
  "https://dl.gpuworks.ai/manifest.json" \
  "https://image-manifest.gpuworks.ai/manifest.json"; do
  got=$(curl -fsS "$url" | sha256sum | cut -d' ' -f1)
  if [ "$got" != "$want" ]; then
    echo "$url serves sha256 $got, want $want" >&2
    exit 1
  fi
done
version=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["catalog_version"])' "$manifest")
echo "manifest $version published to OSS, OBS and R2 (https://image-manifest.gpuworks.ai/manifest.json)"
