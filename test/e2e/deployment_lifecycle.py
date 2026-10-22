#!/usr/bin/env python3
"""Deployment / real container stop, start and delete through the browser.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-deployment-lifecycle
After build: python3 test/e2e/deployment_lifecycle.py --locale en
Requires Python Playwright, system Chrome, Docker and the built server/agent.
The GPU facts are injected only so the scheduler places the deployment; the
container, engine process and gateway request are real Docker/fake-engine.
No real GPU compute, dGPU isolation or vendor driver is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server/agent and
retains the private database/logs for scoped diagnosis (seven days).
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check
import deployment_container_fixture as container_fixture

CONTRACT = "gpuworks/deployment/container-lifecycle"
CASE = "部署 / 真实容器生命周期 / 停止、启动、网关请求、删除与资源回收"
DEPLOYMENTS = "/api/v1/deployments"
MODELS = "/api/v1/models"
APIKEYS = "/api/v1/apikeys"
NODES = "/api/v1/nodes"
MODEL_DIR = ROOT / "test/e2e/testdata/fake-model"
IMAGE = "gpuworks/fake-engine:test"
PROFILE = "/v1/chat/completions"


def strings(locale):
    deployments = json.loads((ROOT / "ui/src/i18n/locales" / locale / "deployments.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return deployments, auth


def wait_online(base, token, node_uuid, agent):
    deadline = time.time() + 40
    while time.time() < deadline:
        status, node = http(base, "GET", NODES + f"/{node_uuid}", token=token)
        if status == 200 and node.get("state") == "online":
            return node
        if agent.poll() is not None:
            raise AssertionError("the agent exited before the node came online")
        time.sleep(0.5)
    raise AssertionError("the node never came online")


INJECT = {"docker_error": False}


def docker_running(container_id):
    if container_id and INJECT["docker_error"]:
        raise AssertionError("injected docker daemon error")
    return container_fixture.docker_running(container_id)


def wait_api(page, server, dep_id, predicate, timeout=90, label="deployment", report=None):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        response = page.request.get(server.base + DEPLOYMENTS + f"/{dep_id}")
        if response.status == 404:
            last = "404"
        elif response.status == 200:
            last = response.json()
            if report is not None:
                container_fixture.remember_containers(report, last)
            if predicate(last):
                return last
        time.sleep(0.5)
    raise AssertionError(f"{label} stayed {last}")


# Contract: gpuworks/deployment/container-lifecycle
# Mainline: 独立 server/SQLite + 真实 agent/Docker + fake-engine 容器（GPU 事实仅用于调度）
#           → 部署运行并可通过网关请求 → UI 停止后容器退出 → UI 启动后新容器运行并再次可请求
#           → UI 删除后部署 404、容器消失 → 停止 agent、删除节点与模型
def test_deployment_lifecycle(page, context, server, artifacts, report, locale,
                              node, deployment, api_key):
    deployments, auth = strings(locale)
    detail = deployments["detail"]
    delete = deployments["delete"]
    dep_id, dep_name, served_name = deployment["id"], deployment["name"], deployment["served_name"]
    page_errors, console_errors, bad_responses = [], [], []
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))

    def console_seen(message):
        if message.type == "error":
            if (not authenticated
                    and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me"
                    and "401" in message.text):
                return
            console_errors.append(message.text)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"method": response.request.method, "path": path,
                                  "status": response.status})

    page.on("console", console_seen)
    page.on("response", response_seen)

    page.goto(server.base + "/deployments")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=deployments["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    initial_doc = wait_api(page, server, dep_id,
                           lambda d: d.get("runtime_state") == "running"
                           and any(i.get("state") == "running" and i.get("container_id")
                                   for i in d.get("instances") or []),
                           label="initial running deployment", report=report)
    initial_container = next(i["container_id"] for i in initial_doc["instances"]
                             if i.get("state") == "running" and i.get("container_id"))
    report["container_id"] = initial_container
    container_fixture.remember_containers(report, initial_doc)
    row = page.get_by_role("row").filter(has_text=dep_name)
    expect(row).to_have_count(1)
    row.get_by_role("button", name=dep_name, exact=True).click()
    drawer = page.get_by_role("dialog").last
    expect(drawer).to_be_visible()
    expect(drawer).to_be_in_viewport(ratio=1)
    expect(drawer.get_by_text(dep_name, exact=True).first).to_be_visible()
    report["last_step"] = "deployment-drawer-open"
    check(docker_running(initial_container) == "true",
          f"the listed container is not running: {initial_container}")

    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}/stop"
                              and r.request.method == "POST") as stopped:
        button(drawer, detail["stop"]).click()
        popconfirm = page.locator(".ant-popconfirm")
        expect(popconfirm).to_be_visible(timeout=5000)
        popconfirm.locator(".ant-btn-primary").click()
    check(stopped.value.status == 200, f"stop HTTP {stopped.value.status}")
    expect(page.locator(".ant-message").get_by_text(detail["stopped"], exact=True)).to_be_visible()
    stopped_doc = wait_api(page, server, dep_id,
                           lambda d: d.get("desired_state") == "stopped"
                           and not any(i.get("state") in ("running", "starting")
                                       for i in d.get("instances") or []),
                           label="stopped", report=report)
    deadline = time.time() + 30
    while time.time() < deadline and docker_running(initial_container) != "absent":
        time.sleep(0.5)
    check(docker_running(initial_container) == "absent",
          f"the stopped container is still present: {initial_container}")
    report["last_step"] = "deployment-stopped"

    expect(button(drawer, detail["start"])).to_be_visible(timeout=30000)
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}/start"
                              and r.request.method == "POST") as started:
        button(drawer, detail["start"]).click()
    check(started.value.status == 200, f"start HTTP {started.value.status}")
    expect(page.locator(".ant-message").get_by_text(detail["started"], exact=True)).to_be_visible()
    running_doc = wait_api(page, server, dep_id,
                           lambda d: d.get("runtime_state") == "running"
                           and any(i.get("state") == "running" and i.get("container_id")
                                   for i in d.get("instances") or []),
                           label="running", report=report)
    running_container = next(i["container_id"] for i in running_doc["instances"]
                             if i.get("state") == "running" and i.get("container_id"))
    check(running_container != initial_container,
          "start reused the stopped container instead of creating a new one")
    if running_container not in report["containers"]:
        report["containers"].append(running_container)
    check(docker_running(running_container) == "true",
          f"the started container is not running: {running_container}")
    chat = page.request.post(server.base + PROFILE,
                             headers={"Authorization": f"Bearer {api_key}"},
                             data={"model": served_name,
                                   "messages": [{"role": "user", "content": "lifecycle"}]})
    container_fixture.assert_chat_response(chat, "gateway chat after start")
    report["last_step"] = "deployment-started-and-serving"

    # The delete button carries an icon whose label prefixes the accessible name.
    drawer.get_by_role("button", name=re.compile(re.escape(delete["button"]) + "$")).click()
    modal = page.get_by_role("dialog").last
    expect(modal.get_by_text(dep_name)).to_be_visible()
    confirm = modal.locator(".ant-modal-footer .ant-btn-primary")
    expect(modal).to_be_in_viewport(ratio=1)
    expect(confirm).to_be_in_viewport(ratio=1)
    # AntD spaces two-character Chinese button text.
    expect(confirm).to_contain_text(
        re.compile(r"\s*".join(re.escape(char) for char in delete["button"])))
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}"
                              and r.request.method == "DELETE") as deleted:
        confirm.click()
    check(deleted.value.status in (200, 202, 204), f"delete HTTP {deleted.value.status}")
    expect(page.locator(".ant-message").get_by_text(delete["started"], exact=True)).to_be_visible()
    deadline = time.time() + 90
    while time.time() < deadline:
        if page.request.get(server.base + DEPLOYMENTS + f"/{dep_id}").status == 404:
            break
        time.sleep(0.5)
    check(page.request.get(server.base + DEPLOYMENTS + f"/{dep_id}").status == 404,
          "the deleted deployment is still readable")
    report["owned_deployment_id"] = None
    deadline = time.time() + 30
    while time.time() < deadline and docker_running(running_container) != "absent":
        time.sleep(0.5)
    check(docker_running(running_container) == "absent",
          f"the deleted deployment's container remains: {running_container}")
    page.goto(server.base + "/deployments")
    expect(page.get_by_role("row").filter(has_text=dep_name)).to_have_count(0, timeout=30000)
    report["last_step"] = "deployment-deleted"

    expected_404 = {"method": "GET", "path": DEPLOYMENTS + f"/{dep_id}", "status": 404}
    unexpected = [item for item in bad_responses if item != expected_404]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    allowed_console = [item for item in console_errors
                       if "404" in item and ("load resource" in item or "加载资源" in item)]
    check(len(allowed_console) <= 2 and len(allowed_console) == len(console_errors)
          and not page_errors,
          f"browser errors: {page_errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/deployments", "row": dep_name,
         "drawer": detail["title"], "stop": detail["stop"], "start": detail["start"],
         "stopped": detail["stopped"], "started": detail["started"],
         "delete": delete["button"]},
        {"layer": "request", "stop": {"path": DEPLOYMENTS + f"/{dep_id}/stop",
                                      "status": stopped.value.status},
         "start": {"path": DEPLOYMENTS + f"/{dep_id}/start",
                   "status": started.value.status},
         "delete": {"path": DEPLOYMENTS + f"/{dep_id}",
                    "status": deleted.value.status},
         "chat": {"path": PROFILE, "status": chat.status, "model": served_name}},
        {"layer": "persistence", "stopped": {"desired_state": stopped_doc.get("desired_state"),
                                             "instances": [i.get("state") for i in
                                                           stopped_doc.get("instances") or []]},
         "running": {"runtime_state": running_doc.get("runtime_state"),
                     "ready_replicas": running_doc.get("ready_replicas")},
         "after_delete": 404},
        {"layer": "execution", "initial_container": {"id": initial_container,
                                                     "running": "true"},
         "stopped_container": {"id": initial_container, "present": "absent"},
         "running_container": {"id": running_container, "running": "true"},
         "deleted_container": {"id": running_container, "present": "absent"},
         "gateway_choices": True, "image": IMAGE},
        {"layer": "cleanup", "deployment_deleted": True, "container_absent": True},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}


def create_fixtures(server, token, run_id, report, holder, agent_bin,
                    inject_model_failure=False, remember=lambda value: None):
    """Create the fixture resources, registering each one before the next.

    A later failure must leave every earlier resource in the report and the
    running agent in the holder so cleanup can reclaim it.
    """
    status, created = http(server.base, "POST", NODES, token=token,
                           body={"name": f"e2e-{run_id}-deploy-node",
                                 "server_url": server.base})
    check(status == 201, f"node fixture HTTP {status}")
    node, node_token = created["node"], created["token"]
    remember(node_token)
    report["owned_node_uuid"] = node["uuid"]
    state = server.work / "agent-state"
    state.mkdir()
    log = (server.work / "agent.log").open("w")
    holder["log"] = log
    env = {**os.environ, "GPUWORKS_FAKE_GPU": "A100x1,24576",
           "GPUWORKS_FAKE_GPU_NO_INJECT": "1"}
    agent = subprocess.Popen([str(agent_bin), "run",
                              "--server", server.base, "--token", node_token,
                              "--state", str(state)],
                             stdout=log, stderr=subprocess.STDOUT, text=True, env=env)
    holder["agent"], holder["log"] = agent, log
    wait_online(server.base, token, node["uuid"], agent)
    if inject_model_failure:
        # Self-test: the agent is already running when preparation fails.
        raise RuntimeError("injected model registration failure")
    status, model = http(server.base, "POST", MODELS + "/local", token=token, body={
        "node_uuid": node["uuid"], "path": str(MODEL_DIR),
        "source_id": f"e2e/{run_id}-lifecycle", "name": f"e2e-{run_id}-model"})
    check(status in (200, 201), f"model fixture HTTP {status}")
    report["owned_model_id"] = model["id"]
    status, key = http(server.base, "POST", APIKEYS, token=token,
                       body={"name": f"e2e-{run_id}-key"})
    check(status == 201, f"inference key fixture HTTP {status}")
    remember(key["key"])
    report["owned_key_name"] = key["name"]
    dep_name = f"e2e-{run_id}-deployment"
    served_name = f"e2e-{run_id}-served"
    status, deployment = http(server.base, "POST", DEPLOYMENTS, token=token, body={
        "model_id": model["id"], "node_uuid": node["uuid"], "name": dep_name,
        "served_name": served_name, "engine": "vllm", "image": IMAGE,
        "placement_mode": "manual",
        "container_overrides": {"command_mode": "engine"}})
    check(status in (200, 201), f"deployment fixture HTTP {status} {deployment}")
    deployment["name"], deployment["served_name"] = dep_name, served_name
    report["owned_deployment_id"] = deployment["id"]
    return node, node_token, model, key, deployment


def cleanup(server, token, report, holder):
    return container_fixture.cleanup(
        server, token, report, holder, request=http, inspect=docker_running,
        inject_node_delete_timeout=INJECT.get("node_delete_timeout", False))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--inject-model-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-docker-error", action="store_true",
                        help=argparse.SUPPRESS)
    args = parser.parse_args()
    INJECT["docker_error"] = args.inject_docker_error
    os.umask(0o077)
    check(shutil.which("docker") is not None, "docker is required for this lane")
    check(subprocess.run(["docker", "image", "inspect", IMAGE], capture_output=True).returncode == 0,
          f"{IMAGE} is missing; run make fake-engine-image")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/deployment-lifecycle.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_deployment_lifecycle.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-agent-docker-fake-gpu-facts", "identity": "admin browser / node agent / Docker",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "container_fixture_sha256": digest(ROOT / "test/e2e/deployment_container_fixture.py"),
        "execution": "real Docker container running the audited fake-engine image; GPU facts are injected only for scheduling",
        "runtime": "real container and gateway request; no real GPU compute, dGPU isolation or vendor driver",
        "not_covered": ["real GPU inference, performance and dGPU isolation",
                        "multi-replica rolling restart and placement across nodes",
                        "deployment creation through the deploy form (created as a fixture)",
                        "engine failure, OOM and health-check recovery",
                        "container terminal and log stream for the deployment",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/deployment_lifecycle.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {"agent": None, "log": None}
    token = ""
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        agent_bin = args.agent_bin.resolve()
        report["agent_bin"] = str(agent_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "password"})
        check(status == 200, f"fixture login HTTP {status}")
        token = login["token"]
        artifacts.remember(token)
        # The environment gate passed; a fixture failure from here is a test
        # failure, not BLOCKED_INFRA.
        report["preflight"] = "PASS"
        node, node_token, model, key, deployment = create_fixtures(
            server, token, run_id, report, holder, agent_bin,
            inject_model_failure=args.inject_model_failure, remember=artifacts.remember)
        artifacts.remember(token)
        artifacts.remember(node_token)
        artifacts.remember(key["key"])
        artifacts.write_json("report.json", report)
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_deployment_lifecycle(page, context, server, artifacts, report,
                                          args.locale, node, deployment, key["key"])
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                report["case_completed"] = True
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                close_ok = True
                try:
                    context.close()
                except Exception:
                    close_ok = False
                    report["context_close_error"] = artifacts.redact(
                        traceback.format_exc())
                try:
                    browser.close()
                except Exception:
                    close_ok = False
                    report["browser_close_error"] = artifacts.redact(
                        traceback.format_exc())
                report["browser_close_ok"] = close_ok
                if not close_ok:
                    report["status"] = "FAIL_TEST"
                    report.setdefault("failure", "the browser did not close cleanly")
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        cleanup_ok = False
        try:
            cleanup_ok = cleanup(server, token, report, holder)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        cleanup_ok = cleanup_ok and report["server_stopped"]
        # No browser was opened when preparation failed; only a close that was
        # attempted and failed makes the case non-PASS.
        browser_ok = report.get("browser_close_ok", True)
        cleanup_ok = cleanup_ok and browser_ok
        report["environment_cleanup_ok"] = cleanup_ok
        layers = {item["layer"] for item in report["evidence"]}
        if (report.get("case_completed") and cleanup_ok
                and report.get("status") not in ("FAIL_TEST", "BLOCKED_INFRA")
                and set(manifest["required_evidence"]) <= layers):
            report["status"] = "PASS"
        elif report["status"] not in ("FAIL_TEST", "BLOCKED_INFRA"):
            report["status"] = "FAIL_TEST"
            report.setdefault("failure",
                              "cleanup or required evidence did not complete")
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        agent_log_path = artifacts.private / "agent.log"
        if agent_log_path.exists():
            (artifacts.directory / "agent.log").write_text(artifacts.redact(agent_log_path.read_text()))
        report["private_state"] = (
            "private/ retains isolated DB/logs and agent state; "
            + ("see remaining_resources or cleanup_errors for scoped recovery; "
               if not cleanup_ok else "all owned processes and resources confirmed gone; ")
            + "remove only this run directory after diagnosis (7 days)")
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; private directory must not be uploaded"
            artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": CONTRACT, "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
