"""Real engine health audit with run-owned Docker/agent/model resources.

The proxy is an external fixture; server/checker/gateway/store/agent stay real.
Its only controlled failures are exact minimal health chat requests.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import time
from urllib.parse import quote

from local_browser_fixture import ROOT, digest, http
from platform_token_read_revoke import check
import deployment_container_fixture as containers
import deployment_replica_fixture as replicas
import deployment_lifecycle as lifecycle

PROXY_SOURCE = ROOT / "test/e2e/testdata/deployment-health-proxy/main.go"


def write_control(directory, mode):
    check(mode in {"ok", "slow", "fail"}, "unsupported controlled engine mode")
    path = Path(directory)
    temporary = path / "control.new"
    temporary.write_text(json.dumps({"mode": mode, "delay_ms": 2000 if mode == "slow" else 0}))
    temporary.replace(path / "control.json")


def audit(directory):
    path = Path(directory) / "audit.jsonl"
    if not path.exists():
        return []
    # Rename is unnecessary for append-only records: ignore only a trailing
    # incomplete write, never malformed completed records or skipped sequence.
    raw = path.read_bytes()
    lines = raw.splitlines(keepends=True)
    if lines and not lines[-1].endswith(b"\n"):
        lines.pop()
    events = [json.loads(line) for line in lines]
    check([event["seq"] for event in events] == list(range(1, len(events) + 1)),
          "engine audit contains duplicate, missing or reordered sequence IDs")
    starts = {event["seq"] for event in events if event["phase"] == "started"}
    terminals = [event["request"] for event in events
                 if event["phase"] in {"completed", "cancelled", "transport_failed"}]
    check(len(terminals) == len(set(terminals)), "health request has multiple terminal events")
    check(set(terminals) <= starts, "health audit terminal without a preceding request")
    check(not any(event["phase"] in {"rejected", "control_failed", "transport_failed"} for event in events),
          "engine fixture rejected protocol or failed outside the declared controlled behavior")
    return events


def probes(directory):
    events = audit(directory)
    result = []
    for start in (item for item in events if item["phase"] == "started"):
        terminal = next((item for item in events if item.get("request") == start["seq"]), None)
        result.append({"start": start, "terminal": terminal})
    return result


def verify_probe(probe, served, mode, outcome):
    start, terminal = probe["start"], probe["terminal"]
    replicas.strict({key: start[key] for key in ("method", "path", "query", "body", "mode")},
        {"method": "POST", "path": "/v1/chat/completions", "query": "", "mode": mode,
         "body": {"model": served, "messages": [{"role": "user", "content": "1"}],
                  "max_tokens": 1, "stream": False}}, "actual engine health request")
    check(terminal is not None, "health request still has no terminal audit")
    check(terminal["phase"] == outcome, f"unexpected health terminal: {terminal}")
    check(terminal["at_ms"] >= start["at_ms"], "health audit time moved backwards")
    return terminal


def inspect_identity(container):
    item = json.loads(replicas.docker("inspect", container))[0]
    check(item["State"]["Running"] is True, "health change stopped the real engine container")
    return {"id": item["Id"], "pid": item["State"]["Pid"], "started_at": item["State"]["StartedAt"],
            "restart_count": item["RestartCount"], "image_id": item["Image"], "image_ref": item["Config"]["Image"]}


def document(server, token, deployment, report):
    status, result = http(server.base, "GET", containers.DEPLOYMENTS + f"/{deployment['id']}", token)
    check(status == 200, f"deployment health independent GET HTTP {status}")
    containers.remember_containers(report, result)
    return result


def wait(page, predicate, label, timeout=90):
    deadline, last = time.monotonic() + timeout, None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        page.wait_for_timeout(150)
    raise AssertionError(f"{label} did not converge; last={last!r}")


def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
    binary = server.storage_root / "health-audit-proxy"
    result = subprocess.run(["go", "build", "-trimpath", "-o", str(binary), str(PROXY_SOURCE)],
        capture_output=True, text=True, timeout=120,
        env={**os.environ, "CGO_ENABLED": "0"})
    check(result.returncode == 0, "health proxy build failed: " + result.stderr)
    binary.chmod(0o755)
    report["health_proxy"] = {"source_sha256": digest(PROXY_SOURCE), "binary_sha256": digest(binary)}
    image = "gpuworks/e2e-health:" + run_id.lower()
    report["owned_image_tag"] = image
    replicas.docker("tag", report["fake_engine_image_id"], image)
    replicas.strict(replicas.docker("image", "inspect", "--format", "{{.Id}}", image).strip(),
                    report["fake_engine_image_id"], "health run-owned image ID")
    report["owned_node_name"] = f"e2e-{run_id}-health-node"
    code, created = http(server.base, "POST", containers.NODES, token,
                         {"name": report["owned_node_name"], "server_url": server.base})
    node = created.get("node", {}) if isinstance(created, dict) else {}
    credential = created.get("token") if isinstance(created, dict) else None
    if isinstance(credential, str) and credential.strip():
        artifacts.remember(credential)
    if isinstance(node, dict) and isinstance(node.get("uuid"), str) and node["uuid"]:
        report["owned_node_uuid"] = node["uuid"]
    check(code == 201 and bool(report.get("owned_node_uuid")), f"health fixture node: HTTP {code}")
    check(isinstance(credential, str) and bool(credential.strip()), "invalid health agent credential")
    state = server.work / "health-agent-state"
    state.mkdir()
    holder["log"] = (server.work / "agent.log").open("w")
    holder["agent"] = subprocess.Popen([str(agent_bin.resolve()), "run", "--server", server.base,
        "--token", credential, "--state", str(state)], stdout=holder["log"], stderr=subprocess.STDOUT,
        env={**os.environ, "GPUWORKS_FAKE_GPU": "A100x2,24576", "GPUWORKS_FAKE_GPU_NO_INJECT": "1"})
    lifecycle.wait_online(server.base, token, node["uuid"], holder["agent"])
    weights = server.storage_root / ("e2e-" + run_id + "-weights")
    shutil.copytree(lifecycle.MODEL_DIR, weights)
    report["health_proxy"]["weights_sha256"] = {str(path.relative_to(weights)): digest(path)
        for path in weights.rglob("*") if path.is_file()}
    report["owned_model_name"] = f"e2e-{run_id}-health-model"
    code, model = http(server.base, "POST", containers.MODELS + "/local", token,
        {"node_uuid": node["uuid"], "path": str(weights), "source_id": "e2e/health-" + run_id,
         "name": report["owned_model_name"]})
    if isinstance(model, dict) and type(model.get("id")) is int and model["id"] > 0:
        report["owned_model_id"] = model["id"]
    check(code in (200, 201) and report.get("owned_model_id"), f"health model fixture: HTTP {code}")
    entries = report.setdefault("health_deployments", [])
    for role in ("target", "guard"):
        directory = server.storage_root / ("health-" + role)
        directory.mkdir()
        write_control(directory, "ok" if role == "target" else "slow")
        name = f"e2e-{run_id}-health-{role}"
        entry = {"role": role, "name": name, "directory": str(directory), "id": None}
        entries.append(entry)
        if role == "target":
            report["owned_deployment_name"] = name
        body = {"name": name, "served_name": name + "-served", "model_id": model["id"],
            "node_uuid": node["uuid"], "engine": "vllm", "image": image, "placement_mode": "manual",
            "replicas": 1, "health_check": True, "health_check_threshold_sec": 5,
            "container_overrides": {"command_mode": "engine", "entrypoint_display": '["/e2e-health-proxy"]',
                "env": {"E2E_HEALTH_DIR": "/e2e-health", "E2E_HEALTH_SERVED": name + "-served"},
                "extra_mounts": [{"host_path": str(binary), "container_path": "/e2e-health-proxy",
                                  "purpose": "data", "read_only": True},
                                 {"host_path": str(directory), "container_path": "/e2e-health",
                                  "purpose": "data", "read_only": False}]}}
        code, deployment = http(server.base, "POST", containers.DEPLOYMENTS, token, body)
        if isinstance(deployment, dict) and type(deployment.get("id")) is int and deployment["id"] > 0:
            entry["id"] = deployment["id"]
            if role == "target":
                report["owned_deployment_id"] = deployment["id"]
        check(code == 201 and entry["id"], f"health {role} fixture deployment HTTP {code}")
        entry["served"] = body["served_name"]
    return {entry["role"]: entry for entry in entries}


def cleanup(server, token, report, holder):
    prior_steps, prior_remaining, guards_ok = [], [], True
    guards = [item for item in report.get("health_deployments", []) if item["role"] == "guard"]
    for entry in guards:
        temporary = {"owned_deployment_id": entry.get("id"), "containers": []}
        try:
            if not entry.get("id"):
                code, listed = http(server.base, "GET", containers.DEPLOYMENTS + "?q=" + quote(entry["name"]), token)
                check(code == 200, "cannot discover owned guard deployment")
                matched = [item for item in listed["items"] if item["name"] == entry["name"]]
                check(len(matched) <= 1, "ambiguous guard ownership")
                if matched:
                    entry["id"] = temporary["owned_deployment_id"] = matched[0]["id"]
            okay = containers.cleanup(server, token, temporary, {}, request=http, inspect=containers.docker_running)
            guards_ok = guards_ok and okay
            prior_steps.extend({**step, "stage": "guard " + step["stage"]} for step in temporary.get("cleanup_steps", []))
            prior_remaining.extend(temporary.get("remaining_resources", []))
            if okay:
                entry["id"] = None
        except Exception as error:
            guards_ok = False
            prior_remaining.append({"kind": "guard-deployment", "id": entry.get("id") or entry["name"],
                "state": str(error), "reclaim": "DELETE exact run-owned guard /deployments/<id>?force=true"})
    try:
        okay = replicas.cleanup(server, token, report, holder)
    except Exception as error:
        okay = False
        report.setdefault("remaining_resources", []).append({"kind": "health-domain-cleanup",
            "id": report.get("owned_deployment_id") or report.get("owned_node_uuid"),
            "state": str(error), "reclaim": "inspect run-owned target/model/node/container IDs only"})
    report["cleanup_steps"] = prior_steps + report.get("cleanup_steps", [])
    report.setdefault("remaining_resources", []).extend(prior_remaining)
    for entry in report.get("health_deployments", []):
        try:
            events = audit(entry["directory"])
            starts = [event for event in events if event["phase"] == "child_started"]
            exits = [event for event in events if event["phase"] == "child_exit"]
            check(len(starts) == len(exits) == entry.get("expected_children", len(starts)),
                  "health engine child did not observably exit")
            check(all(probe["terminal"] for probe in probes(entry["directory"])), "health proxy left active requests")
            report["cleanup_steps"].append({"stage": entry["role"] + " audit child exit and request release", "ok": True})
        except Exception as error:
            okay = False
            report["remaining_resources"].append({"kind": "health-proxy", "id": entry["directory"],
                "state": str(error), "reclaim": "inspect exact run-owned image containers and this audit directory"})
    return okay and guards_ok and not report["remaining_resources"]
