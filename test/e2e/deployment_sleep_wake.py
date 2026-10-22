#!/usr/bin/env python3
"""Deployment sleep and wake: engine state, gateway refusal and L2 recovery.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-deployment-sleep-wake
After build: python3 test/e2e/deployment_sleep_wake.py --locale en
Requires Python Playwright, system Chrome, Docker and the built server/agent.
The GPU facts are injected only so the scheduler places the deployment; the
container and engine are real, and the engine's own /is_sleeping is read over
the host network. No real GPU compute or dGPU memory release is claimed.
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
from urllib.request import urlopen
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check
import deployment_container_fixture as container_fixture

CONTRACT = "gpuworks/deployment/sleep-wake-gateway"
CASE = "部署 / 休眠与唤醒 / 引擎真实休眠、网关 503、L1/L2 唤醒后恢复推理"
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


def engine_sleeping(port):
    try:
        with urlopen(f"http://127.0.0.1:{port}/is_sleeping", timeout=2) as reply:
            return json.loads(reply.read())["is_sleeping"]
    except Exception:
        return None


def wait_engine(port, sleeping, timeout=60):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = engine_sleeping(port)
        if last is sleeping:
            return last
        time.sleep(0.5)
    raise AssertionError(f"engine /is_sleeping stayed {last}, wanted {sleeping}")


def instance_state(doc):
    return next(iter(doc.get("instances") or []), {})


INJECT = {"docker_error": False, "node_delete_timeout": False}


def docker_running(container_id):
    if container_id and INJECT["docker_error"]:
        raise AssertionError("injected docker daemon error")
    return container_fixture.docker_running(container_id)


def sleep_mode_dialog(page, sleep, level):
    dialog = page.get_by_role("dialog").last
    title = sleep["sleepMode"]["titleDeep" if level == 2 else "titleQuick"]
    expect(dialog.get_by_text(title, exact=True)).to_be_visible()
    expect(dialog).to_be_in_viewport(ratio=1)
    expect(button(dialog, sleep["sleepMode"]["confirm"])).to_be_in_viewport(ratio=1)
    expect(dialog.get_by_role("radio", name=sleep["sleepMode"]["wait"])).to_be_checked()
    return dialog


# Contract: gpuworks/deployment/sleep-wake-gateway
# Mainline: 独立 server/SQLite + 真实 agent/Docker + fake-engine（GPU 事实仅用于调度）
#           → 部署运行并真实响应网关请求 → UI 休眠 L1，引擎 /is_sleeping=true，网关 503
#           → UI 唤醒，引擎恢复且网关 200 → UI 深度休眠 L2 并再次唤醒，网关 200
#           → 删除部署、停止 agent、清理节点与模型
def test_deployment_sleep_wake(page, context, server, artifacts, report, locale,
                               deployment, api_key):
    sleep, auth = strings(locale)
    detail = sleep["detail"]
    dep_id, dep_name, served_name = deployment["id"], deployment["name"], deployment["served_name"]
    page_errors, console_errors, bad_responses = [], [], []
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))

    def console_seen(message):
        if message.type == "error":
            path = urlsplit(message.location.get("url", "")).path
            if (not authenticated and path == "/api/v1/auth/me"
                    and "401" in message.text):
                return
            console_errors.append({"text": message.text, "path": path})

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
    expect(page.get_by_role("heading", name=sleep["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    running = wait_api(page, server, dep_id,
                       lambda d: d.get("runtime_state") == "running"
                       and any(i.get("state") == "running" and i.get("container_id")
                               for i in d.get("instances") or []),
                       label="running deployment", report=report)
    instance = instance_state(running)
    api_port = instance["api_port"]
    container = instance["container_id"]
    report["container_id"] = container
    container_fixture.remember_containers(report, running)
    check(docker_running(container) == "true", "the deployment container is not running")

    def chat():
        return page.request.post(server.base + PROFILE,
                                 headers={"Authorization": f"Bearer {api_key}"},
                                 data={"model": served_name,
                                       "messages": [{"role": "user", "content": "sleep"}]})

    baseline = chat()
    container_fixture.assert_chat_response(baseline, "gateway chat before sleep")
    row = page.get_by_role("row").filter(has_text=dep_name)
    expect(row).to_have_count(1)
    row.get_by_role("button", name=dep_name, exact=True).click()
    drawer = page.get_by_role("dialog").last
    expect(drawer).to_be_visible()
    expect(drawer).to_be_in_viewport(ratio=1)
    quick_action = drawer.get_by_role("button", name=sleep["sleep"]["quickAction"],
                                      exact=True)
    expect(quick_action).to_be_visible()
    report["last_step"] = "deployment-drawer-open"

    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}/sleep"
                              and r.request.method == "POST") as slept:
        quick_action.click()
        dialog = sleep_mode_dialog(page, sleep, 1)
        button(dialog, sleep["sleepMode"]["confirm"]).click()
    check(slept.value.status == 200, f"sleep HTTP {slept.value.status}")
    check(urlsplit(slept.value.url).query == "level=1&mode=wait",
          f"unexpected sleep order: {slept.value.url}")
    expect(page.locator(".ant-message").get_by_text(
        sleep["sleep"]["quickStarted"], exact=True)).to_be_visible()
    asleep_doc = wait_api(page, server, dep_id,
                          lambda d: instance_state(d).get("asleep") is True
                          and instance_state(d).get("sleep_level") == 1,
                          label="L1 asleep", report=report)
    check(wait_engine(api_port, True) is True, "the engine did not report itself asleep")
    check(instance_state(asleep_doc)["sleep_level"] == 1
          and instance_state(asleep_doc)["asleep"] is True,
          f"unexpected L1 state: {instance_state(asleep_doc)}")
    tag = sleep["sleep"]["sleeping"].replace("{{level}}", "1").replace(
        "{{name}}", sleep["sleep"]["quick"])
    expect(drawer.get_by_text(tag, exact=True)).to_be_visible(timeout=30000)
    refused = chat()
    check(refused.status == 503 and "asleep" in refused.text().lower(),
          f"the gateway served a sleeping deployment: {refused.status} {refused.text()[:300]}")
    l1_refused_status = refused.status
    check(docker_running(container) == "true",
          "L1 sleep must keep the container and its process")
    report["last_step"] = "deployment-l1-asleep"

    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}/wake"
                              and r.request.method == "POST") as woke:
        button(drawer, sleep["sleep"]["wake"]).click()
    check(woke.value.status == 200, f"wake HTTP {woke.value.status}")
    check(urlsplit(woke.value.url).query == "", f"unexpected wake order: {woke.value.url}")
    expect(page.locator(".ant-message").get_by_text(
        sleep["sleep"]["wakeStarted"], exact=True)).to_be_visible()
    awake_doc = wait_api(page, server, dep_id,
                         lambda d: instance_state(d).get("asleep") is False
                         and instance_state(d).get("sleep_level") == 0,
                         label="L1 awake", report=report)
    check(wait_engine(api_port, False) is False, "the engine did not wake")
    check(instance_state(awake_doc)["asleep"] is False, "the instance still reports asleep")
    served = chat()
    container_fixture.assert_chat_response(served, "gateway chat after L1 wake")
    report["last_step"] = "deployment-l1-awake"

    deep_action = drawer.get_by_role("button", name=sleep["sleep"]["deepAction"],
                                     exact=True)
    expect(deep_action).to_be_visible(timeout=30000)
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}/sleep"
                              and r.request.method == "POST") as slept_deep:
        deep_action.click()
        deep_dialog = sleep_mode_dialog(page, sleep, 2)
        button(deep_dialog, sleep["sleepMode"]["confirm"]).click()
    check(slept_deep.value.status == 200, f"deep sleep HTTP {slept_deep.value.status}")
    check(urlsplit(slept_deep.value.url).query == "level=2&mode=wait",
          f"unexpected deep sleep order: {slept_deep.value.url}")
    expect(page.locator(".ant-message").get_by_text(
        sleep["sleep"]["deepStarted"], exact=True)).to_be_visible()
    deep_doc = wait_api(page, server, dep_id,
                        lambda d: instance_state(d).get("asleep") is True
                        and instance_state(d).get("sleep_level") == 2,
                        label="L2 asleep", report=report)
    check(wait_engine(api_port, True) is True, "the engine did not enter deep sleep")
    tag = sleep["sleep"]["sleeping"].replace("{{level}}", "2").replace(
        "{{name}}", sleep["sleep"]["deep"])
    expect(drawer.get_by_text(tag, exact=True)).to_be_visible(timeout=30000)
    refused = chat()
    check(refused.status == 503 and "asleep" in refused.text().lower(),
          f"the gateway served a deep-sleeping deployment: {refused.status}")
    l2_refused_status = refused.status
    report["last_step"] = "deployment-l2-asleep"

    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}/wake"
                              and r.request.method == "POST") as woke_deep:
        button(drawer, sleep["sleep"]["wake"]).click()
    check(woke_deep.value.status == 200, f"deep wake HTTP {woke_deep.value.status}")
    wait_api(page, server, dep_id,
             lambda d: instance_state(d).get("asleep") is False
             and instance_state(d).get("sleep_level") == 0, label="L2 awake", report=report)
    check(wait_engine(api_port, False) is False, "the engine did not wake from L2")
    served = chat()
    container_fixture.assert_chat_response(served, "gateway chat after L2 wake")
    report["last_step"] = "deployment-l2-awake"

    # Reclaim the fixture here so the cleanup evidence below is observed, not
    # asserted. Deletion through the UI is covered by the lifecycle contract.
    deleted = page.request.delete(server.base + DEPLOYMENTS + f"/{dep_id}")
    check(deleted.status in (200, 202, 204), f"delete HTTP {deleted.status}")
    deadline = time.time() + 90
    while time.time() < deadline and page.request.get(
            server.base + DEPLOYMENTS + f"/{dep_id}").status != 404:
        time.sleep(0.5)
    check(page.request.get(server.base + DEPLOYMENTS + f"/{dep_id}").status == 404,
          "the deployment still exists after deletion")
    report["owned_deployment_id"] = None
    deadline = time.time() + 30
    while time.time() < deadline and docker_running(container) != "absent":
        time.sleep(0.5)
    check(docker_running(container) == "absent",
          f"the deleted deployment's container remains: {container}")
    report["last_step"] = "deployment-deleted"

    # The open drawer may poll once after the deletion made the deployment 404.
    expected = [{"method": "POST", "path": PROFILE, "status": 503},
                {"method": "GET", "path": DEPLOYMENTS + f"/{dep_id}", "status": 404}]
    unexpected = [item for item in bad_responses if item not in expected]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    allowed_console = [item for item in console_errors
                       if item["path"] == DEPLOYMENTS + f"/{dep_id}"
                       and "404" in item["text"]]
    check(not page_errors and len(allowed_console) == len(console_errors),
          f"browser errors: {page_errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/deployments", "row": dep_name,
         "quickAction": sleep["sleep"]["quickAction"],
         "deepAction": sleep["sleep"]["deepAction"], "wake": sleep["sleep"]["wake"],
         "quickStarted": sleep["sleep"]["quickStarted"],
         "deepStarted": sleep["sleep"]["deepStarted"], "tag_levels": [1, 2]},
        {"layer": "request",
         "sleep_l1": {"path": DEPLOYMENTS + f"/{dep_id}/sleep", "status": slept.value.status,
                      "query": urlsplit(slept.value.url).query},
         "sleep_l2": {"path": DEPLOYMENTS + f"/{dep_id}/sleep",
                      "status": slept_deep.value.status,
                      "query": urlsplit(slept_deep.value.url).query},
         "wake_l1": {"path": DEPLOYMENTS + f"/{dep_id}/wake", "status": woke.value.status},
         "wake_l2": {"path": DEPLOYMENTS + f"/{dep_id}/wake", "status": woke_deep.value.status},
         "chat_awake": {"status": served.status},
         "chat_asleep_l1": {"status": l1_refused_status},
         "chat_asleep_l2": {"status": l2_refused_status},
         "delete": {"path": DEPLOYMENTS + f"/{dep_id}", "status": deleted.status}},
        {"layer": "persistence",
         "l1": {"sleep_level": instance_state(asleep_doc)["sleep_level"],
                "asleep": instance_state(asleep_doc)["asleep"]},
         "awake": {"sleep_level": instance_state(awake_doc)["sleep_level"],
                   "asleep": instance_state(awake_doc)["asleep"]},
         "l2": {"sleep_level": instance_state(deep_doc)["sleep_level"],
                "asleep": instance_state(deep_doc)["asleep"]}},
        {"layer": "execution",
         "engine": {"api_port": api_port, "l1_asleep": True, "l1_awake": False,
                    "l2_asleep": True, "l2_awake": False},
         "container": {"id": container, "running_through_l1": True},
         "gateway": {"awake": served.status, "asleep_l1": l1_refused_status,
                     "asleep_l2": l2_refused_status}},
        {"layer": "cleanup", "deployment_deleted": True, "container_absent": True,
         "deployment_id": dep_id, "container_id": container},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}


def create_fixtures(server, token, run_id, report, holder, agent_bin,
                    inject_model_failure=False, remember=lambda value: None):
    status, created = http(server.base, "POST", NODES, token=token,
                           body={"name": f"e2e-{run_id}-sleep-node",
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
        raise RuntimeError("injected model registration failure")
    status, model = http(server.base, "POST", MODELS + "/local", token=token, body={
        "node_uuid": node["uuid"], "path": str(MODEL_DIR),
        "source_id": f"e2e/{run_id}-sleep", "name": f"e2e-{run_id}-model"})
    check(status in (200, 201), f"model fixture HTTP {status}")
    report["owned_model_id"] = model["id"]
    status, key = http(server.base, "POST", APIKEYS, token=token,
                       body={"name": f"e2e-{run_id}-key"})
    check(status == 201, f"inference key fixture HTTP {status}")
    remember(key["key"])
    report["owned_key_name"] = key["name"]
    status, listed = http(server.base, "GET", APIKEYS, token=token)
    check(status == 200, f"listing inference keys failed: HTTP {status}")
    report["owned_key_prefix"] = next(
        (item["prefix"] for item in listed.get("items") or []
         if item.get("name") == key["name"]), None)
    dep_name = f"e2e-{run_id}-sleep"
    served_name = f"e2e-{run_id}-served"
    status, deployment = http(server.base, "POST", DEPLOYMENTS, token=token, body={
        "model_id": model["id"], "node_uuid": node["uuid"], "name": dep_name,
        "served_name": served_name, "engine": "vllm", "image": IMAGE,
        "placement_mode": "manual", "sleepable": True,
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
    parser.add_argument("--inject-node-delete-timeout", action="store_true",
                        help=argparse.SUPPRESS)
    args = parser.parse_args()
    INJECT["docker_error"] = args.inject_docker_error
    INJECT["node_delete_timeout"] = args.inject_node_delete_timeout
    os.umask(0o077)
    check(shutil.which("docker") is not None, "docker is required for this lane")
    check(subprocess.run(["docker", "image", "inspect", IMAGE],
                         capture_output=True).returncode == 0,
          f"{IMAGE} is missing; run make fake-engine-image")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/deployment-sleep-wake.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_deployment_sleep_wake.__name__,
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
        "execution": "real Docker container running the audited fake-engine image; the engine's /is_sleeping is read over the host network; GPU facts are injected only for scheduling",
        "runtime": "real engine sleep/wake and gateway requests; no real GPU memory release, dGPU isolation or vendor driver",
        "not_covered": ["real GPU memory release and dGPU quota",
                        "in-flight request wait/abort modes and cancellation",
                        "wake refusal when co-tenants hold the GPUs",
                        "partial wake and PD role sleep",
                        "L2 weight reload beyond the engine agreeing to wake",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/deployment_sleep_wake.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {"agent": None, "log": None}
    token = None
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
                test_deployment_sleep_wake(page, context, server, artifacts, report,
                                           args.locale, deployment, key["key"])
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
        residual = report.get("remaining_resources") or []
        if residual or not report.get("server_stopped") or not report.get("agent_stopped", True):
            listed = "; ".join(
                f"{item['kind']}:{item.get('id')} ({item['state']}; {item['reclaim']})"
                for item in residual) or "server or agent not confirmed stopped"
            report["private_state"] = (
                "private/ retains isolated DB/logs; residual resources: " + listed
                + "; use the scoped reclaim entries; remove only this run directory "
                  "after diagnosis (7 days)")
        else:
            report["private_state"] = (
                "private/ retains isolated DB/logs and agent state; no process kept; "
                "remove only this run directory after diagnosis (7 days)")
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
