"""Real server/agent/isolated Docker lifecycle for image browser contracts.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Business assertions belong to the caller; no control-plane responses are mocked.
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
import uuid

from playwright.sync_api import sync_playwright
from image_catalog_runner import login
from image_runtime_fixture import ImageRuntimeFixture
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import check


def run(cases, entry_path, extra_sources=(), *, runtime_description=None):
    parser = argparse.ArgumentParser(description="Real CPU image browser contracts")
    parser.add_argument("--case", choices=list(cases), required=True)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    fixture = ImageRuntimeFixture(artifacts.private, run_id)
    manifest_path = ROOT / f"test/e2e/coverage/image-runtime-{args.case}.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["test_function"] == cases[args.case].__name__, "case binding differs")
    sources = [Path(__file__), Path(entry_path), manifest_path,
        *[ROOT / "test/e2e" / name for name in ("image_runtime_fixture.py", "local_browser_fixture.py",
          "image_catalog_runner.py", "image_catalog.py", "platform_token_read_revoke.py")],
        *[ROOT / path for path in extra_sources]]
    report = {"contract": manifest["contract_id"], "case": manifest["name"], "run_id": run_id,
        "status": "NOT_RUN", "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-agent-isolated-docker", "identity": "admin", "locale": args.locale,
        "timezone": "UTC", "viewport": {"width": 1440, "height": 900}, "retries": 0,
        "evidence": [], "not_covered": manifest["missing"],
        "source_hashes": {str(path.resolve().relative_to(ROOT)): digest(path) for path in sources},
        "runtime": runtime_description or "Real archive download/load and CPU container; no Registry/peer/GPU/inference claim",
        "command": ["python3", str(Path(entry_path).resolve().relative_to(ROOT)), *sys.argv[1:]]}
    proxy_env = {key: os.environ.get(key) for key in ("NO_PROXY", "no_proxy")}
    for key, previous in proxy_env.items():
        os.environ[key] = ",".join(filter(None, [previous, "127.0.0.1", "localhost"]))
    try:
        report["binary_sha256"] = {"server": digest(args.server_bin), "agent": digest(args.agent_bin)}
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        report["docker"] = fixture.start()
        status, auth = http(server.base, "POST", "/api/v1/auth/login", body={"username": "admin", "password": "password"})
        check(status == 200 and auth.get("token"), "fixture administrator login failed")
        artifacts.remember(auth["token"])
        node = fixture.start_agent(args.agent_bin, server.base, auth["token"], artifacts.remember)
        check(node["gpu_count"] == 0, "CPU-only lane unexpectedly discovered GPU hardware")
        report["node"] = {key: node.get(key) for key in ("uuid", "name", "state", "runtime_driver", "agent_version", "gpu_count")}
        check(node.get("agent_version") == report["health"]["version"], "agent/server build versions differ")
        report["owned_agent_pid"] = fixture.agent.pid
        report["owned_daemon_pid"] = fixture.daemon_pid
        report["fixture"] = {"image_ref": fixture.image_ref, "image_id": fixture.image_id,
            "archive_sha256": fixture.archive_sha256, "archive_size": fixture.archive_size,
            "archive_url": fixture.archive_url, "arch": fixture.arch}
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], timezone_id="UTC",
                locale="zh-CN" if args.locale == "zh" else "en-US")
            page = context.new_page()
            page.set_default_timeout(15000)
            report["browser_version"] = browser.version
            try:
                login(page, context, server, artifacts, report, args.locale)
                cases[args.case](page, server, fixture, artifacts, report, args.locale)
                check(set(manifest["required_evidence"]) <= {e["layer"] for e in report["evidence"]}, "missing evidence layer")
                check(all(digest(ROOT / p) == h for p, h in report["source_hashes"].items()), "source changed during execution")
                report["status"] = "VERIFYING_ARTIFACTS"
            except Exception as error:
                report.update(status="FAIL_TEST", failure=traceback.format_exc(), exception_type=type(error).__name__,
                    classification="UNTRIAGED: inspect UI/request/agent/runtime evidence")
                if report.get("preflight") == "PASS":
                    page.screenshot(path=artifacts.directory / "failure.png")
                    (artifacts.directory / "failure-aria.txt").write_text(artifacts.redact(page.locator("body").aria_snapshot()))
                report["failure_runtime"] = {"images": fixture.docker(["image", "ls", "--no-trunc", "--format", "{{json .}}"], check=False).stdout,
                    "containers": fixture.docker(["ps", "-a", "--no-trunc", "--format", "{{json .}}"], check=False).stdout,
                    "archive_requests": fixture.archive.snapshot()}
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception as error:
        report.update(status="FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA",
            failure=traceback.format_exc(), exception_type=type(error).__name__)
    finally:
        try:
            report["fixture_cleanup"] = fixture.stop(preserve_runtime=report["status"] != "VERIFYING_ARTIFACTS")
        except Exception:
            report.update(status="FAIL_TEST", cleanup_failure=traceback.format_exc())
        report["server_stopped"] = server.stop()
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        for name in ("server.log", "agent.log", "docker.log"):
            path = artifacts.private / name
            if path.exists():
                (artifacts.directory / name).write_text(artifacts.redact(path.read_text(errors="replace")))
        report["private_state"] = "SQLite/agent/log evidence retained for 7 days; Docker disposition and scoped reclamation are in fixture_cleanup"
        for key, previous in proxy_env.items():
            if previous is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = previous
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
