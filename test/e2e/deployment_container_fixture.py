"""Scoped Docker observations and cleanup shared by deployment browser cases.

E2E standard: test/e2e/browser-e2e-testing-standard.md.
Only IDs obtained from this run's own deployment may be inspected or reclaimed.
"""
import re
import subprocess
import time
import traceback

DEPLOYMENTS = "/api/v1/deployments"
MODELS = "/api/v1/models"
APIKEYS = "/api/v1/apikeys"
NODES = "/api/v1/nodes"


def docker_running(container_id):
    if not container_id:
        return "absent"
    result = subprocess.run(
        ["docker", "inspect", "--format", "{{.State.Running}}", container_id],
        capture_output=True, text=True, timeout=20)
    if result.returncode == 0:
        value = result.stdout.strip()
        if value in ("true", "false"):
            return value
        raise AssertionError(f"docker inspect returned an invalid running state for {container_id}: {value!r}")
    missing = (r"(?:Error:\s*|Error response from daemon:\s*)?"
               r"No such (?:container|object):\s*" + re.escape(container_id))
    if re.fullmatch(missing, result.stderr.strip(), re.IGNORECASE):
        return "absent"
    raise AssertionError(f"docker inspect failed for {container_id}: {result.stderr.strip()}")


def assert_chat_response(response, label="gateway chat"):
    if response.status != 200:
        raise AssertionError(f"{label}: HTTP {response.status}, expected 200")
    try:
        document = response.json()
    except (ValueError, TypeError) as error:
        raise AssertionError(f"{label}: expected JSON chat completion") from error
    if not isinstance(document, dict) or document.get("object") != "chat.completion":
        raise AssertionError(f"{label}: expected a chat.completion object")
    choices = document.get("choices")
    if not isinstance(choices, list) or len(choices) != 1 or not isinstance(choices[0], dict):
        raise AssertionError(f"{label}: expected one completion choice")
    choice = choices[0]
    expected = {"role": "assistant", "content": "hello from fake-engine"}
    if choice.get("message") != expected or choice.get("finish_reason") != "stop":
        raise AssertionError(f"{label}: fake-engine output did not recover: {choice}")


def remember_containers(report, document):
    """Retain IDs even when an instance has not yet passed its health check."""
    containers = report.setdefault("containers", [])
    for instance in document.get("instances") or []:
        container = instance.get("container_id")
        if container and container not in containers:
            containers.append(container)


def cleanup(server, token, report, holder, *, request, inspect,
            inject_node_delete_timeout=False):
    """Reclaim every owned resource; one failure must not skip the rest."""
    cleanup_ok = True
    errors = []
    steps = []
    remaining = []
    report["remaining_resources"] = remaining

    def observe(path):
        try:
            return f"GET {request(server.base, 'GET', path, token=token)[0]}"
        except Exception:
            return "unknown (query failed)"

    def residual(kind, ident, state, reclaim, name=None):
        entry = {"kind": kind, "id": ident, "state": state, "reclaim": reclaim}
        if name:
            entry["name"] = name
        remaining.append(entry)

    def stage(name, action):
        nonlocal cleanup_ok
        try:
            result = bool(action())
        except Exception:
            cleanup_ok = False
            errors.append(f"{name} failed: " + traceback.format_exc().splitlines()[-1])
            result = False
        steps.append({"stage": name, "ok": result})
        cleanup_ok = cleanup_ok and result
        return result

    if report.get("owned_deployment_id"):
        dep_id = report["owned_deployment_id"]

        def inventory_containers():
            status, document = request(server.base, "GET", DEPLOYMENTS + f"/{dep_id}", token=token)
            if status == 200:
                remember_containers(report, document)
            return status in (200, 404)

        stage("container inventory", inventory_containers)

        def delete_deployment():
            status, _ = request(server.base, "DELETE", DEPLOYMENTS + f"/{dep_id}", token=token)
            report["cleanup_deployment_status"] = status
            deadline = time.time() + 90
            while time.time() < deadline and request(
                    server.base, "GET", DEPLOYMENTS + f"/{dep_id}",
                    token=token)[0] != 404:
                time.sleep(0.5)
            if request(server.base, "GET", DEPLOYMENTS + f"/{dep_id}", token=token)[0] != 404:
                forced, _ = request(server.base, "DELETE",
                                 DEPLOYMENTS + f"/{dep_id}?force=true", token=token)
                report["cleanup_deployment_force_status"] = forced
                deadline = time.time() + 20
                while time.time() < deadline and request(
                        server.base, "GET", DEPLOYMENTS + f"/{dep_id}",
                        token=token)[0] != 404:
                    time.sleep(0.5)
            last = request(server.base, "GET", DEPLOYMENTS + f"/{dep_id}",
                        token=token)[0]
            report["cleanup_deployment_state"] = f"GET {last}"
            gone = last == 404
            report["cleanup_deployment_gone"] = gone
            return gone

        # Keep ownership unless disappearance is observed; a failed deletion
        # must leave a directly reclaimable ID in the report.
        if stage("deployment delete", delete_deployment):
            report["owned_deployment_id"] = None
        else:
            residual("deployment", dep_id,
                     report.get("cleanup_deployment_state")
                     or observe(DEPLOYMENTS + f"/{dep_id}"),
                     f"DELETE {DEPLOYMENTS}/{dep_id}?force=true")

    for container in report.get("containers") or []:
        def check_container(container=container):
            deadline = time.time() + 30
            while time.time() < deadline and inspect(container) != "absent":
                time.sleep(0.5)
            absent = inspect(container) == "absent"
            report.setdefault("cleanup_containers", {})[container] = (
                "absent" if absent else "present")
            return absent
        if not stage(f"container {container}", check_container):
            residual("container", container,
                     report.get("cleanup_containers", {}).get(container,
                                                              "unknown (query failed)"),
                     f"docker rm -f {container}")

    if report.get("owned_key_name"):
        key_name = report["owned_key_name"]

        def delete_key():
            status, keys = request(server.base, "GET", APIKEYS, token=token)
            if status != 200:
                raise AssertionError(f"listing inference keys failed: HTTP {status}")
            for item in keys.get("items") or []:
                if item.get("name") == key_name:
                    report["owned_key_prefix"] = item["prefix"]
                    code, _ = request(server.base, "DELETE",
                                   APIKEYS + f"/{item['prefix']}", token=token)
                    report["cleanup_key_status"] = code
                    after_status, after = request(server.base, "GET", APIKEYS,
                                               token=token)
                    if after_status != 200:
                        return False
                    gone = all(entry.get("name") != key_name
                               for entry in after.get("items") or [])
                    report["cleanup_key_gone"] = gone
                    return gone
            report["cleanup_key_gone"] = True
            return True

        if stage("inference key delete", delete_key):
            report["owned_key_name"] = None
        else:
            residual("inference-key", report.get("owned_key_prefix") or "unknown",
                     "present" if report.get("cleanup_key_gone") is False
                     else "unknown (query failed)",
                     f"DELETE {APIKEYS}/<prefix>", name=key_name)

    if report.get("owned_model_id"):
        model_id = report["owned_model_id"]

        def delete_model():
            status, _ = request(server.base, "DELETE",
                             MODELS + f"/{model_id}", token=token)
            report["cleanup_model_status"] = status
            last = request(server.base, "GET", MODELS + f"/{model_id}", token=token)[0]
            report["cleanup_model_state"] = f"GET {last}"
            gone = last == 404
            report["cleanup_model_gone"] = gone
            return gone

        if stage("model delete", delete_model):
            report["owned_model_id"] = None
        else:
            residual("model", model_id,
                     report.get("cleanup_model_state") or observe(MODELS + f"/{model_id}"),
                     f"DELETE {MODELS}/{model_id}")

    def stop_agent():
        agent = holder.get("agent")
        if agent is not None and agent.poll() is None:
            agent.terminate()
            try:
                agent.wait(timeout=10)
            except subprocess.TimeoutExpired:
                agent.kill()
                agent.wait(timeout=5)
        stopped = agent is None or agent.poll() is not None
        report["agent_stopped"] = stopped
        return stopped

    if not stage("agent stop", stop_agent):
        agent = holder.get("agent")
        residual("agent", getattr(agent, "pid", "unknown"), "running",
                 f"kill {getattr(agent, 'pid', '<pid>')}")
    log = holder.get("log")
    if log is not None and not log.closed:
        try:
            log.close()
        except Exception:
            errors.append("agent log close failed")
            cleanup_ok = False
    if report.get("owned_node_uuid"):
        node_uuid = report["owned_node_uuid"]

        def delete_node():
            if inject_node_delete_timeout:
                raise TimeoutError("injected node delete timeout")
            status, _ = request(server.base, "DELETE",
                             NODES + f"/{node_uuid}?force=true", token=token)
            report["cleanup_node_status"] = status
            last = request(server.base, "GET", NODES + f"/{node_uuid}", token=token)[0]
            report["cleanup_node_state"] = f"GET {last}"
            gone = last == 404
            report["cleanup_node_gone"] = gone
            return gone

        if stage("node delete", delete_node):
            report["owned_node_uuid"] = None
        else:
            residual("node", node_uuid,
                     report.get("cleanup_node_state") or observe(NODES + f"/{node_uuid}"),
                     f"DELETE {NODES}/{node_uuid}?force=true")
    report["cleanup_steps"] = steps
    report["cleanup_failed_stages"] = [step["stage"] for step in steps if not step["ok"]]
    if errors:
        report["cleanup_errors"] = errors
    return cleanup_ok
