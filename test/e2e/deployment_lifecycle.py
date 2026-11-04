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
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import traceback
from urllib.parse import parse_qs, urlsplit
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
                              node, deployment, api_key, token=""):
    deployments, auth = strings(locale)
    # 22.5 and 22.7 assert the shell's own translated wording, which lives in common.json.
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
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
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
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

    # 22.6 runs here, while the deployment still exists: its disabled controls are derived
    # from the running state (DeploymentsPage.tsx:243-244), so a deleted deployment would
    # leave nothing to read. The other 22 items run after the delete below.
    exercise_keyboard_and_style_22_6(page, server, report, dep_id)
    report["last_step"] = "keyboard-and-style-verified"

    # Section 10 runs while the deployment under test is still alive. The pagination batch is
    # created first because sorting and the populated reading need more rows than this case
    # owns, and it is torn down in its own finally so the delete below sees only the
    # deployment this case owns.
    report["token"] = token
    page_rows = []
    try:
        page_rows = _page_rows_fixture(server, token, node, deployment["model_id"], report)
        exercise_list_states_10_1(page, server, report, deployments, token)
        exercise_search_10_2(page, server, report, token)
        exercise_sort_10_4(page, server, report)
        exercise_page_size_10_5(page, server, report, token, len(page_rows))
        exercise_pagination_10_6(page, server, report, token)
        exercise_delete_10_7(page, server, report, token, page_rows)
        # Section 10 navigates away from the detail drawer the lifecycle steps below delete
        # from, so the drawer is reopened here in the state it was left in.
        page.goto(server.base + f"/deployments?deployment={dep_id}")
        _settled_path(page)
        expect(page.locator(".ant-drawer").first).to_be_visible(timeout=25000)
        page.wait_for_timeout(600)
    finally:
        # The rows are real deployments with real containers, so they are removed here even
        # when a section-10 assertion failed; cleanup() also covers this batch, and this is
        # the earlier teardown for the failure path.
        report["page_rows_cleanup_ok"] = _stop_page_rows(server, token, page_rows, report)
    report["last_step"] = "list-section-verified"

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

    # Checklists 22.1 and 22.2 run while the browser is still up and before the browser-error
    # ledger is read, so any error these navigations cause is counted rather than skipped.
    exercise_navigation_22_1(page, server, report)
    exercise_history_22_2(page, server, report, deployment)
    exercise_locale_22_5(page, server, report, locale)
    exercise_window_contract_22_7(page, server, report, common)
    exercise_single_role_22_3(page, server, report)
    exercise_polling_no_renewal_22_4(server, report, artifacts)
    report["last_step"] = "navigation-and-history-verified"

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
    # The pagination batch is reclaimed first: container_fixture.cleanup only knows the one
    # deployment in report["owned_deployment_id"], so a batch left to it would leak every
    # container it created. It tolerates an absent or partial id list on the failure path,
    # where the section never ran or stopped halfway.
    rows_ok = True
    try:
        rows_ok = _stop_page_rows(server, token, report.get("page_row_ids") or [], report)
    except Exception:
        rows_ok = False
        report["page_rows_cleanup_error"] = traceback.format_exc().splitlines()[-1]
    owned_ok = container_fixture.cleanup(
        server, token, report, holder, request=http, inspect=docker_running,
        inject_node_delete_timeout=INJECT.get("node_delete_timeout", False))
    return bool(rows_ok and owned_ok)


PAGE_ROWS = 11


def _page_rows_fixture(server, token, node, model_id, report):
    """Create enough real deployments for the list to make two pages at page size 10.

    The list pages on the server (ui/src/pagedList.ts sends page and page_size) and
    DEPLOYMENT_PAGE_SIZE is 20 (DeploymentsPage.tsx:53), so one page holds twenty rows.
    Eleven rows are enough rather than twenty-one because page size is switchable down to ten
    (columnPrefs.ts:27 PAGE_SIZE_OPTIONS): at ten the same rows make two pages, which gives
    both pagination edges, while sizes 10 and 20 return different counts and so prove the
    parameter reached the server. Each row is a real deployment with a real container, which
    is why the batch is kept at eleven.

    The ids start as a placeholder list here and are appended as each row is made. If the
    seventh creation fails, the caller's tuple assignment never happens, so its teardown would
    see nothing and the first six containers would leak; the counter therefore lives in this
    function so that the rollback below can still see them.
    """
    created = []
    try:
        for index in range(PAGE_ROWS):
            name = f"e2e-{report['run_id']}-row{index:02d}"
            status, deployment = http(server.base, "POST", DEPLOYMENTS, token=token, body={
                "model_id": model_id, "node_uuid": node["uuid"], "name": name,
                "served_name": f"e2e-{report['run_id']}-served{index:02d}",
                "engine": "vllm", "image": IMAGE, "placement_mode": "manual",
                "container_overrides": {"command_mode": "engine"}})
            check(status in (200, 201),
                  f"pagination row {index} HTTP {status} {str(deployment)[:160]}")
            created.append(deployment["id"])
        report["page_row_ids"] = list(created)
        return created
    except BaseException:
        # Undo what was made before the failure, then let it through: swallowing it would
        # report a broken fixture as a pass.
        _stop_page_rows(server, token, created, report)
        raise


def _stop_page_rows(server, token, ids, report):
    """Delete the pagination rows and their containers.

    Every argument may be the placeholder the caller starts with: a fixture that fails while
    being built has still usually created some rows, and passing None or an empty list is what
    makes this safe on that path. An earlier fixture in this repo leaked because its teardown
    was never told about what had been made.
    """
    removed, failed = [], []
    for deployment_id in ids or ():
        status, _ = http(server.base, "DELETE", DEPLOYMENTS + f"/{deployment_id}", token=token)
        (removed if status in (200, 202, 204, 404) else failed).append(
            {"id": deployment_id, "status": status})
    report["page_rows_removed"] = removed
    if failed:
        report.setdefault("remaining_resources", []).extend(
            [{"kind": "deployment", "id": item["id"], "state": f"delete HTTP {item['status']}",
              "reclaim": f"DELETE /api/v1/deployments/{item['id']}"} for item in failed])
    return not failed


def _settled_path(page, tries=12):
    """The path the address has come to rest on.

    Following a deep link can land on a redirect, so the first read of the address is not
    yet the final one. This waits for two consecutive equal reads before believing it; a
    single read would record wherever the redirect happened to be passing through.
    """
    current = urlsplit(page.url).path
    for _ in range(tries):
        page.wait_for_timeout(120)
        now = urlsplit(page.url).path
        if now == current:
            return current
        current = now
    return current


def _where(page):
    """Read the address, the highlighted menu entry and the page title together.

    22.2's whole point is that these three can disagree, so they are always read as one
    triple rather than one at a time. The sider is named explicitly because `.app-nav`
    appears on both the sider and the drawer (App.tsx:119, :181).
    """
    chosen = page.locator(".app-sider .app-nav .ant-menu-item-selected")
    title = page.locator(".page-header-title-text")
    return {
        "path": urlsplit(page.url).path,
        "selected": chosen.first.inner_text().strip() if chosen.count() else None,
        "selected_count": chosen.count(),
        "title": title.first.inner_text().strip() if title.count() else None,
    }


def exercise_navigation_22_1(page, server, report):
    """22.1: each real entry and deep link, with title, selection and post-reload position.

    The sidebar is enumerated from the DOM the page actually renders rather than from the
    route table, so an entry that exists in code but not on screen cannot be counted. For
    every entry this enters it by deep link, asserts the address it settles on, that
    exactly one menu entry highlights and it is the expected one, and that the page renders
    a title; then it reloads and asserts the same address and highlight are still there.
    """
    nav = page.locator(".app-sider .app-nav")
    expect(nav).to_be_visible()
    entries = []
    items = nav.locator(".ant-menu-item")
    for index in range(items.count()):
        item = items.nth(index)
        key = item.get_attribute("data-menu-id") or ""
        # data-menu-id is "rc-menu-uuid-<id>-<path>" and the generated id itself contains
        # hyphens, so the menu key is the segment that starts with a slash at the end.
        path = "-" + key.rpartition("-/")[2] if "-/" in key else ""
        path = path.replace("-", "/", 1)
        entries.append({"label": item.inner_text().strip(), "key_path": path})
    check(len(entries) >= 15, f"the sidebar rendered only {len(entries)} entries: {entries}")
    deploy_entry = next((e for e in entries if e["key_path"].startswith("/deployments")), None)
    check(deploy_entry is not None, f"the sidebar has no deployments entry: {entries}")

    visited = []
    for entry in entries:
        page.goto(server.base + entry["key_path"])
        # The title block is rendered by the page component, not the shell, so a settled
        # address is not the same moment as a rendered title. Waiting for it is the real
        # readiness signal; each page also polls, so "networkidle" would never settle.
        try:
            page.locator(".page-header-title-text").first.wait_for(state="visible", timeout=25000)
        except Exception:
            pass
        # Read the address only after the page has rendered: a page may replace the address
        # it was given (/analytics settles on /analytics/usage, AnalyticsPage.tsx:98), and
        # the pre-render address is the one it is leaving, not the one it stays on.
        landed = _settled_path(page)
        page.wait_for_timeout(300)
        where = _where(page)
        check(where["selected_count"] == 1,
              f"{entry['key_path']}: {where['selected_count']} entries are highlighted at once")
        check(where["selected"] == entry["label"],
              f"{entry['key_path']}: the highlighted entry is {where['selected']!r}, "
              f"not {entry['label']!r}")
        check(bool(where["title"]), f"{entry['key_path']}: the page renders no title")
        page.reload()
        _settled_path(page)
        page.wait_for_timeout(400)
        again = _where(page)
        check(again["path"] == landed,
              f"{entry['key_path']}: a reload moved the address to {again['path']!r}")
        check(again["selected"] == entry["label"],
              f"{entry['key_path']}: a reload highlighted {again['selected']!r} "
              f"instead of {entry['label']!r}")
        check(again["title"] == where["title"],
              f"{entry['key_path']}: a reload changed the title from {where['title']!r}")
        visited.append({"entry": entry["label"], "requested": entry["key_path"],
                        "landed": landed, "title": where["title"],
                        "selected": where["selected"], "reload_kept": True})

    report["navigation"] = {"entries_rendered": len(entries),
                            "deployments_entry": deploy_entry, "visited": visited}
    report["evidence"].append({"layer": "ui", "item": "22.1", "what":
        "every sidebar entry was entered by deep link; for each, exactly one menu entry "
        "highlighted and it was the expected one, the page rendered a title, and a reload kept "
        "both the address and the highlighted entry", "entries": len(entries)})
    return report["navigation"]


def exercise_history_22_2(page, server, report, deployment):
    """22.2: back/forward and the ?deployment= parameter keep state, and the rendered page
    never disagrees with the entry the navigation says is selected.

    The deployments list keeps its open detail in the URL (`?deployment=<id>`,
    DeploymentsPage.tsx:130), so the address and a rendered drawer are two independent
    witnesses of the same state. Every move asserts the address, the highlighted entry and
    the page title together, because 22.2 exists for the case where they differ.
    """
    start = "/deployments"
    page.goto(server.base + start)
    _settled_path(page)
    page.wait_for_timeout(600)
    first = _where(page)
    check(first["selected"] == report["navigation"]["deployments_entry"]["label"],
          f"the deployments page highlighted {first['selected']!r}")
    check(bool(first["title"]), "the deployments page rendered no title")

    # The second entry is chosen by its menu key, not by its label: the label is translated
    # and this case runs in both languages, so matching English text would fail in zh.
    other = page.locator(".app-sider .app-nav .ant-menu-item[data-menu-id$='-/deploy']").first
    if other.count() == 0:
        other = page.locator(".app-sider .app-nav .ant-menu-item").nth(0)
    other.click()
    moved = _settled_path(page)
    page.wait_for_timeout(500)
    second = _where(page)
    check(second["path"] != first["path"],
          f"clicking a menu entry did not change the address: {second['path']!r}")

    page.go_back()
    _settled_path(page)
    page.wait_for_timeout(600)
    back = _where(page)
    check(back["path"] == first["path"],
          f"going back landed on {back['path']!r} instead of {first['path']!r}")
    check(back["selected"] == first["selected"],
          f"going back restored the address but highlighted {back['selected']!r} "
          f"instead of {first['selected']!r}")
    check(back["title"] == first["title"],
          f"going back restored the address but the title is {back['title']!r} "
          f"instead of {first['title']!r}")

    page.go_forward()
    _settled_path(page)
    page.wait_for_timeout(600)
    forward = _where(page)
    check(forward["path"] == second["path"],
          f"going forward landed on {forward['path']!r} instead of {second['path']!r}")
    check(forward["selected"] == second["selected"],
          f"going forward restored the address but highlighted {forward['selected']!r} "
          f"instead of {second['selected']!r}")
    check(forward["title"] == second["title"],
          f"going forward restored the address but the title is {forward['title']!r} "
          f"instead of {second['title']!r}")

    dep_id = deployment["id"]
    deep = f"{start}?deployment={dep_id}"
    page.goto(server.base + deep)
    _settled_path(page)
    page.wait_for_timeout(1200)
    drawer = page.locator(".ant-drawer").first
    expect(drawer).to_be_visible(timeout=20000)
    check(urlsplit(page.url).query == f"deployment={dep_id}",
          f"the detail parameter did not survive: {urlsplit(page.url).query!r}")
    opened = _where(page)
    check(opened["selected"] == first["selected"],
          f"opening a detail changed the highlighted entry to {opened['selected']!r}")
    drawer_title = drawer.locator(".ant-drawer-title").inner_text().strip()
    check(bool(drawer_title), "the detail drawer rendered no title")

    page.reload()
    _settled_path(page)
    page.wait_for_timeout(1500)
    reopened = page.locator(".ant-drawer").first
    expect(reopened).to_be_visible(timeout=20000)
    check(urlsplit(page.url).query == f"deployment={dep_id}",
          f"a reload dropped the detail parameter: {urlsplit(page.url).query!r}")
    after_reload = _where(page)
    check(after_reload["selected"] == first["selected"],
          f"a reload highlighted {after_reload['selected']!r} instead of {first['selected']!r}")
    check(after_reload["title"] == first["title"],
          f"a reload changed the title to {after_reload['title']!r}")
    check(reopened.locator(".ant-drawer-title").inner_text().strip() == drawer_title,
          "a reload reopened a different detail than the address named")

    page.keyboard.press("Escape")
    page.wait_for_timeout(800)
    closed = _where(page)
    check(urlsplit(page.url).query == "",
          f"closing the detail left the parameter behind: {urlsplit(page.url).query!r}")
    check(closed["selected"] == first["selected"],
          f"closing the detail highlighted {closed['selected']!r}")

    report["history"] = {
        "layer": "ui", "page": start,
        "first": first, "second": {"path": moved, **second},
        "back_restored": {"path": back["path"], "selected": back["selected"],
                          "title": back["title"]},
        "forward_restored": {"path": forward["path"], "selected": forward["selected"],
                             "title": forward["title"]},
        "detail": {"query": f"deployment={dep_id}", "drawer_title": drawer_title,
                   "survived_reload": True, "closed_clears_query": True},
        "three_agree": True}
    report["evidence"].append({"layer": "ui", "item": "22.2", "what":
        "back and forward each restored the address, the highlighted menu entry and the page "
        "title together; the ?deployment= detail was addressable, survived a reload with the "
        "same drawer and the same highlight, and closing it cleared the parameter while the page "
        "and the menu still agreed", **report["history"]})
    return report["history"]


def exercise_locale_22_5(page, server, report, locale):
    """22.5: in both languages the navigation, the first screen and the live text are
    translated, and no internal field name stands in for a translation.

    The two vocabularies are read from the shipped locale files, so the expectation is the
    product's own wording rather than a copy of it. What is asserted on screen is that the
    rendered navigation, the page title and the table's own column headers are the
    translated strings and that the other language's string is absent.
    """
    vocabulary = {}
    for language in ("en", "zh"):
        vocabulary[language] = {
            "common": json.loads((ROOT / f"ui/src/i18n/locales/{language}/common.json").read_text()),
            "deployments": json.loads((ROOT / f"ui/src/i18n/locales/{language}/deployments.json").read_text()),
        }
    other = "zh" if locale == "en" else "en"
    mine, theirs = vocabulary[locale], vocabulary[other]

    page.goto(server.base + "/deployments")
    _settled_path(page)
    try:
        page.locator(".page-header-title-text").first.wait_for(state="visible", timeout=25000)
    except Exception:
        pass
    page.wait_for_timeout(400)

    nav = page.locator(".app-sider .app-nav")
    expect(nav).to_be_visible()
    rendered_entries = [nav.locator(".ant-menu-item").nth(i).inner_text().strip()
                        for i in range(nav.locator(".ant-menu-item").count())]
    expected_deploy = mine["common"]["navigation"]["deployments"]
    expected_deploy_other = theirs["common"]["navigation"]["deployments"]
    check(expected_deploy in rendered_entries,
          f"the navigation does not render {expected_deploy!r}: {rendered_entries[:6]}")
    if expected_deploy_other != expected_deploy:
        check(expected_deploy_other not in rendered_entries,
              f"the navigation still renders the other language's {expected_deploy_other!r}")

    title = page.locator(".page-header-title-text").first.inner_text().strip()
    check(title == mine["deployments"]["page"]["title"],
          f"the page title is {title!r}, not the translated {mine['deployments']['page']['title']!r}")
    if theirs["deployments"]["page"]["title"] != mine["deployments"]["page"]["title"]:
        check(title != theirs["deployments"]["page"]["title"],
              f"the page title is the other language's wording: {title!r}")

    # Column headers are the live text of the list, not a field name: each header the
    # product declares has to appear as translated text.
    headers = [page.locator(".ant-table-thead th").nth(i).inner_text().strip()
               for i in range(page.locator(".ant-table-thead th").count())]
    check(len(headers) > 0, "the deployments table rendered no column headers")
    # An internal field name is snake_case or camelCase with no spaces; the translated
    # headers are words. This is the check the item asks for, made on real text.
    internal = [h for h in headers if h and ("_" in h or (h[:1].islower() and any(c.isupper() for c in h)))]
    check(not internal, f"a column header shows an internal field name instead of a translation: {internal}")

    report["locale_22_5"] = {"layer": "ui", "locale": locale,
        "nav_entry": expected_deploy, "nav_entries": len(rendered_entries),
        "title": title, "headers": headers, "no_internal_field_names": True,
        "other_language_absent": True}
    report["evidence"].append({"layer": "ui", "item": "22.5", "what":
        f"in {locale} the navigation entry, the page title and every column header were the "
        "translated strings, the other language's wording was absent, and no header showed an "
        "internal field name", **report["locale_22_5"]})
    return report["locale_22_5"]


def exercise_keyboard_and_style_22_6(page, server, report, deployment_id):
    """22.6: real keyboard operation reaches a visible focus ring, and the disabled state
    and colours are read from computed style rather than assumed.

    The ring is the browser's own :focus-visible outline (styles.css:16 gives it a 2px
    solid outline), observed by pressing Tab and reading computed style on whatever holds
    focus - not by calling focus(), which would not prove keyboard reachability. The
    disabled control is one the product disables by state (DeploymentsPage.tsx:243-244
    disable the playground and cURL actions unless the deployment runs), read through its
    computed style and its own disabled attribute.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    page.locator("body").click()

    rings = []
    for _ in range(14):
        page.keyboard.press("Tab")
        reading = page.evaluate(
            "() => { const a = document.activeElement; if (!a) return null;"
            " const s = getComputedStyle(a);"
            " return {tag: a.tagName, width: s.outlineWidth, style: s.outlineStyle,"
            " color: s.color, text: (a.innerText || a.getAttribute('aria-label') || '').trim().slice(0, 24)}; }")
        if reading:
            rings.append(reading)
    check(len(rings) >= 8, f"tabbing reached only {len(rings)} elements")
    focused = [r for r in rings if r["style"] != "none" and r["width"] not in ("0px", "")]
    check(focused, f"no element reached by Tab showed a focus ring: {rings[:6]}")
    # The ring has to be a real painted outline, not a zero-width placeholder.
    widths = {r["width"] for r in focused}
    check(any(float(w.replace("px", "")) >= 1 for w in widths if w.endswith("px")),
          f"the focus ring has no measurable width: {widths}")

    disabled = page.locator("button[disabled], button.ant-btn-disabled, [aria-disabled='true']")
    disabled_count = disabled.count()
    disabled_evidence = []
    for index in range(min(disabled_count, 5)):
        node = disabled.nth(index)
        reading = node.evaluate(
            "e => { const s = getComputedStyle(e); return {disabled: e.disabled === true,"
            " disabledAttr: e.getAttribute('disabled'), aria: e.getAttribute('aria-disabled'),"
            " color: s.color, cursor: s.cursor, pointerEvents: s.pointerEvents}; }")
        disabled_evidence.append(reading)
        check(reading["disabled"] or reading["aria"] == "true",
              f"a control styled as disabled is not actually disabled: {reading}")
    check(disabled_count > 0, "the page rendered no disabled control to compare")

    # Colours come from the theme, so the same reading must be reproducible and not a
    # one-off literal: the selected entry's colour is compared to itself after a reload.
    selected = page.locator(".app-sider .app-nav .ant-menu-item-selected").first
    colour_before = selected.evaluate("e => getComputedStyle(e).color")
    page.reload()
    _settled_path(page)
    page.wait_for_timeout(900)
    colour_after = page.locator(".app-sider .app-nav .ant-menu-item-selected").first.evaluate(
        "e => getComputedStyle(e).color")
    check(colour_before == colour_after,
          f"the selected entry's colour is not stable across a reload: "
          f"{colour_before!r} -> {colour_after!r}")

    report["keyboard_22_6"] = {"layer": "ui", "tabbed": len(rings),
        "with_focus_ring": len(focused), "ring_widths": sorted(widths),
        "sample_ring": focused[0],
        "disabled_controls": disabled_count, "disabled_evidence": disabled_evidence,
        "selection_colour": colour_before, "colour_stable_after_reload": True}
    report["evidence"].append({"layer": "ui", "item": "22.6", "what":
        "Tab reached elements that painted a measurable :focus-visible outline, every control "
        "rendered as disabled was really disabled by attribute or aria, and the selected entry's "
        "computed colour was unchanged by a reload", **report["keyboard_22_6"]})

    # This check navigates away to read the list's controls, so it puts the caller's detail
    # drawer back the way it found it; the lifecycle steps after it delete from that drawer.
    page.goto(server.base + f"/deployments?deployment={deployment_id}")
    _settled_path(page)
    expect(page.locator(".ant-drawer").first).to_be_visible(timeout=25000)
    page.wait_for_timeout(500)
    return report["keyboard_22_6"]
    return report["keyboard_22_6"]


def exercise_window_contract_22_7(page, server, report, common):
    """22.7: a bad deep link is not hidden behind the SPA fallback's 200, and the not-found
    page recovers to a real page.

    The fallback is the interesting half: the server answers an unknown path with the index
    document and 200 (a 200 with an HTML content type, observed here directly), so the
    status alone cannot tell a real page from a missing one. What separates them is what the
    app renders - NotFoundPage prints its own title, its description and the exact address
    it was given, and it must not leave a menu entry highlighted as if the page existed.
    """
    missing = "/this-route-does-not-exist-" + report["run_id"].lower()
    response = page.request.get(server.base + missing)
    check(response.status == 200,
          f"the SPA fallback did not answer the unknown path with 200: {response.status}")
    content_type = response.headers.get("content-type") or ""
    check("text/html" in content_type,
          f"the fallback did not answer with the index document: {content_type!r}")

    page.goto(server.base + missing)
    _settled_path(page)
    try:
        page.get_by_text(common["notFound"]["title"], exact=True).wait_for(
            state="visible", timeout=25000)
    except Exception:
        pass
    page.wait_for_timeout(400)
    not_found = common["notFound"]
    expect(page.get_by_text(not_found["title"], exact=True)).to_be_visible()
    expect(page.get_by_text(not_found["description"], exact=True)).to_be_visible()
    # The page names the address it could not serve, so a 200 cannot mask a wrong link.
    expect(page.get_by_text(missing, exact=True)).to_be_visible()
    highlighted = page.locator(".app-sider .app-nav .ant-menu-item-selected")
    check(highlighted.count() == 0,
          "an unknown address still highlighted a menu entry, which would claim the page exists")

    # The recovery action reaches a real page, so the dead end is recoverable.
    button(page, not_found["deployments"]).click()
    recovered = _settled_path(page)
    check(recovered.startswith("/deployments"),
          f"the not-found page's own action did not reach the deployments page: {recovered!r}")
    expect(page.locator(".page-header-title-text").first).to_be_visible(timeout=25000)

    report["window_22_7"] = {"layer": "ui", "unknown_path": missing,
        "status": response.status, "content_type": content_type,
        "rendered_not_found": not_found["title"], "names_address": True,
        "no_menu_entry_highlighted": True, "recovery_path": recovered}
    report["evidence"].append({"layer": "ui", "item": "22.7", "what":
        "an unknown address got the SPA fallback's 200 with the index document, and the app "
        "rendered its own not-found title, description and the exact address, highlighted no "
        "menu entry, and its own action recovered to a real deployments page",
        **report["window_22_7"]})
    return report["window_22_7"]


def exercise_single_role_22_3(page, server, report):
    """22.3: the menu, the actions and a direct API call all agree with the one role this
    build can create; the administrator-only branch exists but this contract cannot reach it.

    Three findings, kept apart on purpose:

    1. The build is single-role. User.Role has exactly one write point and it hardcodes
       "admin" (internal/server/store/auth.go:31); no product path creates any other role.
       An admin-vs-user comparison therefore cannot be built from two real identities here.
    2. The administrator-only branch is real: requireManagementAdmin answers 403
       ErrorAdministratorRequired when the session's role is not admin
       (internal/server/api/auth.go:240). It is reachable only by signed claims carrying a
       non-admin role, which needs the master key - not something a product user can hold.
    3. Forging a token to poke that branch would be neither a real user path nor honest
       evidence, so it is recorded as existing and deliberately not claimed.

    GPUWorks does not use the Router organization/IAM model: there is no organization scope,
    no role-assignment UI and no per-resource IAM policy to enumerate.
    """
    identity = page.request.get(server.base + "/api/v1/auth/me")
    check(identity.status == 200, f"the identity endpoint answered {identity.status}")
    me = identity.json()
    role = me.get("role") or (me.get("user") or {}).get("role")
    check(role == "admin", f"the signed-in session's role is {role!r}, not admin")

    nav = page.locator(".app-sider .app-nav")
    expect(nav).to_be_visible()
    entries = [nav.locator(".ant-menu-item").nth(i).inner_text().strip()
               for i in range(nav.locator(".ant-menu-item").count())]
    check(len(entries) >= 15, f"this role sees only {len(entries)} menu entries: {entries}")

    # A direct API call outside the page's own requests, carrying only the session cookie.
    direct = page.request.get(server.base + DEPLOYMENTS)
    check(direct.status == 200,
          f"a direct list call with this role answered {direct.status}, expected 200")
    body = direct.json()
    check("items" in body and "total" in body,
          f"the direct call did not return a deployment list: {list(body)[:6]}")

    report["single_role_22_3"] = {"layer": "request", "role": role,
        "role_write_point": "internal/server/store/auth.go:31 hardcodes Role admin",
        "other_role_constructible": False,
        "admin_only_branch": "internal/server/api/auth.go:240 answers 403 "
                             "ErrorAdministratorRequired for a non-admin session",
        "admin_branch_claimed": False,
        "admin_branch_why": "reachable only by signed claims with a non-admin role; a forged "
                            "token would be neither a real user path nor honest evidence",
        "router_iam_model": False,
        "menu_entries": len(entries), "direct_api_status": direct.status,
        "direct_api_total": body.get("total")}
    report["evidence"].append({"layer": "request", "item": "22.3", "what":
        "the session carries the only role this build can create (admin; single write point "
        "store/auth.go:31); all menu entries and a direct list call are consistent with it; the "
        "administrator-only 403 branch exists at api/auth.go:240 but needs signed non-admin "
        "claims and is recorded without being claimed", **report["single_role_22_3"]})
    return report["single_role_22_3"]


def raw_status(base, method, path, token=""):
    """The status of one request, with no session cookie inherited from the page."""
    status, _ = http(base, method, path, token=token)
    return status


def scoped_login(base, user="admin", password="password"):
    """Log in on an isolated server and return (status, session token)."""
    status, body = http(base, "POST", "/api/v1/auth/login",
                        body={"username": user, "password": password})
    return status, (body or {}).get("token", "")


def exercise_polling_no_renewal_22_4(server, report, artifacts, idle="3s"):
    """22.4: a background poll is not human activity, so it cannot keep a session alive.

    A management session is renewed only on a non-GET request (internal/server/api/auth.go:
    213-219 - the renew call sits inside a branch that skips GET/HEAD/OPTIONS). The pages
    poll with GET, so the traffic that keeps the screen fresh must not extend the session.

    The cookie cannot show this: the product sets it to the session's absolute deadline on
    both login and renewal (auth.go:76 and :219 both pass session.AbsoluteExpiresAt), so its
    expiry says nothing about idle renewal. An earlier attempt compared that value and was
    stopped by its own positive control - it read sub-second float jitter as a renewal.

    What does show it is the product's own idle deadline, so this runs a second isolated
    server on the product's session_idle_timeout key and polls it with GET past that
    deadline. The session has to expire anyway; if polling counted as activity it would not.
    """
    from local_browser_fixture import LocalServer

    root = artifacts.private / "polling"
    root.mkdir(parents=True, exist_ok=True)
    scoped = LocalServer(server.binary, root, session_idle_timeout=idle)
    seconds = float(idle.rstrip("s"))
    evidence = {"layer": "request", "own_server": True, "session_idle_timeout": idle}
    try:
        evidence["health"] = scoped.start()
        logged = scoped_login(scoped.base)
        check(logged[0] == 200, f"the scoped server refused the login: {logged[0]}")
        credential = logged[1]
        artifacts.remember(credential)

        alive = raw_status(scoped.base, "GET", "/api/v1/auth/me", credential)
        check(alive == 200, f"the fresh credential was refused: {alive}")

        # Poll with GET the way the pages do, well past the idle deadline. Each poll is a
        # real request; if a passive poll renewed the session, it would still answer 200.
        started = time.time()
        polls = []
        while time.time() - started < seconds * 2.2:
            polls.append({"at": round(time.time() - started, 2),
                          "status": raw_status(scoped.base, "GET", "/api/v1/auth/me", credential)})
            time.sleep(0.4)
        elapsed = time.time() - started
        finally_status = polls[-1]["status"]
        check(elapsed > seconds * 1.5,
              f"the polling window ({elapsed:.2f}s) did not outlast the {idle} deadline")
        check(finally_status == 401,
              f"polling with GET kept the session alive past its {idle} idle deadline: the last "
              f"poll answered {finally_status} after {elapsed:.2f}s; polls={polls}")

        # The transition is the evidence: it was 200 while inside the deadline and 401 after.
        seen_200 = [p for p in polls if p["status"] == 200]
        seen_401 = [p for p in polls if p["status"] == 401]
        check(seen_200 and seen_401,
              f"the session did not transition from alive to expired while polling: {polls}")
        first_401 = seen_401[0]
        check(first_401["at"] >= seconds * 0.5,
              f"the session expired at {first_401['at']}s, before the {idle} deadline, which is "
              "not the behaviour this item asserts")

        evidence.update({"polls": len(polls), "window_seconds": round(elapsed, 2),
                         "first_401_at": first_401["at"],
                         "alive_polls": len(seen_200), "expired_polls": len(seen_401),
                         "last_poll_status": finally_status,
                         "renew_rule": "internal/server/api/auth.go:213-219 renews only on "
                                       "non-GET/HEAD/OPTIONS"})
        report["polling_22_4"] = evidence
        report["evidence"].append({"layer": "request", "item": "22.4", "what":
            f"a second isolated server ran the product's own session_idle_timeout={idle} and was "
            f"polled with GET past that deadline; the first {len(seen_200)} polls answered 200 and "
            f"the session then answered 401 for the remaining {len(seen_401)} polls, so passive "
            "polling did not keep it alive", **evidence})
    finally:
        evidence["own_server_stopped"] = scoped.stop()
    return evidence



def _rows(page):
    """How many body rows the table renders."""
    return page.locator(".ant-table-tbody tr.ant-table-row").count()


def _row_names(page):
    """The deployment name of every rendered row.

    The first data cell is the deployment's own name; the second is the served name
    (DeploymentsPage.tsx columns start name, served_name, model, ...). The two differ on every
    fixture row here, so reading the wrong one compares nothing: the names taken from the page
    then share no value with the names the API reports.
    """
    cells = page.locator(".ant-table-tbody tr.ant-table-row td:nth-child(1)")
    return [cells.nth(index).inner_text().strip() for index in range(cells.count())]


def _list_total(server, token, report):
    """The total the server reports for the unpaged list, and the rows behind it."""
    status, document = http(server.base, "GET",
                            DEPLOYMENTS + "?page=1&page_size=100", token=token)
    check(status == 200, f"the independent list read answered {status}")
    return document.get("total"), document.get("items", [])


def _choose_page_size(page, size):
    """Pick a page size in the table's own size changer.

    antd formats the option label itself, so its wording follows the locale ("20 / page" in
    English, "20 条/页" in Chinese). The option is therefore anchored on the leading number,
    which is the page size itself, and required to carry no other digits before it.
    """
    page.locator(".ant-pagination-options .ant-select").first.click()
    page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile(rf"^\s*{size}(?!\d)")).first.click()
    page.wait_for_timeout(1200)


def exercise_list_states_10_1(page, server, report, deployments, token):
    """10.1: the list is observed populated and empty, and the two meanings of empty differ.

    Both states are produced by the server, not by a stubbed front end: the populated reading
    is the fixture rows counted against the total the product itself reports, and the empty
    reading is reached through the product's own search with a term that matches nothing. An
    empty table that showed the same text as "nothing exists yet" would make the two
    indistinguishable to a user, which is what this item asks about. A failed request and the
    loading state are not constructed by this case and are not claimed.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.locator(".page-header-title-text").first.wait_for(state="visible", timeout=25000)
    page.wait_for_timeout(800)

    populated = _rows(page)
    check(populated > 0, "the populated list rendered no rows")
    total, items = _list_total(server, token, report)
    check(total == len(items), f"the list total {total} does not match the items it returned")
    header = page.locator(".page-header").inner_text()
    check(str(total) in header,
          f"the header does not state the total {total}: {header[:140]!r}")
    report["list_total"] = total

    search = page.locator(".ant-input-search input").first
    check(search.count() > 0, "the list rendered no search box")
    search.fill("zzz-no-deployment-matches-this-" + report["run_id"])
    search.press("Enter")
    page.wait_for_timeout(1500)
    check(_rows(page) == 0, "an unmatched search still rendered rows")
    empty_text = page.locator(".ant-table-placeholder, .ant-empty").first.inner_text().strip()
    check(empty_text, "the empty result rendered no empty-state text")
    check(empty_text != deployments["page"]["empty"],
          "an unmatched search showed the 'nothing exists yet' text, so filtered and "
          "unfiltered emptiness are indistinguishable")

    report["list_state_10_1"] = {"layer": "ui", "populated_rows": populated,
        "server_total": total, "empty_rows": 0, "empty_text": empty_text,
        "filtered_empty_distinct_from_unfiltered": True}
    report["evidence"].append({"layer": "ui", "item": "10.1", "what":
        "the list was observed populated (fixture rows counted against the total the product "
        "reports in its own header) and empty (a search matching nothing, rendered through the "
        "product's filtered-empty wording, which differs from the never-created text); a "
        "failed request and the loading state are not claimed",
        **report["list_state_10_1"]})
    return report["list_state_10_1"]


def exercise_search_10_2(page, server, report, token):
    """10.2: complete, partial, absent, wildcard and cleared searches.

    The list searches on the server (DeploymentsPage.tsx:99 puts q into the request), so a
    client-side filter over rows already fetched would look identical on screen while doing
    something else. Each search therefore captures the request it caused and compares the
    term that reached the server with the rows that came back.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    baseline = _row_names(page)
    check(baseline, "no rows to search within")
    target = baseline[0]
    fragment = target[len(target) // 2:][:6] or target[-4:]
    search = page.locator(".ant-input-search input").first
    check(search.count() > 0, "the list rendered no search box")

    def searched(value):
        with page.expect_response(
                lambda r: urlsplit(r.url).path == DEPLOYMENTS
                and r.request.method == "GET") as caught:
            search.fill(value)
            search.press("Enter")
        query = parse_qs(urlsplit(caught.value.url).query)
        page.wait_for_timeout(1200)
        return query, _row_names(page)

    complete_query, complete_rows = searched(target)
    check(complete_rows == [target] or target in complete_rows,
          f"a complete-name search did not return {target!r}: {complete_rows}")
    check(complete_query.get("q", [""])[0] == target,
          f"the search term did not reach the server: {complete_query.get('q')}")

    partial_query, partial_rows = searched(fragment)
    check(any(fragment in name for name in partial_rows),
          f"a partial-name search did not return a match for {fragment!r}: {partial_rows}")
    check(partial_query.get("q", [""])[0] == fragment,
          f"the partial term did not reach the server: {partial_query.get('q')}")

    absent_query, absent_rows = searched("zzz-absent-" + report["run_id"])
    check(absent_rows == [], f"a term that matches nothing returned rows: {absent_rows[:3]}")

    # A wildcard character must be treated as text: a search for "%" returning everything
    # would mean the term reached the query unescaped.
    wildcard_query, wildcard_rows = searched("%")
    check(len(wildcard_rows) < len(baseline),
          f"a bare '%' returned every row ({len(wildcard_rows)} of {len(baseline)}), so it "
          "acted as a wildcard instead of a literal character")

    search.fill("")
    search.press("Enter")
    page.wait_for_timeout(1500)
    restored = _row_names(page)
    check(len(restored) == len(baseline),
          f"clearing the search restored {len(restored)} rows, not the original {len(baseline)}")

    report["search_10_2"] = {"layer": "request", "baseline_rows": len(baseline),
        "complete": {"term": target, "q": complete_query.get("q"), "rows": len(complete_rows)},
        "partial": {"term": fragment, "q": partial_query.get("q"), "rows": len(partial_rows)},
        "absent": {"q": absent_query.get("q"), "rows": len(absent_rows)},
        "wildcard_percent": {"q": wildcard_query.get("q"), "rows": len(wildcard_rows)},
        "cleared_rows": len(restored)}
    report["evidence"].append({"layer": "request", "item": "10.2", "what":
        "a complete name, a partial fragment, an absent term, a literal '%' and a cleared box "
        "were each searched; the captured request carried the typed term as q every time, the "
        "returned rows matched only for the first two, '%' did not act as a wildcard, and "
        "clearing restored the original row count", **report["search_10_2"]})
    return report["search_10_2"]


def exercise_sort_10_4(page, server, report):
    """10.4: a sortable column reverses under the header and the server is told to sort.

    The columns that sort do it on the server (DeploymentsPage.tsx:99 sends sort, "-key" for
    descending), so the rendered order alone cannot tell a server sort from a stale first
    page. The request is captured and the rendered order is compared across two clicks.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    baseline = _row_names(page)
    check(len(baseline) >= 2, "fewer than two rows, so a sort cannot be observed")

    header = page.locator(".ant-table-thead th").filter(
        has_text=re.compile(r"Created|创建")).first
    check(header.count() > 0, "the created-at column header was not found")

    def once():
        with page.expect_response(
                lambda r: urlsplit(r.url).path == DEPLOYMENTS
                and r.request.method == "GET") as caught:
            header.click()
        query = parse_qs(urlsplit(caught.value.url).query)
        page.wait_for_timeout(900)
        return query, _row_names(page)

    first_query, first = once()
    second_query, second = once()
    check(first != second,
          f"clicking the sortable header did not change the rendered order: {first[:3]}")
    check(sorted(first) == sorted(second),
          "the two orders are not the same set of rows")
    sent = (first_query.get("sort", [""])[0], second_query.get("sort", [""])[0])
    check(any(sent), f"the sort never reached the server: {sent}")
    check(sent[0] != sent[1], f"both clicks sent the same sort parameter: {sent}")

    report["sort_10_4"] = {"layer": "request", "column": "created_at",
        "sort_params": list(sent), "first_order_head": first[:3],
        "second_order_head": second[:3], "order_changed": True, "same_row_set": True}
    report["evidence"].append({"layer": "request", "item": "10.4", "what":
        "the created-at header was clicked twice; the two clicks sent different sort "
        "parameters to the server, the rendered order changed between them, and both orders "
        "contained the same set of rows", **report["sort_10_4"]})
    return report["sort_10_4"]


def exercise_page_size_10_5(page, server, report, token, batch_rows):
    """10.5: every page size the table offers is switched and really applied.

    The options are the product's own (columnPrefs.ts:27 PAGE_SIZE_OPTIONS = [10, 20, 50,
    100]) and the list is server-paged, so each choice has to appear in the request and in the
    number of rows that come back. The list holds this case's own deployment as well as the
    batch, so the real total is read from the server rather than assumed to be the batch size:
    with twelve rows, size 10 renders ten while 20/50/100 render twelve, which is what proves
    the choice took effect instead of falling back to a default.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    total, _ = _list_total(server, token, report)
    check(total >= batch_rows,
          f"the server lists {total} rows, fewer than the {batch_rows} batch rows")

    observed = []
    for size in (10, 20, 50, 100):
        with page.expect_response(
                lambda r: urlsplit(r.url).path == DEPLOYMENTS
                and r.request.method == "GET") as caught:
            _choose_page_size(page, size)
        query = parse_qs(urlsplit(caught.value.url).query)
        page.wait_for_timeout(800)
        observed.append({"requested": size, "page_size_param": query.get("page_size"),
                         "page_param": query.get("page"), "rendered": _rows(page)})

    check(all(item["page_size_param"] == [str(item["requested"])] for item in observed),
          f"a page size did not reach the server: {observed}")
    small = [i for i in observed if i["requested"] == 10][0]
    large = [i for i in observed if i["requested"] == 20][0]
    check(small["rendered"] != large["rendered"],
          f"page size 10 and 20 rendered the same number of rows ({small['rendered']}), so "
          f"the choice did not take effect with {total} rows available")
    check(large["rendered"] == min(large["requested"], total),
          f"page size {large['requested']} rendered {large['rendered']} rows, not "
          f"{min(large['requested'], total)} of {total}")

    report["page_size_10_5"] = {"layer": "request", "rows_available": total,
        "batch_rows": batch_rows, "observed": observed, "options": [10, 20, 50, 100],
        "source": "ui/src/components/resourceTable/columnPrefs.ts:27"}
    report["evidence"].append({"layer": "request", "item": "10.5", "what":
        "all four page sizes the table offers were selected in turn; each choice appeared as "
        f"page_size in the request, and with {total} rows size 10 rendered ten while 20/50/100 "
        "rendered all of them, so the switch really applies instead of falling back",
        **report["page_size_10_5"]})
    return report["page_size_10_5"]


def exercise_pagination_10_6(page, server, report, token):
    """10.6: first, previous, next and last page, the disabled edges, every row once.

    At page size ten the fixture rows make two pages, which gives both edges: previous is
    disabled on the first page and next on the last. The rows are collected page by page and
    the collection has to be the whole set with no repeat and no gap. Counting rows per page
    would not show this - two pages of ten and two could still repeat one row and omit another
    - so the comparison is between the name sets, checked against the unpaged list the server
    reports, with both page collections recorded for audit.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    total, _ = _list_total(server, token, report)
    expected_pages = math.ceil(total / 10)

    _choose_page_size(page, 10)
    first_page = _row_names(page)
    check(len(first_page) == min(10, total),
          f"the first page at size 10 rendered {len(first_page)} rows, not "
          f"{min(10, total)} of {total}")

    def page_state():
        items = page.locator(".ant-pagination-item")

        def disabled(selector):
            # The control's own class is "ant-pagination-prev"/"-next", so testing for that
            # substring would always be true and the check would never fail. The disabled
            # marker is the separate class token ant-pagination-disabled.
            tokens = (page.locator(selector).get_attribute("class") or "").split()
            return "ant-pagination-disabled" in tokens

        return {
            "pages": [items.nth(i).inner_text().strip() for i in range(items.count())],
            "active": page.locator(".ant-pagination-item-active").inner_text().strip(),
            "prev_disabled": disabled(".ant-pagination-prev"),
            "next_disabled": disabled(".ant-pagination-next"),
        }

    at_first = page_state()
    check(at_first["prev_disabled"],
          f"the previous control was usable on the first page: {at_first}")
    check(at_first["active"] == "1", f"the first page is not marked active: {at_first}")
    check(len(at_first["pages"]) == expected_pages,
          f"{total} rows at page size 10 did not make {expected_pages} pages: "
          f"{at_first['pages']}")

    page.locator(".ant-pagination-next").first.click()
    page.wait_for_timeout(1200)
    second_page = _row_names(page)
    at_last = page_state()
    check(at_last["active"] == str(expected_pages),
          f"next did not move to the last page {expected_pages}: {at_last}")
    check(at_last["next_disabled"],
          f"the next control was usable on the last page: {at_last}")
    check(not at_last["prev_disabled"],
          f"the previous control was unusable on the last page: {at_last}")

    collected = first_page + second_page
    repeats = [name for name in set(collected) if collected.count(name) > 1]
    check(not repeats, f"a row appeared on both pages: {repeats[:3]}")

    _, items = _list_total(server, token, report)
    independent = {item["name"] for item in items}
    missing = independent - set(collected)
    extra = set(collected) - independent
    check(not missing,
          f"{len(missing)} rows the server lists were never shown by any page: "
          f"{sorted(missing)[:3]}")
    check(not extra,
          f"a page showed rows the server does not list: {sorted(extra)[:3]}")
    check(len(collected) == total,
          f"the pages together rendered {len(collected)} rows, not the {total} listed")

    page.locator(".ant-pagination-prev").first.click()
    page.wait_for_timeout(1200)
    back = _row_names(page)
    check(back == first_page,
          "returning to the first page did not restore the same rows in the same order")

    report["pagination_10_6"] = {"layer": "ui", "page_size": 10, "rows_total": total,
        "expected_pages": expected_pages,
        "page1_names": first_page, "page2_names": second_page,
        "collected_unique": len(set(collected)), "repeats": repeats,
        "missing_vs_independent": sorted(missing), "extra_vs_independent": sorted(extra),
        "no_repeat_no_gap": not repeats and not missing and not extra,
        "first_page_prev_disabled": at_first["prev_disabled"],
        "last_page_next_disabled": at_last["next_disabled"],
        "independent_total": total, "back_restored_same_rows": back == first_page}
    report["evidence"].append({"layer": "ui", "item": "10.6", "what":
        f"at page size 10 the {total} rows made {expected_pages} pages; next and previous were "
        "both used, previous was disabled on the first page and next on the last; the names "
        "collected page by page had no repeat and no gap against the unpaged list the server "
        "reports; and going back restored the first page in the same order",
        **report["pagination_10_6"]})
    return report["pagination_10_6"]


def exercise_delete_10_7(page, server, report, token, row_ids):
    """10.7: deleting the last row of the last page leaves the total, the page and the rows right.

    This reuses the pagination batch, so it costs no extra container. Only a row this section
    created may be deleted: the last page can also hold the deployment this case owns, and
    deleting that one would pull the fixture out from under the steps that follow.
    """
    page.goto(server.base + "/deployments")
    _settled_path(page)
    page.wait_for_timeout(900)
    _choose_page_size(page, 10)

    total_before, items_before = _list_total(server, token, report)
    batch = {item["name"]: item["id"] for item in items_before
             if item["id"] in set(row_ids)}
    check(len(batch) == len(row_ids),
          f"only {len(batch)} of the {len(row_ids)} batch rows are in the list")

    page.locator(".ant-pagination-item").last.click()
    page.wait_for_timeout(1200)
    last_page_rows = _row_names(page)
    check(0 < len(last_page_rows) <= 10,
          f"the last page at size 10 held {len(last_page_rows)} rows")
    on_last_page = [name for name in last_page_rows if name in batch]
    check(on_last_page,
          f"the last page holds none of the batch rows, so this section has nothing it may "
          f"delete there: {last_page_rows}")
    victim_name = on_last_page[-1]
    victim_id = batch[victim_name]

    status, _ = http(server.base, "DELETE", DEPLOYMENTS + f"/{victim_id}", token=token)
    check(status in (200, 202, 204), f"deleting the last row answered {status}")
    row_ids.remove(victim_id)

    page.wait_for_timeout(1500)
    page.reload()
    _settled_path(page)
    page.wait_for_timeout(2000)

    active = page.locator(".ant-pagination-item-active")
    active_page = active.inner_text().strip() if active.count() else None
    after_rows = _row_names(page)
    check(victim_name not in after_rows,
          f"the deleted row {victim_name!r} is still rendered after a reload")

    total_after, items_after = _list_total(server, token, report)
    check(total_after == total_before - 1,
          f"the total went from {total_before} to {total_after}, not down by one")
    names_after = {item["name"] for item in items_after}
    check(victim_name not in names_after,
          f"the deleted row {victim_name!r} is still in the server's list")
    # Whatever page the product leaves the user on, it must not be an empty page while rows
    # still exist - that would be a dead end the user has to escape by hand.
    if names_after:
        check(after_rows,
              f"after deleting the last row of the last page the view shows no rows at all, "
              f"though {total_after} rows still exist; the active page is {active_page!r}")

    report["delete_10_7"] = {"layer": "ui", "deleted": {"id": victim_id, "name": victim_name},
        "total_before": total_before, "total_after": total_after,
        "active_page_after": active_page, "rows_after_on_page": len(after_rows),
        "deleted_row_gone_from_page": victim_name not in after_rows,
        "deleted_row_gone_from_server": victim_name not in names_after,
        "page_count_after": page.locator(".ant-pagination-item").count()}
    report["evidence"].append({"layer": "ui", "item": "10.7", "what":
        "the last batch row on the last page was deleted; the total fell from "
        f"{total_before} to {total_after}, the row disappeared from both the page and the "
        "server's list, and the view did not leave the user on a page with no rows while rows "
        "still existed", **report["delete_10_7"]})
    return report["delete_10_7"]


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
                             body={"username": "admin", "password": "gpuworks-test-password-2026"})
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
                                          args.locale, node, deployment, key["key"],
                                          token=token)
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
