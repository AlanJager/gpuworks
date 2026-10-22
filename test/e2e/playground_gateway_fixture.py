"""Owned Playground runtime cleanup; failures retain scoped recovery evidence.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import subprocess
import time
import traceback

DEPLOYMENTS = "/api/v1/deployments"
MODELS = "/api/v1/models"
NODES = "/api/v1/nodes"


def cleanup(server, token, report, holder, *, http, docker_running, inject):
    errors, steps, remaining = [], [], []
    report["remaining_resources"] = remaining

    def stage(name, action):
        try:
            ok = bool(action())
        except Exception:
            errors.append(f"{name} failed: " + traceback.format_exc().splitlines()[-1])
            ok = False
        steps.append({"stage": name, "ok": ok})
        return ok

    def residual(kind, ident, state, reclaim):
        remaining.append({"kind": kind, "id": ident, "state": state, "reclaim": reclaim})

    def observe(path):
        try:
            return http(server.base, "GET", path, token=token)[0]
        except Exception:
            return None

    dep_id = report.get("owned_deployment_id")
    if dep_id:
        def discover_containers():
            status, deployment = http(server.base, "GET", DEPLOYMENTS + f"/{dep_id}", token=token)
            if status == 404:
                return True
            if status != 200:
                return False
            containers = report.setdefault("containers", [])
            for instance in deployment.get("instances") or []:
                container = instance.get("container_id")
                if container and container not in containers:
                    containers.append(container)
            return True
        stage("discover containers", discover_containers)

    def delete_owned(kind, key, path, *, query="", wait=0):
        ident = report.get(key)
        if not ident:
            return
        def remove():
            status, _ = http(server.base, "DELETE", path + query, token=token)
            report[f"cleanup_{kind}_status"] = status
            deadline = time.monotonic() + wait
            while True:
                last = http(server.base, "GET", path, token=token)[0]
                if last == 404 or time.monotonic() >= deadline:
                    break
                time.sleep(0.5)
            return last == 404
        ok = stage(f"{kind} delete", remove)
        last = 404 if ok else observe(path)
        report[f"cleanup_{kind}_gone"] = last == 404
        if last == 404:
            report[key] = None
        else:
            residual(kind, ident, f"GET {last}" if last else "unknown (query failed)",
                     f"DELETE {path}{query}")

    if dep_id:
        delete_owned("deployment", "owned_deployment_id", DEPLOYMENTS + f"/{dep_id}", wait=90)

    # Stop the agent before emergency Docker cleanup so a remaining desired
    # deployment cannot recreate a container while its control plane is down.
    def stop_agent():
        agent = holder.get("agent")
        if agent is not None and agent.poll() is None:
            agent.terminate()
            try:
                agent.wait(timeout=10)
            except subprocess.TimeoutExpired:
                agent.kill()
                agent.wait(timeout=5)
        report["agent_stopped"] = agent is None or agent.poll() is not None
        return report["agent_stopped"]
    if not stage("agent stop", stop_agent):
        residual("agent", getattr(holder.get("agent"), "pid", "unknown"), "running or unknown", "stop the owned agent PID")
    log = holder.get("log")
    if log is not None and not log.closed:
        stage("agent log close", lambda: log.close() is None)

    for container in report.get("containers") or []:
        def remove_container(container=container):
            if inject["cleanup_timeout"]:
                raise TimeoutError("injected cleanup timeout")
            deadline = time.monotonic() + (30 if report.get("cleanup_deployment_gone") else 0)
            while time.monotonic() < deadline and docker_running(container) != "absent":
                time.sleep(0.5)
            if docker_running(container) != "absent":
                # This is fixture rescue, not evidence that product deletion
                # worked. Record the failed lifecycle before scoped removal.
                steps.append({"stage": f"container {container} product removal", "ok": False})
                result = subprocess.run(["docker", "rm", "-f", container], capture_output=True, text=True)
                report.setdefault("forced_container_removal", {})[container] = result.returncode
            absent = docker_running(container) == "absent"
            report.setdefault("cleanup_containers", {})[container] = "absent" if absent else "present"
            return absent
        if not stage(f"container {container}", remove_container):
            residual("container", container, "present or unknown", f"docker rm -f {container}")

    if report.get("owned_model_id"):
        model_id = report["owned_model_id"]
        delete_owned("model", "owned_model_id", MODELS + f"/{model_id}")
    if report.get("owned_node_uuid"):
        node_uuid = report["owned_node_uuid"]
        delete_owned("node", "owned_node_uuid", NODES + f"/{node_uuid}", query="?force=true")
    report["cleanup_steps"] = steps
    report["cleanup_errors"] = errors
    return all(step["ok"] for step in steps)
