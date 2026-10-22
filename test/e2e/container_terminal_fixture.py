"""Owned terminal identities and release oracles; all Docker commands are scoped.

An additional read-only account is fixture data in this run's private SQLite.
The browser still performs real authentication and management authorization.
"""
from datetime import datetime, timezone
import re
import sqlite3
import subprocess
import time

import instance_diagnostics_fixture as diagnostics
from local_browser_fixture import http
from platform_token_read_revoke import check


def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
    data = diagnostics.prepare(server, token, run_id, report, holder, agent_bin, artifacts)
    username = "e2e-" + run_id + "-reader"
    report["owned_terminal_username"] = username
    database = server.work / "server-data/gpuworks.db"
    now = datetime.now(timezone.utc).isoformat()
    with sqlite3.connect(database) as connection:
        cursor = connection.execute("INSERT INTO users(username,password_hash,role,created_at,updated_at) SELECT ?,password_hash,'user',?,? FROM users WHERE username='admin'", (username, now, now))
        check(cursor.rowcount == 1, "terminal permission fixture did not create one user")
    data["username"] = username
    holder["terminal_contexts"], holder["terminal_pages"] = [], []
    return data


def audits(server, token):
    status, value = http(server.base, "GET", "/api/v1/terminal/sessions?limit=50", token=token)
    check(status == 200 and isinstance(value, dict) and isinstance(value.get("items"), list),
          "terminal audit query failed")
    return value["items"]


def assert_preflight(value):
    groups = {bool: ("enabled", "can_open", "container_supported"),
              int: ("idle_timeout_min", "max_per_node", "max_global", "active_total", "active_node"),
              str: ("reason", "container_reason", "container_runtime", "container_capability")}
    check(type(value) is dict and set(value) == {field for fields in groups.values() for field in fields},
          "terminal preflight schema differs")
    check(all(type(value[field]) is kind for kind, fields in groups.items() for field in fields),
          "terminal preflight JSON types differ")
    return value


def exit_code(controls):
    exits = [msg for msg in controls if isinstance(msg, dict) and msg.get("type") == "exit"]
    check(len(exits) == 1 and type(exits[0].get("code")) is int,
          "one integer WS exit code is required")
    return exits[0]["code"]


def admission(server, token, node_uuid):
    status, value = http(server.base, "GET", "/api/v1/nodes/" + node_uuid + "/terminal/settings", token=token)
    check(status == 200, "terminal preflight query failed")
    return assert_preflight(value)


def process_identity(raw, expected_pid=None):
    """Linux /proc/PID/stat identity includes start time, protecting PID reuse."""
    match = re.fullmatch(r"([0-9]+) \((.*)\) (.*)", raw.strip())
    check(match is not None, "invalid process stat identity")
    pid, fields = int(match[1]), match[3].split()
    check(pid > 1 and (expected_pid is None or pid == expected_pid) and len(fields) >= 20,
          "terminal process identity differs")
    check(fields[19].isdigit(), "process start time must be numeric")
    return (pid, int(fields[19]))


def observed_process(container, identity):
    check(isinstance(identity, tuple) and len(identity) == 2 and type(identity[0]) is int and identity[0] > 1,
          "only a previously observed terminal process may be queried")
    path = "/proc/" + str(identity[0]) + "/stat"
    result = subprocess.run(["docker", "exec", container, "/bin/sh", "-c",
        'LC_ALL=C exec cat "$1"', "sh", path], capture_output=True, timeout=20)
    if result.returncode == 0:
        return process_identity(result.stdout.decode().strip(), identity[0])
    # The cached Alpine fixture uses BusyBox cat. Only its exact ENOENT
    # diagnostic proves absence; permission, daemon and container errors fail.
    missing = "cat: can't open '" + path + "': No such file or directory"
    if result.returncode == 1 and not result.stdout and result.stderr.decode().strip() == missing:
        return None
    raise AssertionError("terminal process lookup failed: " + result.stderr.decode(errors="replace"))


def wait_audit(page, server, token, node_uuid, ref, prior, active, timeout=20):
    deadline, last = time.monotonic() + timeout, None
    while time.monotonic() < deadline:
        rows = audits(server, token)
        candidates = [row for row in rows if row["id"] not in prior]
        check(len(candidates) <= 1, "one terminal action created multiple audit records")
        if candidates:
            last = candidates[0]
            check(last["username"] == "admin" and last["node_uuid"] == node_uuid
                  and last["target"] == "container" and last["ref"] == ref,
                  "terminal audit points at the wrong principal or target")
            if last["active"] is active:
                if not active:
                    check(bool(last.get("ended_at")) and last.get("close_reason") in {"shell_exited", "client_closed"},
                          "closed audit does not carry an end reason")
                return last
        page.wait_for_timeout(100)
    raise AssertionError(f"terminal audit did not reach active={active}: {last}")


def wait_released(page, server, token, node_uuid, container, identity, audit_id, timeout=25):
    deadline, last = time.monotonic() + timeout, None
    while time.monotonic() < deadline:
        rows = [row for row in audits(server, token) if row["id"] == audit_id]
        check(len(rows) == 1, "owned audit identity disappeared")
        last = rows[0]
        preflight = admission(server, token, node_uuid)
        current = observed_process(container, identity)
        if (last["active"] is False and bool(last.get("ended_at")) and current != identity
                and preflight["active_node"] == 0 and preflight["active_total"] == 0):
            return last
        page.wait_for_timeout(100)
    raise AssertionError(f"terminal process/session was not released: {last}; identity={identity}")


def cleanup(server, token, report, holder):
    """Keep domain cleanup independent from extra context, session and user failures."""
    steps, residuals = [], []
    def stage(name, action):
        try:
            check(action() is True, name + " not independently confirmed")
            okay = True
        except Exception as error:
            okay = False
            residuals.append({"kind": name, "id": report.get("owned_terminal_username", report["run_id"]),
                "state": str(error), "reclaim": "inspect only this run's terminal node, context and fixture user"})
        steps.append({"stage": name, "ok": okay})
        return okay
    for index, target in enumerate(holder.get("terminal_pages", [])):
        stage("terminal page " + str(index), lambda target=target: (target.close(), target.is_closed())[1] if not target.is_closed() else True)
    for index, observed in enumerate(holder.get("terminal_contexts", [])):
        def close_context(observed=observed):
            if not observed["closed"]:
                observed["context"].close()
            return observed["closed"] is True
        stage("terminal context " + str(index), close_context)
    node_uuid = report.get("owned_node_uuid")
    if node_uuid and token:
        def sessions_closed():
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                preflight = admission(server, token, node_uuid)
                if preflight["active_node"] == 0 and preflight["active_total"] == 0:
                    return True
                time.sleep(0.1)
            raise AssertionError("terminal sessions remain active")
        stage("terminal sessions", sessions_closed)
    username = report.get("owned_terminal_username")
    if username:
        def delete_user():
            with sqlite3.connect(server.work / "server-data/gpuworks.db") as connection:
                connection.execute("DELETE FROM users WHERE username=? AND role='user'", (username,))
                count = connection.execute("SELECT count(*) FROM users WHERE username=?", (username,)).fetchone()[0]
            check(count == 0, "owned permission fixture user remains")
            report["owned_terminal_username"] = None
            return True
        stage("terminal fixture user", delete_user)
    try:
        domain_ok = diagnostics.cleanup(server, token, report, holder)
    finally:
        report.setdefault("cleanup_steps", []).extend(steps)
        report.setdefault("remaining_resources", []).extend(residuals)
    return domain_ok and all(step["ok"] for step in steps) and not residuals
