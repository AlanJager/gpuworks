"""Independent Docker/log/Prometheus oracles for the instance diagnostics case.

The source image and Alpine shell layer reuse the audited override fixture.
No server API or browser response supplies expected runtime values.
"""
import json
import re
import subprocess
import time
from urllib.parse import parse_qs, urlencode, urlsplit
from urllib.request import urlopen

import deployment_override_fixture as tools
from local_browser_fixture import http
from platform_token_read_revoke import check

GAUGES = {
    "gpuworks_infer_requests_running": ("vllm:num_requests_running", 2),
    "gpuworks_infer_requests_waiting": ("vllm:num_requests_waiting", 3),
    "gpuworks_infer_gpu_cache_usage_percent": ("vllm:kv_cache_usage_perc", 0.375),
}
ENV = {"FAKE_ENGINE_METRICS_REQUESTS_RUNNING": "2",
       "FAKE_ENGINE_METRICS_REQUESTS_WAITING": "3",
       "FAKE_ENGINE_METRICS_KV_CACHE_USAGE_PERCENT": "0.375"}


def strict_json(actual, expected, label):
    encode = lambda value: json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)
    check(encode(actual) == encode(expected), label + " JSON value or type differs")


def capture_gauges(page, instance_uuid, range_spec, action, timeout=12):
    """Accept only responses to actual requests begun within this UI action."""
    duration, step = range_spec
    started, replies = [], []
    def requested(request):
        query = parse_qs(urlsplit(request.url).query)
        if (urlsplit(request.url).path == "/api/v1/metrics/query"
                and query.get("instance") == [instance_uuid]
                and query.get("metric", [None])[0] in GAUGES
                and query.get("step") == [step]
                and int(query.get("end", [0])[0]) - int(query.get("start", [0])[0]) == duration):
            started.append(request)
    def responded(response):
        if any(response.request is request for request in started):
            replies.append(response)
    page.on("request", requested)
    page.on("response", responded)
    try:
        action()
        deadline = time.monotonic() + timeout
        while len(replies) < 3:
            check(time.monotonic() < deadline, "browser did not complete three new gauge requests")
            page.wait_for_timeout(100)
        check(len(started) == len(replies) == 3, "range change duplicated a gauge request")
        check(all(sum(response.request is request for response in replies) == 1 for request in started),
              "gauge request did not receive exactly one response")
        return replies
    finally:
        page.remove_listener("request", requested)
        page.remove_listener("response", responded)


def docker(*args, timeout=20):
    result = subprocess.run(["docker", *args], capture_output=True, timeout=timeout)
    check(result.returncode == 0, f"docker {args[0]} failed: {result.stderr.decode(errors='replace')}")
    return result.stdout


def inspect(container):
    value = json.loads(docker("inspect", container))
    check(isinstance(value, list) and len(value) == 1 and value[0]["State"]["Running"] is True,
          "expected one running owned container")
    return value[0]


def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
    fixture = tools.prepare(server, token, run_id, report, holder, agent_bin, artifacts.remember)
    body = {"model_id": fixture["model"]["id"], "node_uuid": fixture["node"]["uuid"],
            "name": fixture["name"], "served_name": fixture["served_name"],
            "engine": "vllm", "image": fixture["image"], "placement_mode": "manual", "health_check": False,
            "container_overrides": {"command_mode": "engine", "env": ENV}}
    status, dep = http(server.base, "POST", "/api/v1/deployments", token=token, body=body)
    if isinstance(dep, dict) and type(dep.get("id")) is int:
        report["owned_deployment_id"] = dep["id"]
    check(status in (200, 201), f"diagnostic fixture deployment HTTP {status}: {dep}")
    check(bool(report.get("owned_deployment_id")), "deployment response has no owned identity")
    fixture["deployment"] = dep
    fixture["expected_env"] = dict(ENV)
    return fixture


def cleanup(server, token, report, holder):
    return tools.cleanup(server, token, report, holder)


def running(page, server, fixture, report, timeout=90):
    deadline, last = time.monotonic() + timeout, None
    path = "/api/v1/deployments/" + str(fixture["deployment"]["id"])
    while time.monotonic() < deadline:
        response = page.request.get(server.base + path)
        check(response.status == 200, f"owned deployment GET HTTP {response.status}")
        last = response.json()
        from deployment_container_fixture import remember_containers
        remember_containers(report, last)
        ready = [item for item in last.get("instances", []) if item["state"] == "running"]
        if len(ready) == 1 and ready[0].get("container_id"):
            instance = ready[0]
            actual = inspect(instance["container_id"])
            check(actual["Config"]["Image"] == fixture["image"], "unexpected runtime image reference")
            check(actual["Image"] == report["override_image_id"], "unexpected immutable image identity")
            fixture["instance"], fixture["container"] = instance, actual["Id"]
            return instance
        page.wait_for_timeout(150)
    raise AssertionError(f"diagnostic instance not ready: {last}")


def metric_wire(port, served_name):
    with urlopen(f"http://127.0.0.1:{port}/metrics", timeout=10) as response:
        check(response.status == 200, "engine exposition failed")
        wire = response.read().decode()
    values = {}
    for metric, (engine_metric, expected) in GAUGES.items():
        pattern = re.escape(engine_metric) + r'\{model_name=' + re.escape(json.dumps(served_name)) + r'\} ([^\n]+)'
        matches = re.findall(pattern, wire)
        check(len(matches) == 1 and float(matches[0]) == expected, f"engine gauge differs: {engine_metric}")
        values[metric] = float(matches[0])
    check("vllm:prefix_cache" not in wire and "vllm:spec_decode" not in wire,
          "fixture unexpectedly reports ratio metrics")
    return values


def assert_series(document, instance_uuid, expected, window=None):
    check(isinstance(document, dict) and set(document) == {"items", "total"}
          and type(document["total"]) is int and document["total"] == 1, "metric envelope differs")
    series = document["items"]
    check(isinstance(series, list) and len(series) == 1, "expected exactly one instance metric series")
    check(set(series[0]) == {"labels", "points"}, "metric series keys differ")
    check(series[0]["labels"].get("instance") == instance_uuid, "metric belongs to another instance")
    points = series[0]["points"]
    check(isinstance(points, list) and bool(points), "known gauge sample is missing")
    for point in points:
        check(isinstance(point, list) and len(point) == 2 and type(point[0]) is int
              and type(point[1]) in (int, float) and point[1] == expected,
              f"known gauge sample differs: {point}")
    timestamps = [point[0] for point in points]
    check(timestamps == sorted(set(timestamps)), "metric buckets duplicated or unordered")
    if window:
        start, end, step = window
        check(all(start // step * step <= stamp <= end and stamp % step == 0 for stamp in timestamps),
              "metric sample bucket falls outside the actual query window")
    return points


def assert_launch(facts, actual, image):
    check(facts["runtime_observation_relation"] == {"status": "current"}, "launch observation is not current")
    observed = facts["runtime_observation"]
    check(observed["runtime"] == "docker", "unexpected runtime observation")
    expected = {
        "container_id": actual["Id"], "image_id": actual["Image"],
        "entrypoint": actual["Config"].get("Entrypoint") or [],
        "cmd": actual["Config"].get("Cmd") or [], "path": actual["Path"], "args": actual["Args"],
        "env": dict(item.split("=", 1) for item in actual["Config"].get("Env", [])),
        "mounts": [{"type": m["Type"], "source": m["Source"], "destination": m["Destination"],
                    "read_only": not m["RW"]} for m in actual["Mounts"]],
        "devices": [], "runtime_name": actual["HostConfig"]["Runtime"],
        "network_mode": actual["HostConfig"]["NetworkMode"],
    }
    project = {k: v for k, v in observed["container"].items() if k not in {"status", "observed_at_unix_ms"}}
    check(observed["container"]["status"] == "observed", "container observation is not measured")
    strict_json(project, expected, "independent Docker container observation")
    expected_image = {"image_id": image["Id"], "entrypoint": image["Config"].get("Entrypoint") or [],
                      "cmd": image["Config"].get("Cmd") or [],
                      "env": dict(item.split("=", 1) for item in image["Config"].get("Env", [])),
                      "volumes": sorted((image["Config"].get("Volumes") or {}).keys())}
    project_image = {k: v for k, v in observed["image"].items() if k not in {"status", "observed_at_unix_ms"}}
    check(observed["image"]["status"] == "observed", "image observation is not measured")
    strict_json(project_image, expected_image, "independent Docker image observation")
    check(observed["process"] == {"status": "unknown", "reason": "not_collected"},
          "uncollected process must not be presented as measured")
    for layer in ("container", "image"):
        check(type(observed[layer].get("observed_at_unix_ms")) is int,
              "runtime observation timestamp is missing")
    return expected, expected_image


def write_marker(container, marker):
    line = "INFO " + marker + " 中文诊断"
    # PID 1's stdout is the engine log source; exec's own output is not shipped.
    docker("exec", container, "/bin/sh", "-c", 'printf "%s\\n" "$1" > /proc/1/fd/1', "sh", line)
    lines = docker("logs", container).decode().splitlines()
    check(lines.count(line) == 1, "owned engine stdout did not receive exactly one marker")
    return line


def persisted_marker(page, server, instance_uuid, node_uuid, line, lower_ms, timeout=30):
    path = "/api/v1/logs/search?" + urlencode({"instance": instance_uuid, "limit": 2000})
    deadline, last = time.monotonic() + timeout, None
    while time.monotonic() < deadline:
        response = page.request.get(server.base + path)
        check(response.status == 200, f"log persistence HTTP {response.status}")
        last = response.json()
        hits = [item for item in last["items"] if item["line"] == line]
        if hits:
            check(len(hits) == 1, "persisted marker duplicated")
            hit = hits[0]
            check(set(hit) == {"id", "instance", "node", "level", "source", "time_ms", "line"},
                  "persisted log wire keys differ")
            check(hit["instance"] == instance_uuid and hit["node"] == node_uuid
                  and hit["level"] == "info" and hit["source"] == "instance"
                  and type(hit["time_ms"]) is int and lower_ms <= hit["time_ms"] <= int(time.time() * 1000),
                  "persisted log identity, source, level or arrival timestamp differs")
            return hit
        page.wait_for_timeout(100)
    raise AssertionError(f"owned engine marker was not persisted: {last}")
