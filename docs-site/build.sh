#!/usr/bin/env bash
set -euo pipefail

site_root=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$site_root/.." && pwd)
hugo_bin=${HUGO_BIN:-hugo}
output_dir=${HUGO_DEST:-"$site_root/public"}
theme_repo=${WEBSITE_REPO:-http://dev.zstack.io:9080/wei.wang/gpuworks_website.git}

# The oink theme lives in the gpuworks_website repository; this repository
# carries no theme files. Fetch it once when it is missing. The internal
# repository needs authentication: under CI the WEBSITE_REPO project variable
# carries a read-only deploy-token URL, anywhere else git uses the operator's
# own credentials (or set WEBSITE_REPO to a URL that carries them).
if [ ! -d "$site_root/themes/oink" ]; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	git clone --depth 1 "$theme_repo" "$tmp/website"
	mkdir -p "$site_root/themes"
	cp -R "$tmp/website/oink" "$site_root/themes/oink"
fi

python3 -m unittest discover -s "$site_root" -p 'test_*.py'
python3 "$site_root/stage_docs.py"
rm -rf "$output_dir"
mkdir -p "$output_dir"
"$hugo_bin" --source "$site_root" --destination "$output_dir" \
  --cleanDestinationDir --gc --minify --environment production \
  --printPathWarnings --panicOnWarning

# The generated pages are staged under docs-site/content so Hugo can publish
# them below /docs/ when the artifact is copied to the existing site root.
# OINK builds edit/history links from Hugo's staging path; map those links back
# to the real repository paths before publishing the static artifact.
python3 - "$output_dir" <<'PY'
from pathlib import Path
import sys

output = Path(sys.argv[1])
for page in output.rglob("*.html"):
    text = page.read_text(encoding="utf-8")
    updated = text.replace("master/content/docs/", "master/docs/")
    updated = updated.replace("master/content/_index.md", "master/docs/README.md")
    if updated != text:
        page.write_text(updated, encoding="utf-8")
PY

commit=$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || printf 'local')
short_commit=$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null || printf 'local')
built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
cat > "$output_dir/docs-manifest.json" <<EOF
{
  "project": "gpuworks",
  "ref": "${CI_COMMIT_REF_NAME:-local}",
  "commit": "$commit",
  "short_commit": "$short_commit",
  "built_at": "$built_at",
  "generator": "hugo-oink",
  "generator_version": "$($hugo_bin version | head -1)"
}
EOF

test -s "$output_dir/index.html"
test -s "$output_dir/tutorials/getting-started/index.html"
test -s "$output_dir/offline-search-index.zh."*.json
printf 'built docs at %s from %s\n' "$output_dir" "$short_commit"
