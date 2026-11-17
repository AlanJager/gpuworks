#!/bin/sh
set -eu

# Refuses a community UI build or release while console overlay modules are
# checked out. A separately built console copies its overlay modules into
# ui/src/extensions/<id>/ (git-ignored) and builds with GPUWORKS_UI_OUT_DIR
# pointing outside internal/server/webui/dist; without that variable the
# overlay would be bundled into the community server's embedded UI. `make ui`
# and `make release-packages` run this first.

cd "$(dirname "$0")/.."

overlays=$(find ui/src/extensions -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort)
if [ -n "$overlays" ] && [ -z "${GPUWORKS_UI_OUT_DIR:-}" ]; then
  echo "console overlay modules are checked out:" >&2
  printf '%s\n' "$overlays" | sed 's/^/  /' >&2
  echo "a community build would bundle them into internal/server/webui/dist." >&2
  echo "Remove them, or set GPUWORKS_UI_OUT_DIR to build the overlay console elsewhere." >&2
  exit 1
fi
