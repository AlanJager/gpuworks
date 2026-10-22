"""Targeted node removal acceptance against an isolated real server.

Standard: test/e2e/browser-e2e-testing-standard.md
Run after building the UI and server:
  python3 test/e2e/node_removal.py --server-bin ./bin/gpuworks --locale en
Requires Playwright and Chromium; --browser-path can select an existing binary.
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

CONTRACT = "gpuworks/node/removal-with-explicit-force"


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def button(scope, label):
    return scope.get_by_role("button", name=re.compile(r"^" + r"\s*".join(map(re.escape, label)) + r"$"))


def test_node_removal(page, context, server, artifacts, report, locale):
    # Mainline: cancel default removal -> normal removal fails without SSH and
    # retains the node -> explicitly force removal -> GET and refresh show gone.
    locales = ROOT / "ui/src/i18n/locales" / locale
    k = json.loads((locales / "nodes.json").read_text())
    auth = json.loads((locales / "auth.json").read_text())
    page_errors, deletes = [], []
    page.on("pageerror", lambda error: page_errors.append(str(error)))
    page.on("request", lambda request: deletes.append(request.url)
            if request.method == "DELETE" and "/api/v1/nodes/" in request.url else None)
    page.goto(server.base + "/nodes")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=k["page"]["title"], exact=True)).to_be_visible()
    report["preflight"] = "PASS"
    name = "e2e-" + report["run_id"]
    created = page.request.post(server.base + "/api/v1/nodes", data={"name": name, "server_url": server.base})
    check(created.status == 201, "fixture node creation failed")
    data = created.json()
    artifacts.remember(data["token"])
    path = "/api/v1/nodes/" + data["node"]["uuid"]
    report["node_uuid"] = data["node"]["uuid"]
    artifacts.start_trace(context)
    page.reload()
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)

    def open_removal():
        button(row, k["action"]["more"]).click()
        page.get_by_role("menuitem").filter(has=page.get_by_text(k["action"]["delete"], exact=True)).click()
        dialog = page.get_by_role("dialog", name=k["delete"]["confirm"], exact=True)
        expect(dialog).to_be_visible()
        expect(dialog.get_by_text(name, exact=True)).to_be_visible()
        return dialog

    dialog = open_removal()
    force = dialog.get_by_role("checkbox", name=k["delete"]["force"], exact=True)
    expect(force).not_to_be_checked()
    force.focus()
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    check(deletes == [], "cancellation issued DELETE")
    check(page.request.get(server.base + path).status == 200, "cancellation deleted node")

    dialog = open_removal()
    submit = button(dialog, k["action"]["delete"])
    for locator in [dialog, submit]:
        box = locator.bounding_box()
        check(box and box["x"] >= 0 and box["y"] >= 0 and box["x"] + box["width"] <= 1440
              and box["y"] + box["height"] <= 900, "confirmation outside viewport")
    force = dialog.get_by_role("checkbox", name=k["delete"]["force"], exact=True)
    expect(force).not_to_be_checked()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as normal:
        submit.click()
    check(normal.value.status == 409, "missing SSH should reject normal removal")
    check(urlsplit(normal.value.url).query == "" and normal.value.request.post_data is None,
          "default removal must omit force and body")
    check("agent uninstall was not confirmed" in normal.value.text(), "failure reason missing")
    expect(dialog).to_be_visible()
    expect(force).not_to_be_checked()
    check(page.request.get(server.base + path).status == 200, "failed removal lost node")
    force.focus()
    for enabled in [True, False, True]:
        force.press("Space")
        expect(force).to_be_checked(checked=enabled)
        if enabled:
            expect(dialog.get_by_role("alert")).to_have_text(k["delete"]["forceWarning"])
        else:
            expect(dialog.get_by_role("alert")).to_have_count(0)
    page.screenshot(path=artifacts.directory / "force-confirmation.png")
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as forced:
        submit.click()
    check(forced.value.status == 200, "force removal failed")
    check(urlsplit(forced.value.url).query == "force=true" and forced.value.request.post_data is None,
          "force removal query/body differ from contract")
    forced_body = forced.value.json()
    check(forced_body == {"status": "deleted", "agent_uninstalled": False}, "incorrect force response")
    expect(dialog).not_to_be_visible()
    check(page.request.get(server.base + path).status == 404, "force removal did not persist")
    page.reload()
    expect(row).to_have_count(0)
    check(len(deletes) == 2 and not page_errors, f"unexpected browser behavior: {deletes}, {page_errors}")
    report["evidence"] = [
        {"layer": "ui", "checks": ["named modal in viewport", "default force=false", "Esc sends no DELETE",
            "failure preserves modal", "keyboard off/on/off/on", "force warning"]},
        {"layer": "request", "method": "DELETE", "path": path, "normal": {"query": "", "status": 409},
            "forced": {"query": "force=true", "status": 200, "body": forced_body}},
        {"layer": "persistence", "after_cancel": 200, "after_failure": 200, "after_force": 404, "reload_row_count": 0},
        {"layer": "cleanup", "node": "removed by tested UI action"}]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, required=True)
    parser.add_argument("--browser-path")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest = ROOT / "test/e2e/coverage/node-removal.json"
    check(json.loads(manifest.read_text())["contract_id"] == CONTRACT, "wrong coverage binding")
    report = {"contract": CONTRACT, "run_id": run_id, "status": "NOT_RUN", "retries": 0,
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "test_sha256": digest(Path(__file__)), "fixture_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "manifest_sha256": digest(manifest), "lane": "local-real-server-sqlite", "locale": args.locale,
        "identity": "admin", "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "command": ["python3", "test/e2e/node_removal.py", *sys.argv[1:]],
        "execution": "N/A: force removes management records only; pending fixture has no agent or workload",
        "not_covered": ["normal SSH success (separate Go process/container cases)", "real macOS/systemd host",
            "full modal focus/close matrix", "browser loading barrier (RTL covers duplicate submit)"]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(headless=True, executable_path=args.browser_path)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_node_removal(page, context, server, artifacts, report, args.locale)
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = traceback.format_exc()
                report["dialogs"] = page.get_by_role("dialog").aria_snapshot() if page.get_by_role("dialog").count() == 1 else "no unique dialog"
                page.screenshot(path=artifacts.directory / "failure.png")
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
        report["private_state"] = "isolated DB/logs retained in private/ for diagnosis; owned server stopped"
        artifacts.write_json("report.json", report)
        artifacts.scan()
    print(json.dumps({"status": report["status"], "report": str(artifacts.directory / "report.json")}))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
