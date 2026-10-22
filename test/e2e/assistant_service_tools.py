#!/usr/bin/env python3
"""AI assistant / service configuration, read-only tool loop and stop.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-assistant-service-tools
After build: python3 test/e2e/assistant_service_tools.py --locale en
Requires Python Playwright and system Chrome. The LLM endpoint is an auditable
local fixture; the server, AI service configuration, read-only tools, SQLite
and browser are real. No GPU is claimed.

The first question makes the fixture request ``cluster_overview``; the server
executes that read-only tool and sends its result back before the final answer.
A second question keeps the stream open so the browser Stop path is exercised.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and fixture and
retains the private database/logs for scoped diagnosis (seven days).
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
import time
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from assistant_fixture import TOOL_CALL_ID, AssistantEndpoint
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/assistant/service-tool-read-stream"
CASE = "AI 助手 / 服务配置与只读工具读取 / 流式回答、停止、刷新与删除"
CONFIGS = "/api/v1/ai/configs"
ASK = "/api/v1/ai/ask"
NODES = "/api/v1/nodes"


def strings(locale):
    settings = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    ai = json.loads((ROOT / "ui/src/i18n/locales" / locale / "ai.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return settings, ai, auth


def confirm_popconfirm(page):
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_be_visible(timeout=5000)
    popconfirm.locator(".ant-btn-primary").click()


def wait_fixture(fixture, wanted, timeout=15):
    """Wait until the fixture has recorded exactly the expected request kinds."""
    deadline = time.time() + timeout
    last = []
    while time.time() < deadline:
        snapshot = fixture.snapshot()
        last = [item["kind"] for item in snapshot["requests"]]
        if last == wanted:
            return snapshot
        time.sleep(0.05)
    raise AssertionError(f"fixture requests stayed {last}, want {wanted}")


def cluster_inventory_answer(tool_fact, inventory):
    """Compare tool facts with the independent inventory, which owns expected text."""
    total = tool_fact["summary"]["nodes"]["total"]
    names = [item["name"] for item in tool_fact["nodes_needing_attention"]["items"]]
    expected_names = sorted(item["name"] for item in inventory["items"])
    check(type(total) is int and total == inventory["total"]
          and sorted(names) == expected_names,
          f"tool result differs from the independent inventory: {tool_fact}")
    return (f"The cluster overview reports {inventory['total']} node(s); "
            f"attention includes {expected_names[0]}.")


# Contract: gpuworks/assistant/service-tool-read-stream
# Mainline: 独立 server/SQLite + 可审计 LLM fixture → 设置页新增并设为默认 AI 服务
#           → 助手面板提问 → fixture 请求 cluster_overview → server 执行只读工具并回传
#           → 流式最终回答与工具步骤可见 → 第二个问题在流式途中停止 → 刷新后配置仍在
#           → UI 删除配置并确认列表为空
def test_assistant_service_tools(page, context, server, artifacts, report, locale, fixture):
    settings, ai, auth = strings(locale)
    config_name = f"e2e-{report['run_id']}-assistant"
    page_errors, console_errors, bad_responses, asks = [], [], [], []
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
        path = urlsplit(request.url).path
        if path == ASK and request.method == "POST":
            asks.append(request.post_data_json)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/settings")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    integrations = page.get_by_role("tab", name=settings["page"]["tabs"]["integrations"], exact=True)
    integrations.click()
    expect(integrations).to_have_attribute("aria-selected", "true")
    report["last_step"] = "integrations-tab-selected"

    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    expect(card.get_by_text(settings["ai"]["empty"], exact=True)).to_be_visible()
    # Without an AI service the core product stays usable and the assistant
    # states that an AI service must be configured first. The ask itself is
    # expected to answer 503 AI_ENDPOINT_NOT_CONFIGURED.
    page.get_by_role("button", name=ai["title"], exact=True).click()
    panel = page.get_by_role("dialog").last
    question = panel.get_by_label(ai["panel"]["input"], exact=True)
    question.fill("Can you help with this cluster?")
    with page.expect_response(lambda r: urlsplit(r.url).path == ASK
                              and r.request.method == "POST") as no_config:
        panel.get_by_role("button", name=ai["panel"]["send"], exact=True).click()
    check(no_config.value.status == 503,
          f"the unconfigured ask returned {no_config.value.status}")
    expect(panel.get_by_text(ai["configureFirst"], exact=True)).to_be_visible()
    panel.get_by_role("button", name=ai["panel"]["close"], exact=True).click()
    expect(panel).not_to_be_visible()
    asks.clear()
    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    integrations = page.get_by_role("tab", name=settings["page"]["tabs"]["integrations"],
                                    exact=True)
    expect(integrations).to_have_attribute("aria-selected", "true")
    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    expect(card.get_by_text(settings["ai"]["empty"], exact=True)).to_be_visible()
    report["last_step"] = "no-config-assistant-state"

    button(card, settings["common"]["add"]).click()
    modal = page.get_by_role("dialog")
    expect(modal).to_be_visible()
    expect(modal.get_by_text(settings["ai"]["addService"], exact=True)).to_be_visible()
    modal.locator(".ant-select-selector").first.click()
    custom = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile(r"^" + re.escape(settings["ai"]["presetName"]["custom"]) + r"$"))
    expect(custom).to_be_visible()
    custom.click()
    modal.get_by_label(settings["ai"]["name"], exact=True).fill(config_name)
    modal.get_by_label(settings["ai"]["baseUrl"], exact=True).fill(fixture.base)
    modal.get_by_label(settings["ai"]["model"], exact=True).fill(fixture.model)
    upstream_key = modal.get_by_label(settings["ai"]["upstreamKey"], exact=True)
    expect(upstream_key).to_have_attribute("type", "password")
    upstream_key.fill(fixture.api_key)
    with page.expect_response(lambda r: urlsplit(r.url).path == CONFIGS
                              and r.request.method == "POST") as created:
        button(modal, settings["common"]["add"]).click()
    check(created.value.status == 201, f"AI config HTTP {created.value.status}")
    create_body = created.value.request.post_data_json
    check(create_body["name"] == config_name and create_body["base_url"] == fixture.base
          and create_body["model"] == fixture.model and create_body["is_default"] is True,
          f"AI config body differs: {create_body}")
    expect(page.get_by_text(settings["ai"]["added"], exact=True).first).to_be_visible()
    expect(card.get_by_text(config_name, exact=True)).to_be_visible()
    check(fixture.api_key not in page.locator("body").inner_text(),
          "the settings page rendered the upstream key after saving")

    node_name = f"e2e-{report['run_id']}-assistant-node"
    created_node = page.request.post(server.base + NODES,
                                     data={"name": node_name, "server_url": server.base})
    check(created_node.status == 201, f"assistant node fixture HTTP {created_node.status}")
    node_body = created_node.json()
    artifacts.remember(node_body["token"])
    node_uuid = node_body["node"]["uuid"]
    inventory_response = page.request.get(server.base + NODES)
    check(inventory_response.status == 200,
          f"assistant inventory HTTP {inventory_response.status}")
    inventory = inventory_response.json()
    inventory_names = {item["name"] for item in inventory["items"]}
    check(inventory["total"] == 1 and inventory_names == {node_name},
          f"assistant inventory differs: {inventory}")
    report["last_step"] = "inventory-prepared"

    page.get_by_role("button", name=ai["title"], exact=True).click()
    panel = page.get_by_role("dialog").last
    expect(panel.get_by_text(ai["title"], exact=True).first).to_be_visible()
    question = panel.get_by_label(ai["panel"]["input"], exact=True)
    send = panel.get_by_role("button", name=ai["panel"]["send"], exact=True)
    first_question_text = "How many nodes are in the cluster?"
    question.fill(first_question_text)
    send.click()
    expect(panel.get_by_text(ai["tool"]["cluster_overview"], exact=True)).to_be_visible(timeout=30000)

    first_question = wait_fixture(fixture, ["tool", "final"])
    tool_request, final_request = first_question["requests"]
    check(tool_request["authorization"] == f"Bearer {fixture.api_key}",
          "the assistant request lost the configured API key")
    check(any(tool["function"]["name"] == "cluster_overview"
              for tool in tool_request["body"].get("tools") or []),
          "cluster_overview was not offered to the model")
    check(len(final_request["tool_messages"]) == 1
          and final_request["tool_messages"][0]["name"] == "cluster_overview"
          and final_request["tool_messages"][0]["tool_call_id"] == TOOL_CALL_ID,
          "the server did not return the matching cluster_overview tool result")
    final_messages = final_request["body"].get("messages") or []
    preceding = [message for message in final_messages
                 if message.get("role") == "assistant" and message.get("tool_calls")]
    check(len(preceding) == 1
          and preceding[0]["tool_calls"][0]["id"] == TOOL_CALL_ID
          and preceding[0]["tool_calls"][0]["function"]["name"] == "cluster_overview",
          f"the final request omitted or altered the preceding tool call: "
          f"{final_messages}")
    report["last_step"] = "tool-loop-structure-verified"
    tool_content = final_request["tool_messages"][0]["content"]
    check(not tool_content.startswith("error:")
          and "unavailable" not in tool_content.lower(),
          f"cluster_overview returned an error: {tool_content[:300]}")
    tool_fact = json.loads(tool_content)
    tool_total = tool_fact["summary"]["nodes"]["total"]
    tool_names = [item["name"] for item in tool_fact["nodes_needing_attention"]["items"]]
    expected_answer = cluster_inventory_answer(tool_fact, inventory)
    expect(panel.get_by_text(expected_answer, exact=False)).to_be_visible(timeout=30000)
    report["last_step"] = "tool-result-verified"

    slow_question = "Please answer a slow question."
    question.fill(slow_question)
    # A new question is accepted only after the previous stream's done event;
    # the Send button is re-enabled at that point.
    expect(send).to_be_enabled(timeout=30000)
    send.click()
    check(fixture.wait_slow_started(10), "the slow question never reached the fixture")
    stop = button(panel, ai["stop"])
    expect(stop).to_be_visible()
    stop.click()
    expect(panel.get_by_text(ai["stopped"], exact=True)).to_be_visible()
    fixture.release_slow()
    check(fixture.wait_aborted(10),
          "the fixture did not observe the upstream cancellation")
    report["last_step"] = "stop-observed-upstream"

    denied_question = "Please read the denied route."
    denied_response = page.request.post(
        server.base + ASK,
        data={"task": "ask", "question": denied_question, "locale": locale},
        headers={"Accept": "text/event-stream"})
    check(denied_response.status == 200, f"denied ask HTTP {denied_response.status}")
    denied_text = denied_response.text()
    check("The denied route was rejected" in denied_text
          and "not readable" in denied_text,
          f"denied route was not rejected: {denied_text[:500]}")
    check("PRIVATE KEY" not in denied_text and "ssh-ed25519" not in denied_text,
          "the denied route leaked key material")
    denied_snapshot = fixture.snapshot()
    check([item["kind"] for item in denied_snapshot["requests"]] ==
          ["tool", "final", "slow", "denied", "denied_final"],
          f"fixture requests differ: {[item['kind'] for item in denied_snapshot['requests']]}")
    after_denied_nodes = page.request.get(server.base + NODES + f"/{node_uuid}")
    check(after_denied_nodes.json()["labels"] == {},
          "the assistant tool path changed node labels")
    report["last_step"] = "denied-route-rejected"

    panel.get_by_role("button", name=ai["panel"]["close"], exact=True).click()
    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    expect(page.get_by_role("tab", name=settings["page"]["tabs"]["integrations"], exact=True)
           ).to_have_attribute("aria-selected", "true")
    card = page.locator(".ant-card").filter(has_text=settings["ai"]["service"]).first
    expect(card.get_by_text(config_name, exact=True)).to_be_visible()
    configs = page.request.get(server.base + CONFIGS)
    check(configs.status == 200, f"AI config GET HTTP {configs.status}")
    rows = configs.json()["items"]
    check(len(rows) == 1 and rows[0]["name"] == config_name
          and rows[0]["is_default"] is True
          and rows[0]["preset"] == "custom"
          and rows[0]["base_url"] == fixture.base
          and rows[0]["model"] == fixture.model,
          f"AI config readback differs: {rows}")
    check("api_key" not in rows[0], "the AI config response exposed the upstream key")

    button(card, settings["common"]["delete"]).click()
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_contain_text(settings["ai"]["deleteConfirm"])
    confirm_popconfirm(page)
    expect(page.get_by_text(settings["ai"]["deleted"], exact=True).first).to_be_visible()
    expect(card.get_by_text(settings["ai"]["empty"], exact=True)).to_be_visible()
    empty = page.request.get(server.base + CONFIGS)
    check(empty.status == 200 and empty.json()["items"] == [],
          "the AI config was not deleted")
    deleted_node = page.request.delete(server.base + NODES + f"/{node_uuid}?force=true")
    check(deleted_node.status == 200, f"assistant node cleanup HTTP {deleted_node.status}")
    check(page.request.get(server.base + NODES + f"/{node_uuid}").status == 404,
          "the assistant node is still readable")
    report["last_step"] = "cleanup-complete"

    check(len(asks) == 2 and all(item.get("task") == "ask" for item in asks),
          f"UI ask requests differ: {asks}")
    check(asks[0].get("question") == first_question_text
          and asks[0].get("locale") == locale
          and "history" not in asks[0],
          f"first ask differs: {asks[0]}")
    check(asks[1].get("question") == slow_question
          and asks[1].get("locale") == locale
          and any(item.get("role") == "user"
                  and item.get("content") == first_question_text
                  for item in asks[1].get("history") or []),
          f"second ask differs: {asks[1]}")

    expected_no_config = {"method": "POST", "path": ASK, "status": 503}
    unexpected = [item for item in bad_responses if item != expected_no_config]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    allowed_console = [item for item in console_errors
                       if "503" in item and ("load resource" in item or "加载资源" in item)]
    check(len(allowed_console) <= 1 and len(allowed_console) == len(console_errors)
          and not page_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "settings page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/settings", "tab": settings["page"]["tabs"]["integrations"],
         "card": settings["ai"]["service"], "config": config_name,
         "assistant": ai["title"], "no_config_notice": True,
         "tool_step": ai["tool"]["cluster_overview"], "tool_answer": expected_answer,
         "denied_route": "rejected", "stop": ai["stopped"],
         "delete": settings["common"]["delete"],
         "delete_confirmation": settings["ai"]["deleteConfirm"]},
        {"layer": "request", "create": {"method": "POST", "path": CONFIGS,
         "status": 201, "body": {k: create_body[k] for k in
         ("name", "preset", "base_url", "model", "is_default")}},
         "ask_requests": [{"task": item.get("task"), "question": item.get("question"),
                           "locale": item.get("locale"), "has_history": bool(item.get("history"))}
                          for item in asks],
         "denied_ask": {"status": denied_response.status},
         "no_config_ask": {"status": no_config.value.status},
         "config_get": {"status": 200, "api_key_exposed": False}},
        {"layer": "persistence", "reload": "config row and default flag remain",
         "independent_get": {k: rows[0][k] for k in
                             ("name", "preset", "base_url", "model", "is_default")},
         "assistant_answer_streamed": True},
        {"layer": "execution", "fixture_requests": [item["kind"] for item in
         denied_snapshot["requests"]],
         "tool_call": "cluster_overview", "tool_call_id": TOOL_CALL_ID,
         "preceding_assistant_call": True,
         "tool_result_total": tool_total, "tool_result_names": tool_names,
         "upstream_cancelled": True, "denied_tool_result": "not readable",
         "final_answer": expected_answer},
        {"layer": "cleanup", "config_deleted": True, "node_deleted": True,
         "independent_get_items": 0, "fixture_stopped": True},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "console_allowed": allowed_console,
                                "http": bad_responses,
                                "no_config_ask": expected_no_config, "asks": asks}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/assistant-service-tools.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_assistant_service_tools.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite-llm-fixture", "identity": "admin",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/assistant_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "real server-side read-only tool loop against an audited local LLM fixture; no GPU claim",
        "runtime": "N/A: no agent, container, GPU or real model",
        "not_covered": ["real vendor model quality", "all read-only tools",
                        "multiple and parallel tool calls", "tool self-correction and retry",
                        "write tools (none are offered)", "terminal-route denial with a node UUID",
                        "MCP clients", "prompt/answer secret-redaction assertions",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/assistant_service_tools.py", *sys.argv[1:]]}
    fixture = None
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        fixture = AssistantEndpoint().start()
        artifacts.remember(fixture.api_key)
        report["fixture"] = {"base": fixture.base, "model": fixture.model}
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
                test_assistant_service_tools(page, context, server, artifacts,
                                             report, args.locale, fixture)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                report["failure_screenshot"] = (
                    "omitted: credentials or untrusted model output may be visible")
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
        try:
            if fixture is not None:
                fixture.stop()
            report["fixture_stopped"] = True
        except Exception:
            report["fixture_stopped"] = False
            report["fixture_stop_error"] = artifacts.redact(traceback.format_exc())
        if not report["server_stopped"] or not report["fixture_stopped"]:
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
