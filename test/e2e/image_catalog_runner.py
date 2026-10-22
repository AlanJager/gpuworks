"""Local image-catalog fixture, login and artifact lifecycle.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Business assertions live in image_catalog.py; no management responses are mocked.
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
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check


def login(page, context, server, artifacts, report, locale):
    auth = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/auth.json").read_text())
    page.goto(server.base + "/images")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as result:
        button(page, auth["submit"]).click()
    check(result.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    page.wait_for_url(server.base + "/images")
    images = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/images.json").read_text())
    expect(page.get_by_label(auth["password"], exact=True)).to_have_count(0)
    expect(page.get_by_role("heading", name=images["page"]["title"], exact=True)).to_be_visible()
    report["preflight"] = "PASS"
    artifacts.start_trace(context)


def run(cases, *, entry_path=None, product_sources=()):
    parser = argparse.ArgumentParser(description="Real local image catalog browser contracts")
    parser.add_argument("--case", choices=list(cases), required=True)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / f"test/e2e/coverage/image-catalog-{args.case}.json"
    manifest = json.loads(manifest_path.read_text())
    case = cases[args.case]
    check(manifest["test_function"] == case.__name__, "manifest case binding differs")
    entry_path = Path(entry_path).resolve() if entry_path else ROOT / "test/e2e/image_catalog.py"
    entry_relative = str(entry_path.relative_to(ROOT))
    source_files = [Path(__file__), entry_path, manifest_path,
        ROOT / "test/e2e/local_browser_fixture.py", ROOT / "test/e2e/platform_token_read_revoke.py",
        ROOT / "ui/src/pages/images/ImageImportDrawer.tsx",
        ROOT / "ui/src/i18n/locales/en/images.json", ROOT / "ui/src/i18n/locales/zh/images.json"]
    source_files.extend(ROOT / path for path in product_sources)
    report = {"contract": manifest["contract_id"], "case": manifest["name"], "run_id": run_id,
        "status": "NOT_RUN", "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin", "locale": args.locale,
        "timezone": "UTC", "viewport": {"width": 1440, "height": 900}, "retries": 0,
        "selection": [manifest["contract_id"]], "evidence": [], "not_covered": manifest["missing"],
        "source_hashes": {str(path.relative_to(ROOT)): digest(path) for path in source_files},
        "runtime": "N/A: catalog metadata only; no pull, agent, container or inference claim",
        "command": ["python3", entry_relative, *sys.argv[1:]]}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], timezone_id="UTC",
                locale="zh-CN" if args.locale == "zh" else "en-US")
            page = context.new_page()
            page.set_default_timeout(12000)
            report["browser_version"] = browser.version
            try:
                login(page, context, server, artifacts, report, args.locale)
                case(page, server, artifacts, report, args.locale)
                check(set(manifest["required_evidence"]) <= {e["layer"] for e in report["evidence"]}, "missing evidence")
                check(all(digest(ROOT / path) == value for path, value in report["source_hashes"].items()), "sources changed while running")
                report["status"] = "VERIFYING_ARTIFACTS"
            except Exception as error:
                report.update(status="FAIL_TEST", failure=traceback.format_exc(), exception_type=type(error).__name__,
                    classification="UNTRIAGED: inspect evidence before classifying product, test or infrastructure")
                if report.get("preflight") == "PASS":
                    page.screenshot(path=artifacts.directory / "failure.png")
                    (artifacts.directory / "failure-aria.txt").write_text(artifacts.redact(page.locator("body").aria_snapshot()))
                else:
                    report["failure_capture"] = "omitted before authenticated page readiness; login DOM may contain credentials"
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception as error:
        report.update(status="FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA",
            failure=traceback.format_exc(), exception_type=type(error).__name__)
    finally:
        report["server_stopped"] = server.stop()
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        log = artifacts.private / "server.log"
        if log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))
        report["private_state"] = "isolated DB retained for diagnosis; owned server stopped; remove this run directory within 7 days"
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
            if report["status"] == "VERIFYING_ARTIFACTS":
                report["status"] = "PASS"
        except Exception:
            report.update(status="FAIL_TEST", artifact_error="credential scan rejected artifact; do not upload")
        artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": report["contract"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1
