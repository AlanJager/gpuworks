#!/usr/bin/env python3
"""Build and export pinned images with an explicitly selected, provisioned daemon."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import runner_matrix


def run(argv, **kwargs):
    return subprocess.run(argv, check=True, **kwargs)


def sha256(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(8 << 20), b""):
            h.update(block)
    return h.hexdigest()


def load_images(matrix_path, config_path):
    """The runner matrix owns all base digests and component versions."""
    matrix = yaml.safe_load(Path(matrix_path).read_text())
    config = json.loads(Path(config_path).read_text())
    result = []
    for image_id, tag in config.items():
        row = runner_matrix.select(matrix, image_id)
        if not tag.startswith("gpuworks-local/"):
            raise ValueError("local output tag must stay in gpuworks-local/")
        result.append({**row, "tag": tag, "platform": row["platforms"][0],
                       "version": runner_matrix.engine_version(row)})
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--output", type=Path, required=True)
    p.add_argument("--docker", default="docker")
    p.add_argument("--engine", choices=["vllm", "sglang"])
    p.add_argument("--base-registry", help="optional registry mirror; preserve the pinned repository and digest")
    args = p.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if not os.environ.get("DOCKER_HOST"):
        p.error("DOCKER_HOST must explicitly select the authorized build runtime")
    client_config = Path(os.environ.setdefault("DOCKER_CONFIG", str(output / "client-config"))).resolve()
    if not client_config.is_relative_to(output):
        p.error("DOCKER_CONFIG must stay inside OUTPUT so client caches remain scoped")
    client_config.mkdir(parents=True, exist_ok=True)
    # A context inherited from the caller must not override the chosen endpoint.
    os.environ.pop("DOCKER_CONTEXT", None)
    docker = [args.docker, "--host", os.environ["DOCKER_HOST"]]
    root = Path(run(docker + ["info", "--format", "{{.DockerRootDir}}"],
                    capture_output=True, text=True).stdout.strip()).resolve()
    if root != output / "daemon-data":
        p.error("daemon DockerRootDir must equal OUTPUT/daemon-data; refusing shared daemon")
    context = Path(__file__).resolve().parent
    runner = context.parents[1] / "runner-images"
    images = load_images(runner / "matrix.yaml", context / "build-config.json")
    images = [x for x in images if not args.engine or x["engine"] == args.engine]
    # Build sequentially, preserving images and caches for explicit owner cleanup.
    if shutil.disk_usage(output).free < 100 * (1 << 30):
        p.error("at least 100 GiB free space is required before this sequential build")
    for item in images:
        engine = item["engine"]
        base = item["base"]
        if args.base_registry:
            if "/" in args.base_registry or "@" in args.base_registry:
                p.error("--base-registry must be a host[:port], without scheme or path")
            base = args.base_registry + "/" + base
        evidence = output / engine
        evidence.mkdir(exist_ok=True)
        if (evidence / "result.json").exists():
            p.error(f"{engine} already has a result; use a new output directory")
        with (evidence / "build.log").open("wb") as log:
            # Docker's predefined proxy arguments reach build subprocesses
            # without persisting proxy credentials in the image environment.
            proxy_args = [part for name in ("HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
                                            "http_proxy", "https_proxy", "no_proxy")
                          if os.environ.get(name) for part in ("--build-arg", name)]
            run(docker + ["build", "--pull", "--network=host", "--platform", item["platform"],
                          *proxy_args,
                          "--build-arg", "BASE_IMAGE=" + base,
                          "--build-arg", "BASE_ORIGIN=" + item["base"],
                          "--build-arg", "ENGINE=" + engine,
                          "--build-arg", "ENGINE_VERSION=" + item["version"],
                          "--build-arg", "IMAGE_ID=" + item["id"],
                          "--build-arg", "IMAGE_REVISION=" + item["revision"],
                          "--build-arg", "REQUIRES=" + " ".join(item["requires"]),
                          *[part for key, value in item["args"].items()
                            for part in ("--build-arg", key + "=" + str(value))],
                          "-f", str(runner / "rocm" / ("Dockerfile." + engine)),
                          "-t", item["tag"], str(runner)], stdout=log, stderr=subprocess.STDOUT)
        inspect = json.loads(run(docker + ["image", "inspect", item["tag"]],
                                 capture_output=True, text=True).stdout)[0]
        (evidence / "inspect.json").write_text(json.dumps(inspect, indent=2) + "\n")
        with (evidence / "audit.json").open("wb") as audit, (evidence / "audit.stderr").open("wb") as errors:
            result = subprocess.run(docker + ["run", "--rm", "--network=none",
                                    "--entrypoint", "python3", item["tag"],
                                    "/opt/gpuworks/amd-audit.py"], stdout=audit, stderr=errors)
        receipt = {"engine": engine, "tag": item["tag"], "base": item["base"],
                   "build_base": base,
                   "image_id": inspect["Id"], "component_audit_exit": result.returncode,
                   "hardware_validation": "pending", "artifacts": []}
        # Preserve a failed audit for diagnosis; never label it accepted.
        if result.returncode:
            receipt["status"] = "component-audit-failed"
            (evidence / "result.json").write_text(json.dumps(receipt, indent=2) + "\n")
            raise RuntimeError(f"{engine} component audit failed; inspect {evidence}")
        # Docker stages uncompressed layer tars before streaming docker save.
        # Reserve another full image for the compressed output's worst case,
        # plus room for tar headers and filesystem bookkeeping.
        free = shutil.disk_usage(output).free
        required = 2 * inspect["Size"] + 10 * (1 << 30)
        (evidence / "export-space.json").write_text(json.dumps(
            {"available_bytes": free, "required_bytes": required,
             "image_size_bytes": inspect["Size"]}, indent=2) + "\n")
        if free < required:
            raise RuntimeError(f"{engine} export needs {required} free bytes; available {free}; image retained")
        archive = evidence / (engine + ".docker.tar.gz")
        part = archive.with_suffix(archive.suffix + ".part")
        if archive.exists() or part.exists():
            raise RuntimeError(f"refusing to overwrite {archive} or its partial export")
        with part.open("xb") as stream:
            save = subprocess.Popen(docker + ["save", item["tag"]], stdout=subprocess.PIPE)
            try:
                compressor = "pigz" if shutil.which("pigz") else "gzip"
                flags = ["-p", "4", "-1"] if compressor == "pigz" else ["-1"]
                compressed = subprocess.run([compressor, *flags], stdin=save.stdout, stdout=stream)
                save.stdout.close()
                if save.wait() or compressed.returncode:
                    raise RuntimeError("docker save/compression failed; partial export retained")
            finally:
                if save.poll() is None:
                    save.terminate()
                    save.wait()
        run(["gzip", "-t", str(part)])
        part.rename(archive)
        digest = sha256(archive)
        archive.with_name(archive.name + ".sha256").write_text(digest + "  " + archive.name + "\n")
        receipt["artifacts"].append({"file": archive.name, "bytes": archive.stat().st_size,
                                      "sha256": digest, "format": "docker-save-gzip"})
        receipt["status"] = "built-component-checked-hardware-pending"
        (evidence / "result.json").write_text(json.dumps(receipt, indent=2) + "\n")
        print(json.dumps(receipt), flush=True)



if __name__ == "__main__":
    main()
