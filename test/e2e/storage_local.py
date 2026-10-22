#!/usr/bin/env python3
"""Storage / local storage form, conditional fields and lifecycle.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-storage-local
After build: python3 test/e2e/storage_local.py --locale en
Requires Python Playwright and system Chrome. A local storage record is a
control-plane object; this case does not claim an agent trial mount, capacity
or model-file access. No GPU or container is required.
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

CONTRACT = "gpuworks/storage/local-create-delete"
CASE = "存储 / 本地存储表单与生命周期 / 类型条件字段、字段负例、创建、刷新与删除"
STORAGES = "/api/v1/storages"


def strings(locale):
    storage = json.loads((ROOT / "ui/src/i18n/locales" / locale / "storage.json").read_text())
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return storage, common, auth


def format_text(template: str, **values) -> str:
    for key, value in values.items():
        template = template.replace("{{" + key + "}}", str(value))
    return template


def action(scope, label):
    # AntD icons contribute an accessible-name prefix (for example
    # "plus Add storage"), so match the label inside the accessible name.
    accessible = r"\s*".join(label) if re.fullmatch(r"[㐀-鿿]{2}", label) else re.escape(label)
    return scope.get_by_role("button", name=re.compile(accessible)).first


def confirm_popconfirm(page):
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_be_visible(timeout=5000)
    popconfirm.locator(".ant-btn-primary").click()


# Contract: gpuworks/storage/local-create-delete
# Mainline: 独立 server/SQLite → 存储页新增本地存储 → NFS 类型显示 Export/Mount point
#           且未试挂时不可保存 → 非法名称与相对路径都不发请求 → 合法本地存储创建
#           → 刷新/独立 GET 回读 → UI 删除后列表消失且 GET 404
def test_storage_local(page, context, server, artifacts, report, locale):
    storage, common, auth = strings(locale)
    name = f"e2e-{report['run_id'].lower()}-{uuid.uuid4().hex[:6]}"
    path = str(server.storage_root / f"e2e-{report['run_id'].lower()}")
    outside = f"/tmp/e2e-{report['run_id'].lower()}-outside-root"
    page_errors, console_errors, bad_responses, posts = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))

    refusal_console = []

    def console_seen(message):
        if message.type == "error":
            source = urlsplit(message.location.get("url", "")).path
            if not authenticated and source == "/api/v1/auth/me" and "401" in message.text:
                return
            # Chrome's resource-error line for the asserted out-of-root refusal.
            if source == STORAGES and "status of 400" in message.text:
                refusal_console.append(message.text)
                return
            console_errors.append(message.text)

    def response_seen(response):
        pathname = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and pathname == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"method": response.request.method, "path": pathname,
                                  "status": response.status})

    def request_seen(request):
        if urlsplit(request.url).path == STORAGES and request.method == "POST":
            posts.append(request.post_data_json)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/storage")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=storage["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    report["last_step"] = "storage-page-loaded"

    action(page, storage["page"]["add"]).click()
    modal = page.get_by_role("dialog")
    expect(modal).to_be_visible()
    expect(modal.get_by_text(storage["form"]["addTitle"], exact=True)).to_be_visible()
    save = button(modal, storage["form"]["save"])
    local = modal.get_by_role("radio", name=common["storageType"]["local"], exact=True)
    nfs = modal.get_by_role("radio", name=common["storageType"]["nfs"], exact=True)
    expect(local).to_be_checked()
    expect(modal.get_by_label(storage["form"]["path"], exact=True)).to_be_visible()

    # AntD renders the radio input hidden; the visible label is the click
    # target while the input still carries the checked state.
    modal.get_by_text(common["storageType"]["nfs"], exact=True).click()
    expect(modal.get_by_label(storage["form"]["export"], exact=True)).to_be_visible()
    expect(modal.get_by_label(storage["form"]["mountPoint"], exact=True)).to_be_visible()
    expect(save).to_be_disabled()
    check(posts == [], "an unprobed NFS form saved")
    modal.get_by_text(common["storageType"]["local"], exact=True).click()
    expect(modal.get_by_label(storage["form"]["path"], exact=True)).to_be_visible()
    expect(modal.get_by_label(storage["form"]["export"], exact=True)).to_have_count(0)
    report["last_step"] = "type-switch-verified"

    name_input = modal.get_by_label(storage["form"]["name"], exact=True)
    path_input = modal.get_by_label(storage["form"]["path"], exact=True)
    name_input.fill("Bad Name")
    path_input.fill(path)
    save.click()
    expect(modal.get_by_text(storage["form"]["error"]["name"], exact=True)).to_be_visible()
    check(posts == [], "an invalid name saved")
    report["last_step"] = "invalid-name-rejected"

    name_input.fill(name)
    path_input.fill("relative")
    save.click()
    expect(modal.get_by_text(storage["form"]["error"]["pathAbsolute"], exact=True)).to_be_visible()
    check(posts == [], "a relative path saved")
    report["last_step"] = "invalid-path-rejected"

    # The server, not the form, owns the root boundary: an absolute path outside
    # storage_probe_roots reaches the API, is refused, and the draft stays open.
    path_input.fill(outside)
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as refused:
        save.click()
    refused_body = refused.value.request.post_data_json
    check(refused_body == {"name": name, "type": "local", "all_nodes": True, "targets": [], "path": outside},
          f"out-of-root create body differs: {refused_body}")
    refusal = refused.value.json()
    check(refused.value.status == 400 and refusal.get("code") == "STORAGE_PATH_INVALID",
          f"out-of-root path: expected 400 STORAGE_PATH_INVALID, received {refused.value.status} {refusal}")
    alert = modal.get_by_role("alert").filter(has_text=storage["form"]["saveFailed"])
    expect(alert).to_contain_text(common["apiError"]["STORAGE_PATH_INVALID"])
    expect(name_input).to_have_value(name)
    expect(path_input).to_have_value(outside)
    listed = page.request.get(server.base + STORAGES).json()["items"]
    check(all(item["name"] != name for item in listed), "a refused storage was created")
    # This refusal is the asserted outcome, not a browser error.
    expected_http = {"method": "POST", "path": STORAGES, "status": 400}
    check(bad_responses.count(expected_http) == 1, f"expected exactly one refused POST: {bad_responses}")
    bad_responses.remove(expected_http)
    check(len(refusal_console) == 1, f"expected one console line for the refused POST: {refusal_console}")
    report["last_step"] = "outside-root-path-refused"

    # Retrying in the same dialog is BLOCKED_PRODUCT (coverage/storage-local.json): after a
    # fast refusal the Save button can keep AntD's loading icon in its accessible name.
    # Cancel sends nothing; a fresh form then creates the storage.
    button(modal, storage["form"]["cancel"]).click()
    expect(modal).to_have_count(0)
    check(len(posts) == 1, f"cancel after the refusal sent a create: {posts}")
    action(page, storage["page"]["add"]).click()
    expect(modal.get_by_text(storage["form"]["addTitle"], exact=True)).to_be_visible()
    expect(name_input).to_have_value("")
    name_input.fill(name)
    path_input.fill(path)
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES
                              and r.request.method == "POST") as created:
        save.click()
    check(created.value.status == 201, f"storage create HTTP {created.value.status}")
    body = created.value.request.post_data_json
    check(body == {"name": name, "type": "local", "all_nodes": True, "targets": [], "path": path},
          f"storage create body differs: {body}")
    storage_id = created.value.json()["id"]
    expect(page.get_by_text(format_text(storage["form"]["created"], name=name),
                            exact=True).first).to_be_visible()
    # The form opens the new storage's detail page.
    page.wait_for_url(re.compile(rf".*/storage/{storage_id}$"), timeout=15000)
    expect(page.get_by_text(name, exact=True).first).to_be_visible()
    expect(page.get_by_text(common["storageType"]["local"], exact=True).first).to_be_visible()
    expect(page.get_by_text(path, exact=True).first).to_be_visible()
    expect(page.get_by_text(storage["scope"]["all"], exact=True).first).to_be_visible()
    page.goto(server.base + "/storage")
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)

    page.reload()
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    independent = page.request.get(server.base + STORAGES + f"/{storage_id}")
    check(independent.status == 200, f"independent GET HTTP {independent.status}")
    saved = independent.json()
    check(saved["name"] == name and saved["type"] == "local" and saved["path"] == path,
          f"persisted storage differs: {saved}")
    check(saved["all_nodes"] is True, "the storage did not persist the all-nodes scope")
    report["last_step"] = "storage-persisted"

    row.get_by_role("button", name=storage["actions"]["delete"], exact=True).click()
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_contain_text(
        format_text(storage["actions"]["deleteConfirm"], name=name))
    expect(popconfirm).to_contain_text(storage["actions"]["deleteHelp"])
    with page.expect_response(lambda r: urlsplit(r.url).path == STORAGES + f"/{storage_id}"
                              and r.request.method == "DELETE") as deleted:
        confirm_popconfirm(page)
    check(deleted.value.status in (200, 204), f"storage delete HTTP {deleted.value.status}")
    expect(row).to_have_count(0, timeout=15000)
    expect(page.get_by_text(format_text(storage["actions"]["deleted"], name=name),
                            exact=True).first).to_be_visible()
    check(page.request.get(server.base + STORAGES + f"/{storage_id}").status == 404,
          "the deleted storage is still readable")
    report["last_step"] = "storage-deleted"

    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "storage page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/storage", "modal": storage["form"]["addTitle"],
         "nfs_fields": [storage["form"]["export"], storage["form"]["mountPoint"]],
         "nfs_save_disabled": True,
         "invalid_name": storage["form"]["error"]["name"],
         "invalid_path": storage["form"]["error"]["pathAbsolute"],
         "outside_root_alert": [storage["form"]["saveFailed"], common["apiError"]["STORAGE_PATH_INVALID"]],
         "created": name, "deleted": name,
         "delete_confirmation": {"title": format_text(
             storage["actions"]["deleteConfirm"], name=name),
             "impact": storage["actions"]["deleteHelp"]}},
        {"layer": "request", "post": {"path": STORAGES, "status": 201, "body": body},
         "invalid_submissions": 0,
         "server_rejections": [{"path": STORAGES, "status": 400, "code": refusal["code"],
                                "body": refused_body}],
         "delete": {"path": STORAGES + f"/{storage_id}", "status": deleted.value.status}},
        {"layer": "persistence", "refresh_row": 1,
         "independent_get": {"name": saved["name"], "type": saved["type"],
                             "path": saved["path"], "all_nodes": saved["all_nodes"]}},
        {"layer": "cleanup", "deleted": True, "after_delete": 404},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses, "posts": posts}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/storage-local.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_storage_local.__name__,
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
        "execution": "N/A: a local storage record; no agent trial mount or model-file access",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["agent trial mount and capacity",
                        "creating shared_dir, NFS or NFS-cache storage (only local is created)",
                        "cancel/close/Esc and the unsaved draft",
                        "node and label scopes", "editing a storage", "in-use deletion",
                        "duplicate names and exports", "non-admin permissions",
                        "model files and scans", "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/storage_local.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["server_environment"] = server.environment
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_storage_local(page, context, server, artifacts, report, args.locale)
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
        report["server_stopped"] = server.stop()
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
