#!/usr/bin/env bash
set -euo pipefail

site_root=$(cd "$(dirname "$0")" && pwd)
hugo_bin=${HUGO_BIN:-hugo}
preview_port=${DOCS_PREVIEW_PORT:-1313}

HUGO_BIN="$hugo_bin" "$site_root/build.sh"
exec "$hugo_bin" server --source "$site_root" \
  --bind 127.0.0.1 --port "$preview_port" \
  --baseURL "http://127.0.0.1:$preview_port/docs/" \
  --appendPort=false --disableFastRender
