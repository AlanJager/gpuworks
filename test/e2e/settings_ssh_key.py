#!/usr/bin/env python3
"""Settings / platform SSH key rotation and import through the real browser.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-settings-ssh-key
After build: python3 test/e2e/settings_ssh_key.py --locale en
Requires Python Playwright, system Chrome and ssh-keygen. No agent, GPU or
container is needed: the contract ends at the server's own key pair.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and retains the
private database/logs for scoped diagnosis (seven days). The generated private
key stays in a temporary directory, is registered for artifact redaction, and
is removed before the case returns.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/settings/platform-ssh-key-rotate-import"
CASE = "设置 / 平台 SSH 密钥 / 重新生成、拒绝损坏私钥、导入既有私钥并刷新回读"
KEY_PATH = "/api/v1/ssh-key"
ROTATE_PATH = "/api/v1/ssh-key/rotate"


def strings(locale):
    ui = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return ui, auth


def generate_key(directory: Path, comment: str):
    key = directory / "id_ed25519"
    subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment,
                    "-f", str(key)], check=True)
    public = Path(str(key) + ".pub").read_text().split()
    fingerprint = subprocess.check_output(
        ["ssh-keygen", "-lf", str(key) + ".pub"], text=True).split()[1]
    return key.read_text(), {"type": public[0], "blob": public[1], "fingerprint": fingerprint}


# Contract: gpuworks/settings/platform-ssh-key-rotate-import
# Mainline: 独立 server/SQLite → 设置页读取平台公钥与指纹 → 重新生成并确认后指纹变化
#           → 刷新/独立 GET 回读 → 导入损坏私钥被拒且原密钥不变
#           → 导入本次临时生成的私钥 → 指纹与公钥部分匹配、刷新回读 → 清理临时私钥
def test_platform_ssh_key(page, context, server, artifacts, report, locale,
                          private_pem, public, inject_echo_failure=False):
    ui, auth = strings(locale)
    ssh = ui["sshKey"]
    page_errors, console_errors, bad_responses, mutations = [], [], [], []
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
        path = urlsplit(request.url).path
        if path in {KEY_PATH, ROTATE_PATH} and request.method in {"POST", "PUT"}:
            mutations.append({"method": request.method, "path": path})

    page.on("console", console_seen)
    page.on("response", response_seen)
    page.on("request", request_seen)

    page.goto(server.base + "/settings")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=ui["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    nodes_tab = page.get_by_role("tab", name=ui["page"]["tabs"]["nodes"], exact=True)
    nodes_tab.click()
    expect(nodes_tab).to_have_attribute("aria-selected", "true")
    report["last_step"] = "nodes-tab-selected"

    initial_response = page.request.get(server.base + KEY_PATH)
    check(initial_response.status == 200, f"SSH key GET HTTP {initial_response.status}")
    initial = initial_response.json()
    check(initial["fingerprint"] and initial["public_key"], "initial SSH key is incomplete")
    check(initial.get("authorize_command")
          and initial["public_key"] in initial["authorize_command"],
          "the authorize command does not embed the current public key")
    card = page.locator(".ant-card").filter(has_text=ssh["title"]).first
    expect(card.get_by_text(initial["fingerprint"], exact=True)).to_be_visible()
    expect(card.get_by_text(initial["authorize_command"], exact=True)).to_be_visible()

    button(card, ssh["rotate"]).click()
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_be_visible(timeout=5000)
    expect(popconfirm).to_contain_text(ssh["rotateConfirm"])
    expect(popconfirm).to_contain_text(ssh["rotateWarning"])
    popconfirm.locator(".ant-btn-primary").click()
    expect(page.get_by_text(ssh["rotated"], exact=True).first).to_be_visible()
    rotated_response = page.request.get(server.base + KEY_PATH)
    check(rotated_response.status == 200, f"rotated GET HTTP {rotated_response.status}")
    rotated = rotated_response.json()
    check(rotated["fingerprint"] != initial["fingerprint"], "rotation kept the same fingerprint")
    check(rotated["public_key"] != initial["public_key"], "rotation kept the same public key")
    check(rotated["authorize_command"] != initial["authorize_command"],
          "rotation kept the same authorize command")
    page.reload()
    expect(page.get_by_role("tab", name=ui["page"]["tabs"]["nodes"], exact=True)
           ).to_have_attribute("aria-selected", "true")
    card = page.locator(".ant-card").filter(has_text=ssh["title"]).first
    expect(card.get_by_text(rotated["fingerprint"], exact=True)).to_be_visible()
    expect(card.get_by_text(rotated["authorize_command"], exact=True)).to_be_visible()
    report["last_step"] = "rotation-persisted"

    report["sensitive_phase"] = "import dialog accepts a private key"
    button(card, ssh["import"]).click()
    modal = page.get_by_role("dialog")
    expect(modal).to_be_visible()
    modal.get_by_label(ssh["privateKey"], exact=True).fill("not a private key")
    button(modal, ssh["import"]).click()
    expect(page.get_by_text(re.compile(re.escape(ssh["importFailed"].split("{{")[0].strip()))).first).to_be_visible()
    check(modal.is_visible(), "a rejected import closed the dialog")
    unchanged = page.request.get(server.base + KEY_PATH)
    check(unchanged.json()["fingerprint"] == rotated["fingerprint"],
          "a rejected import changed the platform key")
    button(modal, ui["common"]["cancel"]).click()
    expect(modal).not_to_be_visible()
    report["last_step"] = "invalid-import-rejected"

    button(card, ssh["import"]).click()
    modal = page.get_by_role("dialog")
    expect(modal).to_be_visible()
    report["sensitive_phase"] = "import dialog accepts a private key"
    try:
        modal.get_by_label(ssh["privateKey"], exact=True).fill(private_pem)
        button(modal, ssh["import"]).click()
        expect(page.get_by_text(ssh["imported"], exact=True).first).to_be_visible()
        expect(modal).not_to_be_visible()
    except Exception:
        # Playwright timeouts can embed the fill value. Do not chain the
        # original exception into the report's traceback.
        raise AssertionError("the private-key import step failed") from None
    report["sensitive_phase"] = "import committed; no-echo checks pending"
    report["last_step"] = "valid-import-committed"

    imported_response = page.request.get(server.base + KEY_PATH)
    check(imported_response.status == 200, f"imported GET HTTP {imported_response.status}")
    imported = imported_response.json()
    check(imported["fingerprint"] == public["fingerprint"],
          f"imported fingerprint differs: {imported['fingerprint']}")
    imported_parts = imported["public_key"].split()
    check(imported_parts[0] == public["type"] and imported_parts[1] == public["blob"],
          "imported public key does not match the generated private key")
    check(not any("private" in key.lower() for key in imported),
          f"the SSH key response exposes private fields: {sorted(imported)}")
    check(imported.get("authorize_command")
          and imported["public_key"] in imported["authorize_command"],
          "the imported authorize command does not embed the new public key")
    check("PRIVATE KEY" not in imported_response.text(),
          "the SSH key response contains private-key material")
    check("PRIVATE KEY" not in page.locator("body").inner_text(),
          "the settings page renders private-key material")

    # Reopening the import dialog must not echo the private key that was just
    # submitted; this is the observable half of "once displayed, never again".
    button(card, ssh["import"]).click()
    reopen = page.get_by_role("dialog")
    expect(reopen).to_be_visible()
    echoed = reopen.get_by_label(ssh["privateKey"], exact=True).input_value()
    if inject_echo_failure:
        # Reproducible artifact-safety regression: fail here while the secret
        # phase is active and prove no artifact captures the key.
        echoed = "injected echo"
    check(echoed == "", "the import dialog echoed the last private key")
    button(reopen, ui["common"]["cancel"]).click()
    expect(reopen).not_to_be_visible()
    report.pop("sensitive_phase", None)

    page.reload()
    expect(page.get_by_role("tab", name=ui["page"]["tabs"]["nodes"], exact=True)
           ).to_have_attribute("aria-selected", "true")
    card = page.locator(".ant-card").filter(has_text=ssh["title"]).first
    expect(card.get_by_text(public["fingerprint"], exact=True)).to_be_visible()
    expect(card.get_by_text(imported["authorize_command"], exact=True)).to_be_visible()
    report["last_step"] = "import-persisted"

    check(mutations == [{"method": "POST", "path": ROTATE_PATH},
                        {"method": "PUT", "path": KEY_PATH},
                        {"method": "PUT", "path": KEY_PATH}],
          f"unexpected key mutations: {mutations}")
    expected_reject = {"method": "PUT", "path": KEY_PATH, "status": 400}
    unexpected = [item for item in bad_responses
                  if not (item["method"] == expected_reject["method"]
                          and item["path"] == expected_reject["path"]
                          and item["status"] == expected_reject["status"])]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    # The rejected import is the one expected 400; Chrome logs one matching
    # resource error. Any other console error still fails the case.
    allowed_console = [item for item in console_errors
                       if "400" in item and ("load resource" in item or "加载资源" in item)]
    other_console = [item for item in console_errors if item not in allowed_console]
    check(len(allowed_console) == 1 and not other_console,
          f"browser errors: {page_errors}, {console_errors}")
    check(not page_errors, "unhandled browser errors: " + str(page_errors))
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "settings page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/settings", "tab": ui["page"]["tabs"]["nodes"],
         "card": ssh["title"],
         "initial_fingerprint": initial["fingerprint"],
         "rotated_fingerprint": rotated["fingerprint"],
         "invalid_import_left_dialog_open": True,
         "imported_fingerprint": imported["fingerprint"],
         "private_key_not_echoed": True},
        {"layer": "request", "mutations": mutations,
         "invalid_import": {"method": "PUT", "path": KEY_PATH, "status": 400}},
        {"layer": "persistence", "rotate": {"fingerprint_changed": True,
         "refresh": rotated["fingerprint"]},
         "import": {"fingerprint": imported["fingerprint"],
                    "public_key_matches_generated": True,
                    "private_fields_absent": True,
                    "refresh": public["fingerprint"]}},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "console_allowed": allowed_console,
                                "http": bad_responses, "mutations": mutations}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    # Self-test for the PASS aggregation: a teardown exception must keep the
    # run non-PASS instead of being overwritten from case_completed.
    parser.add_argument("--inject-close-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-echo-failure", action="store_true",
                        help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/settings-ssh-key.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_platform_ssh_key.__name__,
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
        "execution": "N/A: the platform key pair is a control-plane artifact; no agent or workload is involved",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["authorizing the key on a real sshd and using it for node operations",
                        "an encrypted private key rejection",
                        "non-admin authorization",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/settings_ssh_key.py", *sys.argv[1:]]}
    work = Path(tempfile.mkdtemp(prefix="gpuworks-e2e-ssh-"))
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        private_pem, public = generate_key(work, f"e2e-{run_id}")
        artifacts.remember(private_pem)
        report["public_key_fingerprint"] = public["fingerprint"]
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
                test_platform_ssh_key(page, context, server, artifacts, report,
                                      args.locale, private_pem, public,
                                      inject_echo_failure=args.inject_echo_failure)
                report["case_completed"] = True
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                # This case handles a private key for its whole run; a
                # screenshot is never safe here.
                report["failure_screenshot"] = artifacts.capture_failure(
                    page, sensitive=True)
                artifacts.write_json("report.json", report)
            finally:
                artifacts.write_json("report.json", report)
                close_ok = True
                try:
                    if args.inject_close_failure:
                        raise RuntimeError("injected context close failure")
                    context.close()
                except Exception:
                    close_ok = False
                    report["context_close_error"] = artifacts.redact(
                        traceback.format_exc())
                try:
                    browser.close()
                except Exception:
                    close_ok = False
                    report["browser_close_error"] = artifacts.redact(
                        traceback.format_exc())
                report["browser_close_ok"] = close_ok
                if not close_ok:
                    report["status"] = "FAIL_TEST"
                    report.setdefault("failure",
                                      "the browser did not close cleanly")
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        cleanup_ok = True
        try:
            shutil.rmtree(work)
        except OSError as error:
            cleanup_ok = False
            report["cleanup_error"] = (
                f"private key directory removal failed: {error.__class__.__name__}")
        report["private_key_removed"] = not work.exists()
        cleanup_ok = cleanup_ok and report["private_key_removed"]
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        cleanup_ok = cleanup_ok and report["server_stopped"]
        browser_ok = report.get("browser_close_ok") is True
        cleanup_ok = cleanup_ok and browser_ok
        report["evidence"] = [item for item in report["evidence"]
                              if item.get("layer") != "cleanup"]
        report["evidence"].append({"layer": "cleanup",
            "private_key_removed": report["private_key_removed"],
            "server_stopped": report["server_stopped"],
            "browser_close_ok": browser_ok,
            "cleanup_ok": cleanup_ok})
        layers = {item["layer"] for item in report["evidence"]}
        if (report.get("case_completed") and cleanup_ok
                and report.get("status") not in ("FAIL_TEST", "BLOCKED_INFRA")
                and set(manifest["required_evidence"]) <= layers):
            report["status"] = "PASS"
        elif report["status"] not in ("FAIL_TEST", "BLOCKED_INFRA"):
            report["status"] = "FAIL_TEST"
            report["failure"] = (report.get("failure")
                                 or "cleanup or required evidence did not complete")
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        report["private_state"] = (
            "private/ retains isolated DB/logs; no process kept; remove only this run directory after diagnosis (7 days)"
            if report["private_key_removed"] else
            f"the private key directory remains at {work}; remove it after diagnosis")
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
