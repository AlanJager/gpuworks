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
from urllib.parse import quote, urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)
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
def hold_local_register(page, ok_button, report, body):
    # setSubmitting runs immediately before the local register POST, so the
    # loading class is on while that request is held.
    held = []
    pattern = re.compile(re.escape(LOCAL_REGISTER) + r"(?:\?|$)")

    def hold_post(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != LOCAL_REGISTER:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    ok_button.click()
    report["last_step"] = "local-register-inflight"
    expect(ok_button).to_have_class(re.compile(r"\bant-btn-loading\b"))
    ok_button.click()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and not held:
        page.wait_for_timeout(100)
    check(len(held) == 1, "duplicate local registration while the first request is held")
    sent = held[0].request.post_data_json
    check(all(sent.get(key) == body[key] for key in body), "held local registration body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == LOCAL_REGISTER and r.request.method == "POST") as response:
        held[0].fulfill(status=502, content_type="application/json",
                        body=json.dumps({"message": "upstream fixture is unreachable"}))
    page.unroute(pattern, hold_post)
    check(response.value.status == 502, "local registration failure was not a 502")
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True,
        "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})


def test_models_local_register(page, context, server, artifacts, report, locale,
                               node, agent, agent_log, token):
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

    register_writes = []

    def request_seen(request):
        if request.method == "POST" and urlsplit(request.url).path in {PREVIEW, LOCAL_REGISTER}:
            register_writes.append(urlsplit(request.url).path)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/models")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
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
    heading = page.get_by_role("heading", name=models["page"]["title"], exact=True)
    register = action(page, models["action"]["register"])
    before_models = page.request.get(server.base + MODELS)
    check(before_models.status == 200, f"models GET before dismissals HTTP {before_models.status}")
    baseline_models = before_models.json()

    def open_register():
        register.click()
        opened = page.get_by_role("dialog", name=models["action"]["register"], exact=True)
        expect(opened).to_be_visible()
        return opened

    # Cancel, close, Escape and the mask each apply. The register button stays mounted.
    modal = assert_nested_dismissals(
        open_register(), heading, open_register,
        [("cancel", cancel_button(models["action"]["cancel"])),
         ("escape", escape), ("close", close_button), ("mask", mask)],
        lambda: len(register_writes), register)
    after_models = page.request.get(server.base + MODELS)
    check(after_models.status == 200 and after_models.json() == baseline_models,
          "register dialog dismissal changed stored models")
    check(register_writes == [], "register dialog dismissal sent a preview or registration write")
    report["evidence"].append({
        "layer": "ui", "register_dialog": models["action"]["register"],
        "dismissals": ["cancel", "escape", "close", "mask"],
        "focus_returned": True, "register_writes": 0})
    report["last_step"] = "register-dialog-dismissals"
    modal.get_by_label(models["column"]["source"], exact=True).click()
    local_option = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile(r"^" + re.escape(models["register"]["localPath"]) + r"$"))
    expect(local_option).to_be_visible()
    local_option.click()
    report["last_step"] = "local-node-show-search"
    # The combobox is the search input inside the select. Ancestor walk reaches
    # the root that carries ant-select-show-search. A has= filter built from the
    # dialog-scoped combobox looks for that dialog inside .ant-select.
    select_root = "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-select ')][1]"
    node_box = modal.get_by_role("combobox", name=models["detail"]["node"], exact=True)
    node_select = node_box.locator(select_root)
    expect(node_select).to_have_count(1, timeout=12000)
    expect(node_select).to_have_class(re.compile(r"\bant-select-show-search\b"), timeout=12000)
    expect(node_select.locator(".ant-select-selection-placeholder")).to_have_text(
        models["register"]["selectOnlineNode"], timeout=12000)
    expect(node_select.locator(".ant-select-selection-item")).to_have_count(0)
    expect(modal.locator("label.ant-form-item-required").filter(
        has_text=re.compile("^" + re.escape(models["detail"]["node"]) + "$"))).to_have_count(1, timeout=12000)
    # The source select is not searchable, so its selected label is the only
    # selection item in the dialog. The search input's ancestor does not expose it.
    selected = modal.locator(".ant-select-selection-item")
    expect(selected).to_have_count(1, timeout=12000)
    expect(selected).to_have_text(models["register"]["localPath"], timeout=12000)
    expect(modal.locator("label.ant-form-item-required").filter(
        has_text=re.compile("^" + re.escape(models["column"]["source"]) + "$"))).to_have_count(1, timeout=12000)
    directory_field = modal.get_by_label(models["register"]["modelDirectory"], exact=True)
    expect(directory_field).to_have_value("")
    expect(directory_field).to_have_attribute("placeholder", "/models/Qwen/Qwen3-8B", timeout=12000)
    check(directory_field.get_attribute("maxlength") is None, "model directory has a maximum length")
    expect(modal.locator("label.ant-form-item-required").filter(
        has_text=re.compile("^" + re.escape(models["register"]["modelDirectory"]) + "$"))).to_have_count(1, timeout=12000)
    expect(modal.get_by_text(models["register"]["modelDirectoryHelp"], exact=True)).to_be_visible()
    name_box = modal.get_by_label(models["column"]["name"], exact=True)
    expect(name_box).to_have_value("")
    expect(name_box).to_have_attribute("placeholder", models["register"]["displayNamePlaceholder"], timeout=12000)
    expect(modal.locator("label.ant-form-item-required").filter(
        has_text=re.compile("^" + re.escape(models["column"]["name"]) + "$"))).to_have_count(0)
    expect(modal.get_by_role("textbox")).to_have_count(2)
    expect(modal.get_by_role("combobox")).to_have_count(2)
    expect(modal.get_by_label(models["register"]["storage"], exact=True)).to_have_count(0)
    expect(modal.get_by_label(models["register"]["modelID"], exact=True)).to_have_count(0)
    expect(modal.get_by_label(models["register"]["directoryURL"], exact=True)).to_have_count(0)
    expect(modal.get_by_label(models["detail"]["revision"], exact=True)).to_have_count(0)
    expect(modal.get_by_role("spinbutton")).to_have_count(0)
    report["local_node_show_search"] = True
    node_box.click()
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
    report["last_step"] = "local-preview-alert"
    preview_alert = modal.locator(".ant-alert-success")
    expect(preview_alert).to_have_count(1, timeout=12000)
    expect(preview_alert).to_contain_text(models["register"]["localParsed"], timeout=12000)
    report["local_preview_alert"] = True
    report["last_step"] = "preview-read"
    expect(modal.get_by_label(models["column"]["name"], exact=True)).to_have_value(
        MODEL_DIR.name, timeout=12000)

    modal.get_by_label(models["register"]["modelID"], exact=True).fill(source_id)
    modal.get_by_label(models["column"]["name"], exact=True).fill(display_name)
    directory = modal.get_by_label(models["register"]["modelDirectory"], exact=True)
    # Register confirm is an antd Modal. Identity, 1440x900 stacking, and the focus trap.
    assert_dialog_shell(page, modal, directory, ok_button)
    report["evidence"].append({
        "layer": "ui", "dialog_shell": models["action"]["register"], "viewport": "1440x900"})
    hold_local_register(page, ok_button, report, {
        "node_uuid": node["uuid"], "path": str(MODEL_DIR),
        "source_id": source_id, "name": display_name})
    expect(modal).to_be_visible()
    expect(modal.get_by_label(models["register"]["modelID"], exact=True)).to_have_value(source_id)
    expect(modal.get_by_label(models["column"]["name"], exact=True)).to_have_value(display_name)
    expect(modal.get_by_label(models["register"]["modelDirectory"], exact=True)).to_have_value(str(MODEL_DIR))
    expect(page.locator(".ant-message")).to_contain_text(models["register"]["failed"].split("{{detail}}")[0].strip())
    expect(page.locator(".ant-message")).to_contain_text("upstream fixture is unreachable")
    listing = page.request.get(server.base + MODELS + "?source_id=" + quote(source_id, safe=""))
    check(listing.status == 200 and listing.json()["items"] == [], "failed local registration stored a model")
    check(bad_responses == [{"method": "POST", "path": LOCAL_REGISTER, "status": 502}],
          "unexpected browser HTTP errors during the held registration: " + str(bad_responses))
    check(console_errors and all("502" in text for text in console_errors),
          "unexpected console errors during the held registration: " + str(console_errors))
    bad_responses.clear()
    console_errors.clear()
    expect(ok_button).not_to_have_class(re.compile(r"\bant-btn-loading\b"))
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
    page.get_by_role("img", name=models["view"]["list"], exact=True).click()
    expect(page.get_by_role("radio", name=models["view"]["list"], exact=True)).to_be_checked()

    before_bad, before_console = len(bad_responses), len(console_errors)
    from model_hub_registration_browser import delete_ui
    delete_ui(page, server, models, {"name": display_name, "repo": source_id}, model["id"], token, report, locale)
    path = MODELS + f"/{model['id']}"
    refused = bad_responses[before_bad:]
    check(refused == [{"method": "DELETE", "path": path, "status": 409},
                      {"method": "DELETE", "path": path, "status": 409}],
          f"model delete refusals {refused}")
    echoed = console_errors[before_console:]
    check(echoed and all("409" in text for text in echoed), f"model delete console {echoed}")
    del bad_responses[before_bad:]
    del console_errors[before_console:]
    report["owned_model_id"] = None

    omit_id = f"e2e/{report['run_id']}-omit"
    report["last_step"] = "omit-name-register"
    register.click()
    omit_modal = page.get_by_role("dialog", name=models["action"]["register"], exact=True)
    expect(omit_modal).to_be_visible()
    omit_modal.get_by_label(models["column"]["source"], exact=True).click()
    omit_local = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile(r"^" + re.escape(models["register"]["localPath"]) + r"$"))
    expect(omit_local).to_be_visible()
    omit_local.click()
    omit_node = omit_modal.get_by_role("combobox", name=models["detail"]["node"], exact=True)
    omit_node.click()
    omit_option = page.locator(".ant-select-dropdown:visible .ant-select-item-option").filter(
        has_text=re.compile("^" + re.escape(node["name"]) + r"$"))
    expect(omit_option).to_be_visible()
    omit_option.click()
    omit_modal.get_by_label(models["register"]["modelDirectory"], exact=True).fill(str(MODEL_DIR))
    omit_ok = omit_modal.locator(".ant-modal-footer .ant-btn-primary")
    with page.expect_response(lambda r: urlsplit(r.url).path == PREVIEW
                              and r.request.method == "POST") as omit_preview:
        omit_ok.click()
    check(omit_preview.value.status == 200, f"omit preview HTTP {omit_preview.value.status}")
    expect(omit_modal.get_by_text(models["register"]["localParsed"], exact=True)).to_be_visible()
    expect(omit_modal.locator(".ant-alert-success")).to_have_count(1, timeout=12000)
    omit_name = omit_modal.get_by_label(models["column"]["name"], exact=True)
    expect(omit_name).to_have_value(MODEL_DIR.name, timeout=12000)
    omit_name.fill("")
    expect(omit_name).to_have_value("")
    omit_modal.get_by_label(models["register"]["modelID"], exact=True).fill(omit_id)
    expect(omit_name).to_have_value("")
    with page.expect_response(lambda r: urlsplit(r.url).path == LOCAL_REGISTER
                              and r.request.method == "POST") as omitted:
        omit_ok.click()
    check(omitted.value.status == 201, f"omit register HTTP {omitted.value.status}")
    omit_body = omitted.value.request.post_data_json
    check(omit_body["node_uuid"] == node["uuid"]
          and omit_body["path"] == str(MODEL_DIR)
          and omit_body["source_id"] == omit_id
          and omit_body["name"] == "",
          "omit register body differs")
    omit_model = omitted.value.json()
    report["owned_model_id"] = omit_model["id"]
    expect(page.get_by_text(models["register"]["registered"], exact=True).first).to_be_visible()
    expect(page.get_by_text(MODEL_DIR.name, exact=True).first).to_be_visible()
    omit_get = page.request.get(server.base + MODELS + f"/{omit_model['id']}")
    check(omit_get.status == 200, f"omit model GET HTTP {omit_get.status}")
    omit_saved = omit_get.json()
    check(omit_saved["source"] == "local"
          and omit_saved["source_id"] == omit_id
          and omit_saved["name"] == MODEL_DIR.name
          and omit_saved["name"] != omit_id
          and omit_saved["arch"] == "Qwen2ForCausalLM",
          "omitted display name was not stored as the directory basename")
    page.reload()
    expect(page.get_by_role("heading", name=models["page"]["title"], exact=True)).to_be_visible()
    expect(page.get_by_text(MODEL_DIR.name, exact=True).first).to_be_visible()
    omit_deleted = page.request.delete(server.base + MODELS + f"/{omit_model['id']}")
    check(omit_deleted.status == 204, f"omit model delete HTTP {omit_deleted.status}")
    check(page.request.get(server.base + MODELS + f"/{omit_model['id']}").status == 404,
          "the omitted-name model is still readable")
    report["owned_model_id"] = None
    report["omitted_name"] = {"source_id": omit_id, "stored_name": MODEL_DIR.name,
                              "request_name_empty": True}
    report["last_step"] = "omit-name-stored"

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
                             "name": saved["name"], "arch": saved["arch"],
                             "weight_bytes": round(saved["weight_gib"] * 1024 ** 3),
                             "node_presence": presence},
         "omitted_name": {"source_id": omit_id, "request_name": "",
                          "stored_name": omit_saved["name"]}},
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
                             body={"username": "admin", "password": "gpuworks-test-password-2026"})
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
                                           args.locale, node, agent, agent_log, admin_token)
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
