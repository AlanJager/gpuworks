#!/usr/bin/env python3
"""Cloud account edit: saved proxy overrides round-trip through the real server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-provider-endpoint-edit
External boundary: strict local RunPod v2 pods and account SSH keys fixture;
no management API mocks. Fixture tests:
PYTHONPATH=test/e2e python3 -m unittest test_provider_endpoint_fixture -v
PYTHONPATH=test/e2e python3 -m unittest test_provider_endpoint_cleanup -v
"""
from __future__ import annotations

import json
import re
import time
import traceback
from urllib.parse import urlsplit

from playwright.sync_api import TimeoutError as PlaywrightTimeout, expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check, collection
from provider_endpoint_fixture import RunPodFixture

CONTRACT = "gpuworks/providers/endpoint-edit-roundtrip"


def cleanup_stage(report, name, action):
    """Attempt every owned close independently and never upgrade an earlier failure."""
    try:
        result = action()
        check(result is None or result is True, f"incomplete cleanup: {result!r}")
        report.setdefault("cleanup_stages", {})[name] = "ok"
        return True
    except Exception:
        report.setdefault("cleanup_stages", {})[name] = traceback.format_exc()
        report["status"] = "FAIL_TEST"
        return False


def copy_server_log(artifacts):
    log = artifacts.private / "server.log"
    if log.exists():
        (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))


def _box(locator):
    return locator.evaluate("""element => {
        const rect = element.getBoundingClientRect();
        return {left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom,
                width: rect.width, height: rect.height};
    }""")


def _separated(first, second):
    return (first["right"] <= second["left"] or second["right"] <= first["left"]
            or first["bottom"] <= second["top"] or second["bottom"] <= first["top"])


def exercise_provider_label_keyboard(page, dialog, copy, label):
    """Left label column, Tab through the add dialog, Enter sends no POST."""
    name_input = dialog.get_by_label(copy["config"]["name"], exact=True)
    endpoint_input = dialog.get_by_label(label("endpoint"), exact=True)
    key_input = dialog.get_by_label(label("api_key"), exact=True)
    name_item = name_input.locator(
        "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")
    label_col = name_item.locator(".ant-form-item-label")
    expect(label_col).to_have_class(re.compile(r"\bant-form-item-label-left\b"), timeout=12000)
    align = label_col.evaluate("element => getComputedStyle(element).textAlign")
    check(align == "start", f"name label alignment is {align}")
    kind = dialog.get_by_role("combobox")
    kind.focus()
    expect(kind).to_be_focused()
    kind.press("Tab")
    expect(name_input).to_be_focused()
    name_input.press("Tab")
    expect(key_input).to_be_focused()
    key_input.press("Tab")
    expect(endpoint_input).to_be_focused()
    add = button(dialog, copy["config"]["add"])
    cancel = button(dialog, copy["config"]["cancel"])
    add.focus()
    expect(add).to_be_focused()
    name_input.focus()
    expect(name_input).to_be_focused()
    submitted = False
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path == "/api/v1/providers"
                                  and request.method == "POST", timeout=1000):
            name_input.press("Enter")
        submitted = True
    except PlaywrightTimeout:
        submitted = False
    check(not submitted, "Enter in the empty name submitted a provider")
    expect(name_input).to_have_value("")
    dialog.scroll_into_view_if_needed()
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    for locator in (name_input, endpoint_input, key_input, cancel, add):
        expect(locator).to_be_in_viewport(ratio=1)
    name_box, add_box = _box(name_input), _box(add)
    check(name_box["width"] > 0 and add_box["width"] > 0, "name or Add has no box")
    check(_separated(name_box, add_box), "name overlaps Add")
    return {"label_class": "ant-form-item-label-left", "text_align": align,
            "tab": ["type", "name", "api key", "endpoint"], "add_focused": True,
            "enter_submitted": False, "viewport": view, "overlaps_add": False}


def _provider_mutation(page, action):
    submitted = False
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path.startswith("/api/v1/providers")
                                  and request.method in {"POST", "PUT", "DELETE"}, timeout=1000):
            action()
        submitted = True
    except PlaywrightTimeout:
        submitted = False
    return submitted


def exercise_provider_no_colon_dismiss(page, dialog, copy, label, locale, edit, read, endpoint, saved_name):
    """No-colon name label, then Escape, close, and Cancel each leave the saved account."""
    name_input = dialog.get_by_label(copy["config"]["name"], exact=True)
    name_item = name_input.locator(
        "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")
    name_label = name_item.locator("label")
    expect(name_label).to_have_class(re.compile(r"\bant-form-item-no-colon\b"), timeout=12000)
    endpoint_input = dialog.get_by_label(label("endpoint"), exact=True)
    # rc-dialog keeps aria-label "Close" on the button. The localized word sits on the icon wrapper.
    close = dialog.locator("button.ant-modal-close")
    close_label = "关闭" if locale == "zh" else "Close"
    expect(close.locator(".ant-modal-close-x")).to_have_attribute("aria-label", close_label, timeout=12000)
    cancel = button(dialog, copy["config"]["cancel"])
    discarded = "https://discarded.invalid/v2"

    def dismiss(prepare, action):
        endpoint_input.fill(discarded)
        expect(endpoint_input).to_have_value(discarded)
        prepare()
        check(not _provider_mutation(page, action), "dismiss sent a provider mutation")
        expect(dialog).not_to_be_visible(timeout=12000)
        read(endpoint, saved_name)

    dismiss(lambda: None, lambda: page.keyboard.press("Escape"))
    edit(endpoint, saved_name)
    dismiss(lambda: expect(close.locator(".ant-modal-close-x")).to_have_attribute(
                "aria-label", close_label, timeout=12000),
            lambda: close.click(timeout=12000))
    edit(endpoint, saved_name)
    dismiss(lambda: expect(cancel).to_be_visible(timeout=12000),
            lambda: cancel.click(timeout=12000))
    return {"label_class": "ant-form-item-no-colon", "close_label": close_label,
            "dismissed": ["escape", "close", "cancel"], "mutation": False}


# Mainline: UI新建自定义代理账号 → 刷新/独立GET → 编辑回显且密钥空白 → 仅改名称仍用原代理与密钥
#           → 修改代理遇到真实上游401不落盘 → 原草稿重试成功 → 刷新再编辑回显新代理 → UI删除
# Contract: gpuworks/providers/endpoint-edit-roundtrip
def test_provider_endpoint_edit(page, context, server, artifacts, report, locale):
    texts = ROOT / "ui/src/i18n/locales" / locale
    auth = json.loads((texts / "auth.json").read_text())
    copy = json.loads((texts / "providers.json").read_text())
    errors, console_errors, bad_http = [], [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(server.base + "/providers")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=copy["scope"]["public"]["title"], exact=True)).to_be_visible()
    report["preflight"] = "PASS"
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)
    page.on("response", lambda r: bad_http.append({"path": urlsplit(r.url).path, "status": r.status, "method": r.request.method}) if r.status >= 400 else None)
    kinds = collection(page.request.get(server.base + "/api/v1/providers/kinds"))
    runpod = next(kind for kind in kinds if kind["kind"] == "runpod")
    fields = {field["name"]: field for field in runpod["credential_fields"]}
    check(fields["endpoint"]["secret"] is False and fields["api_key"]["secret"] is True, "RunPod credential contract changed")
    label = lambda name: fields[name]["label"][locale]
    name = "e2e-" + report["run_id"] + "-proxy"
    secret = "e2e-runpod-" + report["run_id"]
    artifacts.remember(secret)
    # Existing account keys belong to the user; platform registration must keep them.
    existing_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILXGDN/SclOozk1xsDztpmhGiKkkrfQB9SKoO8dSIQQZ existing@example.com"
    platform_key = page.request.get(server.base + "/api/v1/ssh-key")
    check(platform_key.status == 200, "platform SSH key is unavailable")
    expected_keys = [existing_key, platform_key.json()["public_key"]]
    with RunPodFixture(secret, keys=[existing_key]) as fixture:
        endpoints = [fixture.base + "/proxy-a/v2", fixture.base + "/proxy-b/v2"]
        add = copy["page"]["addKind"].replace("{{kind}}", copy["kind"]["runpod"])
        button(page, add).click()
        dialog = page.get_by_role("dialog")
        endpoint_default = fields["endpoint"]["default_value"]
        report["provider_label_keyboard"] = exercise_provider_label_keyboard(
            page, dialog, copy, label)
        from provider_create_inventory import exercise_provider_create_inventory
        report["provider_create_inventory"] = exercise_provider_create_inventory(
            dialog, copy, label, endpoint_default,
            fields["endpoint"]["placeholder"][locale], fields["api_key"]["placeholder"][locale])
        expect(dialog.locator(".ant-select-selection-item")).to_have_text(copy["kind"]["runpod"])
        expect(dialog.get_by_label(copy["config"]["name"], exact=True)).to_have_value("")
        expect(dialog.get_by_label(label("endpoint"), exact=True)).to_have_value(endpoint_default)
        expect(dialog.get_by_label(label("api_key"), exact=True)).to_have_value("")
        report["evidence"].append({"layer": "ui", "first_open": {"kind": "runpod", "name_empty": True,
            "endpoint_default": endpoint_default, "api_key_empty": True}})
        dialog.get_by_label(copy["config"]["name"], exact=True).fill(name)
        dialog.get_by_label(label("endpoint"), exact=True).fill(endpoints[0])
        dialog.get_by_label(label("api_key"), exact=True).fill(secret)
        expected_create = {"name": name, "kind": "runpod", "endpoint": endpoints[0], "api_key": secret}
        held = []

        def hold_post(route):
            if route.request.method != "POST" or urlsplit(route.request.url).path != "/api/v1/providers":
                route.continue_()
                return
            held.append(route)

        page.route("**/api/v1/providers", hold_post)
        button(dialog, copy["config"]["add"]).click()
        inflight = dialog.locator(".ant-modal-footer button.ant-btn-primary")
        expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
        inflight.click()
        dialog.get_by_label(copy["config"]["name"], exact=True).press("Enter")
        check(len(held) == 1, "duplicate create from a second click or Enter while the first request is held")
        check(held[0].request.post_data_json == expected_create, "held provider body differs")
        with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/providers" and r.request.method == "POST") as created:
            held[0].continue_()
        page.unroute("**/api/v1/providers", hold_post)
        check(created.value.status == 201, f"POST /api/v1/providers: expected 201, received {created.value.status}")
        provider_id = created.value.json()["id"]
        report["remaining_resources"] = [{"kind": "provider", "id": provider_id,
                                         "scope": "isolated SQLite database in private/",
                                         "reclaim": "discard only this run directory after diagnosis"}]
        create_body = created.value.request.post_data_json
        check(create_body == {"name": name, "kind": "runpod", "endpoint": endpoints[0], "api_key": secret}, "create wire mismatch")
        path = f"/api/v1/providers/{provider_id}"
        expect(dialog).not_to_be_visible()
        artifacts.start_trace(context)
        report["evidence"].append({"layer": "request", "method": "POST", "path": "/api/v1/providers", "body": create_body, "status": 201})

        def read(endpoint, expected_name):
            response = page.request.get(server.base + path)
            expected = {"id": provider_id, "kind": "runpod", "name": expected_name, "endpoint": endpoint, "public_key": "", "extra": {}}
            check(response.status == 200 and response.json() == expected, "independent edit read mismatch or secret exposure")
            report["evidence"].append({"layer": "persistence", "path": path, "body": expected, "status": 200})

        def edit(endpoint, expected_name):
            page.reload()
            row = page.get_by_role("row").filter(has=page.get_by_text(expected_name, exact=True))
            gate = {"held": None}

            def hold_get(route):
                if route.request.method == "GET" and urlsplit(route.request.url).path == path and gate["held"] is None:
                    gate["held"] = route
                    return
                route.continue_()

            page.route("**/api/v1/providers/*", hold_get)
            try:
                button(row, copy["table"]["edit"]).click()
                deadline = time.monotonic() + 5
                while gate["held"] is None:
                    if time.monotonic() > deadline:
                        raise AssertionError("edit configuration read did not start")
                    page.wait_for_timeout(50)
                expect(dialog.get_by_label(label("endpoint"), exact=True)).to_have_value("")
                expect(dialog.get_by_label(label("api_key"), exact=True)).to_have_value("")
                held_save = dialog.locator(".ant-modal-footer button.ant-btn-primary")
                expect(held_save).to_be_disabled()
                with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "GET") as loaded:
                    gate["held"].continue_()
                    gate["held"] = None
                check(loaded.value.status == 200, "edit configuration read failed")
                expect(dialog.get_by_label(label("endpoint"), exact=True)).to_have_value(endpoint)
                expect(dialog.get_by_label(label("api_key"), exact=True)).to_have_value("")
                expect(dialog.get_by_label(copy["config"]["name"], exact=True)).to_have_value(expected_name)
                expect(button(dialog, copy["config"]["save"])).to_be_enabled()
                report["evidence"].append({"layer": "ui", "reload_edit_endpoint": endpoint, "secret_empty": True,
                                           "save_disabled_until_edit_get": True})
            finally:
                if gate["held"] is not None:
                    gate["held"].continue_()
                page.unroute("**/api/v1/providers/*", hold_get)

        def save(endpoint, expected_name, status):
            with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "PUT") as updated:
                button(dialog, copy["config"]["save"]).click()
            response = updated.value
            report["last_request"] = {"method": "PUT", "path": path, "status": response.status,
                                      "expected_status": status, "body": response.request.post_data_json,
                                      "response_text": response.text()}
            check(response.status == status, f"PUT {path}: expected {status}, received {response.status}")
            check(response.request.post_data_json == {"name": expected_name, "endpoint": endpoint}, "edit must keep blank secret omitted")
            report["evidence"].append({"layer": "request", "method": "PUT", "path": path, "body": response.request.post_data_json, "status": status})
            if status == 200:
                expect(dialog).not_to_be_visible()
            else:
                # This legacy provider error is a plain writeErr response, without a ProductError code.
                reason = "credential validation failed: runpod: invalid credentials (status 401)"
                check(response.json() == {"error": reason}, "provider validation error body differs")
                expect(dialog).to_be_visible()
                expect(dialog.get_by_label(label("endpoint"), exact=True)).to_have_value(endpoint)
                message = (copy["config"]["operationFailed"]
                           .replace("{{operation}}", copy["config"]["operation"]["update"])
                           .replace("{{detail}}", reason))
                expect(page.get_by_text(message, exact=True)).to_be_visible()
                # Left labels, Tab, Enter and viewport already passed. The visible
                # rejection is the remaining 1.15 clause.
                report["provider_label_left"] = True
                report["evidence"].append({"layer": "ui", "validation_failure": response.json(),
                                           "localized_operation_error": message, "draft_endpoint": endpoint})

        read(endpoints[0], name)
        edit(endpoints[0], name)
        renamed = name + "-renamed"
        dialog.get_by_label(copy["config"]["name"], exact=True).fill(renamed)
        save(endpoints[0], renamed, 200)
        read(endpoints[0], renamed)
        edit(endpoints[0], renamed)
        dialog.get_by_label(label("endpoint"), exact=True).fill(endpoints[1])
        fixture.reject = True
        save(endpoints[1], renamed, 400)
        read(endpoints[0], renamed)
        fixture.reject = False
        save(endpoints[1], renamed, 200)
        read(endpoints[1], renamed)
        edit(endpoints[1], renamed)
        report["provider_dismiss"] = exercise_provider_no_colon_dismiss(
            page, dialog, copy, label, locale, edit, read, endpoints[1], renamed)
        report["provider_no_colon"] = True
        row = page.get_by_role("row").filter(has=page.get_by_text(renamed, exact=True))
        button(row, copy["table"]["delete"]).click()
        with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as deleted:
            button(page.get_by_role("tooltip"), copy["table"]["delete"]).click()
        check(deleted.value.status == 200, "provider cleanup failed")
        check(not any(p["id"] == provider_id for p in collection(page.request.get(server.base + "/api/v1/providers"))), "fixture account leaked")
        check(all(r["authorized"] for r in fixture.requests), "updates failed to retain the stored API key")
        check([(r["method"], r["path"], r["status"]) for r in fixture.requests] == [
            ("GET", "/proxy-a/v2/pods", 200),
            ("GET", "/proxy-a/v2/account/ssh-keys", 200),
            ("PUT", "/proxy-a/v2/account/ssh-keys", 200),
            ("GET", "/proxy-a/v2/pods", 200), ("GET", "/proxy-b/v2/pods", 401),
            ("GET", "/proxy-b/v2/pods", 200)], f"unexpected cloud API requests: {fixture.requests}")
        check(fixture.requests[2]["body"] == {"keys": expected_keys} and fixture.keys == expected_keys,
              "platform key registration replaced or lost existing account keys")
        report["evidence"].append({"layer": "runtime", "requests": fixture.requests, "boundary": "local RunPod REST fixture, real server credentials decrypt/validate"})
        report["evidence"].append({"layer": "cleanup", "provider_id": provider_id, "deleted": True})
        report["remaining_resources"] = []
    report["external_fixture_stopped"] = not fixture.thread.is_alive()
    check(not errors, f"browser page errors: {errors}")
    check(bad_http == [{"path": path, "status": 400, "method": "PUT"}], "unexpected browser HTTP failures")
    check(len(console_errors) == 1 and "400" in console_errors[0], "unexpected console errors")
    report["browser_errors"] = {"page": errors, "console": console_errors, "http": bad_http, "expected": "one PUT 400 from the deliberately rejected upstream validation"}

def main():
    import argparse
    from datetime import datetime, timezone
    import os
    from pathlib import Path
    import subprocess
    import sys
    import uuid
    from playwright.sync_api import sync_playwright
    from local_browser_fixture import Artifacts, LocalServer, ROOT, digest

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / "test/e2e/coverage/provider-endpoint-edit.json"
    manifest = json.loads(manifest_path.read_text())
    if manifest["contract_id"] != CONTRACT or manifest["test_function"] != test_provider_endpoint_edit.__name__:
        raise AssertionError("manifest function binding drift")
    report = {
        "contract": manifest["contract_id"], "case": manifest["name"],
        "status": "NOT_RUN", "run_id": run_id,
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "target_branch": "wei/master",
        "target_sha": subprocess.check_output(["git", "rev-parse", "wei/master"], cwd=ROOT, text=True).strip(),
        "lane": "local-real-server-sqlite", "identity": "admin browser",
        "locale": args.locale, "viewport": {"width": 1440, "height": 900},
        "timezone": "UTC", "selection": [manifest["contract_id"]], "retries": 0, "evidence": [],
        "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/platform_token_read_revoke.py"),
        "provider_fixture_sha256": digest(ROOT / "test/e2e/provider_endpoint_fixture.py"),
        "provider_inventory_sha256": digest(ROOT / "test/e2e/provider_create_inventory.py"),
        "coverage_manifest_sha256": digest(manifest_path),
        "command": ["python3", "test/e2e/provider_endpoint_edit.py", *sys.argv[1:]],
    }
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"], report["owned_server_pid"] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_provider_endpoint_edit(page, context, server, artifacts, report, args.locale)
                layers = {item["layer"] for item in report["evidence"]}
                if not set(manifest["required_evidence"]) <= layers:
                    raise AssertionError("missing required evidence")
                report["business_complete"] = True
            except Exception:
                report["status"], report["failure"] = "FAIL_TEST", traceback.format_exc()
            finally:
                cleanup_stage(report, "trace", lambda: artifacts.stop_trace(context))
                cleanup_stage(report, "context", context.close)
                cleanup_stage(report, "browser", browser.close)
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        report["server_stopped"] = cleanup_stage(report, "server", server.stop)
        if not report["server_stopped"]:
            report.setdefault("remaining_resources", []).append({"kind": "process",
                "pid": server.process.pid if server.process else None,
                "reclaim": f"kill {server.process.pid}" if server.process else "server never started"})
        cleanup_stage(report, "server-log", lambda: copy_server_log(artifacts))
        report["private_state"] = "isolated DB/logs retained for diagnosis; remove only this run directory within 7 days"
        cleanup_stage(report, "report-before-scan", lambda: artifacts.write_json("report.json", report))
        cleanup_stage(report, "artifact-scan", artifacts.scan)
        if report.get("business_complete") and report["status"] == "NOT_RUN" and not report.get("remaining_resources"):
            report["status"] = "PASS"
        cleanup_stage(report, "final-report", lambda: artifacts.write_json("report.json", report))
    print(json.dumps({"status": report["status"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
