#!/usr/bin/env python3
"""Browser creates a deployment from its registered model and running spec.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-deployment-wizard-create
After build: python3 test/e2e/deployment_wizard_create.py --locale en
Requires Chrome, Python Playwright, Docker, built server and agent. Only GPU
facts are synthetic. Runtime is real Docker and the audited fake-engine; this
case does not prove GPU inference. Artifacts: secrets/e2e/<runId> (7 days).
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
import time
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask,
)
from platform_token_read_revoke import button, check
from deploy_submit_hold import hold_deploy_create
from deployment_batch_checks import created_notice, rejected_notice, open_deployment_row, delete_name_preview
import deployment_launch_checks as launch_checks
import deployment_container_fixture as container_fixture
import deployment_wizard_fixture as wizard_fixture
from deployment_selector_checks import (
    watch_weight_lookups, choose_registered_model, assert_weight_match, choose_manual_node)
from deployment_wizard_fixture import expected_body

CONTRACT = "gpuworks/deployment/wizard-create"
CASE = "部署 / 页面创建 / 自有模型与 spec、手工节点、本地权重镜像、真实启动和网关"
DEPLOYMENTS = container_fixture.DEPLOYMENTS
IMAGE = wizard_fixture.IMAGE


def until(page, predicate, label, timeout=90):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        # Dispatch browser events during bounded condition polling.
        page.wait_for_timeout(100)
    raise AssertionError(f"{label} did not complete; last={last!r}")


def select(page, locator, text):
    locator.click()
    controls = locator.get_attribute("aria-controls")
    check(bool(controls), "combobox does not identify its controlled popup")
    popup = page.locator("[id=" + json.dumps(controls) + "]").locator("..")
    popup.get_by_text(text, exact=True).filter(visible=True).click()


def assert_json(actual, expected, label):
    # JSON true and 1 are different wire values even though Python says True == 1.
    canonical = lambda value: json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)
    check(canonical(actual) == canonical(expected), f"{label}: expected {expected!r}; actual {actual!r}")




# Mainline: browser selects own model, vendor, recommended running spec, exact
# node and runtime image → blank-name refusal → genuine backend 400/recovery
# → one successful POST → independent persistence → real Docker/gateway
# → refresh → named UI delete → independent 404 and Docker absence.
def test_deployment_wizard_create(page, context, server, artifacts, report,
                                  locale, fixture, token):
    labels = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deploy.json").read_text())
    deployments = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deployments.json").read_text())
    auth = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/auth.json").read_text())
    common = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/common.json").read_text())
    weight_lookups = watch_weight_lookups(page)
    errors, creates, console_errors, http_errors, create_responses = [], [], [], [], []
    report["page_errors"] = errors
    report["console_errors"] = console_errors
    report["http_errors"] = http_errors
    evaluation_responses = []
    deployment_requests = []
    delete_requests = []
    report["deployment_post_requests"] = deployment_requests
    report["deployment_delete_requests"] = delete_requests
    authenticated = False
    def console_seen(message):
        if message.type == "error":
            path = urlsplit(message.location.get("url", "")).path
            if not authenticated and path == "/api/v1/auth/me" and "401" in message.text:
                return
            console_errors.append({"path": path, "text": message.text})
    def request_seen(request):
        path = urlsplit(request.url).path
        if path == DEPLOYMENTS and request.method == "POST":
            deployment_requests.append({"method": request.method, "path": path,
                "query": urlsplit(request.url).query, "body": request.post_data_json})
        if request.method == "DELETE" and path.startswith(DEPLOYMENTS + "/"):
            delete_requests.append(path)
    page.on("request", request_seen)
    page.on("console", console_seen)
    page.on("pageerror", lambda error: errors.append(str(error)))
    def observed(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not (not authenticated and path == "/api/v1/auth/me" and response.status == 401):
                http_errors.append({"method": response.request.method, "path": path,
                                    "status": response.status})
        if path == DEPLOYMENTS and response.request.method == "POST":
            create_responses.append(response)
        if path == "/api/v1/evaluate" and response.request.method == "POST":
            evaluation_responses.append(response)
    def record_create(response):
        # Response callbacks above perform no protocol I/O. Parse only after
        # expect_response returns control to this main browser flow.
        doc = response.json()
        if response.status == 201:
            report["owned_deployment_id"] = doc["id"]
        creates.append({"method": "POST", "path": DEPLOYMENTS,
                        "body": response.request.post_data_json,
                        "status": response.status, "response": doc})
        report["create_requests"] = creates
        return doc
    page.on("response", observed)
    page.goto(server.base + "/deploy")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    report["last_step"] = "browser-login"
    expect(button(page, labels["page"]["deploy"])).to_be_disabled()
    check(deployment_requests == [], "incomplete initial draft created a deployment")
    name_input = page.get_by_role("textbox", name=re.compile(re.escape(labels["basic"]["name"]) + "$"))
    route_input = page.get_by_role("textbox", name=labels["basic"]["servedName"], exact=True)
    model = fixture["model"]
    choose_registered_model(page, fixture, locale, report)
    with page.expect_response(lambda response: urlsplit(response.url).path == "/api/v1/evaluate"
                              and response.request.method == "POST") as initial_evaluation:
        select(page, page.locator("#target_vendor"), labels["basic"]["vendor"]["nvidia"])
    check(initial_evaluation.value.status == 200, "initial registered-model evaluation failed")
    report["initial_evaluation"] = {"status": initial_evaluation.value.status,
        "request": initial_evaluation.value.request.post_data_json}
    expect(button(page, labels["page"]["deploy"])).to_be_enabled(timeout=30000)
    scheme = page.get_by_role("button").filter(has_text=fixture["template"]["display_title"])
    expect(scheme).to_have_count(1)
    scheme.click()
    expect(scheme).to_have_class(re.compile("is-selected"))
    weights = page.get_by_role("dialog", name=labels["weightSourceModal"]["confirmTitle"], exact=True)
    marker = weights.get_by_role("radio", name=re.compile(re.escape(model["name"])))
    expect(marker).to_be_checked()
    assert_weight_match(page, marker, fixture, locale, report, weight_lookups)
    assert_dialog_shell(page, weights, marker, button(weights, labels["weightSourceModal"]["useModel"]))
    button(weights, labels["weightSourceModal"]["useModel"]).click()
    expect(weights).not_to_be_visible()
    choose_manual_node(page, fixture, labels, report)
    image = page.get_by_role("combobox", name=labels["workbench"]["acquisition"]["imageOptions"])
    image.fill(IMAGE)
    image.press("Tab")
    expect(image).to_have_value(IMAGE)
    route_input.fill(fixture["served_name"])
    name_input.fill(fixture["name"])
    deploy = button(page, labels["page"]["deploy"])
    try:
        expect(deploy).to_be_enabled(timeout=30000)
    except Exception:
        report["last_evaluations"] = []
        for response in evaluation_responses[-6:]:
            observed = {"status": response.status}
            try:
                observed["request"] = response.request.post_data_json
            except Exception as error:
                observed["request_parse_error"] = artifacts.redact(str(error))
            try:
                observed["response"] = response.json()
            except Exception as error:
                observed["parse_error"] = artifacts.redact(str(error))
            report["last_evaluations"].append(observed)
        raise
    launch_preview = launch_checks.draft_checks(
        page, labels, expected_body(fixture, fixture["name"]), report, token=token)
    report["last_step"] = "draft-selected"
    # Exact local cache forecast: both image and weights are on the chosen node.
    forecast = page.get_by_role("table").filter(has=page.get_by_text(labels["workbench"]["nodeAcquisition"]["forecastNote"], exact=True))
    expect(forecast.get_by_role("row").last).to_have_text(
        fixture["node"]["name"] + labels["workbench"]["nodeAcquisition"]["present"] * 2)
    launch_checks.boolean_rejection.seed(page, labels, expected_body(fixture, fixture["name"]), report, token, launch_checks.boolean_rejection.declared_mount(fixture["model_dir"]))
    name_input.fill("")
    deploy.click()
    expect(page.get_by_text(labels["basic"]["nameRequired"], exact=True)).to_be_visible()
    check(deployment_requests == [], "blank name emitted a deployment mutation")
    report["last_step"] = "required-name-refused"
    # The UI accepts text, while the real server limits display names to 256
    # runes. A 257-rune name exercises backend refusal without any mocking.
    invalid = "x" * 257
    name_input.fill(invalid)
    rejected = hold_deploy_create(page, report)
    record_create(rejected)
    check(rejected.status == 400, f"overlong name HTTP {rejected.status}")
    detail = "must contain valid text without control characters and at most 256 characters"
    assert_json(rejected.json(), {"code": "FIELD_INVALID", "error": "name " + detail,
          "params": {"field": "name", "detail": detail}}, "wrong server validation wire")
    assert_json(creates[0]["body"], launch_checks.boolean_rejection.expected_bad_body(expected_body(fixture, invalid), report), "rejected create body differs")
    error_text = common["apiError"]["FIELD_INVALID"].replace("{{field}}", "name").replace("{{detail}}", detail)
    rejected_notice(page, labels["action"]["createFailed"].replace("{{detail}}", error_text), labels, report)
    expect(name_input).to_have_value(invalid)
    expect(image).to_have_value(IMAGE)
    status, empty = http(server.base, "GET", DEPLOYMENTS, token=token)
    check(status == 200 and empty["total"] == 0, "rejected request created a deployment")
    launch_checks.boolean_rejection.retained(page, labels, expected_body(fixture, fixture["name"]), report, token, rejected, "name")
    name_input.fill(fixture["name"])
    launch_checks.boolean_rejection.restore(page, labels, expected_body(fixture, fixture["name"]), report, token)
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS
                              and r.request.method == "POST") as created:
        deploy.click()
    record_create(created.value)
    check(created.value.status == 201, f"browser create HTTP {created.value.status}: {created.value.text()}")
    created_notice(page, labels, report)
    check(len(creates) == 2 and [c["status"] for c in creates] == [400, 201], "unexpected mutation count/order")
    assert_json(creates[1]["body"], expected_body(fixture, fixture["name"]), "successful create body differs")
    dep_id = report["owned_deployment_id"]
    report["last_step"] = "browser-created"
    def running():
        status, document = http(server.base, "GET", DEPLOYMENTS + f"/{dep_id}", token=token)
        report["last_deployment_observation"] = json.loads(artifacts.redact(json.dumps({
            "http_status": status,
            "runtime_state": document.get("runtime_state") if isinstance(document, dict) else None,
            "instances": document.get("instances", []) if isinstance(document, dict) else [],
            "errors": document.get("errors", document.get("error")) if isinstance(document, dict) else document,
        })))
        check(status == 200, f"deployment GET {status}")
        container_fixture.remember_containers(report, document)
        return document if document["runtime_state"] == "running" else None
    saved = until(page, running, "real agent deployment running")
    for key in ("model_id", "node_uuid", "name", "served_name", "engine", "image", "template_id",
                "template_spec_source", "template_spec_name", "quick_params", "nnodes", "replicas", "placement_strategy"):
        assert_json(saved[key], expected_body(fixture, fixture["name"])[key], f"persisted {key}")
    check(saved["template_scenario"] == "balanced", "persisted another template scenario")
    assert_json(saved["node_requirements"], {"gpuworks.ai/node": [fixture["node"]["uuid"]],
          "gpuworks.ai/vendor": ["nvidia"]}, "persisted placement constraints differ")
    snapshot = saved["catalog_selection_snapshot"]
    expected_snapshot = {"spec_source": "user", "spec_name": wizard_fixture.SPEC,
        "engine": "vllm", "image": IMAGE, "env": {"E2E_WIZARD": report["run_id"]},
        "weight_provider": "local", "weight_repo_id": "e2e/" + report["run_id"],
        "weight_revision": "local"}
    assert_json({key: snapshot.get(key) for key in expected_snapshot}, expected_snapshot,
          "persisted running spec / local weights snapshot differs")
    assert_json(saved["operations"], [{"type": "openai.chat", "endpoint": "/v1/chat/completions",
                                   "renderer": "chat"}], "persisted inference contract differs")
    # /deployments includes finished generations. A recovered port collision
    # can leave a failed historical instance beside the one filling the replica
    # (internal/proto/instance.go InstanceFillsReplica).
    retired = {"stopped", "failed", "draining", "pending-removal", "removing"}
    current = [item for item in saved["instances"] if item["state"] not in retired]
    check(len(current) == 1 and current[0]["state"] == "running",
          f"expected exactly one running current replica, got {current}")
    instance = current[0]
    launch_checks.snapshot_checks(saved, expected_body(fixture, fixture["name"]), report)
    launch_checks.runtime_checks(instance, expected_body(fixture, fixture["name"]),
                                 launch_preview, fixture["model_dir"], report)
    container = instance["container_id"]
    check(instance["node_uuid"] == fixture["node"]["uuid"], "agent used another node")
    check(container_fixture.docker_running(container) == "true", "Docker container is not running")
    inspected = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
    check(inspected["Config"]["Image"] == IMAGE, "Docker used another image")
    check(inspected["Image"] == report["fake_engine_image_id"], "Docker image id differs from the preflight image")
    check(any(m["Source"] == fixture["model_dir"] for m in inspected["Mounts"]), "Docker did not mount selected local weights")
    check("E2E_WIZARD=" + report["run_id"] in inspected["Config"]["Env"], "spec env did not reach Docker")
    chat = page.request.post(server.base + "/v1/chat/completions",
        headers={"Authorization": "Bearer " + fixture["key"]["key"]},
        data={"model": fixture["served_name"], "messages": [{"role": "user", "content": "wizard"}]})
    container_fixture.assert_chat_response(chat)
    report["last_step"] = "docker-gateway-executed"
    page.reload()
    row = page.get_by_role("row").filter(has_text=fixture["name"])
    expect(row).to_have_count(1)
    drawer = open_deployment_row(page, row, fixture["name"], dep_id, report)
    expect(drawer.get_by_text(fixture["served_name"], exact=True)).to_be_visible()
    expect(drawer.get_by_text(IMAGE, exact=True).first).to_be_visible()
    delete_title = deployments["delete"]["title"].replace("{{name}}", fixture["name"])
    def open_delete():
        # The drawer button's icon contributes an accessible name prefix.
        drawer.get_by_role("button", name=re.compile(re.escape(deployments["delete"]["button"]) + "$")).click()
        dialog = page.get_by_role("dialog", name=delete_title, exact=True)
        expect(dialog).to_be_visible()
        return dialog
    confirm = open_delete()
    delete_name_preview(confirm, delete_title, report)
    assert_dialog_shell(
        page, confirm, confirm.get_by_text(deployments["delete"]["description"], exact=True),
        button(confirm, deployments["delete"]["button"]))
    confirm = assert_nested_dismissals(
        confirm, drawer, open_delete,
        [("cancel", cancel_button(deployments["delete"]["cancel"])),
         ("escape", escape), ("close", close_button), ("mask", mask)],
        lambda: len(delete_requests))
    with page.expect_response(lambda r: urlsplit(r.url).path == DEPLOYMENTS + f"/{dep_id}"
                              and r.request.method == "DELETE") as deleted:
        button(confirm, deployments["delete"]["button"]).click()
    check(deleted.value.status in (200, 202, 204), "browser deletion refused")
    until(page, lambda: http(server.base, "GET", DEPLOYMENTS + f"/{dep_id}", token=token)[0] == 404,
          "deployment deletion")
    until(page, lambda: container_fixture.docker_running(container) == "absent", "container reclamation")
    expect(row).to_have_count(0)
    report["owned_deployment_id"] = None
    check(len(create_responses) == len(creates) == 2, "unexpected deployment response count")
    assert_json(deployment_requests, launch_checks.boolean_rejection.expected_mutations([
        {"method": "POST", "path": DEPLOYMENTS, "query": "", "body": expected_body(fixture, invalid)},
        {"method": "POST", "path": DEPLOYMENTS, "query": "", "body": expected_body(fixture, fixture["name"])}],
        report), "unexpected deployment POST request count/query/body")
    allowed_http = [{"method": "POST", "path": DEPLOYMENTS, "status": 400},
                    {"method": "GET", "path": DEPLOYMENTS + f"/{dep_id}", "status": 404}]
    check(all(item in allowed_http for item in http_errors), f"unexpected browser HTTP errors: {http_errors}")
    check(all((item["path"] == DEPLOYMENTS and "400" in item["text"])
              or (item["path"] == DEPLOYMENTS + f"/{dep_id}" and "404" in item["text"])
              for item in console_errors), f"unexpected browser console errors: {console_errors}")
    check(not errors, f"browser page errors: {errors}")
    report["last_step"] = "deletion-observed"
    report["evidence"].extend([
        {"layer": "ui", "model": model["name"], "spec": wizard_fixture.SPEC,
         "node": fixture["node"]["name"], "image": IMAGE, "refreshed_route": fixture["served_name"],
         "required_name_error": labels["basic"]["nameRequired"], "server_error": error_text},
        {"layer": "request", "creates": creates, "delete": {"path": DEPLOYMENTS + f"/{dep_id}", "status": deleted.value.status}},
        {"layer": "persistence", "id": dep_id, "identity": {k: saved[k] for k in
         ("model_id", "node_uuid", "name", "served_name", "engine", "image", "template_id",
          "template_spec_source", "template_spec_name", "quick_params", "replicas", "nnodes", "placement_strategy")},
         "local_spec_snapshot": expected_snapshot},
        {"layer": "execution", "container": container, "running": True, "image": IMAGE,
         "local_weights": fixture["model_dir"], "spec_env": True, "gateway": chat.json()},
        {"layer": "cleanup", "deployment_status": 404, "container": container, "docker_state": "absent"}])

def main(*, manifest_path=None, exercise=None, fixture_module=None):
    # Deployment cases share this resource lifecycle, while the manifest and
    # explicit callbacks keep business assertions and prerequisite ownership local.
    exercise = exercise or test_deployment_wizard_create
    fixture_module = fixture_module or wizard_fixture
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--check-family", choices=launch_checks.FAMILIES, default="all")
    parser.add_argument("--mutation", help="Product mutation label recorded in evidence")
    args = parser.parse_args()
    os.umask(0o077)
    check(shutil.which("docker") is not None, "docker is required for this lane")
    check(subprocess.run(["docker", "image", "inspect", IMAGE],
                         capture_output=True).returncode == 0,
          f"{IMAGE} is missing; run make fake-engine-image")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = manifest_path or ROOT / "test/e2e/coverage/deployment-wizard-create.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["test_function"] == exercise.__name__,
          "coverage manifest does not bind this case")
    check((ROOT / manifest["test_file"]).resolve() == Path(exercise.__code__.co_filename).resolve(),
          "coverage manifest does not bind the exercise source file")
    report = {"contract": manifest["contract_id"], "case": manifest["name"], "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-agent-docker-fake-gpu-facts", "identity": "admin browser / node agent / Docker",
        "product_mutation": args.mutation,
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [manifest["contract_id"]], "retries": 0, "evidence": [], "last_step": "preflight",
        "coverage_manifest_sha256": digest(manifest_path),
        "test_file": manifest["test_file"],
        "test_sha256": digest(ROOT / manifest["test_file"]),
        "runner_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "container_fixture_sha256": digest(ROOT / "test/e2e/deployment_container_fixture.py"),
        "wizard_fixture_sha256": digest(ROOT / "test/e2e/deployment_wizard_fixture.py"),
        "batch_checks_sha256": digest(ROOT / "test/e2e/deployment_batch_checks.py"),
        "selector_checks_sha256": digest(ROOT / "test/e2e/deployment_selector_checks.py"),
        "boolean_checks_sha256": digest(ROOT / "test/e2e/deployment_boolean_checks.py"),
        "boolean_contracts_sha256": digest(ROOT / "test/e2e/deployment_boolean_contracts.py"),
        "case_fixture_file": str(Path(fixture_module.__file__).relative_to(ROOT)),
        "case_fixture_sha256": digest(Path(fixture_module.__file__)),
        "target_sha": subprocess.check_output(["git", "rev-parse", "wei/master"], cwd=ROOT, text=True).strip(),
        "execution": "browser creates the deployment; real agent and Docker launch the audited fake-engine; synthetic GPU facts only",
        "runtime": "real engine and gateway requests; no real GPU compute or vendor driver",
        "not_covered": manifest["missing"],
        "command": ["python3", manifest["test_file"], *sys.argv[1:]]}
    launch_checks.freeze(report, args.check_family)
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {"agent": None, "log": None}
    token = None

    def artifact_stage(label, action):
        # Diagnostics must never prevent closing browser handles or reclaiming
        # the server, agent and run-owned resources.
        try:
            action()
            return True
        except Exception:
            report["status"] = "FAIL_TEST"
            failure = artifacts.redact(traceback.format_exc())
            report.setdefault("failure", failure)
            report.setdefault("artifact_errors", []).append({"stage": label, "error": failure})
            return False

    def save_report():
        return artifact_stage("report.json", lambda: artifacts.write_json("report.json", report))

    try:
        report["fake_engine_image_id"] = subprocess.check_output(
            ["docker", "image", "inspect", "--format", "{{.Id}}", IMAGE], text=True).strip()
        report["docker_host"] = os.environ.get("DOCKER_HOST", "unix:///var/run/docker.sock")
        report["docker_root"] = subprocess.check_output(
            ["docker", "info", "--format", "{{.DockerRootDir}}"], text=True).strip()
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        agent_bin = args.agent_bin.resolve()
        report["agent_bin"] = str(agent_bin)
        try:
            report["health"] = server.start()
        finally:
            server_pid = getattr(getattr(server, "process", None), "pid", None)
            if type(server_pid) is int:
                report["server_pid"] = server_pid
        report["base_url"] = server.base
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "password"})
        check(status == 200, f"fixture login HTTP {status}")
        token = login["token"]
        check(isinstance(token, str) and bool(token.strip()), "fixture login returned no valid token")
        artifacts.remember(token)
        report["preflight"] = "PASS"
        fixture = fixture_module.prepare(server, token, run_id, report, holder,
                                         agent_bin, artifacts.remember)
        report["last_step"] = "fixture-prepared"
        save_report()
        with sync_playwright() as play:
            browser = context = page = None
            try:
                browser = play.chromium.launch(channel="chrome", headless=True)
                context = browser.new_context(viewport=report["viewport"],
                    locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
                report["browser_version"] = browser.version
                page = context.new_page()
                page.set_default_timeout(10000)
                exercise(page, context, server, artifacts, report,
                                           args.locale, fixture, token)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) <= layers,
                      "required evidence is missing")
                launch_checks.frozen(report)
                report["case_completed"] = True
            except Exception:
                report["status"] = "FAIL_TEST"
                report.setdefault("failure", artifacts.redact(traceback.format_exc()))
                save_report()
                if page is not None:
                    artifact_stage("failure.png", lambda: page.screenshot(path=artifacts.directory / "failure.png"))
            finally:
                save_report()
                close_ok = True
                try:
                    if context is not None:
                        context.close()
                except Exception:
                    close_ok = False
                    report["context_close_error"] = artifacts.redact(
                        traceback.format_exc())
                try:
                    if browser is not None:
                        browser.close()
                except Exception:
                    close_ok = False
                    report["browser_close_error"] = artifacts.redact(
                        traceback.format_exc())
                report["browser_close_ok"] = close_ok
                if not close_ok:
                    report["status"] = "FAIL_TEST"
                    report.setdefault("failure", "the browser did not close cleanly")
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report.setdefault("failure", artifacts.redact(traceback.format_exc()))
    finally:
        cleanup_ok = False
        try:
            cleanup_ok = fixture_module.cleanup(server, token, report, holder)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
            # A callback failure cannot bypass the process boundary cleanup.
            agent = holder.get("agent")
            try:
                if agent is not None and agent.poll() is None:
                    agent.terminate()
                    try:
                        agent.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        agent.kill()
                        agent.wait(timeout=5)
                report["agent_stopped"] = agent is None or agent.poll() is not None
            except Exception:
                report["agent_stopped"] = False
                report["agent_stop_error"] = artifacts.redact(traceback.format_exc())
            log = holder.get("log")
            try:
                if log is not None and not log.closed:
                    log.close()
            except Exception:
                report["agent_log_close_error"] = artifacts.redact(traceback.format_exc())
            remaining = report.setdefault("remaining_resources", [])
            name = report.get("owned_deployment_name")
            if name and not report.get("owned_deployment_id"):
                remaining.append({"kind": "deployment-name", "id": name, "state": "unknown (cleanup callback failed)",
                    "reclaim": "GET /api/v1/deployments?q=" + name + "; DELETE only the id with this exact name"})
            for field, kind, reclaim in (
                ("owned_deployment_id", "deployment", "DELETE /api/v1/deployments/{id}?force=true"),
                ("owned_model_id", "model", "DELETE /api/v1/models/{id}"),
                ("owned_node_uuid", "node", "DELETE /api/v1/nodes/{id}?force=true"),
                ("owned_key_name", "inference-key-name", "GET /api/v1/apikeys; DELETE only the prefix matching name {id}"),
                ("owned_template_model_id", "template", "DELETE /api/v1/inference-templates/spec?model_id={id}&spec=browser-vllm"),
                ("owned_override_image", "image", "docker image rm {id}")):
                ident = report.get(field)
                if ident and not any(item.get("kind") == kind and item.get("id") == ident for item in remaining):
                    remaining.append({"kind": kind, "id": ident, "state": "unknown (cleanup callback failed)",
                                      "reclaim": reclaim.format(id=ident)})
            if not report["agent_stopped"]:
                ident = getattr(agent, "pid", "unknown")
                remaining.append({"kind": "agent", "id": ident, "state": "not confirmed stopped", "reclaim": f"kill {ident}"})
            for field, kind, reclaim in (("containers", "container", "docker rm -f {id}"),
                    ("owned_override_source_tags", "image", "docker image rm {id}")):
                for ident in report.get(field, []):
                    if not any(item.get("kind") == kind and item.get("id") == ident for item in remaining):
                        remaining.append({"kind": kind, "id": ident, "state": "unknown (cleanup callback failed)", "reclaim": reclaim.format(id=ident)})
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        if not report["server_stopped"]:
            server_pid = report.get("server_pid", "unknown")
            report.setdefault("remaining_resources", []).append({"kind": "server", "id": server_pid,
                "state": "not confirmed stopped", "reclaim": f"kill {server_pid}"})
        cleanup_ok = cleanup_ok and report["server_stopped"]
        browser_ok = report.get("browser_close_ok", True)
        cleanup_ok = cleanup_ok and browser_ok
        report["environment_cleanup_ok"] = cleanup_ok
        layers = {item["layer"] for item in report["evidence"]}
        if (report.get("case_completed") and cleanup_ok
                and report.get("status") not in ("FAIL_TEST", "BLOCKED_INFRA")
                and set(manifest["required_evidence"]) <= layers):
            report["status"] = "PASS"
        elif report["status"] not in ("FAIL_TEST", "BLOCKED_INFRA"):
            report["status"] = "FAIL_TEST"
            report.setdefault("failure",
                              "cleanup or required evidence did not complete")
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            artifact_stage("server.log", lambda: (artifacts.directory / "server.log").write_text(
                artifacts.redact(server_log.read_text())))
        agent_log_path = artifacts.private / "agent.log"
        if agent_log_path.exists():
            artifact_stage("agent.log", lambda: (artifacts.directory / "agent.log").write_text(
                artifacts.redact(agent_log_path.read_text())))
        residual = report.get("remaining_resources") or []
        if residual or not report.get("server_stopped") or not report.get("agent_stopped", True):
            listed = "; ".join(
                f"{item['kind']}:{item.get('id')} ({item['state']}; {item['reclaim']})"
                for item in residual) or "server or agent not confirmed stopped"
            report["private_state"] = (
                "private/ retains isolated DB/logs; residual resources: " + listed
                + "; use the scoped reclaim entries; remove only this run directory "
                  "after diagnosis (7 days)")
        else:
            report["private_state"] = (
                "private/ retains isolated DB/logs and agent state; no process kept; "
                "remove only this run directory after diagnosis (7 days)")
        save_report()
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; private directory must not be uploaded"
            save_report()
    print(json.dumps({"status": report["status"], "contract": report["contract"], "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
