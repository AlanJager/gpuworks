#!/bin/sh
set -eu

# Persistence and API serialization must start from UTC. Keeping this check
# mechanical prevents a future local-time write from reintroducing mixed
# SQLite DATETIME offsets or a non-Z RFC3339 response.
if command -v rg >/dev/null 2>&1; then
  violations="$(rg -n --pcre2 'time\.Now\(\)(?!\.UTC\(\))' internal/server/store internal/server/api \
    --glob '*.go' --glob '!**/*_test.go' || true)"
else
  violations="$(find internal/server/store internal/server/api -type f -name '*.go' ! -name '*_test.go' \
    -exec awk '{
      remainder = $0
      while (match(remainder, /time\.Now\(\)/)) {
        suffix = substr(remainder, RSTART + RLENGTH)
        if (suffix !~ /^\.UTC\(\)/) {
          print FILENAME ":" FNR ":" $0
          break
        }
        remainder = suffix
      }
    }' {} +)"
fi
if [ -n "$violations" ]; then
  echo "bare time.Now() is not allowed in store/api production code; use time.Now().UTC():" >&2
  echo "$violations" >&2
  exit 1
fi

echo "time policy check passed"
