#!/usr/bin/env python3
"""HTTP model preview/registration and upstream loss recovery against real server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-http-model-registration (builds, then both English cases).
CLI after make build:
  python3 test/e2e/http_model_registration.py --locale en --case register
Choices: --locale en|zh; --case register|recovery|all.
Set NO_PROXY and no_proxy to 127.0.0.1,localhost.
Requires Python Playwright/system Chrome. Each case owns a server/SQLite and
an audited loopback HTTP directory; no agent download or GPU execution claim.
Artifacts: secrets/e2e/<runId>/report.json, trace and source-request audit.
Failed state stays private for diagnosis; only this case's listeners are stopped.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from http_model_fixture import ModelDirectory
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from platform_token_read_revoke import button, check, modal_geometry, theme_color

MODELS = "/api/v1/models"
PREVIEW = MODELS + "/preview-url"
REGISTER = MODELS + "/register-url"


def model_list(page, server):
    response = page.request.get(server.base + MODELS)
    check(response.status == 200, "independent model GET failed")
    data = response.json()
    check(set(data) == {"items", "total"} and data["total"] == len(data["items"]), "model page envelope drift")
    return data["items"]


# Contract: gpuworks/models/http-register; gpuworks/models/http-register-recovery
# Mainline (register): 独立HTTP目录 → UI预览、覆盖名称/来源ID并注册 → GET+刷新核对元数据、真实来源和零副本 → UI删除、GET消失。
# Mainline (recovery): 独立HTTP目录成功预览 → 上游真实503 → UI注册502且草稿保留/零持久化 → 恢复上游并原样重试 → GET+刷新正确 → UI删除。
def test_model_registration(page, context, server, fixture, artifacts, report, locale, scenario):
    directory = ROOT / "ui/src/i18n/locales" / locale
    k = json.loads((directory / "models.json").read_text())
    auth = json.loads((directory / "auth.json").read_text())
    common = json.loads((directory / "common.json").read_text())
    mutations, failures, errors, console_errors = [], [], [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(server.base + "/models")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=k["page"]["title"], exact=True)).to_be_visible()
    check(model_list(page, server) == [], "fresh isolated database contains models")
    report["preflight"] = "PASS"
    report["active_evidence_layer"] = "ui"
    page.on("request", lambda r: mutations.append({"method": r.method, "path": urlsplit(r.url).path})
            if urlsplit(r.url).path.startswith(MODELS) and r.method in {"POST", "DELETE", "PATCH"} else None)
    page.on("console", lambda message: console_errors.append({"path": urlsplit(message.location.get("url", "")).path, "text": message.text}) if message.type == "error" else None)
    page.on("response", lambda r: failures.append({"path": urlsplit(r.url).path, "status": r.status}) if r.status >= 400 else None)
    artifacts.start_trace(context)
    # Choose the product's list view through its real segmented control.
    page.get_by_role("img", name=k["view"]["list"], exact=True).click()
    expect(page.get_by_role("radio", name=k["view"]["list"], exact=True)).to_be_checked()
    # Empty state repeats the same action; the header's first occurrence is stable document order.
    trigger = button(page, k["action"]["register"]).first
    trigger.click()
    dialog = page.get_by_role("dialog", name=k["action"]["register"], exact=True)
    source = dialog.get_by_label(k["column"]["source"], exact=True)
    source.click()
    page.get_by_text(k["source"]["url"], exact=True).click()
    url = dialog.get_by_label(k["register"]["directoryURL"], exact=True)
    expect(url).to_have_value("")
    parse = button(dialog, k["register"]["parseDirectory"])
    parse.click()
    expect(dialog.get_by_text(k["register"]["directoryURLRequired"], exact=True)).to_be_visible()
    expect(url).to_have_attribute("aria-invalid", "true")
    check(mutations == [], "empty directory sent a preview or mutation")
    url.fill(fixture.url + "/")
    url.press("Tab")
    page.keyboard.press("Shift+Tab")
    expect(url).to_be_focused()
    theme_color(url, "borderColor", "primary")
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(trigger).to_be_focused()
    check(mutations == [], "Esc sent a mutation")
    trigger.click()
    source.click()
    page.get_by_text(k["source"]["url"], exact=True).click()
    url.fill(fixture.url + "/")
    report["active_evidence_layer"] = "preview"
    with page.expect_response(lambda r: urlsplit(r.url).path == PREVIEW and r.request.method == "POST") as preview_response:
        parse.click()
    preview = preview_response.value
    expected_preview = {"source_url": fixture.url, "source_id": fixture.root.lstrip("/"), "name": "tiny-model",
        "arch": "Qwen3ForCausalLM", "precision": "bfloat16", "context_length": 40960,
        "weight_bytes": len(fixture.files["model.safetensors"]), "files_count": 2, "input_modalities": ["text"]}
    check(preview.status == 200 and preview.request.post_data_json == {"url": fixture.url + "/"}
          and not urlsplit(preview.url).query and preview.json() == expected_preview, "preview request/metadata mismatch")
    expect(dialog.get_by_text(k["register"]["urlParsed"], exact=True)).to_be_visible()
    expect(dialog.get_by_text("Qwen3ForCausalLM", exact=True)).to_be_visible()
    expect(dialog.get_by_text("bfloat16", exact=True)).to_be_visible()
    expect(dialog.get_by_text("40,960", exact=True)).to_be_visible()
    expect(dialog.get_by_text(f"{len(fixture.files['model.safetensors']) / 1024:.1f} KiB", exact=True)).to_be_visible()
    expect(dialog.get_by_text(fixture.url, exact=True)).to_be_visible()
    expect(url).to_have_value(fixture.url)
    name = dialog.get_by_label(k["column"]["name"], exact=True)
    source_id = dialog.get_by_label(k["register"]["modelID"], exact=True)
    expect(name).to_have_value("tiny-model")
    expect(source_id).to_have_value(expected_preview["source_id"])
    check(model_list(page, server) == [], "preview persisted a model")
    before_submit = fixture.audit()
    check(any(r["method"] == "GET" and r["path"] == fixture.root + "/config.json" and r["status"] == 200 for r in before_submit), "preview did not read actual config")
    check(all(r["sha256"] == hashlib.sha256(fixture.files["config.json"]).hexdigest() for r in before_submit if r["path"] == fixture.root + "/config.json"), "audited config bytes differ from fixture authority")
    check(any(r["method"] == "HEAD" and r["path"] == fixture.root + "/model.safetensors" and r["status"] == 200 for r in before_submit), "preview did not verify exact weight bytes")
    target_name = f"e2e-{report['run_id']}-registered"
    target_source = f"e2e-{report['run_id']}/operator-model"
    name.fill(target_name)
    source_id.fill(target_source)
    submitted = {"source_url": fixture.url, "source_id": target_source, "name": target_name}
    # AntD may retain a decorative loading image during its leave transition;
    # anchor the button to its exact visible label, independent of the icon name.
    submit = dialog.get_by_role("button").filter(has_text=re.compile("^" + re.escape(k["register"]["confirm"]) + "$"))
    expect(submit).to_be_enabled()
    modal_geometry(dialog, submit)
    for _ in range(8):
        page.keyboard.press("Tab")
        check(dialog.evaluate("el => el.contains(document.activeElement)"), "Tab escaped registration modal")
    page.screenshot(path=artifacts.directory / "preview.png")
    report["evidence"].append({"layer": "ui", "checks": ["empty URL blocked without request", "Esc focus return", "URL normalized after preview", "metadata/defaults visible", "operator overrides retained", "Tab containment and viewport", "theme focus border"]})
    report["evidence"].append({"layer": "preview", "path": PREVIEW, "body": preview.request.post_data_json,
        "status": preview.status, "response": expected_preview, "fixture_requests": before_submit})
    if scenario == "recovery":
        report["active_evidence_layer"] = "recovery"
        fixture.set_available(False)
        with page.expect_response(lambda r: urlsplit(r.url).path == REGISTER and r.request.method == "POST") as rejected:
            submit.click()
        rejection = rejected.value
        check(rejection.status == 502 and rejection.request.post_data_json == submitted
              and rejection.json()["code"] == "HTTP_DIRECTORY_UNREACHABLE", "stale preview was trusted or wrong error contract")
        expect(page.get_by_text(re.compile(re.escape(common["apiError"]["HTTP_DIRECTORY_UNREACHABLE"])))).to_be_visible()
        expect(dialog).to_be_visible()
        expect(url).to_have_value(fixture.url)
        expect(name).to_have_value(target_name)
        expect(source_id).to_have_value(target_source)
        check(model_list(page, server) == [], "failed registration partially persisted")
        failed_reads = fixture.audit()[len(before_submit):]
        check(failed_reads and all(r["status"] == 503 for r in failed_reads), "registration did not re-read unavailable directory")
        report["evidence"].append({"layer": "recovery", "status": 502, "body": rejection.json(), "fixture_requests": failed_reads,
            "draft": "URL, source ID and name unchanged; zero model records"})
        fixture.set_available(True)
    report["active_evidence_layer"] = "request-and-persistence"
    read_offset = len(fixture.audit())
    expect(submit).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == REGISTER and r.request.method == "POST") as registration:
        submit.click()
    response = registration.value
    check(response.status == 201 and response.request.post_data_json == submitted and not urlsplit(response.url).query, "registration request/status mismatch")
    created = response.json()
    report["owned_model_id"] = created["id"]
    expect(dialog).not_to_be_visible()
    path = MODELS + "/" + str(created["id"])
    read = page.request.get(server.base + path)
    check(read.status == 200, "persisted model detail unavailable")
    persisted = read.json()
    for key, value in {"source": "url", **submitted, "revision": "", "arch": "Qwen3ForCausalLM", "precision": "bfloat16",
                       "context_length": 40960, "weight_gib": len(fixture.files["model.safetensors"]) / (1024 ** 3)}.items():
        check(persisted.get(key) == value and created.get(key) == value, f"persisted {key} differs from authority")
    check(not persisted.get("nodes") and not persisted.get("storages"), "registration invented a local model copy")
    check(len(model_list(page, server)) == 1, "registration duplicated model")
    final_reads = fixture.audit()[read_offset:]
    check(any(r["method"] == "GET" and r["path"] == fixture.root + "/config.json" and r["status"] == 200 for r in final_reads), "registration reused stale config")
    page.reload()
    row = page.get_by_role("row").filter(has_text=target_name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(target_source)
    detail_trigger = button(row, target_name)
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "GET") as detail_response:
        detail_trigger.click()
    check(detail_response.value.status == 200, "browser detail GET failed")
    detail = page.get_by_role("dialog", name=target_name, exact=True)
    expect(detail).to_be_in_viewport()
    expect(detail.get_by_text("Qwen3ForCausalLM", exact=True)).to_be_visible()
    expect(detail.get_by_text("bfloat16", exact=True)).to_be_visible()
    expect(detail.get_by_text("40,960", exact=True)).to_be_visible()
    page.wait_for_function("el => el.contains(document.activeElement)", arg=detail.element_handle())
    page.keyboard.press("Escape")
    expect(detail).not_to_be_visible()
    expect(detail_trigger).to_be_focused()
    report["evidence"].extend([{"layer": "request", "path": REGISTER, "body": submitted, "status": 201},
        {"layer": "persistence", "detail": persisted, "reload": "one row; detail architecture/precision/context match"},
        {"layer": "source", "fixture_requests": final_reads, "limit": "Directory metadata read; no agent/download/GPU execution"}])
    report["active_evidence_layer"] = "cleanup"
    button(row, k["action"]["more"]).click()
    page.get_by_role("menu").get_by_text(k["action"]["deleteModel"], exact=True).click()
    confirmation = k["delete"]["confirm"].replace("{{name}}", target_name)
    deletion = page.get_by_role("dialog", name=confirmation, exact=True)
    modal_geometry(deletion, button(deletion, k["action"]["delete"]))
    expect(deletion.get_by_text(k["delete"]["listDescription"], exact=True)).to_be_visible()
    button(deletion, k["action"]["cancel"]).click()
    expect(deletion).not_to_be_visible()
    check(not any(m["method"] == "DELETE" for m in mutations), "cancel sent DELETE")
    check(page.request.get(server.base + path).status == 200, "cancel removed model")
    button(row, k["action"]["more"]).click()
    page.get_by_role("menu").get_by_text(k["action"]["deleteModel"], exact=True).click()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as deleted:
        button(deletion, k["action"]["delete"]).click()
    check(deleted.value.status == 204 and deleted.value.request.post_data is None and not urlsplit(deleted.value.url).query, "delete contract mismatch")
    expect(row).to_have_count(0)
    check(page.request.get(server.base + path).status == 404 and model_list(page, server) == [], "deleted resource remains reachable")
    page.reload()
    expect(page.get_by_role("row").filter(has_text=target_name)).to_have_count(0)
    check(mutations == [{"method": "POST", "path": PREVIEW}] + [{"method": "POST", "path": REGISTER}] * (2 if scenario == "recovery" else 1) + [{"method": "DELETE", "path": path}], "unexpected/duplicate mutation")
    expected_failures = [{"path": REGISTER, "status": 502}] if scenario == "recovery" else []
    check(failures == expected_failures and not errors, "unexpected browser request failure or exception")
    check((len(console_errors) == 1 and console_errors[0]["path"] == REGISTER and "502" in console_errors[0]["text"]) if scenario == "recovery" else not console_errors, "unexpected browser console error")
    check(page.evaluate("document.documentElement.scrollWidth <= window.innerWidth"), "page has horizontal overflow")
    report["browser_errors"] = {"pageerrors": errors, "responses": failures, "console": console_errors}
    report["owned_model_id"] = None
    report["evidence"].append({"layer": "cleanup", "ui_delete": 204, "get_after_delete": 404, "remaining_models": 0,
        "scope": "Only the run-owned model; isolated server and HTTP fixture stopped by runner"})


def run_case(args, scenario):
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts((args.artifact_dir / scenario if args.case == "all" else args.artifact_dir)
                          if args.artifact_dir else ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    fixture = ModelDirectory(run_id).start()
    manifest_path = ROOT / "test/e2e/coverage/http-model-registration.json"
    manifest = json.loads(manifest_path.read_text())
    contract = manifest["cases"][scenario]
    report = {"run_id": run_id, "status": "NOT_RUN", "contract": contract["id"], "case": contract["name"],
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite-http-directory", "identity": "admin", "locale": args.locale,
        "timezone": "UTC", "viewport": {"width": 1440, "height": 900}, "retries": 0, "evidence": [],
        "not_covered": manifest["missing"], "command": ["python3", "test/e2e/http_model_registration.py", "--case", scenario, "--locale", args.locale],
        "source_hashes": {name: digest(ROOT / name) for name in ["test/e2e/http_model_registration.py", "test/e2e/http_model_fixture.py",
            "test/e2e/local_browser_fixture.py", "test/e2e/platform_token_read_revoke.py", "test/e2e/coverage/http-model-registration.json"]}}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_model_registration(page, context, server, fixture, artifacts, report, args.locale, scenario)
                check(set(contract["required_evidence"]) <= {item["layer"] for item in report["evidence"]}, "missing required evidence")
                check(all(digest(ROOT / name) == value for name, value in report["source_hashes"].items()), "source changed during execution")
                report["status"] = "VERIFYING_ARTIFACTS"
            except Exception as error:
                report["exception_type"] = type(error).__name__
                report["failure_attribution"] = "UNTRIAGED: FAIL_TEST denotes execution failure; inspect evidence layer to distinguish product and test defects"
                report["status"] = "FAIL_TEST"
                report["failure"] = traceback.format_exc()
                if report.get("preflight") == "PASS":
                    page.screenshot(path=artifacts.directory / "failure.png")
                    (artifacts.directory / "failure-aria.txt").write_text(artifacts.redact(page.locator("body").aria_snapshot()))
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        report["fixture_requests"] = fixture.audit()
        report["server_stopped"] = server.stop()
        report["fixture_stopped"] = fixture.stop()
        if not report["server_stopped"] or not report["fixture_stopped"]:
            report["status"] = "FAIL_TEST"
        report["private_state"] = "Isolated DB/raw log retained under private/; no process retained. Owner removes scoped directory after diagnosis within seven days."
        log = artifacts.private / "server.log"
        if log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
            if report["status"] == "VERIFYING_ARTIFACTS":
                report["status"] = "PASS"
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; do not upload"
        artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": report["contract"], "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return report["status"] == "PASS"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--case", choices=["register", "recovery", "all"], default="all")
    args = parser.parse_args()
    os.umask(0o077)
    results = [run_case(args, scenario) for scenario in (["register", "recovery"] if args.case == "all" else [args.case])]
    return 0 if all(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
