#!/usr/bin/env python3
"""Convert a single-platform OCI archive to the classic docker-save layout.

Docker 26 cannot load BuildKit's OCI archive, while udocker 1.3.17 mistakes the
mixed archive emitted by newer Docker image stores for OCI and then fails on a
nested index.  The classic layout is the compatibility intersection: both
runtimes understand manifest.json plus config JSON and layer directories.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
import tarfile


OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
GZIP_LAYERS = {
    "application/vnd.oci.image.layer.v1.tar+gzip",
    "application/vnd.docker.image.rootfs.diff.tar.gzip",
}
PLAIN_LAYERS = {
    "application/vnd.oci.image.layer.v1.tar",
    "application/vnd.docker.image.rootfs.diff.tar",
}


def parse_digest(value: str) -> tuple[str, str]:
    try:
        algorithm, digest = value.split(":", 1)
    except ValueError as exc:
        raise ValueError(f"invalid digest {value!r}") from exc
    if algorithm != "sha256" or len(digest) != 64:
        raise ValueError(f"unsupported digest {value!r}")
    return algorithm, digest


def read_member(archive: tarfile.TarFile, name: str) -> bytes:
    member = archive.getmember(name)
    stream = archive.extractfile(member)
    if stream is None:
        raise ValueError(f"archive member {name!r} is not a file")
    return stream.read()


def canonical_json(value: object) -> bytes:
    return json.dumps(value, separators=(",", ":"), sort_keys=True).encode() + b"\n"


def add_bytes(archive: tarfile.TarFile, name: str, data: bytes, mode: int = 0o644) -> None:
    info = tarfile.TarInfo(name)
    info.size = len(data)
    info.mode = mode
    info.mtime = int(os.environ.get("SOURCE_DATE_EPOCH", "0"))
    info.uid = info.gid = 0
    info.uname = info.gname = "root"
    archive.addfile(info, io.BytesIO(data))


def add_directory(archive: tarfile.TarFile, name: str) -> None:
    info = tarfile.TarInfo(name.rstrip("/") + "/")
    info.type = tarfile.DIRTYPE
    info.mode = 0o755
    info.mtime = int(os.environ.get("SOURCE_DATE_EPOCH", "0"))
    info.uid = info.gid = 0
    info.uname = info.gname = "root"
    archive.addfile(info)


def convert(source: str, destination: str, image: str, expected_arch: str) -> str:
    with tarfile.open(source, "r:") as oci:
        index = json.loads(read_member(oci, "index.json"))
        descriptors = index.get("manifests", [])
        if len(descriptors) != 1 or descriptors[0].get("mediaType") != OCI_MANIFEST:
            raise ValueError("OCI archive must point directly to one image manifest")
        descriptor = descriptors[0]
        platform = descriptor.get("platform", {})
        if platform.get("os") != "linux" or platform.get("architecture") != expected_arch:
            raise ValueError(f"OCI platform mismatch: {platform!r}, expected linux/{expected_arch}")

        algorithm, manifest_digest = parse_digest(descriptor["digest"])
        manifest = json.loads(read_member(oci, f"blobs/{algorithm}/{manifest_digest}"))
        if manifest.get("mediaType") != OCI_MANIFEST:
            raise ValueError(f"unexpected image manifest media type: {manifest.get('mediaType')!r}")
        algorithm, config_digest = parse_digest(manifest["config"]["digest"])
        config_raw = read_member(oci, f"blobs/{algorithm}/{config_digest}")
        if hashlib.sha256(config_raw).hexdigest() != config_digest:
            raise ValueError("image config digest mismatch")
        config = json.loads(config_raw)
        if config.get("os") != "linux" or config.get("architecture") != expected_arch:
            raise ValueError(f"image config platform mismatch: {config.get('os')}/{config.get('architecture')}")

        layers = manifest.get("layers", [])
        diff_ids = config.get("rootfs", {}).get("diff_ids", [])
        if not layers or len(layers) != len(diff_ids):
            raise ValueError(f"layer/diff-id mismatch: {len(layers)} layers, {len(diff_ids)} diff IDs")
        converted_layers: list[tuple[str, bytes]] = []
        for layer, diff_id in zip(layers, diff_ids):
            algorithm, compressed_digest = parse_digest(layer["digest"])
            layer_raw = read_member(oci, f"blobs/{algorithm}/{compressed_digest}")
            media_type = layer.get("mediaType")
            if media_type in GZIP_LAYERS:
                layer_raw = gzip.decompress(layer_raw)
            elif media_type not in PLAIN_LAYERS:
                raise ValueError(f"unsupported layer media type: {media_type!r}")
            _, layer_id = parse_digest(diff_id)
            if hashlib.sha256(layer_raw).hexdigest() != layer_id:
                raise ValueError(f"uncompressed layer digest mismatch for {layer_id}")
            converted_layers.append((layer_id, layer_raw))

    repository, tag = image.rsplit(":", 1)
    if not repository or not tag or "/" not in repository:
        raise ValueError(f"image must be an explicit repository:tag reference: {image!r}")
    config_name = config_digest + ".json"
    layer_paths = [layer_id + "/layer.tar" for layer_id, _ in converted_layers]
    with tarfile.open(destination, "w", format=tarfile.PAX_FORMAT) as docker:
        add_bytes(docker, config_name, config_raw)
        parent = ""
        for layer_id, layer_raw in converted_layers:
            add_directory(docker, layer_id)
            add_bytes(docker, layer_id + "/VERSION", b"1.0")
            metadata = {
                "architecture": expected_arch,
                "config": config.get("config", {}),
                "container_config": {},
                "created": config.get("created", "1970-01-01T00:00:00Z"),
                "id": layer_id,
                "os": "linux",
            }
            if parent:
                metadata["parent"] = parent
            add_bytes(docker, layer_id + "/json", canonical_json(metadata))
            add_bytes(docker, layer_id + "/layer.tar", layer_raw)
            parent = layer_id
        add_bytes(
            docker,
            "manifest.json",
            canonical_json([{"Config": config_name, "Layers": layer_paths, "RepoTags": [image]}]),
        )
        add_bytes(docker, "repositories", canonical_json({repository: {tag: parent}}))
    return "sha256:" + config_digest


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("source")
    parser.add_argument("destination")
    parser.add_argument("--image", required=True)
    parser.add_argument("--arch", required=True, choices=("amd64", "arm64"))
    args = parser.parse_args()
    print(convert(args.source, args.destination, args.image, args.arch))


if __name__ == "__main__":
    main()
