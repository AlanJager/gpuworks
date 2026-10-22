#!/usr/bin/env python3
"""Settings / request trace export through the real browser and an OTLP fixture.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-settings-telemetry
After build: python3 test/e2e/settings_telemetry.py --locale en
Requires Python Playwright and system Chrome. The OpenTelemetry collector is an
auditable local OTLP/HTTP fixture; the server, encrypted header storage, browser
and SQLite are real. No agent, GPU, container or engine is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server and fixture and
retains the private database/logs for scoped diagnosis (seven days).
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
from otlp_fixture import OTLPEndpoint
from platform_token_read_revoke import button, check

CONTRACT = "gpuworks/settings/telemetry-export"
CASE = "设置 / 请求 Trace 导出 / OTLP 测试 span、加密请求头、采样比例、保存与关闭"
TELEMETRY = "/api/v1/telemetry/export"
TEST = TELEMETRY + "/test"


def strings(locale):
    settings = json.loads((ROOT / "ui/src/i18n/locales" / locale / "settings.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return settings, auth


def format_text(template: str, **values) -> str:
    for key, value in values.items():
        template = template.replace("{{" + key + "}}", str(value))
    return template


def accessible(label: str) -> str:
    # AntD inserts a display space into two-character Chinese button text.
    return (r"\s*".join(label) if re.fullmatch(r"[㐀-鿿]{2}", label)
            else re.escape(label))


def all_spans(request: dict) -> list[dict]:
    spans = []
    for resource in request["body"]["resourceSpans"]:
        for scope in resource["scopeSpans"]:
            spans.extend(scope["spans"])
    return spans


def only_span(request: dict) -> dict:
    check(request.get("errors") == [],
          f"the fixture rejected the OTLP document: {request.get('errors')}")
    spans = all_spans(request)
    check(len(spans) == 1, f"the test export carried {len(spans)} spans, want 1")
    return spans[0]


# Contract: gpuworks/settings/telemetry-export
# Mainline: 独立 server/SQLite + OTLP fixture → 设置/集成页填写草稿并发测试 span
#           → fixture 收到带请求头/采样/Gzip 的真实 span → 失败 collector 显示错误
#           → 保存启用、采样比例与 trace link → 刷新/独立 GET 回读且不返回请求头值
#           → 留空保存保留已存请求头，测试 span 再次收到 → 清理恢复默认
def test_telemetry_export(page, context, server, artifacts, report, locale, fixture):
    settings, auth = strings(locale)
    telemetry = settings["telemetry"]
    page_errors, console_errors, bad_responses, puts = [], [], [], []
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
        if urlsplit(request.url).path == TELEMETRY and request.method == "PUT":
            puts.append(request.post_data_json)

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
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"
    integrations = page.get_by_role("tab", name=settings["page"]["tabs"]["integrations"],
                                    exact=True)
    integrations.click()
    expect(integrations).to_have_attribute("aria-selected", "true")
    report["last_step"] = "integrations-tab-selected"

    card = page.locator(".ant-card").filter(has_text=telemetry["title"]).first
    switch = card.get_by_role("switch", name=telemetry["enabled"], exact=True)
    endpoint = card.get_by_label(telemetry["endpoint"], exact=True)
    ratio = card.get_by_label(telemetry["sampleRatio"], exact=True)
    trace_link_input = card.get_by_label(telemetry["traceLink"], exact=True)
    # The button gains a loading icon while a test runs; match the label inside
    # the accessible name and wait for it to be enabled before each click.
    test_button = card.get_by_role(
        "button", name=re.compile(accessible(telemetry["test"]))).first
    save_button = card.get_by_role(
        "button", name=re.compile(accessible(settings["common"]["save"]))).first
    expect(card.get_by_text(telemetry["title"], exact=True)).to_be_visible()
    expect(switch).not_to_be_checked()
    expect(endpoint).to_have_value("")
    expect(ratio).to_have_value("100")

    initial_response = page.request.get(server.base + TELEMETRY)
    check(initial_response.status == 200, f"initial telemetry GET HTTP {initial_response.status}")
    initial = initial_response.json()
    check(initial["enabled"] is False and initial["endpoint"] == ""
          and initial["headers"] == [], f"initial telemetry differs: {initial}")
    report["last_step"] = "telemetry-initial"

    endpoint.fill(fixture.base)
    # The AntD plus icon contributes its own accessible-name prefix.
    card.get_by_role("button", name=re.compile(accessible(telemetry["addHeader"]))).first.click()
    name_input = card.get_by_label(format_text(telemetry["headerName"], n=1), exact=True)
    value_input = card.get_by_label(format_text(telemetry["headerValueOf"], n=1), exact=True)
    name_input.fill("X-E2E-Token")
    secret = f"e2e-{report['run_id']}-header-secret"
    artifacts.remember(secret)
    value_input.fill(secret)
    ratio.fill("50")
    trace_link = f"https://traces.example/e2e/{report['run_id']}/{{trace_id}}"
    trace_link_input.fill(trace_link)
    report["last_step"] = "telemetry-draft-filled"

    expect(test_button).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == TEST
                              and r.request.method == "POST") as tested:
        test_button.click()
    check(tested.value.status == 200, f"test span HTTP {tested.value.status}")
    result = tested.value.json()
    check(result["ok"] is True and result["traces_url"] == fixture.base + "/v1/traces"
          and re.fullmatch(r"[0-9a-f]{32}", result.get("trace_id", "")),
          f"test span result differs: {result}")
    expect(card.get_by_text(telemetry["testOK"], exact=True)).to_be_visible()
    expect(card.get_by_text(result["trace_id"], exact=True)).to_be_visible()
    calls = fixture.snapshot()
    check(len(calls) == 1, f"the collector received {len(calls)} requests, want 1")
    call = calls[0]
    check(call["path"] == "/v1/traces" and not call["failed"],
          f"the collector request differs: {call['path']}")
    check(call["headers"].get("x-e2e-token") == secret,
          "the collector did not receive the configured header")
    check(call["headers"].get("content-encoding") == "gzip",
          "the export was not gzip-encoded")
    span = only_span(call)
    check(span["name"] == "gpuworks test span" and span["traceId"] == result["trace_id"],
          f"the test span differs: {span}")
    attributes = {item["key"]: item["value"] for item in span.get("attributes") or []}
    check(attributes.get("gpuworks.test") == {"boolValue": True},
          f"the test span attribute differs: {attributes}")
    unpersisted = page.request.get(server.base + TELEMETRY).json()
    check(unpersisted["enabled"] is False and unpersisted["endpoint"] == ""
          and unpersisted["headers"] == [],
          "sending a test span persisted the draft")
    report["last_step"] = "test-span-verified"

    endpoint.fill(fixture.fail_base)
    expect(test_button).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == TEST
                              and r.request.method == "POST") as failed_test:
        test_button.click()
    check(failed_test.value.status == 200, f"failing test span HTTP {failed_test.value.status}")
    failed_result = failed_test.value.json()
    check(failed_result["ok"] is False and failed_result.get("error"),
          f"the failing collector was reported as success: {failed_result}")
    expect(card.get_by_text(telemetry["testFailed"], exact=True)).to_be_visible()
    failed_calls = fixture.snapshot()
    check(len(failed_calls) == 2 and failed_calls[1]["path"] == "/fail/v1/traces"
          and failed_calls[1]["failed"],
          f"the failing collector request differs: {failed_calls}")
    still = page.request.get(server.base + TELEMETRY).json()
    check(still["enabled"] is False and still["endpoint"] == "",
          "a failing test span persisted the draft")
    endpoint.fill(fixture.base)
    report["last_step"] = "test-span-failure"

    switch.click()
    expect(switch).to_be_checked()
    expect(save_button).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == TELEMETRY
                              and r.request.method == "PUT") as saved:
        save_button.click()
    check(saved.value.status == 200, f"telemetry save HTTP {saved.value.status}")
    put_body = saved.value.request.post_data_json
    check(put_body == {"enabled": True, "endpoint": fixture.base,
                       "headers": [{"name": "X-E2E-Token", "value": secret}],
                       "sample_ratio": 0.5, "trace_link": trace_link},
          f"telemetry PUT differs: {put_body}")
    saved_doc = saved.value.json()
    check(saved_doc["enabled"] is True and saved_doc["endpoint"] == fixture.base
          and saved_doc["sample_ratio"] == 0.5 and saved_doc["trace_link"] == trace_link
          and saved_doc["status"]["active"] is True
          and saved_doc["status"]["traces_url"] == fixture.base + "/v1/traces",
          f"saved telemetry differs: {saved_doc}")
    expect(page.get_by_text(telemetry["saved"], exact=True).first).to_be_visible()
    persisted_response = page.request.get(server.base + TELEMETRY)
    check(persisted_response.status == 200, f"telemetry GET HTTP {persisted_response.status}")
    persisted = persisted_response.json()
    check(persisted["headers"] == [{"name": "X-E2E-Token"}],
          f"the header name was not persisted alone: {persisted['headers']}")
    check(secret not in persisted_response.text(),
          "the telemetry GET exposed the stored header value")
    report["last_step"] = "telemetry-saved"

    page.reload()
    expect(page.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    expect(page.get_by_role("tab", name=settings["page"]["tabs"]["integrations"], exact=True)
           ).to_have_attribute("aria-selected", "true")
    card = page.locator(".ant-card").filter(has_text=telemetry["title"]).first
    switch = card.get_by_role("switch", name=telemetry["enabled"], exact=True)
    endpoint = card.get_by_label(telemetry["endpoint"], exact=True)
    ratio = card.get_by_label(telemetry["sampleRatio"], exact=True)
    trace_link_input = card.get_by_label(telemetry["traceLink"], exact=True)
    test_button = card.get_by_role(
        "button", name=re.compile(accessible(telemetry["test"]))).first
    save_button = card.get_by_role(
        "button", name=re.compile(accessible(settings["common"]["save"]))).first
    expect(switch).to_be_checked()
    expect(endpoint).to_have_value(fixture.base)
    expect(ratio).to_have_value("50")
    expect(trace_link_input).to_have_value(trace_link)
    expect(card.get_by_label(format_text(telemetry["headerName"], n=1), exact=True)
           ).to_have_value("X-E2E-Token")
    expect(card.get_by_label(format_text(telemetry["headerValueOf"], n=1), exact=True)
           ).to_have_value("")
    report["last_step"] = "telemetry-reloaded"

    expect(save_button).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == TELEMETRY
                              and r.request.method == "PUT") as kept:
        save_button.click()
    check(kept.value.status == 200, f"kept-header save HTTP {kept.value.status}")
    kept_body = kept.value.request.post_data_json
    check(kept_body["headers"] == [{"name": "X-E2E-Token", "value": None}],
          f"a blank stored header was not sent as null: {kept_body}")
    before = len(fixture.snapshot())
    expect(test_button).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == TEST
                              and r.request.method == "POST") as kept_test:
        test_button.click()
    check(kept_test.value.status == 200 and kept_test.value.json()["ok"] is True,
          "the test span after keeping the stored header failed")
    calls = fixture.snapshot()
    check(len(calls) == before + 1
          and calls[-1]["headers"].get("x-e2e-token") == secret,
          "the stored header value was not kept for the test span")
    report["last_step"] = "stored-header-kept"

    switch.click()
    expect(switch).not_to_be_checked()
    endpoint.fill("")
    card.get_by_role("button", name=format_text(telemetry["removeHeader"], n=1),
                     exact=True).click()
    ratio.fill("100")
    trace_link_input.fill("")
    expect(save_button).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == TELEMETRY
                              and r.request.method == "PUT") as cleared:
        save_button.click()
    check(cleared.value.status == 200, f"telemetry clear HTTP {cleared.value.status}")
    cleared_doc = page.request.get(server.base + TELEMETRY).json()
    check(cleared_doc["enabled"] is False and cleared_doc["endpoint"] == ""
          and cleared_doc["headers"] == [] and cleared_doc["trace_link"] == ""
          and cleared_doc["sample_ratio"] == 1,
          f"the telemetry setting was not restored: {cleared_doc}")
    expect(card.get_by_text(telemetry["state"]["off"], exact=True)).to_be_visible()
    report["last_step"] = "cleanup-restored"

    check(bad_responses == [], "unexpected browser HTTP errors: " + str(bad_responses))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"),
          "settings page has horizontal overflow")
    report["last_step"] = "browser-errors-clear"

    report["evidence"].extend([
        {"layer": "ui", "page": "/settings", "tab": settings["page"]["tabs"]["integrations"],
         "card": telemetry["title"], "test_ok": telemetry["testOK"],
         "test_failed": telemetry["testFailed"], "saved": telemetry["saved"],
         "reloaded": {"endpoint": fixture.base, "ratio": 50, "header": "X-E2E-Token"},
         "restored_state": telemetry["state"]["off"]},
        {"layer": "request", "put_bodies": puts,
         "test_span": {"status": tested.value.status, "ok": result["ok"],
                       "trace_id": result["trace_id"]},
         "failed_test_span": {"status": failed_test.value.status, "ok": failed_result["ok"]},
         "kept_header_put": {"headers": kept_body["headers"]}},
        {"layer": "persistence", "initial": {"enabled": initial["enabled"],
                                             "endpoint": initial["endpoint"]},
         "saved": {"enabled": saved_doc["enabled"], "endpoint": saved_doc["endpoint"],
                   "sample_ratio": saved_doc["sample_ratio"],
                   "trace_link": saved_doc["trace_link"],
                   "headers": persisted["headers"],
                   "secret_returned": False},
         "restored": {"enabled": cleared_doc["enabled"],
                      "endpoint": cleared_doc["endpoint"],
                      "headers": cleared_doc["headers"],
                      "sample_ratio": cleared_doc["sample_ratio"]}},
        {"layer": "execution", "collector_requests": [
            {"path": item["path"], "failed": item["failed"],
             "header": item["headers"].get("x-e2e-token"),
             "span": only_span(item)["name"],
             "trace_id": only_span(item)["traceId"]} for item in calls],
         "failure_request": {"path": failed_calls[1]["path"],
                             "failed": failed_calls[1]["failed"]},
         "stored_header_reused": True},
        {"layer": "cleanup", "restored": True,
         "observed": "independent GET equals the disabled defaults"},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses, "puts": puts}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/settings-telemetry.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_telemetry_export.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite-otlp-fixture", "identity": "admin",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "fixture_sha256": digest(ROOT / "test/e2e/otlp_fixture.py"),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "execution": "the server exported real OTLP/HTTP spans to the audited local collector",
        "runtime": "N/A: no agent, container, GPU or engine",
        "not_covered": ["real tracing backends and their query UI",
                        "trace export of actual gateway requests (only the test span)",
                        "non-admin authorization",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/settings_telemetry.py", *sys.argv[1:]]}
    fixture = OTLPEndpoint().start()
    server = LocalServer(args.server_bin, artifacts.private)
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["collector"] = fixture.base
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
                test_telemetry_export(page, context, server, artifacts, report,
                                      args.locale, fixture)
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
        try:
            fixture.stop()
            report["fixture_stopped"] = True
        except Exception as error:
            report["fixture_stopped"] = False
            report["fixture_stop_error"] = f"collector fixture shutdown failed: {error.__class__.__name__}"
        if not report["server_stopped"] or not report["fixture_stopped"]:
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
