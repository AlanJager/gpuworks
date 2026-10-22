#!/usr/bin/env python3
"""MCP / copied endpoint or generic JSON / unchanged address serves real tools.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-mcp-copied-config
After build: python3 test/e2e/mcp_copied_config.py --case endpoint --locale en
Selections: endpoint / generic-json; locales: en / zh. Each invocation owns an
isolated real server, SQLite database and browser context; no GPU/agent needed.
Requires Playwright Python and Chrome. Artifacts: secrets/e2e/<runId>/report.json.
Copied values contain only the documented token placeholder. Fixture credentials
are created before tracing, substituted in memory and consumed by a cookie-free
HTTP client; they never enter the page, clipboard, command line or report.
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
import urllib.error
import urllib.request
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check, collection
from mcp_settings_lifecycle import STATUS, SETTINGS, TOKENS, capture_failure, cleanup_fixture, cleanup_stage, close_browser, copy_server_log, stop_server

# Mainline: 自有network与read令牌 → UI复制endpoint → 展示/剪贴板/origin+status.path逐字比较
# → UI开关及GET/刷新 → 原地址initialize与读取network → 关闭拒绝/重开恢复 → 撤销401与清理。
# Contract: gpuworks/settings/mcp-copied-endpoint
# Mainline: 自有network与read令牌 → 展开/折叠并复制generic JSON且折叠不变化 → JSON解析原URL/header
# → 仅替换密钥占位符 → UI开关与GET/刷新 → 原URL读取network → 撤销401与清理。
# Contract: gpuworks/settings/mcp-copied-generic-json
CASES = {"endpoint": "gpuworks/settings/mcp-copied-endpoint",
         "generic-json": "gpuworks/settings/mcp-copied-generic-json"}
NETWORKS = "/api/v1/networks"
PRODUCT_FILES = ("ui/src/pages/SettingsPage.tsx", "ui/src/pages/settings/MCPCard.tsx", "internal/server/api/mcp.go",
                 "internal/server/mcp/mcp.go", "internal/server/aihelper/apitools.go", "internal/server/aihelper/shaping.go")
HELPER_FILES = ("test/e2e/local_browser_fixture.py", "test/e2e/platform_token_read_revoke.py",
                "test/e2e/mcp_settings_lifecycle.py")


def copied_request(url, headers, body):
    # Consume the actual copied URL directly. No server.base/path reconstruction,
    # cookies, redirects, host rewriting or proxy can rescue a bad copied URL.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, response_headers, newurl):
            return None
    request = urllib.request.Request(url, method="POST", headers={"Content-Type": "application/json", **headers},
                                     data=json.dumps(body).encode())
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        response = opener.open(request, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        raw = response.read()
        return response.status, json.loads(raw) if raw else None


def toggle(page, switch, base, initial, enabled, evidence):
    switch.scroll_into_view_if_needed()
    expect(switch).to_be_in_viewport(ratio=1)
    with page.expect_response(lambda r: urlsplit(r.url).path == SETTINGS and r.request.method == "PUT") as saved:
        switch.click()
    response = saved.value
    expected = {**initial, "enabled": enabled}
    check(response.status == 200 and response.request.post_data_json == {"enabled": enabled}
          and not urlsplit(response.url).query and response.json() == expected, "MCP switch request/response drift")
    evidence.append({"layer": "request", "method": "PUT", "path": SETTINGS, "body": {"enabled": enabled}, "status": 200})
    independent = page.request.get(base + STATUS)
    check(independent.status == 200 and independent.json() == expected, "MCP switch not persisted")
    page.reload()
    expect(page).to_have_url(re.compile(r"/settings#access$"))
    expect(page.get_by_role("tab", selected=True)).to_have_count(1)
    expect(switch).to_be_enabled()
    expect(switch).to_be_checked(checked=enabled)
    evidence.append({"layer": "persistence", "status": expected, "independent_get": 200, "reload_checked": enabled})


# Mainline: 每个独立case以自有真实资源为基线 → 浏览器复制所选接入格式并保持URL不变
# → 持久化开关控制真实JSON-RPC读取 → 恢复默认、吊销token和删除fixture后扫描产物。
def test_mcp_copied_config(page, context, server, artifacts, report, locale, selection):
    texts = ROOT / "ui/src/i18n/locales" / locale
    auth = json.loads((texts / "auth.json").read_text())
    settings = json.loads((texts / "settings.json").read_text())
    m = settings["mcp"]
    evidence = report["evidence"]
    errors, console_errors, http_errors, mutations = [], [], [], []
    report["browser_errors"] = {"page": errors, "console": console_errors, "http": http_errors}
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(server.base + "/settings")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    report["last_step"] = "login-and-settings-heading"
    # The MCP card lives on the Security & access tab; entering through the real tab
    # also puts its key in the URL fragment, so every reload below reopens that tab.
    access = page.get_by_role("tab", name=settings["page"]["tabs"]["access"], exact=True)
    access.click()
    expect(access).to_have_attribute("aria-selected", "true")
    expect(page).to_have_url(re.compile(r"/settings#access$"))
    evidence.append({"layer": "ui", "tab": "access", "selected": True, "url": page.url})
    report["last_step"] = "access-tab-selected"
    report["preflight"] = "PASS"
    status = page.request.get(server.base + STATUS)
    check(status.status == 200, "MCP status failed")
    initial = status.json()
    check(set(initial) == {"enabled", "path", "protocol_version", "tools"} and initial["enabled"] is False
          and initial["path"] == "/api/v1/mcp" and "call_api" in initial["tools"], "fresh MCP contract drift")
    draft = {"name": f"e2e-{report['run_id']}-mcp-network", "description": "copied MCP URL proof",
             "roles": ["serving"], "type": "host_ethernet", "cidr": "10.77.0.0/16", "interface": "ens*"}
    created = page.request.post(server.base + NETWORKS, data=draft)
    check(created.status == 201, "network fixture create failed")
    network = created.json()
    network_id = network["id"]
    report["owned_resources"] = {"network_id": network_id}
    check(all(network[k] == v for k, v in draft.items()), "network fixture persistence fields differ")
    token_response = page.request.post(server.base + TOKENS, data={"name": f"e2e-{report['run_id']}-mcp", "scope": "read"})
    payload = token_response.json()
    if isinstance(payload.get("token"), str):
        artifacts.remember(payload["token"])
    if isinstance(payload.get("item"), dict) and "id" in payload["item"]:
        report["owned_resources"]["platform_token_id"] = payload["item"]["id"]
    check(token_response.status == 201 and isinstance(payload.get("token"), str) and bool(payload["token"])
          and "platform_token_id" in report["owned_resources"] and payload["item"].get("scope") == "read",
          "read-token fixture returned an incomplete credential")
    token, token_id = payload["token"], payload["item"]["id"]
    artifacts.start_trace(context)
    page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)
    page.on("response", lambda r: http_errors.append({"path": urlsplit(r.url).path, "status": r.status}) if r.status >= 400 else None)
    page.on("request", lambda r: mutations.append({"method": r.method, "path": urlsplit(r.url).path, "body": r.post_data_json})
            if urlsplit(r.url).path == SETTINGS and r.method != "GET" else None)
    switch = page.get_by_role("switch", name=m["switch"], exact=True)
    expect(switch).to_be_enabled()
    expect(switch).not_to_be_checked()
    origin = page.evaluate("window.location.origin")
    canonical = origin + initial["path"]
    endpoint_row = page.get_by_role("row").filter(has=page.get_by_text(m["endpoint"], exact=True))
    displayed_endpoint = endpoint_row.get_by_text(canonical, exact=True)
    expect(displayed_endpoint).to_be_visible()
    check(displayed_endpoint.inner_text() == canonical, "endpoint display differs from status and browser origin")

    sentinel = "e2e-" + report["run_id"]
    page.evaluate("value => navigator.clipboard.writeText(value)", sentinel)
    check(page.evaluate("navigator.clipboard.readText()") == sentinel, "clipboard fixture was not prepared")
    if selection == "endpoint":
        # Endpoint copy is scoped by the visible endpoint row, never by DOM depth or nth button.
        copy = endpoint_row.get_by_role("button")
        copy.scroll_into_view_if_needed()
        expect(copy).to_be_in_viewport(ratio=1)
        copy.focus()
        copy.press("Enter")
        page.wait_for_function("expected => navigator.clipboard.readText().then(value => value === expected)", arg=canonical)
        copied_url = page.evaluate("navigator.clipboard.readText()")
        check(copied_url == displayed_endpoint.inner_text() == canonical, "endpoint clipboard mismatch")
        copied_headers = {"Authorization": "Bearer <gwp_token>"}
        evidence.append({"layer": "ui", "control": "endpoint copy", "display": canonical, "clipboard": copied_url})
    else:
        # The header has its own named role; its nested copy button must not toggle it.
        header = page.get_by_role("button", name=re.compile(re.escape(m["otherClients"])))
        header.scroll_into_view_if_needed()
        expect(header).to_have_attribute("aria-expanded", "false")
        header.click()
        expect(header).to_have_attribute("aria-expanded", "true")
        expected_config = {"mcpServers": {"gpuworks": {"type": "http", "url": canonical,
                           "headers": {"Authorization": "Bearer <gwp_token>"}}}}
        displayed = page.get_by_text(re.compile(r'^\{\s*"mcpServers"'))
        expect(displayed).to_be_visible()
        visible_json = displayed.inner_text()
        check(json.loads(visible_json) == expected_config, "displayed generic JSON differs from canonical config")
        header.click()
        expect(header).to_have_attribute("aria-expanded", "false")
        expect(displayed).not_to_be_visible()
        copy = header.get_by_role("button")
        expect(copy).to_be_in_viewport(ratio=1)
        copy.focus()
        copy.press("Enter")
        page.wait_for_function("expected => navigator.clipboard.readText().then(value => value === expected)", arg=visible_json)
        copied_json = page.evaluate("navigator.clipboard.readText()")
        check(copied_json == visible_json, "generic JSON clipboard differs byte-for-byte from display")
        expect(header).to_have_attribute("aria-expanded", "false")
        expect(displayed).not_to_be_visible()
        parsed = json.loads(copied_json)
        check(parsed == expected_config, "copied JSON schema or values differ")
        config = parsed["mcpServers"]["gpuworks"]
        copied_url, copied_headers = config["url"], config["headers"]
        header.click()
        expect(header).to_have_attribute("aria-expanded", "true")
        expect(displayed).to_have_text(visible_json)
        expanded_sentinel = "e2e-expanded-" + report["run_id"]
        page.evaluate("value => navigator.clipboard.writeText(value)", expanded_sentinel)
        check(page.evaluate("navigator.clipboard.readText()") == expanded_sentinel, "expanded clipboard fixture was not prepared")
        copy.click()
        page.wait_for_function("expected => navigator.clipboard.readText().then(value => value === expected)", arg=visible_json)
        expect(header).to_have_attribute("aria-expanded", "true")
        check(page.evaluate("navigator.clipboard.readText()") == visible_json, "expanded copy differs")
        evidence.append({"layer": "ui", "control": "generic JSON", "display_and_clipboard": parsed,
                         "collapsed_copy_preserved": True, "expanded_again_unchanged": True})
    actual, browser_origin = urlsplit(copied_url), urlsplit(origin)
    check((actual.scheme, actual.hostname, actual.port, actual.path) ==
          (browser_origin.scheme, browser_origin.hostname, browser_origin.port, initial["path"])
          and not actual.query and not actual.fragment and not actual.username, "copied URL boundary differs")
    check(copied_headers == {"Authorization": "Bearer <gwp_token>"}, "unknown secret placeholder")
    headers = {key: value.replace("<gwp_token>", token) for key, value in copied_headers.items()}
    report["last_step"] = "copied-config-validated"
    # Only the declared credential placeholder is substituted, never the URL.
    toggle(page, switch, server.base, initial, True, evidence)
    init = {"jsonrpc": "2.0", "id": "initialize", "method": "initialize", "params": {
        "protocolVersion": initial["protocol_version"], "capabilities": {},
        "clientInfo": {"name": report["run_id"], "version": "1"}}}
    code, reply = copied_request(copied_url, headers, init)
    check(code == 200 and reply.get("jsonrpc") == "2.0" and reply.get("id") == "initialize"
          and "error" not in reply and reply["result"]["protocolVersion"] == initial["protocol_version"]
          and reply["result"]["serverInfo"]["name"] == "gpuworks" and "tools" in reply["result"]["capabilities"],
          "copied URL did not initialize the actual MCP server")
    evidence.append({"layer": "runtime", "url": copied_url, "request": init, "status": code, "response": reply})
    code, reply = copied_request(copied_url, headers, {"jsonrpc": "2.0", "method": "notifications/initialized"})
    check(code == 202 and reply is None, "MCP notification not accepted")
    canonical_response = page.request.get(server.base + NETWORKS)
    check(canonical_response.status == 200 and set(canonical_response.json()) == {"items", "roles", "types", "type_roles"}, "network list envelope differs")
    canonical_networks = canonical_response.json()["items"]
    owned = [n for n in canonical_networks if n["id"] == network_id]
    check(len(owned) == 1 and all(owned[0][k] == v for k, v in draft.items()), "known network absent from independent inventory")
    # MCP deliberately humanizes timestamps. Request the exact stable business fields
    # with its documented jq projection instead of comparing presentation ages.
    expected_network = [{key: owned[0][key] for key in ("id", *draft)}]
    request = {"jsonrpc": "2.0", "id": "network", "method": "tools/call", "params": {"name": "call_api",
               "arguments": {"path": NETWORKS, "jq": f'.items | map(select(.id == {network_id}) | {{id, name, description, roles, type, cidr, interface}})'}}}
    for phase in ("initial", "recovered"):
        code, reply = copied_request(copied_url, headers, request)
        check(code == 200 and reply.get("id") == request["id"] and reply.get("jsonrpc") == "2.0" and "error" not in reply,
              "copied URL did not return JSON-RPC response")
        result = reply["result"]
        check(result.get("isError") is False and len(result["content"]) == 1 and result["content"][0]["type"] == "text"
              and json.loads(result["content"][0]["text"]) == expected_network, "copied client did not read exact known network")
        evidence.append({"layer": "runtime", "phase": phase, "url": copied_url, "auth": "read bearer; no cookies",
                         "request": request, "status": code, "response": reply, "canonical_network": expected_network})
        if phase == "initial":
            toggle(page, switch, server.base, initial, False, evidence)
            code, denied = copied_request(copied_url, headers, request)
            check(code == 403 and denied == {"code": "MCP_DISABLED", "error": "the MCP service is turned off"}, "disabled copied client still reads")
            evidence.append({"layer": "runtime", "phase": "disabled", "status": code, "body": denied})
            toggle(page, switch, server.base, initial, True, evidence)
    deleted = page.request.delete(server.base + TOKENS + f"/{token_id}")
    check(deleted.status == 204 and all(t["id"] != token_id for t in collection(page.request.get(server.base + TOKENS))), "token revoke not persisted")
    del report["owned_resources"]["platform_token_id"]
    code, denied = copied_request(copied_url, headers, request)
    check(code == 401 and denied.get("code") == "AUTHENTICATION_REQUIRED", "revoked copied client remains authenticated")
    evidence.append({"layer": "runtime", "phase": "revoked", "status": code, "body": denied})
    toggle(page, switch, server.base, initial, False, evidence)
    link = page.get_by_role("link", name=m["createToken"], exact=True)
    check(link.get_attribute("href") == "/apikeys", "token link points elsewhere")
    link.click()
    expect(page).to_have_url(server.base + "/apikeys")
    api_strings = json.loads((texts / "apikeys.json").read_text())
    expect(page.get_by_role("heading", name=api_strings["title"], exact=True)).to_be_visible()
    page.go_back()
    # Back returns to the tab the user left, which the fragment records.
    expect(page).to_have_url(server.base + "/settings#access")
    expect(page.get_by_role("tab", name=settings["page"]["tabs"]["access"], exact=True)).to_have_attribute("aria-selected", "true")
    expect(switch).not_to_be_checked()
    expect(switch).to_be_enabled()
    evidence.append({"layer": "ui", "navigation": "/settings#access -> /apikeys -> browser back /settings#access", "restored_enabled": False})
    report["last_step"] = "browser-back-restored-access-tab"
    removed = page.request.delete(server.base + NETWORKS + f"/{network_id}")
    read_after_delete = page.request.get(server.base + NETWORKS)
    check(removed.status == 200 and removed.json() == {"status": "deleted"} and read_after_delete.status == 200
          and set(read_after_delete.json()) == {"items", "roles", "types", "type_roles"}
          and all(n["id"] != network_id for n in read_after_delete.json()["items"]), "network cleanup failed")
    del report["owned_resources"]["network_id"]
    check(mutations == [{"method": "PUT", "path": SETTINGS, "body": {"enabled": value}} for value in [True, False, True, False]], "unexpected setting mutation")
    check(not errors and not console_errors and not http_errors, f"unexpected browser errors: {report['browser_errors']!r}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "settings horizontal overflow")
    page.screenshot(path=artifacts.directory / "mcp-restored.png")
    report["browser_errors"] = {"page": errors, "console": console_errors, "http": http_errors}
    evidence.append({"layer": "cleanup", "enabled": False, "revoked_token_id": token_id, "deleted_network_id": network_id,
                     "independent_get_absence": True})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--case", choices=CASES, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / "test/e2e/coverage/mcp-copied-config.json"
    manifest = json.loads(manifest_path.read_text())
    selected = next(case for case in manifest["cases"] if case["selection"] == args.case)
    check(selected["contract_id"] == CASES[args.case] and selected["test_function"] == test_mcp_copied_config.__name__, "manifest binding drift")
    report = {"contract": CASES[args.case], "case": selected["name"], "status": "NOT_RUN", "run_id": run_id,
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "target_branch": "wei/master", "target_sha": subprocess.check_output(["git", "rev-parse", "wei/master"], cwd=ROOT, text=True).strip(),
        "lane": "local-real-server-sqlite", "identity": "admin browser / read bearer MCP client", "locale": args.locale,
        "viewport": {"width": 1440, "height": 900}, "timezone": "UTC", "selection": [CASES[args.case]], "retries": 0, "evidence": [],
        "test_sha256": digest(Path(__file__)), "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "helper_sha256": {name: digest(ROOT / name) for name in HELPER_FILES},
        "product_sha256": {name: digest(ROOT / name) for name in PRODUCT_FILES},
        "coverage_manifest_sha256": digest(manifest_path), "command": ["python3", "test/e2e/mcp_copied_config.py", *sys.argv[1:]]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"], report["owned_server_pid"] = server.base, server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US",
                                          timezone_id="UTC", permissions=["clipboard-read", "clipboard-write"])
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_mcp_copied_config(page, context, server, artifacts, report, args.locale, args.case)
                check(set(manifest["required_evidence"]) <= {e["layer"] for e in report["evidence"]}, "required evidence missing")
                check(report["test_sha256"] == digest(Path(__file__)) and report["coverage_manifest_sha256"] == digest(manifest_path), "test or manifest changed during run")
                check(report["helper_sha256"] == {name: digest(ROOT / name) for name in HELPER_FILES}, "helpers changed during run")
                check(report["product_sha256"] == {name: digest(ROOT / name) for name in PRODUCT_FILES}, "product source changed during run")
                check(report["binary_sha256"] == digest(args.server_bin), "binary changed during run")
                check(report["source_sha"] == subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(), "HEAD changed during run")
                report["business_complete"] = True
            except Exception:
                report["status"], report["failure"] = "FAIL_TEST", traceback.format_exc()
                capture_failure(page, artifacts, report)
            finally:
                cleanup_fixture(page, server, report)
                close_browser(report, artifacts, context, browser)
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        stop_server(report, server)
        cleanup_stage(report, "server-log", lambda: copy_server_log(artifacts))
        report["private_state"] = "owned DB/logs retained for diagnosis; remove only this run directory within 7 days"
        cleanup_stage(report, "report", lambda: artifacts.write_json("report.json", report))
        try:
            artifacts.scan()
            report["artifact_scan"] = "PASS"
            if report.get("business_complete") and report["server_stopped"] and report["status"] == "NOT_RUN":
                report["status"] = "PASS"
        except Exception:
            report["status"], report["artifact_scan"] = "FAIL_TEST", "FAIL"
        if not cleanup_stage(report, "report-final", lambda: artifacts.write_json("report.json", report)):
            cleanup_stage(report, "report-failure", lambda: artifacts.write_json("report.json", report))
    print(json.dumps({"status": report["status"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
