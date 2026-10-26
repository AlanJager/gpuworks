#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

python3 - "${repo_root}" <<'PY'
import hashlib
import json
import pathlib
import sys
import tarfile

root = pathlib.Path(sys.argv[1])
assets = root / "internal" / "assets" / "bin" / "runtime-assets"
manifest = json.loads((assets / "manifest.json").read_text())
seen = set()
for bundle in manifest["bundles"]:
    key = (bundle["backend"], bundle["platform_arch"])
    if key in seen:
        raise SystemExit(f"duplicate runtime bundle: {key}")
    seen.add(key)
    for item in bundle["files"]:
        path = assets / item["path"]
        if not path.is_file():
            raise SystemExit(f"missing runtime asset: {path}")
        actual = hashlib.sha256(path.read_bytes()).hexdigest()
        if actual != item["sha256"]:
            raise SystemExit(f"checksum mismatch: {item['path']}: {actual} != {item['sha256']}")

required = {
    ("ascend-dgpu", "amd64"),
    ("ascend-dgpu", "arm64"),
    ("ppu-dgpu", "amd64"),
}
missing = required - seen
if missing:
    raise SystemExit(f"missing required runtime bundles: {sorted(missing)}")

for arch in ("amd64", "arm64"):
    archive = assets / "probe" / f"partition-probe-linux-{arch}.tar.gz"
    checksum = archive.with_suffix(archive.suffix + ".sha256")
    if not archive.is_file() or not checksum.is_file():
        raise SystemExit(f"missing probe image archive/checksum for {arch}")
    expected, filename = checksum.read_text().split()
    if filename != archive.name:
        raise SystemExit(f"probe checksum must use a relative filename: {checksum}")
    actual = hashlib.sha256(archive.read_bytes()).hexdigest()
    if actual != expected:
        raise SystemExit(f"probe checksum mismatch for {arch}: {actual} != {expected}")

    # Docker 26 cannot load a pure OCI archive, while udocker 1.3.17 detects
    # oci-layout before manifest.json and cannot recurse through the mixed
    # archive emitted by newer Docker image stores. The compatibility
    # intersection is the classic docker-save layout below.
    with tarfile.open(archive, "r:gz") as image_tar:
        names = set(image_tar.getnames())
        for required_name in ("manifest.json", "repositories"):
            if required_name not in names:
                raise SystemExit(f"probe archive for {arch} lacks {required_name}")
        forbidden = {"oci-layout", "index.json", "blobs"} & names
        if forbidden:
            raise SystemExit(f"probe archive for {arch} mixes OCI content into classic Docker layout: {sorted(forbidden)}")
        manifests = json.load(image_tar.extractfile("manifest.json"))
        if len(manifests) != 1:
            raise SystemExit(f"probe archive for {arch} must contain one image, got {len(manifests)}")
        image = manifests[0]
        if image.get("RepoTags") != ["gpuworks/partition-probe:v4"]:
            raise SystemExit(f"probe archive tag mismatch for {arch}: {image.get('RepoTags')}")
        config_name = image.get("Config", "")
        if not config_name.endswith(".json") or config_name not in names:
            raise SystemExit(f"probe archive config is invalid for {arch}: {config_name!r}")
        config_raw = image_tar.extractfile(config_name).read()
        config_digest = config_name[:-5]
        if hashlib.sha256(config_raw).hexdigest() != config_digest:
            raise SystemExit(f"probe archive config digest mismatch for {arch}")
        config = json.loads(config_raw)
        if config.get("os") != "linux" or config.get("architecture") != arch:
            raise SystemExit(f"probe archive platform mismatch for {arch}: {config.get('os')}/{config.get('architecture')}")
        layers = image.get("Layers", [])
        diff_ids = config.get("rootfs", {}).get("diff_ids", [])
        if not layers or len(layers) != len(diff_ids):
            raise SystemExit(f"probe archive layer/diff-id mismatch for {arch}")
        for layer_path, diff_id in zip(layers, diff_ids):
            layer_id, slash, filename = layer_path.partition("/")
            if not slash or filename != "layer.tar" or layer_path not in names:
                raise SystemExit(f"invalid classic Docker layer for {arch}: {layer_path!r}")
            for metadata_name in (f"{layer_id}/VERSION", f"{layer_id}/json"):
                if metadata_name not in names:
                    raise SystemExit(f"missing classic Docker layer metadata for {arch}: {metadata_name}")
            if image_tar.extractfile(f"{layer_id}/VERSION").read().strip() != b"1.0":
                raise SystemExit(f"invalid classic Docker layer version for {arch}: {layer_id}")
            layer_metadata = json.load(image_tar.extractfile(f"{layer_id}/json"))
            if layer_metadata.get("id") != layer_id:
                raise SystemExit(f"classic Docker layer ID mismatch for {arch}: {layer_metadata}")
            try:
                algorithm, expected_diff = diff_id.split(":", 1)
            except ValueError as exc:
                raise SystemExit(f"invalid diff ID for {arch}: {diff_id}") from exc
            if algorithm != "sha256" or layer_id != expected_diff:
                raise SystemExit(f"classic Docker layer path/diff ID mismatch for {arch}: {layer_id}/{diff_id}")
            actual_diff = hashlib.sha256(image_tar.extractfile(layer_path).read()).hexdigest()
            if actual_diff != expected_diff:
                raise SystemExit(f"classic Docker layer content mismatch for {arch}: {actual_diff} != {expected_diff}")
        repositories = json.load(image_tar.extractfile("repositories"))
        if repositories != {"gpuworks/partition-probe": {"v4": layers[-1].split("/", 1)[0]}}:
            raise SystemExit(f"probe archive repositories mismatch for {arch}: {repositories}")
print("runtime asset manifest, architecture assets, probe checksums, and classic Docker archive structure verified")
PY
