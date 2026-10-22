#!/usr/bin/env python3
"""Real single-server inference credential lifecycle; no GPU inference claim.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run after make build: python3 test/e2e/inference_key_lifecycle.py
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check, collection, modal_geometry, strings, theme_color

CONTRACT = "gpuworks/inference-key/create-copy-delete"
CASE = "推理令牌 / UI 创建复制并删除 / 刷新回读、真实网关鉴权与即时撤销生效"
KEY_PATH = "/api/v1/apikeys"
DENIED = {"error": {"message": "invalid or missing API key", "type": "gpuworks_proxy"}}
MODELS = {"object": "list", "data": []}


# Contract: gpuworks/inference-key/create-copy-delete
# Mainline: 独立 server/SQLite 和同轮 guard Key → UI 校验、创建并真实复制推理令牌 → 刷新/GET脱敏回读
#           → 原样Bearer调用/v1/models且拒绝管理API → UI取消删除仍可用、确认删除后401 → guard仍可用并清理
def test_inference_key_lifecycle(page, context, server, artifacts, report, locale):
    k, auth = strings(locale)
    name = f"e2e-{report['run_id']}-inference"
    guard_name = f"e2e-{report['run_id']}-guard"
    errors, console_errors, bad_responses, mutations = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: errors.append(str(error)))

    def console_seen(message):
        if message.type == "error":
            if not authenticated and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me" and "401" in message.text:
                return
            console_errors.append(message.text)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"path": path, "status": response.status})

    def request_seen(request):
        if urlsplit(request.url).path.startswith(KEY_PATH) and request.method in {"POST", "DELETE"}:
            mutations.append({"method": request.method, "path": urlsplit(request.url).path})

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)
    page.goto(server.base + "/apikeys")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=k["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    # A fresh real server seeds one default key. Remove only that owned bootstrap
    # resource before constructing this case's explicit two-key fixture.
    bootstrap = collection(page.request.get(server.base + KEY_PATH))
    check(len(bootstrap) == 1 and bootstrap[0]["name"] == "default", "fresh-server bootstrap key contract differs")
    removed = page.request.delete(server.base + KEY_PATH + "/" + bootstrap[0]["prefix"])
    check(removed.status == 200 and removed.json() == {"status": "deleted"}, "owned bootstrap key cleanup failed")
    check(collection(page.request.get(server.base + KEY_PATH)) == [], "bootstrap key still present")
    # Zero configured keys intentionally enables the product's anonymous dev mode.
    # Keep a scoped guard key until the revoked key has been proved unauthorized.
    guard_response = page.request.post(server.base + KEY_PATH, data={"name": guard_name})
    check(guard_response.status == 201, "guard fixture create failed")
    guard_payload = guard_response.json()
    check(set(guard_payload) == {"name", "key"} and guard_payload["name"] == guard_name, "guard create envelope drift")
    guard = guard_payload["key"]
    artifacts.remember(guard)
    guard_prefix = guard[:12]
    check(http(server.base, "GET", "/v1/models") == (401, DENIED), "guard failed to enable real gateway authentication")
    report["fixture"] = {"guard_name": guard_name, "guard_prefix": guard_prefix, "owned_bootstrap_key_removed": bootstrap[0]["prefix"],
                         "reason": "Keep authentication enabled while proving target-key revocation; zero keys allows anonymous development access"}
    artifacts.start_trace(context)
    create = button(page, k["create"]["button"])
    create.click()
    dialog = page.get_by_role("dialog", name=k["create"]["title"], exact=True)
    submit = button(dialog, k["actions"]["create"])
    field = dialog.get_by_label(k["table"]["name"], exact=True)
    modal_geometry(dialog, submit)
    expect(field).to_have_value("")
    for invalid in ["", "   "]:
        field.fill(invalid)
        submit.click()
        expect(dialog.get_by_text(k["create"]["nameRequired"], exact=True)).to_be_visible()
        expect(field).to_have_attribute("aria-invalid", "true")
        check(mutations == [], "invalid form sent mutation")
    field.fill(name)
    for _ in range(8):
        page.keyboard.press("Tab")
        check(dialog.evaluate("el => el.contains(document.activeElement)"), "Tab escaped creation modal")
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(create).to_be_focused()
    check(mutations == [], "cancelled create sent mutation")
    create.click()
    field.fill(name)
    field.press("Tab")
    page.keyboard.press("Shift+Tab")
    expect(field).to_be_focused()
    theme_color(field, "borderColor", "primary")
    modal_geometry(dialog, submit)
    page.screenshot(path=artifacts.directory / "create-form.png")
    report["evidence"].append({"layer": "ui", "checks": ["empty/whitespace rejected without POST", "Tab trapped inside modal", "Esc without POST and focus return", "named dialog and footer in viewport", "focus border inherits theme.primary"]})
    artifacts.stop_trace(context)
    report["sensitive_phase"] = True
    with page.expect_response(lambda r: urlsplit(r.url).path == KEY_PATH and r.request.method == "POST") as created:
        field.press("Enter")
    response = created.value
    check(response.status == 201 and response.request.post_data_json == {"name": name} and not urlsplit(response.url).query,
          "incorrect inference create method/path/query/body/status")
    payload = response.json()
    check(set(payload) == {"name", "key"} and payload["name"] == name, "incorrect inference create envelope")
    secret = payload["key"]
    artifacts.remember(secret)
    check(isinstance(secret, str) and secret.startswith("gw-") and len(secret) == 51, "incorrect inference credential shape")
    secret_dialog = page.get_by_role("dialog", name=k["secret"]["title"], exact=True)
    expect(secret_dialog).to_be_visible()
    check(secret_dialog.get_by_text(secret, exact=True).count() == 1, "one-time display differs from response")
    modal_geometry(secret_dialog, button(secret_dialog, k["secret"]["saved"]))
    button(secret_dialog, k["secret"]["copy"]).click()
    copied = page.evaluate("navigator.clipboard.readText()")
    check(copied == secret, "real clipboard differs from displayed credential")
    button(secret_dialog, k["secret"]["saved"]).click()
    expect(secret_dialog).not_to_be_visible()
    page.wait_for_function("value => !document.documentElement.outerHTML.includes(value)", arg=secret)
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    report["evidence"].append({"layer": "request", "method": "POST", "path": KEY_PATH,
        "body": {"name": name}, "status": 201, "response_fields": ["name", "key"], "clipboard": "matches display and response; used unchanged; secret omitted"})
    page.reload()
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(secret[:12])
    items = collection(page.request.get(server.base + KEY_PATH))
    check(len(items) == 2 and {item["name"] for item in items} == {name, guard_name}, "independent GET resource set drift")
    item = next(item for item in items if item["name"] == name)
    check(set(item) == {"name", "prefix", "created_at"} and item["prefix"] == secret[:12], "list fields/prefix drift or secret leak")
    check(datetime.fromisoformat(item["created_at"].replace("Z", "+00:00")).timestamp() > 0, "invalid persisted created time")
    check(secret not in page.content() and secret not in json.dumps(items), "persisted list or DOM reveals full secret")
    report["evidence"].append({"layer": "persistence", "method": "GET", "path": KEY_PATH, "status": 200,
        "target": item, "total": 2, "refresh": "exactly one target row; prefix only"})
    check(http(server.base, "GET", "/v1/models", copied) == (200, MODELS), "copied key cannot consume real OpenAI model collection")
    check(http(server.base, "GET", "/v1/models", copied + "x") == (401, DENIED), "incorrect secret sharing a valid prefix was accepted")
    code, denied_management = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 401 and denied_management["code"] == "AUTHENTICATION_REQUIRED", "inference key unexpectedly authorizes management")
    used = next(item for item in collection(page.request.get(server.base + KEY_PATH)) if item["name"] == name)
    check(used.get("last_used") and {key: value for key, value in used.items() if key != "last_used"} == item,
          "successful gateway use did not persist last_used or changed credential metadata")
    report["evidence"].append({"layer": "runtime", "auth": "bearer only; no cookies", "path": "/v1/models", "status": 200,
        "body": MODELS, "wrong_secret": 401, "management": {"status": code, "body": denied_management}, "last_used": used["last_used"],
        "limit": "Empty deployed model inventory proves gateway authentication only; no generation/GPU claim"})
    delete = button(row, k["actions"]["delete"])
    delete.click()
    confirmation = k["delete"]["confirm"].replace("{{name}}", name)
    popup = page.get_by_role("tooltip").filter(has_text=confirmation)
    expect(popup).to_be_visible()
    expect(popup).to_be_in_viewport(ratio=1)
    button(popup, k["actions"]["cancel"]).click()
    expect(popup).not_to_be_visible()
    check(len(mutations) == 1 and http(server.base, "GET", "/v1/models", copied) == (200, MODELS), "cancel delete mutated or revoked key")
    delete.click()
    delete_path = KEY_PATH + "/" + item["prefix"]
    with page.expect_response(lambda r: urlsplit(r.url).path == delete_path and r.request.method == "DELETE") as deleted:
        button(popup, k["actions"]["delete"]).click()
    check(deleted.value.status == 200 and deleted.value.json() == {"status": "deleted"}, "unexpected delete response")
    check(deleted.value.request.post_data is None and not urlsplit(deleted.value.url).query, "delete body/query drift")
    expect(row).to_have_count(0)
    remaining = collection(page.request.get(server.base + KEY_PATH))
    check(len(remaining) == 1 and remaining[0]["name"] == guard_name and remaining[0]["prefix"] == guard_prefix, "delete changed unrelated guard or retained target")
    check(http(server.base, "GET", "/v1/models", copied) == (401, DENIED), "deleted key retained runtime authorization")
    check(http(server.base, "GET", "/v1/models") == (401, DENIED), "target delete disabled gateway authentication")
    check(http(server.base, "GET", "/v1/models", guard) == (200, MODELS), "target delete revoked unrelated guard")
    page.reload()
    expect(row).to_have_count(0)
    expect(page.get_by_role("row").filter(has_text=guard_name)).to_have_count(1)
    check(mutations == [{"method": "POST", "path": KEY_PATH}, {"method": "DELETE", "path": delete_path}], "extra browser mutations")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page horizontal overflow")
    page.screenshot(path=artifacts.directory / "deleted.png")
    guard_cleanup = page.request.delete(server.base + KEY_PATH + "/" + guard_prefix)
    check(guard_cleanup.status == 200 and guard_cleanup.json() == {"status": "deleted"}, "guard cleanup failed")
    check(collection(page.request.get(server.base + KEY_PATH)) == [], "owned keys leaked")
    check(http(server.base, "GET", "/v1/models") == (200, MODELS), "zero-key cleanup did not restore documented anonymous development mode")
    check(not errors and not console_errors and not bad_responses, "unexpected browser errors: " + str([errors, console_errors, bad_responses]))
    report["evidence"].append({"layer": "cleanup", "method": "DELETE", "path": delete_path, "status": 200,
        "cancel": "no DELETE, original key still 200", "revoked_key": 401, "anonymous": 401, "guard_key": 200,
        "refresh_and_get": "target absent, guard unchanged", "fixture_cleanup": "guard DELETE 200 then collection empty; anonymous /v1/models 200 in zero-key development mode", "browser_mutations": mutations})
    report["browser_errors"] = {"page": errors, "console": console_errors, "http": bad_responses}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / "test/e2e/coverage/inference-key-lifecycle.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT and manifest["test_function"] == test_inference_key_lifecycle.__name__, "manifest does not bind case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN", "evidence": [],
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin creates; inference bearer consumes without cookies",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900}, "selection": [CONTRACT], "retries": 0,
        "coverage_manifest_sha256": digest(manifest_path), "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/platform_token_read_revoke.py"),
        "execution": "N/A: gateway key authorization requires no agent; model inventory is empty",
        "not_covered": manifest["missing"], "command": ["python3", "test/e2e/inference_key_lifecycle.py", *sys.argv[1:]]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US",
                timezone_id="UTC", permissions=["clipboard-read", "clipboard-write"])
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_inference_key_lifecycle(page, context, server, artifacts, report, args.locale)
                check(set(manifest["required_evidence"]) <= {item["layer"] for item in report["evidence"]}, "missing evidence")
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = traceback.format_exc()
                artifacts.write_json("report.json", report)
                if report.get("sensitive_phase") or any(value in page.content() for value in artifacts.secrets if len(value) > 8):
                    report["failure_screenshot"] = "omitted: a credential may be visible"
                else:
                    page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        report["server_stopped"] = server.stop()
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        log = artifacts.private / "server.log"
        if log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))
        report["private_state"] = "private/ retains isolated DB/log; no process retained; owner removes scoped run after diagnosis within 7 days"
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; do not upload"
            artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": CONTRACT, "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
