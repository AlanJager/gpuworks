#!/usr/bin/env python3
"""Start an isolated real server and fake-H100 recording agent for Chrome acceptance.

This is an explicit local acceptance fixture. It uses the product binaries and
real HTTP APIs, with only GPU facts and the container runtime replaced by the
agent's built-in test adapters. The caller owns the binaries; build them before
running this script. State and logs remain in --artifact-dir for diagnosis.
"""

from __future__ import annotations

import argparse
import io
import json
import os
import signal
import socket
import subprocess
import tarfile
import time
import urllib.error
import urllib.request
from pathlib import Path


MODEL_ID = "gpuworks/deployment-browser-e2e"


def free_port() -> int:
    for port in range(8800, 8900, 2):
        with socket.socket() as probe:
            try:
                probe.bind(("127.0.0.1", port))
            except OSError:
                continue
        return port
    raise RuntimeError("no free local server port in 8800, 8802, ..., 8898")


def list_items(body):
    """Items of a management-API collection, strictly {"items": [...], "total": N}."""
    assert isinstance(body, dict) and set(body) == {"items", "total"}, f"want a {{items, total}} envelope, got {str(body)[:200]}"
    assert isinstance(body["items"], list) and body["total"] == len(body["items"]), f"inconsistent list envelope: {str(body)[:200]}"
    return body["items"]


def request(base: str, path: str, *, token: str = "", method: str = "GET", body: object = None,
            content_type: str = "application/json") -> object:
    data = None if body is None else body if isinstance(body, bytes) else json.dumps(body).encode()
    headers = {"Content-Type": content_type}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            raw = response.read()
            return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as error:
        raise RuntimeError(f"{method} {path}: HTTP {error.code}: {error.read().decode()[:3000]}") from error


def wait_until(check, description: str, timeout: float = 40) -> object:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except Exception as error:
            last_error = error
        time.sleep(0.25)
    raise TimeoutError(f"{description} did not become ready; last error: {last_error}")


def catalog_archive(model_file: Path) -> bytes:
    manifest = b"schema_version: 1\npublished_at: 2026-09-19T00:00:00Z\nmodels: 1\nspecs: 2\n"
    entries = {
        "manifest.yaml": manifest,
        f"models/{MODEL_ID}/model.yaml": model_file.read_bytes(),
    }
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as archive:
        for name, content in entries.items():
            info = tarfile.TarInfo(name)
            info.size = len(content)
            info.mode = 0o644
            archive.addfile(info, io.BytesIO(content))
    return output.getvalue()


def start(command: list[str], env: dict[str, str], log_path: Path) -> tuple[subprocess.Popen, object]:
    log_file = log_path.open("wb")
    process = subprocess.Popen(command, env=env, stdout=log_file, stderr=subprocess.STDOUT)
    return process, log_file


def stop(process: subprocess.Popen | None) -> None:
    if process is None or process.poll() is not None:
        return
    process.send_signal(signal.SIGTERM)
    try:
        process.wait(timeout=8)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def prepare(args: argparse.Namespace) -> tuple[str, dict[str, object], list[subprocess.Popen], list[object]]:
    root = Path(__file__).resolve().parents[2]
    work = args.artifact_dir.resolve()
    work.mkdir(parents=True, exist_ok=True)
    # Preserve the fixture for debugging without offering generated signing
    # keys, credentials, databases, model trees, or runtime state for commit.
    (work / ".gitignore").write_text("/server-data/\n/agent-data/\n/model/\n")
    if (work / "server-data" / "gpuworks.db").exists():
        raise RuntimeError(f"{work} already contains a server database; use a fresh artifact directory")
    (work / "server-data").mkdir(exist_ok=True)
    agent_data = work / "agent-data"
    agent_data.mkdir(exist_ok=True)
    # macOS /tmp can assign wheel as the inherited directory group. The
    # runtime mount manager correctly insists on the agent user's primary gid.
    # Prepare the fixture-owned directories with that gid before the agent
    # creates managed child mounts.
    os.chown(agent_data, os.getuid(), os.getgid())
    runtime_mounts = agent_data / "runtime-mounts"
    runtime_mounts.mkdir(exist_ok=True)
    os.chown(runtime_mounts, os.getuid(), os.getgid())
    model_dir = work / "model"
    model_dir.mkdir(exist_ok=True)
    (model_dir / "config.json").write_text(json.dumps({
        "model_type": "qwen3", "architectures": ["Qwen3ForCausalLM"],
        "max_position_embeddings": 4096, "num_hidden_layers": 2,
        "hidden_size": 64, "num_attention_heads": 4,
        "num_key_value_heads": 4, "torch_dtype": "bfloat16",
    }))
    (model_dir / "model.safetensors").write_bytes(b"browser acceptance fixture")
    port = free_port()
    base = f"http://127.0.0.1:{port}"
    env = os.environ.copy()
    # The first start creates the admin account from this file, as after an install.
    (work / "server-data").mkdir(exist_ok=True)
    (work / "server-data" / "initial-credentials").write_text(json.dumps({"user": "admin", "password": "password"}))
    server, server_log = start([
        str(args.server_bin.resolve()), "serve", "--addr", f"127.0.0.1:{port}",
        "--data", str(work / "server-data"), "--log-level", "warn",
    ], env, work / "server.log")
    processes = [server]
    logs = [server_log]
    try:
        wait_until(lambda: request(base, "/api/v1/auth/login", method="POST", body={
            "username": "admin", "password": "password",
        }).get("token"), "server login")
        token = str(request(base, "/api/v1/auth/login", method="POST", body={
            "username": "admin", "password": "password",
        })["token"])
        created = request(base, "/api/v1/nodes", token=token, method="POST", body={"name": "browser-h100", "server_url": base})
        node_uuid = str(created["node"]["uuid"])
        node_token = str(created["token"])
        agent_env = os.environ.copy()
        agent_env.update(
            GPUWORKS_FAKE_GPU="H100x2,81920",
            GPUWORKS_FAKE_GPU_VENDOR="nvidia",
            GPUWORKS_FAKE_GPU_CC="9.0",
            GPUWORKS_FAKE_RUNTIME_RECORD=str(work / "workload.json"),
            GPUWORKS_LOG_LEVEL="warn",
        )
        agent, agent_log = start([
            str(args.agent_bin.resolve()), "run", "--server", base,
            "--token", node_token, "--state", str(work / "agent-data"),
        ], agent_env, work / "agent.log")
        processes.append(agent)
        logs.append(agent_log)
        node = wait_until(lambda: next((item for item in list_items(request(base, "/api/v1/nodes", token=token))
            if item.get("uuid") == node_uuid and item.get("state") == "online"), None), "fake H100 node")
        model = request(base, "/api/v1/models", token=token, method="POST", body={
            "source": "huggingface", "source_id": MODEL_ID, "revision": "main",
            "name": MODEL_ID, "browser_metadata": True, "weight_bytes": 1048576,
            "config_raw": (model_dir / "config.json").read_text(),
        })
        request(base, f"/api/v1/models/{model['id']}/register-path", token=token, method="POST", body={
            "node_uuid": node_uuid, "path": str(model_dir),
        })
        archive = catalog_archive(root / "test/e2e/testdata/deployment-flow-model.yaml")
        imported = request(base, "/api/v1/inference-templates/import", token=token,
            method="POST", body=archive, content_type="application/gzip")
        template = request(base, f"/api/v1/inference-templates/model/{MODEL_ID}", token=token)
        fixture = {
            "base_url": base, "node_uuid": node_uuid, "model_id": model["id"],
            "template_id": template["id"], "template_model_id": MODEL_ID,
            "specs": ["browser-vllm", "browser-sglang"],
            "node": node, "catalog_import": imported,
            "record_path": str(work / "workload.json"), "model_path": str(model_dir),
            "server_pid": server.pid, "agent_pid": agent.pid,
        }
        (work / "fixture.json").write_text(json.dumps(fixture, ensure_ascii=False, indent=2))
        return base, fixture, processes, logs
    except BaseException:
        for process in reversed(processes):
            stop(process)
        for log in logs:
            log.close()
        raise


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--server-bin", type=Path, required=True)
    parser.add_argument("--agent-bin", type=Path, required=True)
    parser.add_argument("--artifact-dir", type=Path, required=True)
    parser.add_argument("--hold", action="store_true", help="keep the fixture alive until Ctrl+C")
    args = parser.parse_args()
    base, fixture, processes, logs = prepare(args)
    print(f"fixture ready at {base}; details: {args.artifact_dir / 'fixture.json'}", flush=True)
    try:
        if args.hold:
            while all(process.poll() is None for process in processes):
                time.sleep(0.5)
        else:
            script = Path(__file__).with_name("deployment_flow_acceptance.py")
            subprocess.run(["python3", str(script), "--fixture", str(args.artifact_dir / "fixture.json"),
                "--artifact-dir", str(args.artifact_dir)], check=True)
    except KeyboardInterrupt:
        pass
    finally:
        for process in reversed(processes):
            stop(process)
        for log in logs:
            log.close()
        print("fixture processes stopped; state and logs kept for review", flush=True)


if __name__ == "__main__":
    main()
