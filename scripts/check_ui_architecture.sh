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
  if [ "$count" -lt "$limit" ]; then
    echo "WARNING: $key is below its budget ($count < $limit); budget can be tightened" >&2
  fi
  [ "$count" -le "$limit" ]
}

# Keep the command status: a failed audit must never look like zero findings.
failed=0
for kind in long caveat helper message sentences clauses command en inlinehelp; do
  count=$(node scripts/ui_copy_audit.mjs "$kind")
  key=ui_copy_${kind}_strings
  [ "$kind" = inlinehelp ] && key=ui_copy_inline_help_refs
  if ! check_max "$key" "$count"; then
    node scripts/ui_copy_audit.mjs list | awk -F '\t' -v kind="$kind" '$1 == kind' >&2
    failed=1
  fi
done
[ "$failed" -eq 0 ] || exit 1

imports=$(node scripts/ui_overlay_imports.mjs)
count=$(printf '%s\n' "$imports" | awk 'NF { n++ } END { print n+0 }')
if ! check_max ee_ui_internal_imports "$count"; then
  printf '%s\n' "$imports" >&2
  exit 1
fi
node scripts/ui_antd_inventory.mjs --check
