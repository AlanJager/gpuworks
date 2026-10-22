#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
manifest="${repo_root}/internal/assets/bin/runtime-assets/manifest.json"

python3 - "${repo_root}" "${manifest}" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
manifest_path = pathlib.Path(sys.argv[2])
document = json.loads(manifest_path.read_text())
assets = root / "internal" / "assets" / "bin" / "runtime-assets"
for bundle in document["bundles"]:
    for item in bundle["files"]:
        path = assets / item["path"]
        if not path.is_file():
            raise SystemExit(f"missing runtime asset: {path}")
        item["sha256"] = hashlib.sha256(path.read_bytes()).hexdigest()
manifest_path.write_text(json.dumps(document, ensure_ascii=False, indent=2) + "\n")
PY
