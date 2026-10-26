#!/usr/bin/env python3
"""Dashboard / fleet inventory and label grouping through the real browser.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-dashboard-fleet
After build: python3 test/e2e/dashboard_fleet.py --locale en
Requires Python Playwright and system Chrome. Three pending node definitions are
prepared through the product API so the dashboard read path is exercised with
deterministic fleet counts and labels. No agent, GPU or container is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and retains the
private database/logs for scoped diagnosis (seven days).
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/dashboard/fleet-inventory-label-grouping"
CASE = "概览 / 未上线节点库存与标签容量分组 / UI 计数、切换标签键、刷新与独立 GET 一致"
NODES = "/api/v1/nodes"
SUMMARY = "/api/v1/dashboard/summary"


def strings(locale):
    ui = json.loads((ROOT / "ui/src/i18n/locales" / locale / "dashboard.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return ui, auth


def format_text(template: str, **values) -> str:
    for key, value in values.items():
        template = template.replace("{{" + key + "}}", str(value))
    return template


# Contract: gpuworks/dashboard/fleet-inventory-label-grouping
# Mainline: 独立 server/SQLite + API 准备三个带标签的待接入节点 → 概览页显示总数/待接入数
#           → 默认按 team 分组显示 alpha 与 beta → 切换到 rack 后显示 rack=r1 与未设置
#           → 刷新与独立 GET 的计数/分组一致 → 强制删除自有节点
def test_dashboard_fleet(page, context, server, artifacts, report, locale):
    ui, auth = strings(locale)
    name_a = f"e2e-{report['run_id']}-dashboard-a"
    name_a2 = f"e2e-{report['run_id']}-dashboard-a2"
    name_b = f"e2e-{report['run_id']}-dashboard-b"
    page_errors, console_errors, bad_responses, summary_requests = [], [], [], []
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

    def request_seen(request):
        if urlsplit(request.url).path == SUMMARY and request.method == "GET":
            summary_requests.append(urlsplit(request.url).query)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/dashboard")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=ui["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"

    nodes = []
    # A2+B1: two alpha nodes share rack=r1, the single beta node has no rack.
    # The team grouping is 2+1; the rack grouping is 2 labeled + 1 unlabeled,
    # so the same three nodes distinguish both group sets.
    for name, labels in ((name_a, {"team": "alpha", "rack": "r1"}),
                         (name_a2, {"team": "alpha", "rack": "r1"}),
                         (name_b, {"team": "beta"})):
        created = page.request.post(server.base + NODES,
                                    data={"name": name, "server_url": server.base})
        check(created.status == 201, f"node fixture HTTP {created.status}")
        body = created.json()
        artifacts.remember(body.get("token", ""))
        node = body["node"]
        labeled = page.request.put(server.base + NODES + f"/{node['uuid']}/labels",
                                   data={"labels": labels})
        check(labeled.status == 200, f"label fixture HTTP {labeled.status}")
        nodes.append(node)

    page.reload()
    expect(page.get_by_role("heading", name=ui["title"], exact=True)).to_be_visible()

    def statistic(label):
        card = page.locator(".ant-card").filter(has_text=label).first
        return card.locator(".ant-statistic-content-value").first

    expect(statistic(ui["stats"]["nodesTotal"])).to_have_text("3")
    expect(statistic(ui["stats"]["nodesOnline"])).to_have_text("0")
    expect(statistic(ui["stats"]["gpusTotal"])).to_have_text("0")
    for label in (ui["stats"]["nodesTotal"], ui["stats"]["nodesOnline"], ui["stats"]["gpusTotal"]):
        expect(page.locator(".ant-card").filter(has_text=label).first.locator(".ant-statistic-content-suffix")).to_have_count(0)
    expect(page.get_by_role("spinbutton")).to_have_count(0)
    offline_text = format_text(ui["situation"]["offlineNodes"], count=0)
    offline_tag = page.locator(".ant-tag").filter(has_text=offline_text)
    expect(offline_tag).to_have_count(1)
    expect(offline_tag).to_have_class(re.compile(r"\bant-tag-green\b"))
    expect(page.get_by_text(ui["health"]["normal"], exact=True)).to_be_visible()

    def group_row(name):
        text = label_card.get_by_text(name, exact=True)
        expect(text).to_have_count(1)
        # The row's horizontal Space holds the name and the node-count Tag.
        return text.locator(
            "xpath=ancestor::div[contains(@class,'ant-space-horizontal')][1]")

    def node_count(count):
        return format_text(ui["capacity"]["nodeCount"], count=count)

    def group_evidence(groups):
        return [{"key": key, "unlabeled": unlabeled, "nodes": count}
                for (key, unlabeled), count in sorted(groups.items())]

    label_card = page.locator(".ant-card").filter(has_text=ui["capacity"]["byLabel"]).first
    groups = label_card.locator("strong")
    # Retrying assertion: the summary response arriving does not mean React
    # has committed the new group rows.
    expect(groups).to_have_text(["team=alpha", "team=beta"])
    expect(group_row("team=alpha").get_by_text(node_count(2), exact=True)).to_be_visible()
    expect(group_row("team=beta").get_by_text(node_count(1), exact=True)).to_be_visible()
    expect(label_card.locator(".ant-select-selection-item").first).to_have_text("team")
    label_key = label_card.get_by_role("combobox", name=ui["capacity"]["labelKeyAria"], exact=True)
    expect(label_key).to_have_count(1)
    check(label_key.get_attribute("aria-required") != "true", "the label key is marked required")
    report["last_step"] = "default-groups-verified"

    # The search input is covered by the selected item; open the control from
    # its selector surface.
    label_card.locator(".ant-select-selector").click()
    dropdown = page.locator(".ant-select-dropdown:visible .ant-select-item-option")
    expect(dropdown.filter(has_text=re.compile(r"^team$"))).to_be_visible()
    option = dropdown.filter(has_text=re.compile(r"^rack$"))
    expect(option).to_be_visible()
    with page.expect_response(lambda r: urlsplit(r.url).path == SUMMARY
                              and "label_key=rack" in r.url) as switched:
        option.click()
    check(switched.value.status == 200, f"rack summary HTTP {switched.value.status}")
    expect(groups).to_have_text([ui["capacity"]["unlabeled"], "rack=r1"])
    expect(group_row("rack=r1").get_by_text(node_count(2), exact=True)).to_be_visible()
    expect(group_row(ui["capacity"]["unlabeled"]).get_by_text(
        node_count(1), exact=True)).to_be_visible()
    expect(label_card.get_by_text("team=alpha", exact=True)).to_have_count(0)
    report["last_step"] = "rack-groups-verified"

    default_response = page.request.get(server.base + SUMMARY)
    check(default_response.status == 200, f"default summary HTTP {default_response.status}")
    default = default_response.json()
    check(default["nodes"]["total"] == 3 and default["nodes"]["offline"] == 0
          and default["nodes"]["connecting"] == 3,
          f"default node counts differ: {default['nodes']}")
    check(default["label_keys"] == ["team", "rack"],
          f"label keys differ: {default['label_keys']}")
    check(default["label_key"] == "team", f"default label key differs: {default['label_key']}")
    default_groups = {(group.get("key", ""), bool(group.get("unlabeled"))): group["nodes"]
                      for group in default["by_label"]}
    check(default_groups == {("team=alpha", False): 2, ("team=beta", False): 1},
          f"team groups differ: {default['by_label']}")
    check(sum(default_groups.values()) == default["nodes"]["total"],
          "default group counts do not add up to the fleet total")

    rack_response = page.request.get(server.base + SUMMARY + "?label_key=rack")
    check(rack_response.status == 200, f"rack summary HTTP {rack_response.status}")
    rack = rack_response.json()
    check(rack["label_key"] == "rack", f"rack label key differs: {rack['label_key']}")
    rack_groups = {(group.get("key", ""), bool(group.get("unlabeled"))): group["nodes"]
                   for group in rack["by_label"]}
    check(rack_groups == {("rack=r1", False): 2, ("", True): 1},
          f"rack groups differ: {rack['by_label']}")
    check(sum(rack_groups.values()) == rack["nodes"]["total"],
          "rack group counts do not add up to the fleet total")
    report["last_step"] = "rack-summary-verified"

    page.reload()
    expect(page.get_by_role("heading", name=ui["title"], exact=True)).to_be_visible()
    expect(statistic(ui["stats"]["nodesTotal"])).to_have_text("3")
    expect(groups).to_have_text(["team=alpha", "team=beta"])
    expect(group_row("team=alpha").get_by_text(node_count(2), exact=True)).to_be_visible()
    expect(group_row("team=beta").get_by_text(node_count(1), exact=True)).to_be_visible()
    expect(label_card.locator(".ant-select-selection-item").first).to_have_text("team")
    reloaded_response = page.request.get(server.base + SUMMARY)
    reloaded = reloaded_response.json()
    reloaded_groups = {(group.get("key", ""), bool(group.get("unlabeled"))): group["nodes"]
                       for group in reloaded["by_label"]}
    check(reloaded["label_key"] == "team" and reloaded_groups == default_groups,
          f"reloaded summary differs: {reloaded}")
    check(any("label_key=rack" in query for query in summary_requests),
          "the label-key switch did not request the rack summary")
    report["last_step"] = "reload-consistent"

    for node in nodes:
        deleted = page.request.delete(server.base + NODES + f"/{node['uuid']}?force=true")
        check(deleted.status == 200, f"node cleanup HTTP {deleted.status}")
    for node in nodes:
        check(page.request.get(server.base + NODES + f"/{node['uuid']}").status == 404,
              f"cleaned node {node['uuid']} is still readable")

    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "dashboard page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/dashboard", "nodes_total": 3, "nodes_online": 0,
         "gpus_total": 0, "statistic_suffixes": 0, "spinbuttons": 0, "offline_tag_green": True,
         "offline_tag": format_text(ui["situation"]["offlineNodes"], count=0),
         "label_key_required": False, "label_key_persisted": False,
         "health": ui["health"]["normal"],
         "default_groups": {"team=alpha": 2, "team=beta": 1},
         "switched_groups": {"rack=r1": 2, ui["capacity"]["unlabeled"]: 1}},
        {"layer": "request", "summary_queries": summary_requests,
         "rack_status": switched.value.status},
        {"layer": "persistence", "refresh_counts": default["nodes"],
         "default": {"label_key": default["label_key"],
                     "groups": group_evidence(default_groups)},
         "rack": {"label_key": rack["label_key"],
                  "groups": group_evidence(rack_groups)}},
        {"layer": "cleanup", "force_deleted": len(nodes), "after_delete": 404},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses, "summary_queries": summary_requests}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/dashboard-fleet.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_dashboard_fleet.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "N/A: the dashboard reads fleet records; the three nodes are pending (connecting) definitions",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["real GPU capacity and live metrics", "time-series charts and history",
                        "provider health and invalid clouds", "an empty fleet",
                        "a tie between label keys and an unknown requested key",
                        "group-row sorting", "loading, error and stale states",
                        "zh/en navigation and error text beyond the asserted labels",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/dashboard_fleet.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_dashboard_fleet(page, context, server, artifacts, report, args.locale)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                try:
                    context.close()
                finally:
                    browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        report["private_state"] = "private/ retains isolated DB/logs; no process kept; remove only this run directory after diagnosis (7 days)"
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
