#!/usr/bin/env python3
"""Login / two-tab logout / old cookie and login bearer revoked, re-login recovers.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-session-logout
After building: python3 test/e2e/session_logout.py --locale en  # or zh
Prerequisites: local Chrome and Playwright Python; an existing server binary for
an isolated LocalServer/SQLite. No agent, GPU or remote credentials are needed.
Set NO_PROXY=127.0.0.1,localhost and no_proxy likewise when using an HTTP proxy.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and retains the
private database/logs for scoped diagnosis (remove within seven days).
Login and re-login are untraced; captured credentials stay in memory and are
registered for artifact redaction. Auth response barriers observe original
bytes, then continue without request/status/header/body overrides.
"""
from __future__ import annotations

import argparse
import base64
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
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/session/two-tab-logout-relogin"
LOGIN, LOGOUT, ME, ACTIVITY = ("/api/v1/auth/" + name for name in ("login", "logout", "me", "activity"))
COOKIE = "gpuworks_session"
DENIED = {"code": "AUTHENTICATION_REQUIRED", "error": "authentication required"}


def exercise_login_prefix(page, auth):
    """The username affix wrapper carries the input-prefix class."""
    username = page.get_by_role("textbox", name=auth["username"], exact=True)
    affix = username.locator(
        "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-input-affix-wrapper ')][1]")
    expect(affix.locator(".ant-input-prefix")).to_have_count(1, timeout=12000)
    return username


def cookie_identity(play, base, value):
    # A fresh isolated API client sends only the captured cookie. It cannot
    # inherit the browser's replacement cookie or an Authorization header.
    client = play.request.new_context(extra_http_headers={"Cookie": f"{COOKIE}={value}"})
    try:
        response = client.get(base + ME)
        return response.status, response.json()
    finally:
        client.dispose()


def capture_credentials(response, context, artifacts, body):
    # Register secrets before any assertion can fail; never attach raw bodies.
    token = body.get("token", "")
    artifacts.remember(token)
    cookies = [cookie for cookie in context.cookies() if cookie["name"] == COOKIE]
    for cookie in cookies:
        artifacts.remember(cookie["value"])
    check(response.status == 200 and set(body) == {"token", "user"}, "login response contract differs")
    check(len(cookies) == 1 and len(cookies[0]["value"]) == 64 and cookies[0]["httpOnly"]
          and cookies[0]["sameSite"] == "Lax" and cookies[0]["path"] == "/", "session cookie contract differs")
    check(isinstance(token, str) and token.count(".") == 2, "login bearer missing")
    check(body["user"]["username"] == "admin" and body["user"]["role"] == "admin", "unexpected login identity")
    check(set(body["user"]) == {"id", "username", "role"}, "login identity fields differ")
    check(response.headers.get("cache-control") == "no-store", "login response must not be cached")
    return cookies[0]["value"], token, body["user"]


def capture_auth_response(cdp, event, artifacts):
    # Login/logout UI consumes headers only; Chrome may discard unread bodies
    # when the form unmounts. Read real bytes at a response-stage barrier, then
    # release UNCHANGED. Never supply status/headers/body/request overrides.
    request_id = event["requestId"]
    try:
        path = urlsplit(event["request"]["url"]).path
        check(path in (LOGIN, LOGOUT) and event["request"]["method"] == "POST", "unexpected auth capture target")
        raw = cdp.send("Fetch.getResponseBody", {"requestId": request_id})
        body = json.loads(base64.b64decode(raw["body"]) if raw.get("base64Encoded") else raw["body"])
        artifacts.remember(body.get("token", ""))
        return path, {"status": event["responseStatusCode"], "body": body}
    finally:
        cdp.send("Fetch.continueResponse", {"requestId": request_id})


# Mainline: 独立真实server/SQLite → 深链错误密码后UI登录 → 两标签刷新与身份回读
# → UI注销 → 原Cookie和登录Bearer真实401、第二标签重新认证 → UI重登后双标签恢复且旧凭据仍401。
# Contract: gpuworks/session/two-tab-logout-relogin
def test_two_tab_logout_relogin(play, context, server, artifacts, report, locale):
    strings = ROOT / "ui/src/i18n/locales" / locale
    auth = json.loads((strings / "auth.json").read_text())
    common = json.loads((strings / "common.json").read_text())
    networks = json.loads((strings / "networks.json").read_text())
    settings = json.loads((strings / "settings.json").read_text())
    errors, http_errors, console_errors, login_requests, logout_requests = [], [], [], [], []
    report["browser_errors"] = {"page": errors, "http": http_errors, "console": console_errors}

    auth_bodies = {}

    def observe(page):
        cdp = context.new_cdp_session(page)
        auth_bodies[page] = {LOGIN: [], LOGOUT: []}

        def capture_auth(event):
            path, captured = capture_auth_response(cdp, event, artifacts)
            auth_bodies[page][path].append(captured)
        cdp.on("Fetch.requestPaused", capture_auth)
        cdp.send("Fetch.enable", {"patterns": [{"urlPattern": server.base + path, "requestStage": "Response"}
                                              for path in (LOGIN, LOGOUT)]})
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.on("response", lambda r: http_errors.append({"path": urlsplit(r.url).path, "status": r.status})
                if r.status >= 400 and urlsplit(r.url).path not in (ME, LOGIN, ACTIVITY) else None)

        def console(message):
            if message.type != "error":
                return
            # Chrome logs the deliberately rejected login/anonymous probe.
            if (urlsplit(message.location.get("url", "")).path in (LOGIN, ME, ACTIVITY)
                    and message.text == "Failed to load resource: the server responded with a status of 401 (Unauthorized)"):
                return
            console_errors.append(message.text)
        page.on("console", console)

        def request(req):
            path = urlsplit(req.url).path
            if path == LOGIN and req.method == "POST":
                # Count only: the request body is asserted in memory below.
                login_requests.append({"method": "POST", "path": path})
            if path == LOGOUT:
                logout_requests.append({"method": req.method, "path": path, "body": req.post_data})
        page.on("request", request)

    def auth_body(page, path):
        captured = auth_bodies[page][path][-1]
        check(captured["status"] == 200, "successful real authentication response not captured")
        return captured["body"]

    # 未登录访问网络深链；空字段不提交，错误密码真实拒绝并保留用户名。
    page = context.new_page()
    observe(page)
    with page.expect_response(lambda r: urlsplit(r.url).path == ME) as anonymous:
        page.goto(server.base + "/networks")
    check(anonymous.value.status == 401 and anonymous.value.json() == DENIED, "anonymous deep link gained authentication")
    expect(page.get_by_role("heading", name=auth["title"], exact=True)).to_be_visible()
    expect(page).to_have_url(server.base + "/networks")
    report["preflight"] = "PASS"
    # requiredMark={false} puts this class on the form. The accessible name of
    # the textbox changes when the mark is shown, so this check comes first.
    login_form = page.locator("form.ant-form")
    expect(login_form).to_have_count(1, timeout=12000)
    expect(login_form).to_have_class(re.compile(r"\bant-form-hide-required-mark\b"), timeout=12000)
    username = exercise_login_prefix(page, auth)
    from login_inventory import exercise_language_switcher, exercise_login_inventory
    report["login_language_switcher"] = exercise_language_switcher(
        page, auth, locale, common["language"]["switch"])
    report["login_inventory"] = exercise_login_inventory(page, auth)
    secret = page.get_by_label(auth["password"], exact=True)
    expect(username).to_be_empty()
    expect(secret).to_be_empty()
    expect(secret).to_have_attribute("type", "password")
    # 7.1: the mask is a state, not a styling choice, and the reveal toggle is a real control.
    # The value is typed, required to stay masked while the toggle is untouched, required to become
    # readable after one activation with the characters unchanged, and required to be masked again
    # after a second - so a toggle that only redrew the icon, or one that lost the value, fails here.
    probe = "e2e-probe-secret"
    secret.fill(probe)
    expect(secret).to_have_attribute("type", "password")
    check(secret.get_attribute("type") != "text", "a typed password was readable without its toggle")
    toggle = page.locator(".ant-input-password-icon")
    expect(toggle).to_have_count(1, timeout=8000)
    toggle.click()
    expect(secret).to_have_attribute("type", "text")
    check(secret.input_value() == probe, "revealing the password changed its value")
    toggle.click()
    expect(secret).to_have_attribute("type", "password")
    check(secret.input_value() == probe, "re-masking the password changed its value")
    secret.fill("")
    report["credential_mask"] = {"masked_default": True, "reveal_round_trip": True}
    button(page, auth["submit"]).click()
    # 1.3: each message must be located at the field it belongs to, not merely somewhere on the page,
    # so the item is asserted by walking from the field to the message inside its own form item.
    for field, message in ((username, auth["usernameRequired"]), (secret, auth["passwordRequired"])):
        item = page.locator(".ant-form-item").filter(
            has=page.locator("label").filter(has_text=re.compile("^" + re.escape(
                auth["username"] if field is username else auth["password"]) + "$")))
        expect(item).to_have_count(1, timeout=8000)
        expect(item.get_by_text(message, exact=True)).to_be_visible()
        expect(item.locator(".ant-form-item-explain-error")).to_have_count(1, timeout=8000)
    expect(page.get_by_role("alert")).to_have_count(0)
    check(not login_requests, "empty login submitted a request")
    # 1.15: the keyboard order and the layout this form is used in. Tab is required to reach both
    # fields in reading order with no stop skipped and no hidden control in between, Enter in the
    # username field must not submit before a password exists, and at 1440x900 the fields and the
    # submit button are required to be on screen and not overlapping.
    page.set_viewport_size({"width": 1440, "height": 900})
    username.focus()
    expect(username).to_be_focused()
    page.keyboard.press("Tab")
    expect(secret).to_be_focused()
    page.keyboard.press("Shift+Tab")
    expect(username).to_be_focused()
    before_tab_enter = len(login_requests)
    username.fill("admin")
    button(page, auth["submit"]).click()
    check(len(login_requests) == before_tab_enter,
          "clicking submit on an incomplete form sent a request")
    page.keyboard.press("Enter")
    check(len(login_requests) == before_tab_enter,
          "Enter in the username field submitted an incomplete form")
    expect(page.get_by_text(auth["passwordRequired"], exact=True)).to_be_visible()
    username.fill("")
    for target in (username, secret, button(page, auth["submit"])):
        box = target.bounding_box()
        check(box is not None and box["width"] > 0 and box["height"] > 0,
              f"a control is not rendered at 1440x900: {box}")
        check(box["y"] >= 0 and box["y"] + box["height"] <= 900,
              f"a control falls outside the 1440x900 viewport: {box}")
    user_box, secret_box, submit_box = (username.bounding_box(), secret.bounding_box(),
                                        button(page, auth["submit"]).bounding_box())
    check(user_box["y"] + user_box["height"] <= secret_box["y"] + 1,
          "the two fields overlap")
    check(secret_box["y"] + secret_box["height"] <= submit_box["y"] + 1,
          "the submit button overlaps the field above it")
    report["login_keyboard_and_layout"] = {"tab_order": ["username", "password"],
        "enter_incomplete_no_post": True, "viewport": "1440x900",
        "controls_visible": True, "overlap": False,
        "boxes": {"username_y": round(user_box["y"]), "password_y": round(secret_box["y"]),
                  "submit_y": round(submit_box["y"])}}

    # 1.8: a whitespace-only value is not a value. Both fields carry whitespace: true, so spaces
    # alone must be refused in the form and must not reach the server, while a real value in the
    # other field is kept.
    before_blank = len(login_requests)
    username.fill("   ")
    secret.fill("   ")
    button(page, auth["submit"]).click()
    expect(page.get_by_text(auth["usernameRequired"], exact=True)).to_be_visible()
    expect(page.get_by_text(auth["passwordRequired"], exact=True)).to_be_visible()
    expect(username).to_have_value("   ")
    expect(secret).to_have_value("   ")
    check(len(login_requests) == before_blank, "a whitespace-only login submitted a request")
    report["login_whitespace_only"] = {"refused": True, "requests": 0, "value_kept": True}

    invalid = "e2e-invalid-" + report["run_id"]
    artifacts.remember(invalid)
    username.fill("admin")
    secret.fill(invalid)
    with page.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as rejected:
        secret.press("Enter")
    check(rejected.value.status == 401 and rejected.value.json() ==
          {"code": "INVALID_CREDENTIALS", "error": "invalid username or password"}, "invalid login not rejected precisely")
    check(rejected.value.request.post_data_json == {"username": "admin", "password": invalid}, "incorrect invalid-login request")
    expect(page.get_by_role("alert")).to_have_text(auth["invalidCredentials"])
    # requiredMark={false} hides the asterisk. The class lives on the form.
    login_form = page.locator("form.ant-form")
    expect(login_form).to_have_count(1, timeout=12000)
    expect(login_form).to_have_class(re.compile(r"\bant-form-hide-required-mark\b"), timeout=12000)
    for label in (auth["username"], auth["password"]):
        marked = page.locator("label").filter(has_text=re.compile("^" + re.escape(label) + "$"))
        expect(marked).to_have_count(1, timeout=12000)
        expect(marked).to_have_class(re.compile(r"\bant-form-item-required-mark-hidden\b"), timeout=12000)
    expect(username).to_have_value("admin")
    expect(secret).to_have_value(invalid)
    expect(page.get_by_role("heading", name=auth["title"], exact=True)).to_be_visible()
    expect(page).to_have_url(server.base + "/networks")
    expect(page.get_by_role("dialog")).to_have_count(0)
    check(not any(c["name"] == COOKIE for c in context.cookies()), "failed login created a cookie")
    check(http(server.base, "GET", ME) == (401, DENIED), "failed login made anonymous client authorized")
    report["login_hide_required_mark"] = True
    report["evidence"].append({"layer": "ui", "empty_required_no_post": True, "secret_masked": True,
        "wrong_credential": "401 INVALID_CREDENTIALS; field retained; actionable translated alert"})

    # 正确登录后，两标签刷新保留深链；独立客户端证明原 Cookie/Bearer 均可用。
    # 提交挂起时再点一次并按 Enter，仍只有这一次登录。
    secret.fill("gpuworks-test-password-2026")
    expect(page.get_by_role("alert")).to_have_count(0)
    held_logins = []

    def hold_login(route):
        if route.request.method == "POST" and urlsplit(route.request.url).path == LOGIN:
            held_logins.append(route)
            return
        route.continue_()

    page.route("**/api/v1/auth/login", hold_login)
    submit = page.locator("button.auth-submit")
    expect(submit).to_have_count(1, timeout=12000)
    submit.click()
    for _ in range(40):
        if held_logins:
            break
        page.wait_for_timeout(50)
    check(len(held_logins) == 1, "sign-in did not stay in flight")
    # The duplicate attempts are checked before the visual busy state, so the case fails on the
    # contract - one sign-in, one request - rather than on the class that happens to enforce it.
    submit.click(timeout=12000)
    secret.press("Enter")
    page.wait_for_timeout(500)
    check(len(held_logins) == 1, "duplicate login while the first request is held")
    expect(submit).to_have_class(re.compile(r"\bant-btn-loading\b"), timeout=12000)
    try:
        with page.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as accepted:
            held_logins[0].continue_()
        check(accepted.value.request.post_data_json == {"username": "admin", "password": "gpuworks-test-password-2026"}
              and not urlsplit(accepted.value.url).query, "incorrect successful login request")
    finally:
        page.unroute("**/api/v1/auth/login", hold_login)
    expect(page.get_by_role("heading", name=networks["title"], exact=True)).to_be_visible()
    expect(page.get_by_label(auth["password"], exact=True)).to_have_count(0)
    old_cookie, old_bearer, identity = capture_credentials(accepted.value, context, artifacts, auth_body(page, LOGIN))
    check(cookie_identity(play, server.base, old_cookie) == (200, identity), "original cookie is not usable before logout")
    check(http(server.base, "GET", ME, old_bearer) == (200, identity), "original login bearer is not usable before logout")
    report["login_single_submit"] = True
    report["evidence"].append({"layer": "request", "path": LOGIN, "method": "POST", "status": 200,
        "body": {"username": "admin", "password": "[REDACTED]"}, "identity": identity,
        "cookie": "opaque, HttpOnly, SameSite=Lax, Path=/", "cache_control": "no-store"})
    artifacts.start_trace(context)
    from session_persistence import exercise_session_persistence
    peer, report["session_persistence"] = exercise_session_persistence(
        page, context, play, server, identity, networks, settings, auth, observe, old_cookie, old_bearer)
    report["evidence"].append({"layer": "persistence", "paths_after_reload": ["/networks", "/settings"],
        "original_credentials_before_logout": {"cookie": 200, "login_bearer": 200}, "identity": identity,
        "reentered_page_matches_independent_get": True})

    # UI 注销撤销原会话；第二标签要求重登，原 Cookie/Bearer 必须真实返回 401。
    with page.expect_response(lambda r: urlsplit(r.url).path == LOGOUT and r.request.method == "POST") as logged_out:
        button(page, common["header"]["logout"]).click()
    response = logged_out.value
    check(response.status == 200 and auth_body(page, LOGOUT) == {"status": "logged_out"}
          and response.request.post_data is None and not urlsplit(response.url).query, "logout request/response differs")
    report["evidence"].append({"layer": "request", "method": "POST", "path": LOGOUT,
        "body": None, "query": "", "status": 200, "response": {"status": "logged_out"}})
    expect(page.get_by_role("heading", name=auth["title"], exact=True)).to_be_visible()
    dialog = peer.get_by_role("dialog", name=auth["sessionExpiredTitle"], exact=True)
    expect(dialog).to_be_visible()
    expect(dialog).to_be_in_viewport(ratio=1)
    expect(dialog.get_by_text(auth["sessionExpiredDescription"], exact=True)).to_be_visible()
    expect(page).to_have_url(server.base + "/networks")
    expect(peer).to_have_url(server.base + "/settings")
    check(not any(c["name"] == COOKIE for c in context.cookies()), "logout left browser cookie alive")
    check(cookie_identity(play, server.base, old_cookie) == (401, DENIED), "captured cookie survived logout")
    check(http(server.base, "GET", ME, old_bearer) == (401, DENIED), "captured bearer survived logout")
    check(http(server.base, "GET", ME) == (401, DENIED), "anonymous transport was contaminated")
    report["evidence"].append({"layer": "runtime", "after": "UI logout", "method": "GET", "path": ME,
        "captured_cookie_status": 401, "captured_bearer_status": 401, "anonymous_status": 401, "error": DENIED,
        "peer": "named reauthentication dialog in viewport; both deep links retained"})
    artifacts.stop_trace(context)

    # 第二标签重登后双标签恢复，原凭据仍被撤销；登录及返回密钥始终不录 trace。
    dialog.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    dialog.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with peer.expect_response(lambda r: urlsplit(r.url).path == LOGIN and r.request.method == "POST") as restored:
        dialog.get_by_label(auth["password"], exact=True).press("Enter")
    check(restored.value.request.post_data_json == {"username": "admin", "password": "gpuworks-test-password-2026"}, "re-login request differs")
    expect(dialog).to_have_count(0)
    new_cookie, new_bearer, new_identity = capture_credentials(restored.value, context, artifacts, auth_body(peer, LOGIN))
    check(new_identity == identity and new_cookie != old_cookie and new_bearer != old_bearer, "re-login did not replace the revoked session")
    expect(page.get_by_role("heading", name=networks["title"], exact=True)).to_be_visible()
    expect(peer.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    artifacts.start_trace(context)
    for tab, path, title in ((page, "/networks", networks["title"]), (peer, "/settings", settings["page"]["title"])):
        tab.reload()
        expect(tab).to_have_url(server.base + path)
        expect(tab.get_by_role("heading", name=title, exact=True)).to_be_visible()
        check(tab.evaluate("document.documentElement.scrollWidth <= innerWidth"), "restored deep link overflows")
        read = tab.request.get(server.base + ME)
        check(read.status == 200 and read.json() == identity, "restored browser identity differs")
    check(cookie_identity(play, server.base, new_cookie) == (200, identity), "replacement cookie failed")
    check(http(server.base, "GET", ME, new_bearer) == (200, identity), "replacement bearer failed")
    check(cookie_identity(play, server.base, old_cookie) == (401, DENIED), "re-login resurrected old cookie")
    check(http(server.base, "GET", ME, old_bearer) == (401, DENIED), "re-login resurrected old bearer")
    report["evidence"].append({"layer": "persistence", "after": "UI re-login and both tabs reload",
        "replacement_cookie_status": 200, "replacement_bearer_status": 200, "original_cookie_status": 401,
        "original_bearer_status": 401, "paths": ["/networks", "/settings"], "identity": identity})
    # 22.1: with a live session, walk the sidebar the page actually renders and check
    # each entry's deep link, heading, highlighted entry and reload position.
    from session_persistence import exercise_navigation_entries
    report["navigation"] = exercise_navigation_entries(page, server, report)
    page.screenshot(path=artifacts.directory / "recovered-networks.png")
    with peer.expect_response(lambda r: urlsplit(r.url).path == LOGOUT) as cleanup:
        button(peer, common["header"]["logout"]).click()
    check(cleanup.value.status == 200 and auth_body(peer, LOGOUT) == {"status": "logged_out"}, "cleanup logout failed")
    check(cookie_identity(play, server.base, new_cookie) == (401, DENIED), "cleanup cookie survived")
    check(http(server.base, "GET", ME, new_bearer) == (401, DENIED), "cleanup bearer survived")
    check(len(login_requests) == 3 and logout_requests == [{"method": "POST", "path": LOGOUT, "body": None}] * 2,
          "unexpected duplicate login/logout request")
    check(not errors and not http_errors and not console_errors, "unexpected browser errors; see report")
    report["browser_errors"] = {"page": errors, "http": http_errors, "console": console_errors}
    report["evidence"].append({"layer": "cleanup", "replacement_session_revoked": True,
        "login_requests": login_requests, "logout_requests": logout_requests})


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
    manifest_path = ROOT / "test/e2e/coverage/session-logout.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT and manifest["test_function"] == test_two_tab_logout_relogin.__name__, "manifest binding drift")
    report = {"contract": CONTRACT, "case": manifest["name"], "status": "NOT_RUN", "run_id": run_id,
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin browser; independent original cookie and login bearer clients",
        "locale": args.locale, "viewport": {"width": 1440, "height": 900}, "timezone": "UTC",
        "selection": [CONTRACT], "retries": 0, "evidence": [], "test_sha256": digest(Path(__file__)),
        "persistence_sha256": digest(ROOT / "test/e2e/session_persistence.py"),
        "login_inventory_sha256": digest(ROOT / "test/e2e/login_inventory.py"),
        "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/platform_token_read_revoke.py"),
        "coverage_manifest_sha256": digest(manifest_path),
        "command": ["python3", "test/e2e/session_logout.py", *sys.argv[1:]]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"], report["owned_server_pid"] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            context.set_default_timeout(10000)
            report["browser_version"] = browser.version
            try:
                test_two_tab_logout_relogin(play, context, server, artifacts, report, args.locale)
                check(set(manifest["required_evidence"]) <= {e["layer"] for e in report["evidence"]}, "missing required evidence")
                report["status"] = "VERIFYING_ARTIFACTS"
            finally:
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
        report["private_state"] = "isolated DB/logs retained; executing owner removes only this run directory within 7 days"
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
            if report["status"] == "VERIFYING_ARTIFACTS":
                report["status"] = "PASS"
        except Exception:
            report["status"], report["artifact_error"] = "FAIL_TEST", "credential scan rejected artifacts"
        artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
