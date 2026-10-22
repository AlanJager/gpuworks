#!/usr/bin/env python3
"""Settings / branding through the real browser, server and sign-in page.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-settings-branding
After build: python3 test/e2e/settings_branding.py --locale en
Requires Python Playwright and system Chrome. No agent, GPU or container is
needed: branding is a control-plane setting rendered by the console and the
public sign-in page.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and retains the
private database/logs for scoped diagnosis (seven days).
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
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check
from settings_branding_fixture import watch_public_page

CONTRACT = "gpuworks/settings/branding"
CASE = "设置 / 品牌外观 / 文字与图标保存、刷新回读、登录页与浏览器标题生效"
BRANDING = "/api/v1/branding"


def strings(locale):
    settings = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    common = json.loads((ROOT / "ui/src/i18n/locales" / locale / "common.json").read_text())
    return settings, auth, common


def branding_doc(response, label):
    check(response.status == 200, f"{label} branding HTTP {response.status}")
    return response.json()


# Contract: gpuworks/settings/branding
# Mainline: 独立 server/SQLite → 登录页默认品牌 → 设置页拒绝非图片文件
#           → 裁切弹窗确认后接受 SVG → 保存产品名/标题/登录文案/图标
#           → 控制台标题、标签页与公开登录页生效 → 刷新/独立 GET 回读
#           → 恢复默认并再次核对
def test_branding(page, context, browser, server, artifacts, report, locale):
    settings, auth, common = strings(locale)
    branding = settings["branding"]
    default_console_title = common["branding"]["defaultConsoleTitle"].replace(
        "{{product}}", "GPUWorks")
    product_name = f"e2e-{report['run_id'][-8:]}-brand"
    console_title = f"e2e-{report['run_id']}-console"
    login_description = f"Signed in to {report['run_id']}\nSecond line"
    svg = (b'<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32">'
           b'<rect width="32" height="32" fill="#0076f7"/></svg>')
    logo = "data:image/svg+xml;base64," + base64.b64encode(svg).decode()
    page_errors, console_errors, bad_responses, patches = [], [], [], []
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
        if urlsplit(request.url).path == BRANDING and request.method == "PATCH":
            patches.append(request.post_data_json)

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/login")
    expect(page.get_by_text("GPUWorks", exact=True).first).to_be_visible()
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    page.goto(server.base + "/settings")
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    general = page.get_by_role("tab", name=settings["page"]["tabs"]["general"], exact=True)
    expect(general).to_have_attribute("aria-selected", "true")
    report["last_step"] = "settings-general"

    initial = branding_doc(page.request.get(server.base + BRANDING), "initial")
    check(initial == {"product_name": "", "console_title": "",
                      "login_description": "", "logo": ""},
          f"initial branding differs: {initial}")

    card = page.locator("#branding")
    expect(card.get_by_text(branding["title"], exact=True).first).to_be_visible()

    def field_input(label, selector="input"):
        # The AntD label is not associated with the control, so scope by the
        # form item that carries the visible label.
        item = card.locator(".ant-form-item").filter(has_text=label).first
        return item.locator(selector).first

    product_input = field_input(branding["productName"])
    title_input = field_input(branding["consoleTitle"])
    description_input = field_input(branding["loginDescription"], "textarea")
    expect(product_input).to_have_value("")
    expect(title_input).to_have_value("")
    expect(description_input).to_have_value("")

    upload = card.locator('input[type="file"]')
    upload.set_input_files({"name": "bad.txt", "mimeType": "text/plain",
                            "buffer": b"not an image"})
    expect(page.get_by_text(branding["logoError"]["notImage"], exact=True).first).to_be_visible()
    check(patches == [], "a rejected logo submitted branding")
    upload.set_input_files({"name": "logo.svg", "mimeType": "image/svg+xml", "buffer": svg})
    crop = page.get_by_role("dialog").last
    expect(crop.get_by_text(branding["crop"]["title"], exact=True)).to_be_visible()
    expect(crop.locator(".logo-crop-image")).to_be_visible()
    expect(crop.get_by_test_id("logo-crop-preview")).to_be_visible()
    confirm_crop = button(crop, branding["crop"]["confirm"])
    expect(confirm_crop).to_be_enabled(timeout=15000)
    confirm_crop.click()
    expect(crop).to_be_hidden()
    expect(card.locator("img.app-brand-logo")).to_have_attribute("src", logo)
    report["last_step"] = "logo-cropped-and-validated"

    product_input.fill(product_name)
    title_input.fill(console_title)
    description_input.fill(login_description)
    with page.expect_response(lambda r: urlsplit(r.url).path == BRANDING
                              and r.request.method == "PATCH") as saved:
        button(card, settings["common"]["save"]).click()
    check(saved.value.status == 200, f"branding save HTTP {saved.value.status}")
    expected_body = {"product_name": product_name, "console_title": console_title,
                     "login_description": login_description, "logo": logo}
    check(saved.value.request.post_data_json == expected_body,
          f"branding PATCH differs: {saved.value.request.post_data_json}")
    expect(page.get_by_text(branding["saved"], exact=True).first).to_be_visible()
    expect(page.locator(".app-header-title")).to_have_text(console_title)
    check(page.title() == product_name, f"document title is {page.title()!r}")
    saved_logo = page.locator("img.app-brand-logo").first
    expect(saved_logo).to_have_attribute("src", logo)
    expect(saved_logo).to_be_visible()
    expect(saved_logo).to_have_js_property("complete", True)
    expect(saved_logo).to_have_js_property("naturalWidth", 32)
    expect(saved_logo).to_have_js_property("naturalHeight", 32)
    report["last_step"] = "branding-saved"

    public_context = browser.new_context(
        viewport=report["viewport"],
        locale="zh-CN" if locale == "zh" else "en-US", timezone_id="UTC")
    public_errors = []
    try:
        public_page = public_context.new_page()
        public_errors = watch_public_page(public_page)
        public_page.goto(server.base + "/login")
        expect(public_page.get_by_text(product_name, exact=True).first).to_be_visible()
        expect(public_page.get_by_text(login_description, exact=True)).to_be_visible()
        public_logo = public_page.locator("img.app-brand-logo").first
        expect(public_logo).to_have_attribute("src", logo)
        expect(public_logo).to_be_visible()
        expect(public_logo).to_have_js_property("complete", True)
        expect(public_logo).to_have_js_property("naturalWidth", 32)
        expect(public_logo).to_have_js_property("naturalHeight", 32)
        check(public_page.title() == product_name,
              f"sign-in document title is {public_page.title()!r}")
    finally:
        public_context.close()
    report["last_step"] = "public-login-branded"

    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    card = page.locator("#branding")
    expect(field_input(branding["productName"])).to_have_value(product_name)
    expect(field_input(branding["consoleTitle"])).to_have_value(console_title)
    expect(field_input(branding["loginDescription"], "textarea")
           ).to_have_value(login_description)
    expect(card.locator("img.app-brand-logo")).to_have_attribute("src", logo)
    persisted = branding_doc(page.request.get(server.base + BRANDING), "persisted")
    check(persisted == expected_body, f"persisted branding differs: {persisted}")
    report["last_step"] = "branding-reloaded"

    button(card, branding["restoreDefault"]).click()
    field_input(branding["productName"]).fill("")
    field_input(branding["consoleTitle"]).fill("")
    field_input(branding["loginDescription"], "textarea").fill("")
    with page.expect_response(lambda r: urlsplit(r.url).path == BRANDING
                              and r.request.method == "PATCH") as cleared:
        button(card, settings["common"]["save"]).click()
    check(cleared.value.status == 200, f"branding clear HTTP {cleared.value.status}")
    check(cleared.value.request.post_data_json == {
        "product_name": "", "console_title": "", "login_description": "", "logo": ""},
        f"branding clear body differs: {cleared.value.request.post_data_json}")
    cleared_doc = branding_doc(page.request.get(server.base + BRANDING), "cleared")
    check(cleared_doc == {"product_name": "", "console_title": "",
                          "login_description": "", "logo": ""},
          f"branding was not restored: {cleared_doc}")
    expect(page.locator(".app-header-title")).to_have_text(default_console_title)
    check(page.title() == "GPUWorks", f"document title after restore is {page.title()!r}")
    expect(page.locator("img.app-brand-logo")).to_have_count(0)
    expect(page.locator(".app-brand-mark").first).to_have_text("G")
    report["last_step"] = "branding-restored"

    check(not public_errors, "public sign-in page errors: " + str(public_errors))
    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "settings page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/settings", "card": branding["title"],
         "logo_rejected": branding["logoError"]["notImage"],
         "logo_type": "image/svg+xml", "header_title": console_title,
         "document_title": product_name, "sign_in_description": "Second line",
         "restored": branding["restoreDefault"]},
        {"layer": "request", "patch_bodies": [
            {"product_name": item.get("product_name"),
             "console_title": item.get("console_title"),
             "login_description": item.get("login_description"),
             "logo_type": item.get("logo", "").split(";")[0]} for item in patches]},
        {"layer": "persistence", "initial": initial,
         "saved": {"product_name": persisted["product_name"],
                   "console_title": persisted["console_title"],
                   "login_description": persisted["login_description"],
                   "logo_matches": persisted["logo"] == logo},
         "restored": cleared_doc},
        {"layer": "cleanup", "restored": cleared_doc == {
            "product_name": "", "console_title": "",
            "login_description": "", "logo": ""}},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses, "public_page": public_errors}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/settings-branding.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_branding.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin / public sign-in page",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "branding_helper_sha256": digest(ROOT / "test/e2e/settings_branding_fixture.py"),
        "execution": "N/A: branding is rendered from the control-plane setting; no workload",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["maximum-length boundaries and whitespace-only text",
                        "PNG/JPEG/WebP compression and the tooLarge path",
                        "non-admin authorization",
                        "branding defaults in the other locale",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/settings_branding.py", *sys.argv[1:]]}
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
                test_branding(page, context, browser, server, artifacts, report, args.locale)
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
