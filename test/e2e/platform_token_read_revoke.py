#!/usr/bin/env python3
"""Single-machine browser closure: read token creation, authorization, revocation.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-platform-token
Requires the existing Python Playwright installation and system Chrome.
"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
import re
from pathlib import Path
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright

from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http


CONTRACT = "gpuworks/platform-token/read-create-revoke"
CASE = "平台 API 令牌 / UI 创建只读令牌并吊销 / 刷新持久化、只读权限生效且吊销立即失效"
TOKEN_PATH = "/api/v1/platform-tokens"
ITEM_FIELDS = {"id", "name", "prefix", "scope", "created_at", "expires_at", "last_used_at"}


def strings(locale):
    directory = ROOT / "ui/src/i18n/locales" / locale
    return (json.loads((directory / "apikeys.json").read_text()),
            json.loads((directory / "auth.json").read_text()))


def check(condition, description):
    # Never include actual credentials in assertion diagnostics.
    if not condition:
        raise AssertionError(description)


def button(scope, label):
    # AntD inserts a display space into two-character Chinese button text.
    accessible = r"\s*".join(label) if re.fullmatch(r"[\u3400-\u9fff]{2}", label) else re.escape(label)
    return scope.get_by_role("button", name=re.compile("^" + accessible + "$"))


def collection(response):
    check(response.status == 200, f"collection HTTP {response.status}")
    body = response.json()
    check(set(body) == {"items", "total"}, "unexpected collection envelope")
    check(isinstance(body["items"], list) and body["total"] == len(body["items"]), "incorrect collection total")
    return body["items"]


def modal_geometry(dialog, submit):
    expect(dialog).to_be_visible()
    expect(dialog).to_be_in_viewport(ratio=1)
    expect(submit).to_be_in_viewport(ratio=1)
    # Modal scale animation changes both rectangles. Read them in one frame only
    # after that animation ends; separate protocol calls compare different frames.
    dialog.page.wait_for_function("""({dialog, submit}) => {
        if (dialog.getAnimations().some(a => a.pending || a.playState === 'running')) return false;
        const box = dialog.getBoundingClientRect();
        const button = submit.getBoundingClientRect();
        return box.width > 0 && box.height > 0 && button.width > 0 && button.height > 0 &&
            box.left >= 0 && box.top >= 0 && box.right <= innerWidth && box.bottom <= innerHeight &&
            button.left >= box.left && button.top >= box.top &&
            button.right <= box.right && button.bottom <= box.bottom;
    }""", arg={"dialog": dialog.element_handle(), "submit": submit.element_handle()}, timeout=5000)


def theme_color(locator, property_name, token_name):
    source = (ROOT / "ui/src/theme/tokens.ts").read_text()
    match = re.search(rf'^  {re.escape(token_name)}: "(#[0-9a-fA-F]{{6}})"', source, re.M)
    check(match is not None, f"theme color {token_name} absent")
    expected = [int(match[1][i:i + 2], 16) for i in (1, 3, 5)] + [255]
    # Normalize CSS Color serialization without inserting or changing UI DOM.
    locator.page.wait_for_function("""({el, property, expected}) => {
        const ctx = new OffscreenCanvas(1, 1).getContext('2d');
        ctx.fillStyle = getComputedStyle(el)[property];
        ctx.fillRect(0, 0, 1, 1);
        return Array.from(ctx.getImageData(0, 0, 1, 1).data).every((value, i) => value === expected[i]);
    }""", arg={"el": locator.element_handle(), "property": property_name, "expected": expected}, timeout=5000)


# Contract: gpuworks/platform-token/read-create-revoke
# Mainline: 独立真实 server/SQLite → UI 校验、取消并创建默认只读令牌 → 真实复制且刷新/GET脱敏回读
#           → 无Cookie调用读取200、写入403且无副作用 → UI取消吊销仍可用、确认吊销 → 刷新消失且旧令牌401
def test_read_token_create_revoke(page, context, server, artifacts, report, locale):
    k, auth = strings(locale)
    platform = k["platform"]
    resource_name = f"e2e-{report['run_id']}-read"
    page_errors, console_errors, bad_responses, mutations = [], [], [], []
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))
    def console_seen(message):
        if message.type != "error":
            return
        if (not authenticated and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me"
                and "401" in message.text):
            return
        console_errors.append(message.text)

    page.on("console", console_seen)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return  # Exactly the anonymous session probe, before login.
            bad_responses.append({"path": path, "status": response.status})

    def request_seen(request):
        if urlsplit(request.url).path.startswith(TOKEN_PATH) and request.method in {"POST", "DELETE"}:
            mutations.append({"method": request.method, "path": urlsplit(request.url).path})

    page.on("response", response_seen)
    page.on("request", request_seen)
    page.goto(server.base + "/apikeys")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "browser login failed")
    # The UI consumes the HttpOnly session, not the unused login JWT body.
    # Do not retain that additional credential or wait for its body download.
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=k["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "fixture token collection must start empty")
    initial_keys = collection(page.request.get(server.base + "/api/v1/apikeys"))
    artifacts.start_trace(context)

    create = button(page, platform["create"])
    create.click()
    dialog = page.get_by_role("dialog", name=platform["createTitle"], exact=True)
    submit = button(dialog, k["actions"]["create"])
    name = dialog.get_by_label(k["table"]["name"], exact=True)
    modal_geometry(dialog, submit)
    expect(name).to_have_value("")
    expect(dialog.get_by_role("radio", name=platform["scopes"]["read"], exact=True)).to_be_checked()
    expect(dialog.get_by_text(platform["noExpiry"], exact=True)).to_be_visible()
    for invalid in ["", "   "]:
        name.fill(invalid)
        submit.click()
        expect(dialog.get_by_text(k["create"]["nameRequired"], exact=True)).to_be_visible()
        expect(name).to_have_attribute("aria-invalid", "true")
        check(not mutations, "invalid name sent a token mutation")
    name.fill(resource_name)
    name.press("Tab")
    for _ in range(12):
        check(dialog.evaluate("el => el.contains(document.activeElement)"), "Tab escaped modal focus trap")
        page.keyboard.press("Tab")
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(create).to_be_focused()
    check(not mutations, "cancelled form sent a mutation")
    report["evidence"].append({"layer": "ui", "checks": ["default read/no expiry", "empty/whitespace rejected without mutation", "Esc cancellation", "focus trap/return", "modal viewport"]})

    create.click()
    name.fill(resource_name)
    modal_geometry(dialog, submit)
    # Keyboard focus triggers the shared input focus style in the browser.
    name.press("Tab")
    page.keyboard.press("Shift+Tab")
    expect(name).to_be_focused()
    theme_color(name, "borderColor", "primary")
    page.screenshot(path=artifacts.directory / "create-form.png")
    # Stop recording BEFORE the POST or the one-time secret reaches the DOM.
    artifacts.stop_trace(context)
    report["sensitive_phase"] = True
    with page.expect_response(lambda r: urlsplit(r.url).path == TOKEN_PATH and r.request.method == "POST") as created:
        submit.click()
    response = created.value
    check(response.status == 201, f"create HTTP {response.status}")
    check(response.request.post_data_json == {"name": resource_name, "scope": "read"}, "incorrect create JSON/default expiry omission")
    check(not urlsplit(response.url).query, "unexpected create query")
    payload = response.json()
    check(set(payload) == {"token", "item"}, "incorrect created envelope")
    secret, item = payload["token"], payload["item"]
    artifacts.remember(secret)
    check(isinstance(secret, str) and secret.startswith("gwp_"), "missing one-time platform token")
    check(set(item) == ITEM_FIELDS, "created item fields expose secret or drifted")
    check(item["name"] == resource_name and item["scope"] == "read" and item["expires_at"] is None,
          "created item does not preserve defaults")
    check(item["prefix"] == secret[:12] and isinstance(item["id"], int), "incorrect token prefix/id")
    check(datetime.fromisoformat(item["created_at"].replace("Z", "+00:00")).timestamp() > 0, "invalid creation time")
    report["resource_id"] = item["id"]
    secret_dialog = page.get_by_role("dialog", name=k["secret"]["title"], exact=True)
    expect(secret_dialog).to_be_visible()
    check(secret_dialog.get_by_text(secret, exact=True).count() == 1, "one-time display differs from created token")
    modal_geometry(secret_dialog, button(secret_dialog, k["secret"]["saved"]))
    theme_color(secret_dialog.get_by_text(secret, exact=True), "backgroundColor", "canvas")
    button(secret_dialog, k["secret"]["copy"]).click()
    copied = page.evaluate("navigator.clipboard.readText()")
    check(copied == secret, "clipboard differs from the one-time token")
    button(secret_dialog, k["secret"]["saved"]).click()
    expect(secret_dialog).not_to_be_visible()
    # The hidden modal must also release the secret before snapshots resume.
    page.wait_for_function("secret => !document.documentElement.outerHTML.includes(secret)", arg=secret, timeout=5000)
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    report["evidence"].append({"layer": "request", "method": "POST", "path": TOKEN_PATH,
        "body": {"name": resource_name, "scope": "read"}, "status": 201, "item": item,
        "computed_styles": {"keyboard_focus_border": "theme.colors.primary", "secret_background": "theme.colors.canvas"},
        "clipboard": "equal to one-time value; consumed unchanged as bearer; secret omitted"})

    page.reload()
    row = page.get_by_role("row").filter(has_text=resource_name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(platform["scopes"]["read"])
    expect(row).to_contain_text(platform["noExpiry"])
    expect(secret_dialog).not_to_be_visible()
    check(secret not in page.content(), "reload reveals full secret")
    persisted = collection(page.request.get(server.base + TOKEN_PATH))
    check(persisted == [item], "independent GET differs from created record")
    check(secret not in json.dumps(persisted), "list leaks full secret")
    report["evidence"].append({"layer": "persistence", "method": "GET", "path": TOKEN_PATH, "status": 200, "items": persisted, "reload": "one row, no secret"})

    # No browser request context/cookies: this is actual bearer authorization.
    code, identity = http(server.base, "GET", "/api/v1/auth/me", copied)
    check(code == 200 and identity["username"] == "admin" and identity["role"] == "admin", "token attributed to incorrect principal")
    code, nodes = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 200 and nodes == {"items": [], "total": 0}, "read token cannot read isolated node inventory")
    report["evidence"].append({"layer": "runtime", "auth": "bearer only; no cookies", "path": "/api/v1/nodes", "status": code, "body": nodes, "principal": "admin"})
    code, denied = http(server.base, "POST", "/api/v1/apikeys", copied, {"name": resource_name + "-denied"})
    check(code == 403 and denied == {"code": "PLATFORM_TOKEN_READ_ONLY", "error": "a read token can only read", "params": {"scope": "read"}}, "read token write was not denied by scope")
    check(collection(page.request.get(server.base + "/api/v1/apikeys")) == initial_keys, "denied write mutated inference tokens")
    report["evidence"].append({"layer": "runtime", "method": "POST", "path": "/api/v1/apikeys", "status": code, "body": denied, "side_effects": "none (session GET equals baseline)"})
    code, denied = http(server.base, "GET", TOKEN_PATH, copied)
    check(code == 403 and denied["code"] == "PLATFORM_TOKEN_NOT_ALLOWED", "platform token accessed session-only management")
    report["evidence"].append({"layer": "runtime", "method": "GET", "path": TOKEN_PATH, "status": code, "body": denied})
    used = collection(page.request.get(server.base + TOKEN_PATH))[0]
    check(used["id"] == item["id"] and used["last_used_at"] is not None, "successful bearer use was not persisted")

    revoke = button(row, platform["revoke"])
    revoke.click()
    confirmation = platform["revokeConfirm"].replace("{{name}}", resource_name)
    confirm_text = page.get_by_text(confirmation, exact=True)
    expect(confirm_text).to_be_visible()
    expect(confirm_text).to_be_in_viewport(ratio=1)
    button(page, k["actions"]["cancel"]).click()
    expect(confirm_text).not_to_be_visible()
    check(len(mutations) == 1, "cancel revoke sent a mutation")
    code, _ = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 200, "cancel revoke invalidated token")
    revoke.click()
    expect(confirm_text).to_be_visible()
    # Confirmation lives in AntD's accessible tooltip popup, not in the row.
    popup = page.get_by_role("tooltip").filter(has_text=confirmation)
    confirm = button(popup, platform["revoke"])
    expect(confirm).to_be_in_viewport(ratio=1)
    delete_path = f"{TOKEN_PATH}/{item['id']}"
    with page.expect_response(lambda r: urlsplit(r.url).path == delete_path and r.request.method == "DELETE") as deleted:
        confirm.click()
    check(deleted.value.status == 204, "revoke did not return 204")
    check(deleted.value.request.post_data is None and not urlsplit(deleted.value.url).query, "unexpected revoke body/query")
    expect(row).to_have_count(0)
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "revoked token still persisted")
    code, denied = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 401 and denied["code"] == "AUTHENTICATION_REQUIRED", "revoked token remains authorized")
    page.reload()
    expect(page.get_by_text(platform["empty"], exact=True)).to_be_visible()
    expect(row).to_have_count(0)
    check(secret not in page.content(), "secret reappeared after revocation")
    check(mutations == [{"method": "POST", "path": TOKEN_PATH}, {"method": "DELETE", "path": delete_path}], "extra token mutations")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page has horizontal overflow")
    page.screenshot(path=artifacts.directory / "revoked.png")
    check(not page_errors, "unhandled browser errors: " + str(page_errors))
    check(not console_errors, "unexpected browser console errors: " + str(console_errors))
    check(not bad_responses, "unexpected browser HTTP errors: " + str(bad_responses))
    report["evidence"].append({"layer": "cleanup", "method": "DELETE", "path": delete_path,
        "status": 204, "cancel": "no mutation; bearer still 200", "refresh_and_get": "empty",
        "revoked_bearer": {"status": code, "body": denied}, "mutations": mutations})
    report["browser_errors"] = {"page": page_errors, "console": console_errors, "http": bad_responses}


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
    manifest_path = ROOT / "test/e2e/coverage/platform-token-read-revoke.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT and manifest["test_function"] == test_read_token_create_revoke.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin creates / read bearer consumes",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "N/A: platform authorization has no agent/workload",
        "not_covered": ["write scope", "expiry options/boundaries", "full field/list matrix", "non-admin owner isolation", "service-failure recovery"],
        "command": ["python3", "test/e2e/platform_token_read_revoke.py", *sys.argv[1:]]}
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
                test_read_token_create_revoke(page, context, server, artifacts, report, args.locale)
                layers = {e["layer"] for e in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers, "required evidence is missing")
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"  # Raw failure retained; triage may identify a product defect.
                report["failure"] = traceback.format_exc()
                artifacts.write_json("report.json", report)
                if report.get("sensitive_phase") or any(value in page.content() for value in artifacts.secrets if len(value) > 8):
                    report["failure_screenshot"] = "omitted at capture boundary: a credential may be visible; use preceding safe trace/form screenshot"
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
