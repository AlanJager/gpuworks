"""Owned shared-directory resource cleanup, including partial failures.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import subprocess
import traceback

STORAGES = "/api/v1/storages"
NODES = "/api/v1/nodes"


def cleanup(server, token, report, holder, mount_path, inject_timeout=False,
            inject_node_delete_timeout=False, *, http, unmount_shared):
    """Reclaim leftovers; one failure must not skip the unmount or the rest."""
    cleanup_ok = True
    errors = []
    steps = []
    remaining = report.setdefault("remaining_resources", [])

    def observe(path):
        try:
            return f"GET {http(server.base, 'GET', path, token=token)[0]}"
        except Exception:
            return "unknown (query failed)"

    def residual(kind, ident, state, reclaim):
        remaining.append({"kind": kind, "id": ident, "state": state,
                          "reclaim": reclaim})

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

    if report.get("owned_storage_id"):
        storage_id = report["owned_storage_id"]

        def delete_storage():
            status, _ = http(server.base, "DELETE",
                             STORAGES + f"/{storage_id}", token=token)
            report["cleanup_storage_status"] = status
            last = http(server.base, "GET", STORAGES + f"/{storage_id}",
                        token=token)[0]
            report["cleanup_storage_state"] = f"GET {last}"
            gone = last == 404
            report["cleanup_storage_gone"] = gone
            return gone

        # Keep ownership unless the readback observes the storage is gone; a
        # failed deletion must leave a directly reclaimable ID in the report.
        if stage("storage delete", delete_storage):
            report["owned_storage_id"] = None
        else:
            state = report.get("cleanup_storage_state") or observe(STORAGES + f"/{storage_id}")
            if state == "GET 404":
                report["owned_storage_id"] = None
            else:
                residual("storage", storage_id, state, f"DELETE {STORAGES}/{storage_id}")

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
            status, _ = http(server.base, "DELETE",
                             NODES + f"/{node_uuid}?force=true", token=token)
            report["cleanup_node_status"] = status
            if inject_timeout:
                raise TimeoutError("injected cleanup readback timeout")
            last = http(server.base, "GET", NODES + f"/{node_uuid}", token=token)[0]
            report["cleanup_node_state"] = f"GET {last}"
            gone = last == 404
            report["cleanup_node_gone"] = gone
            return gone

        if stage("node delete", delete_node):
            report["owned_node_uuid"] = None
        else:
            state = report.get("cleanup_node_state") or observe(
                NODES + f"/{node_uuid}")
            if state == "GET 404":
                report["owned_node_uuid"] = None
            else:
                residual("node", node_uuid, state,
                         f"DELETE {NODES}/{node_uuid}?force=true")

    def unmount():
        unmounted = unmount_shared(mount_path)
        report["mount_unmounted"] = unmounted
        return unmounted

    if not stage("mount unmount", unmount):
        residual("mount", str(mount_path), "mounted or unknown",
                 f"umount {mount_path}")
    report["cleanup_steps"] = steps
    report["cleanup_failed_stages"] = [step["stage"] for step in steps if not step["ok"]]
    if errors:
        report["cleanup_errors"] = errors
    return cleanup_ok
