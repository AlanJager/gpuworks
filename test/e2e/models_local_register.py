#!/usr/bin/env python3
"""Models / local directory registration through a real local agent.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-models-local-register
After build: python3 test/e2e/models_local_register.py --locale en
Requires Python Playwright, system Chrome and the built agent binary. The node
is a real agent process on this host; it reads a tiny model directory and the
server registers that preview. No container, GPU or engine is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and agent and
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
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/model/local-directory-registration"
CASE = "模型 / 节点本地目录登记 / 真实 agent 读取预览、UI 登记、刷新回读与清理"
MODEL_DIR = ROOT / "test/e2e/testdata/fake-model"
NODES = "/api/v1/nodes"
MODELS = "/api/v1/models"
PREVIEW = "/api/v1/models/local/preview"
LOCAL_REGISTER = "/api/v1/models/local"


def strings(locale):
    models = json.loads((ROOT / "ui/src/i18n/locales" / locale / "models.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return models, auth


def action(scope, label):
    accessible = r"\s*".join(label) if re.fullmatch(r"[㐀-鿿]{2}", label) else re.escape(label)
    return scope.get_by_role("button", name=re.compile(accessible)).first


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


# Contract: gpuworks/model/local-directory-registration
# Mainline: 独立 server/SQLite + 真实本机 agent → UI 登记模型并选择本地路径
#           → 预览经 agent 读取 config/权重大小 → 确认模型 ID 并登记
#           → 刷新/独立 GET 回读 → 删除模型、停止 agent、强制删除节点
def test_models_local_register(page, context, server, artifacts, report, locale,
                               node, agent, agent_log):
    models, auth = strings(locale)
    source_id = f"e2e/{report['run_id']}-fake"
    display_name = f"e2e-{report['run_id']}-local"
    page_errors, console_errors, bad_responses = [], [], []
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

    page.goto(server.base + "/models")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=models["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "models-page-loaded"

    action(page, models["action"]["register"]).click()
    modal = page.get_by_role("dialog")
    expect(modal).to_be_visible()
    modal.get_by_label(models["column"]["source"], exact=True).click()
    local_option = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile(r"^" + re.escape(models["register"]["localPath"]) + r"$"))
    expect(local_option).to_be_visible()
    local_option.click()
    modal.get_by_role("combobox", name=models["detail"]["node"], exact=True).click()
    node_option = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile("^" + re.escape(node["name"]) + r"$"))
    expect(node_option).to_be_visible()
    node_option.click()
    modal.get_by_label(models["register"]["modelDirectory"], exact=True).fill(str(MODEL_DIR))

    # The modal's primary footer button is Parse preview before the preview and
    # Confirm registration after it; the role is stable across that swap.
    ok_button = modal.locator(".ant-modal-footer .ant-btn-primary")
    with page.expect_response(lambda r: urlsplit(r.url).path == PREVIEW
                              and r.request.method == "POST") as preview_response:
        ok_button.click()
    check(preview_response.value.status == 200,
          f"preview HTTP {preview_response.value.status}")
    preview_body = preview_response.value.request.post_data_json
    check(preview_body == {"node_uuid": node["uuid"], "path": str(MODEL_DIR)},
          f"preview body differs: {preview_body}")
    preview = preview_response.value.json()
    expected_bytes = sum(item.stat().st_size for item in MODEL_DIR.iterdir() if item.is_file())
    check(preview["arch"] == "Qwen2ForCausalLM" and preview["weight_bytes"] == expected_bytes,
          f"preview read differs: {preview}")
    expect(modal.get_by_text(models["register"]["localParsed"], exact=True)).to_be_visible()
    report["last_step"] = "preview-read"

    modal.get_by_label(models["register"]["modelID"], exact=True).fill(source_id)
    modal.get_by_label(models["column"]["name"], exact=True).fill(display_name)
    with page.expect_response(lambda r: urlsplit(r.url).path == LOCAL_REGISTER
                              and r.request.method == "POST") as registered:
        ok_button.click()
    check(registered.value.status == 201, f"register HTTP {registered.value.status}")
    register_body = registered.value.request.post_data_json
    check(register_body["node_uuid"] == node["uuid"]
          and register_body["path"] == str(MODEL_DIR)
          and register_body["source_id"] == source_id
          and register_body["name"] == display_name,
          f"register body differs: {register_body}")
    model = registered.value.json()
    report["owned_model_id"] = model["id"]
    expect(page.get_by_text(models["register"]["registered"], exact=True).first).to_be_visible()
    expect(page.get_by_text(display_name, exact=True).first).to_be_visible()
    report["last_step"] = "model-registered"

    independent = page.request.get(server.base + MODELS + f"/{model['id']}")
    check(independent.status == 200, f"model GET HTTP {independent.status}")
    saved = independent.json()
    check(saved["source"] == "local" and saved["source_id"] == source_id
          and saved["name"] == display_name and saved["arch"] == "Qwen2ForCausalLM"
          and abs(saved["weight_gib"] * 1024 ** 3 - expected_bytes) < 1,
          f"persisted model differs: {saved}")
    node_presence = saved.get("nodes") or []
    check(len(node_presence) == 1, f"the model has no node-file association: {saved.get('nodes')}")
    presence = node_presence[0]
    check(presence.get("node_uuid") == node["uuid"]
          and presence.get("node_name") == node["name"]
          and presence.get("path") == str(MODEL_DIR)
          and presence.get("state") == "registered"
          and presence.get("acquisition") == "registered"
          and presence.get("storage_id", 0) > 0
          and presence.get("storage_type") == "local"
          and presence.get("storage_name")
          and presence.get("size_bytes") == expected_bytes
          and presence.get("downloaded") == expected_bytes,
          f"model file association differs: {presence}")
    check(not saved.get("storages"),
          f"a node-only model reported shared storages: {saved.get('storages')}")
    report["last_step"] = "model-association-read"

    page.reload()
    tile = page.get_by_role("button").filter(has_text=display_name).first
    expect(tile).to_be_visible()
    tile.click()
    files = page.get_by_role("dialog").last
    expect(files.get_by_text(str(MODEL_DIR), exact=True)).to_be_visible()
    expect(files.get_by_text(node["name"], exact=True).first).to_be_visible()
    expect(files.get_by_text(models["presence"]["registered"], exact=True).first).to_be_visible()
    viewport = page.viewport_size
    box = files.bounding_box()
    check(box and box["x"] >= 0 and box["y"] >= 0
          and box["x"] + box["width"] <= viewport["width"] + 1
          and box["y"] + box["height"] <= viewport["height"] + 1,
          f"model files dialog is outside the viewport: {box}")
    files.locator(".ant-modal-close").click()
    expect(files).not_to_be_visible()
    report["last_step"] = "model-files-visible"

    deleted = page.request.delete(server.base + MODELS + f"/{model['id']}")
    check(deleted.status in (200, 204), f"model delete HTTP {deleted.status}")
    report["owned_model_id"] = None
    check(page.request.get(server.base + MODELS + f"/{model['id']}").status == 404,
          "the deleted model is still readable")

    agent.terminate()
    try:
        agent.wait(timeout=10)
    except subprocess.TimeoutExpired:
        agent.kill()
        agent.wait(timeout=5)
    agent_log.close()
    report["agent_stopped"] = agent.poll() is not None
    removed = page.request.delete(server.base + NODES + f"/{node['uuid']}?force=true")
    report["owned_node_uuid"] = None
    check(removed.status == 200, f"node cleanup HTTP {removed.status}")
    check(page.request.get(server.base + NODES + f"/{node['uuid']}").status == 404,
          "the cleaned node is still readable")
    report["last_step"] = "cleanup-complete"

    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/models", "modal": models["action"]["register"],
         "source": models["register"]["localPath"], "preview": models["register"]["localParsed"],
         "registered": models["register"]["registered"], "model": display_name,
         "files_node": node["name"], "files_path": str(MODEL_DIR),
         "files_state": models["presence"]["registered"]},
        {"layer": "request", "preview": {"path": PREVIEW, "status": 200, "body": preview_body},
         "register": {"path": LOCAL_REGISTER, "status": 201, "body": register_body}},
        {"layer": "persistence", "refresh": display_name,
         "independent_get": {"source": saved["source"], "source_id": saved["source_id"],
                             "arch": saved["arch"],
                             "weight_bytes": round(saved["weight_gib"] * 1024 ** 3),
                             "node_presence": presence}},
        {"layer": "execution", "agent_pid": agent.pid,
         "agent_read_path": preview["path"], "arch": preview["arch"],
         "weight_bytes": preview["weight_bytes"],
         "registered_path": presence["path"], "storage_id": presence["storage_id"]},
        {"layer": "cleanup", "model_deleted": True, "agent_stopped": report["agent_stopped"],
         "node_deleted": True},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}


def cleanup(server, admin_token, report, agent, agent_log):
    if report.get("owned_model_id"):
        status, _ = http(server.base, "DELETE", MODELS + f"/{report['owned_model_id']}",
                         token=admin_token)
        report["cleanup_model_status"] = status
        report["owned_model_id"] = None
    if agent is not None and agent.poll() is None:
        agent.terminate()
        try:
            agent.wait(timeout=10)
        except subprocess.TimeoutExpired:
            agent.kill()
            agent.wait(timeout=5)
        report["agent_stopped"] = agent.poll() is not None
    if agent_log is not None and not agent_log.closed:
        agent_log.close()
    if report.get("owned_node_uuid"):
        status, _ = http(server.base, "DELETE",
                         NODES + f"/{report['owned_node_uuid']}?force=true", token=admin_token)
        report["cleanup_node_status"] = status
        report["owned_node_uuid"] = None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/models-local-register.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_models_local_register.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-agent-local-files", "identity": "admin browser / node agent",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "a real local agent reads the model directory; no container, GPU or engine",
        "runtime": "N/A: the agent only reads files; no container, GPU or engine",
        "not_covered": ["downloads, copies and model file deletion on the node",
                        "GGUF-only, config-less and corrupt directories", "an offline node",
                        "node-picker search", "duplicate registration and identity conflicts",
                        "storage choice", "non-admin permissions",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/models_local_register.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    agent = None
    agent_log = None
    admin_token = ""
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "password"})
        check(status == 200, f"fixture login HTTP {status}")
        admin_token = login["token"]
        node_name = f"e2e-{run_id}-model-node"
        status, created = http(server.base, "POST", NODES, token=admin_token,
                               body={"name": node_name, "server_url": server.base})
        check(status == 201, f"node fixture HTTP {status}")
        node, node_token = created["node"], created["token"]
        artifacts.remember(node_token)
        report["owned_node_uuid"] = node["uuid"]
        state = artifacts.private / "agent-state"
        state.mkdir()
        agent_log = (artifacts.private / "agent.log").open("w")
        agent = subprocess.Popen([str(args.agent_bin), "run", "--server", server.base,
                                  "--token", node_token, "--state", str(state)],
                                 stdout=agent_log, stderr=subprocess.STDOUT, text=True)
        wait_online(server.base, admin_token, node["uuid"], agent)
        report["agent_pid"] = agent.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_models_local_register(page, context, server, artifacts, report,
                                           args.locale, node, agent, agent_log)
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
                context.close()
                browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        cleanup(server, admin_token, report, agent, agent_log)
        report["server_stopped"] = server.stop()
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        agent_log_path = artifacts.private / "agent.log"
        if agent_log_path.exists():
            (artifacts.directory / "agent.log").write_text(artifacts.redact(agent_log_path.read_text()))
        report["private_state"] = "private/ retains isolated DB/logs and agent state; no process kept; remove only this run directory after diagnosis (7 days)"
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
