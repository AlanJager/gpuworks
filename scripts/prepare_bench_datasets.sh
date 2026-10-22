#!/usr/bin/env bash
set -euo pipefail

# Validate or refresh redistributable benchmark assets embedded by pkg/bench.
# Pass --refresh-upstream to download the pinned OASST1 snapshot and rebuild
# the deterministic subset. Normal builds remain network-free.
root="$(cd "$(dirname "$0")/.." && pwd)"
data="$root/pkg/bench/dataset/data"
mkdir -p "$data"
if [ "${1:-}" = "--refresh-upstream" ]; then
  go run "$root/scripts/prepare_multiturn_dataset" -output "$data/multiturn.jsonl.gz" -max-compressed-bytes 2500000
fi
test -s "$data/multiturn.jsonl.gz"
gzip -t "$data/multiturn.jsonl.gz"
size=$(wc -c < "$data/multiturn.jsonl.gz")
if [ "$size" -gt 5242880 ]; then
  echo "compressed benchmark dataset assets exceed 5 MiB: $size" >&2
  exit 1
fi
printf 'compressed benchmark dataset assets: %s bytes\n' "$size"
