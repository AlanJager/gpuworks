#!/usr/bin/env bash
set -euo pipefail

site_root=$(cd "$(dirname "$0")" && pwd)
hugo_bin=${HUGO_BIN:-hugo}
preview_port=${DOCS_PREVIEW_PORT:-1313}
preview_bind=${DOCS_PREVIEW_BIND:-127.0.0.1}
preview_base_url=${DOCS_PREVIEW_BASE_URL:-"http://127.0.0.1:$preview_port/docs/"}

HUGO_BIN="$hugo_bin" "$site_root/build.sh"
exec "$hugo_bin" server --source "$site_root" \
  --bind "$preview_bind" --port "$preview_port" \
  --baseURL "$preview_base_url" \
  --appendPort=false --disableFastRender
