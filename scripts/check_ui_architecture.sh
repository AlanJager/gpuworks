#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

check_max() {
  key=$1
  count=$2
  limit=$(awk -F '\t' -v key="$key" '$1 == key && $3 == "max" { print $2 }' scripts/arch-constraints.txt)
  case "$limit" in ''|*[!0-9]*) echo "Missing or invalid max budget for $key" >&2; exit 1 ;; esac
  case "$count" in ''|*[!0-9]*) echo "Invalid count for $key: $count" >&2; exit 1 ;; esac
  echo "$key $count/$limit (max)"
  [ "$count" -le "$limit" ]
}

# Keep the command status: a failed audit must never look like zero findings.
long=$(node scripts/ui_copy_audit.mjs long)
caveat=$(node scripts/ui_copy_audit.mjs caveat)
if ! check_max ui_copy_long_strings "$long" || ! check_max ui_copy_caveat_strings "$caveat"; then
  node scripts/ui_copy_audit.mjs list >&2
  exit 1
fi

imports=$(node scripts/ui_overlay_imports.mjs)
count=$(printf '%s\n' "$imports" | awk 'NF { n++ } END { print n+0 }')
if ! check_max ee_ui_internal_imports "$count"; then
  printf '%s\n' "$imports" >&2
  exit 1
fi
node scripts/ui_antd_inventory.mjs --check
