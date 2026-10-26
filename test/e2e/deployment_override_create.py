#!/usr/bin/env python3
"""Browser container overrides -> real agent/Docker exec/files/gateway.

E2E standard: test/e2e/browser-e2e-testing-standard.md.
Run: DOCKER_HOST=unix:///path/to/dedicated/docker.sock make test-e2e-deployment-container-overrides
Requires local alpine:3.20, fake-engine:test, built server/agent, Chrome.
DOCKER_CONTEXT must be empty; the default shared Docker socket is refused.
Only GPU facts are synthetic. No real GPU inference is claimed.
"""
import json
import os
import re
import time
from urllib.parse import urlsplit

from playwright.sync_api import expect
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)
from local_browser_fixture import ROOT, http
from platform_token_read_revoke import button, check
import deployment_container_fixture as containers
import deployment_override_fixture as fixture_tools
import deployment_wizard_create as wizard

CONTRACT = "gpuworks/deployment/container-overrides"
CASE = "部署 / 容器覆盖 / 环境设置与删除、只读可写挂载、共享内存与真实网关"


def expected_body(fixture, name):
    expected = wizard.expected_body(fixture, name)
    expected["image"] = fixture["image"]
    expected["container_overrides"] = fixture_tools.expected_overrides(fixture)
    return expected


def expected_requests(fixture, deleted_id=None):
    expected = [{"method": "POST", "path": containers.DEPLOYMENTS, "query": "", "body": expected_body(fixture, value)}
                for value in ("x" * 257, fixture["name"])]
    if deleted_id is not None:
        expected.append({"method": "DELETE", "path": containers.DEPLOYMENTS + f"/{deleted_id}", "query": "", "body": None})
    return expected


def hold_deploy_create(page, report):
    # setSubmitting runs before createDeployment, so Deploy is loading while
    # the first POST is still held. Continue into the real server response.
    path = containers.DEPLOYMENTS
    held = []
    pattern = re.compile(re.escape(path) + r"(?:\?|$)")
    ok = page.locator(".deploy-submit-bar .ant-btn-primary")

    def hold_post(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    ok.click()
    report["last_step"] = "deploy-create-inflight"
    expect(ok).to_have_class(re.compile(r"\bant-btn-loading\b"))
    ok.click()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and not held:
        page.wait_for_timeout(100)
    check(len(held) == 1, "duplicate deployment create while the first request is held")
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as received:
        held[0].continue_()
    page.unroute(pattern, hold_post)
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True})
    return received.value


def remember_created(response, report):
    document = response.json()
    ident = document.get("id") if isinstance(document, dict) else None
    valid = type(ident) is int and ident > 0
    if valid:
        report["owned_deployment_id"] = ident
    # A malformed ID must leave the pre-registered unique name available to
    # cleanup's independent lookup, rather than selecting /True or /null.
    check(valid, "created deployment response returned no valid numeric ID")
    return ident


def configure(page, labels, fixture):
    rows = page.locator(".image-defaults-env-row")
    expect(rows).to_have_count(1)
    expect(rows.get_by_placeholder("KEY", exact=True)).to_have_value("E2E_WIZARD")
    expect(rows.get_by_placeholder("VALUE", exact=True)).to_have_value(fixture["run_id"])
    rows.get_by_placeholder("VALUE", exact=True).fill(fixture["override_value"])
    unset = page.locator("#container-overrides-env-unset")
    unset.fill("HF_HUB_OFFLINE")
    unset.press("Enter")
    shm = page.locator("#container-overrides-shm")
    expect(shm).to_have_value("")
    shm.fill("0.25")
    shm.press("Tab")
    for index, (name, read_only) in enumerate((("ro", True), ("rw", False))):
        page.locator("#container-overrides-mount-add").click()
        mount = page.locator(".image-defaults-mount-row").nth(index)
        mount.get_by_placeholder(labels["containerOverrides"]["hostPathPlaceholder"], exact=True).fill(fixture[name + "_dir"])
        mount.get_by_placeholder(labels["containerOverrides"]["containerPathPlaceholder"], exact=True).fill("/e2e-" + name)
        expect(mount.get_by_role("checkbox")).not_to_be_checked()
        if read_only:
            mount.get_by_role("checkbox").check()
        expect(mount.get_by_role("checkbox")).to_be_checked() if read_only else expect(mount.get_by_role("checkbox")).not_to_be_checked()


def assert_draft(page, fixture):
    expect(page.locator(".image-defaults-env-row").get_by_placeholder("VALUE", exact=True)).to_have_value(fixture["override_value"])
    expect(page.locator("#container-overrides-shm")).to_have_value("0.25")
    expect(page.locator('.ant-select-selection-item[title="HF_HUB_OFFLINE"]')).to_be_visible()
    for index, name in enumerate(("ro", "rw")):
        mount = page.locator(".image-defaults-mount-row").nth(index)
        inputs = mount.get_by_role("textbox")
        expect(inputs.nth(0)).to_have_value(fixture[name + "_dir"])
        expect(inputs.nth(1)).to_have_value("/e2e-" + name)
        if name == "ro":
            expect(mount.get_by_role("checkbox")).to_be_checked()
        else:
            expect(mount.get_by_role("checkbox")).not_to_be_checked()


def assert_resolved(document, fixture):
    retired = {"stopped", "failed", "draining", "pending-removal", "removing"}
    current = [item for item in document["instances"] if item["state"] not in retired]
    check(len(current) == 1 and current[0]["state"] == "running", "expected one current running replica")
    instance = current[0]
    wizard.assert_json(instance["env"], {"E2E_WIZARD": fixture["override_value"],
        "TRANSFORMERS_OFFLINE": "1", "GLOO_SOCKET_IFNAME": "lo", "NCCL_SOCKET_IFNAME": "lo",
        "VLLM_HOST_IP": "127.0.0.1"}, "resolved environment (unset/system/explicit) differs")
    wizard.assert_json(instance["shm_bytes"], fixture_tools.SHM_BYTES, "resolved shm bytes")
    wizard.assert_json(instance["mounts"], [
        {"source": fixture["ro_dir"], "target": "/e2e-ro", "read_only": True, "purpose": "data"},
        {"source": fixture["rw_dir"], "target": "/e2e-rw", "purpose": "data"}], "resolved mounts differ")
    return instance


def test_deployment_container_overrides(page, context, server, artifacts, report, locale, fixture, token):
    labels = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deploy.json").read_text())
    listings = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deployments.json").read_text())
    auth = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/auth.json").read_text())
    common = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/common.json").read_text())
    requests, errors, http_errors, console_errors = [], [], [], []
    report.update({"mutation_requests": requests, "page_errors": errors,
                   "http_errors": http_errors, "console_errors": console_errors})
    authenticated = False
    def request_seen(request):
        path = urlsplit(request.url).path
        if path.startswith(containers.DEPLOYMENTS) and request.method in ("POST", "PUT", "PATCH", "DELETE"):
            requests.append({"method": request.method, "path": path,
                "query": urlsplit(request.url).query,
                "body": request.post_data_json if request.post_data else None})
    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400 and not (not authenticated and path == "/api/v1/auth/me" and response.status == 401):
            http_errors.append({"method": response.request.method, "path": path, "status": response.status})
    def console_seen(message):
        if message.type == "error":
            path = urlsplit(message.location.get("url", "")).path
            if not (not authenticated and path == "/api/v1/auth/me" and "401" in message.text):
                console_errors.append({"path": path, "text": message.text})
    page.on("request", request_seen)
    page.on("response", response_seen)
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("console", console_seen)
    page.goto(server.base + "/deploy")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    authenticated = True
    with page.expect_response(lambda r: urlsplit(r.url).path == containers.MODELS and "q=" in r.url):
        page.locator("#model_id").fill(fixture["model"]["name"])
    model = page.locator("#model_id")
    popup = page.locator("[id=" + json.dumps(model.get_attribute("aria-controls")) + "]").locator("..")
    popup.get_by_text(fixture["model"]["name"], exact=True).filter(visible=True).click()
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/evaluate") as evaluated:
        wizard.select(page, page.locator("#target_vendor"), labels["basic"]["vendor"]["nvidia"])
    check(evaluated.value.status == 200, "registered-model evaluation failed")
    deploy = button(page, labels["page"]["deploy"])
    expect(deploy).to_be_enabled(timeout=30000)
    page.get_by_role("button").filter(has_text=fixture["template"]["display_title"]).click()
    weights = page.get_by_role("dialog", name=labels["weightSourceModal"]["confirmTitle"])
    source = weights.get_by_role("radio", name=re.compile(re.escape(fixture["model"]["name"])))
    expect(source).to_be_checked()
    use_model = button(weights, labels["weightSourceModal"]["useModel"])
    # Weight-source confirm is an antd Modal. Identity, 1440x900 stacking, and the focus trap.
    assert_dialog_shell(page, weights, source, use_model)
    report.setdefault("evidence", []).append({
        "layer": "ui", "dialog_shell": labels["weightSourceModal"]["confirmTitle"], "viewport": "1440x900"})
    use_model.click()
    page.get_by_text(labels["workbench"]["node"]["manual"], exact=True).click()
    wizard.select(page, page.get_by_text(labels["workbench"]["node"]["selectNodeAndGpu"], exact=True)
        .locator("..").locator("..").get_by_role("combobox"), fixture["node"]["name"] + " · 1 GPU")
    image = page.get_by_role("combobox", name=labels["workbench"]["acquisition"]["imageOptions"])
    image.fill(fixture["image"])
    image.press("Tab")
    name = page.get_by_role("textbox", name=re.compile(re.escape(labels["basic"]["name"]) + "$"))
    page.get_by_role("textbox", name=labels["basic"]["servedName"], exact=True).fill(fixture["served_name"])
    name.fill(fixture["name"])
    configure(page, labels, fixture)
    assert_draft(page, fixture)
    expect(deploy).to_be_enabled(timeout=30000)
    check(requests == [], "editing draft emitted deployment mutation")
    report["last_step"] = "overrides-draft-configured"
    invalid = "x" * 257
    name.fill(invalid)
    rejected = hold_deploy_create(page, report)
    report["rejection"] = {"status": rejected.status, "body": rejected.json()}
    detail = "must contain valid text without control characters and at most 256 characters"
    wizard.assert_json(report["rejection"], {"status": 400, "body": {
        "code": "FIELD_INVALID", "error": "name " + detail,
        "params": {"field": "name", "detail": detail}}}, "backend refusal differs")
    reason = common["apiError"]["FIELD_INVALID"].replace("{{field}}", "name").replace("{{detail}}", detail)
    expect(page.get_by_text(labels["action"]["createFailed"].replace("{{detail}}", reason), exact=True)).to_be_visible()
    expect(name).to_have_value(invalid)
    expect(image).to_have_value(fixture["image"])
    assert_draft(page, fixture)
    code, listing = http(server.base, "GET", containers.DEPLOYMENTS, token=token)
    check(code == 200 and listing["total"] == 0, "rejection partially created deployment")
    name.fill(fixture["name"])
    report["last_step"] = "backend-refused-draft-corrected"
    try:
        expect(deploy).to_be_enabled()
    except Exception:
        try:
            report["submit_buttons"] = page.get_by_role("button").all_text_contents()
            report["submit_bar"] = page.locator(".deploy-submit-bar").inner_html()
        except Exception as diagnostic_error:
            report["submit_diagnostic_error"] = str(diagnostic_error)
        raise
    with page.expect_response(lambda r: urlsplit(r.url).path == containers.DEPLOYMENTS and r.request.method == "POST") as created:
        deploy.click()
    check(created.value.status == 201, f"override create HTTP {created.value.status}: {created.value.text()}")
    dep_id = remember_created(created.value, report)
    wizard.assert_json(requests, expected_requests(fixture), "full create mutation count/query/body differs")
    report["last_step"] = "browser-created"
    def running():
        status, document = http(server.base, "GET", containers.DEPLOYMENTS + f"/{dep_id}", token=token)
        report["last_deployment"] = document
        check(status == 200, f"deployment GET {status}")
        containers.remember_containers(report, document)
        return document if document["runtime_state"] == "running" else None
    saved = wizard.until(page, running, "real override launch")
    wizard.assert_json(fixture_tools.stored_overrides(server, dep_id), fixture_tools.expected_overrides(fixture), "persisted raw container overrides")
    for field in ("name", "image", "model_id", "node_uuid", "served_name"):
        wizard.assert_json(saved[field], expected_body(fixture, fixture["name"])[field], "persisted " + field)
    container = assert_resolved(saved, fixture)["container_id"]
    runtime = fixture_tools.verify_runtime(container, fixture, report)
    response = page.request.post(server.base + "/v1/chat/completions",
        headers={"Authorization": "Bearer " + fixture["key"]["key"]},
        data={"model": fixture["served_name"], "messages": [{"role": "user", "content": "overrides"}]})
    containers.assert_chat_response(response)
    wizard.assert_json(response.json(), {"id": "fake-engine-completion", "object": "chat.completion",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello from fake-engine"},
                     "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 16, "completion_tokens": 8, "total_tokens": 24}},
        "exact audited fake-engine completion")
    report["last_step"] = "container-exec-and-gateway"
    page.reload()
    row = page.get_by_role("row").filter(has_text=fixture["name"])
    expect(row).to_have_count(1)
    row.get_by_role("button", name=fixture["name"], exact=True).click()
    drawer = page.get_by_role("dialog").last
    expect(drawer.get_by_text(fixture["image"], exact=True).first).to_be_visible()
    status, refreshed = http(server.base, "GET", containers.DEPLOYMENTS + f"/{dep_id}", token=token)
    check(status == 200, "reload independent GET failed")
    stored = fixture_tools.stored_overrides(server, dep_id)
    wizard.assert_json(stored, fixture_tools.expected_overrides(fixture), "refreshed raw container overrides")
    assert_resolved(refreshed, fixture)
    parent = page.get_by_role("dialog", name=fixture["name"], exact=True)
    expect(parent).to_be_visible()
    delete_title = listings["delete"]["title"].replace("{{name}}", fixture["name"])
    # The header Delete button includes the icon's accessible name.
    remove = parent.locator(".ant-drawer-extra").get_by_role(
        "button", name=listings["delete"]["button"])
    dep_path = containers.DEPLOYMENTS + f"/{dep_id}"

    def open_delete():
        remove.click()
        dialog = page.get_by_role("dialog", name=delete_title, exact=True)
        expect(dialog).to_be_visible()
        return dialog

    before_delete = len(requests)
    dialog = assert_nested_dismissals(
        open_delete(), parent, open_delete,
        [("cancel", cancel_button(listings["delete"]["cancel"])),
         ("escape", escape), ("close", close_button), ("mask", mask)],
        lambda: len(requests), remove)
    cancel_button(listings["delete"]["cancel"])(dialog)
    expect(dialog).to_be_hidden()
    expect(parent).to_be_visible()
    check(len(requests) == before_delete, "delete dialog dismissal removed the deployment")
    stayed = page.request.get(server.base + dep_path)
    check(stayed.status == 200 and stayed.json().get("name") == fixture["name"],
          "delete dialog dismissal changed the stored deployment")
    report["evidence"].append({
        "layer": "ui", "delete_dialog": delete_title, "parent": fixture["name"],
        "dismissals": ["cancel", "escape", "close", "mask"],
        "focus_returned": True, "delete_requests": 0, "stored_name": fixture["name"]})
    confirm = page.get_by_role("dialog", name=delete_title, exact=True)
    remove.click()
    expect(confirm.get_by_text(listings["delete"]["description"], exact=True)).to_be_visible()
    with page.expect_response(lambda r: urlsplit(r.url).path == containers.DEPLOYMENTS + f"/{dep_id}" and r.request.method == "DELETE") as removed:
        button(confirm, listings["delete"]["button"]).click()
    check(removed.value.status in (200, 202, 204), "delete refused")
    wizard.until(page, lambda: http(server.base, "GET", containers.DEPLOYMENTS + f"/{dep_id}", token=token)[0] == 404, "deployment disappearance")
    wizard.until(page, lambda: containers.docker_running(container) == "absent", "container disappearance")
    expect(row).to_have_count(0)
    report["owned_deployment_id"] = None
    wizard.assert_json(requests, expected_requests(fixture, dep_id), "final mutation sequence")
    allowed = [{"method": "POST", "path": containers.DEPLOYMENTS, "status": 400},
               {"method": "GET", "path": containers.DEPLOYMENTS + f"/{dep_id}", "status": 404}]
    check(all(item in allowed for item in http_errors), f"unexpected HTTP errors: {http_errors}")
    check(all((item["path"] == containers.DEPLOYMENTS and "400" in item["text"]) or
              (item["path"] == containers.DEPLOYMENTS + f"/{dep_id}" and "404" in item["text"])
              for item in console_errors), f"unexpected console errors: {console_errors}")
    check(not errors, f"page errors: {errors}")
    report["last_step"] = "deletion-observed"
    report["evidence"].extend([
        {"layer": "ui", "overrides": fixture_tools.expected_overrides(fixture), "cancelled_delete": True, "backend_error": reason},
        {"layer": "request", "mutations": requests, "response_statuses": [400, 201, removed.value.status]},
        {"layer": "persistence", "raw_sqlite_overrides": stored, "resolved_get_instance":
         {key: assert_resolved(refreshed, fixture)[key] for key in ("env", "mounts", "shm_bytes")}, "id": dep_id},
        {"layer": "execution", "container": container, "runtime": runtime, "gateway": response.json()},
        {"layer": "cleanup", "deployment_http": 404, "container_state": "absent"}])


def require_dedicated_docker():
    # This contract launches real managed containers. Another control plane's
    # orphan sweep can delete them on a shared default daemon.
    docker_host = os.environ.get("DOCKER_HOST", "").strip()
    parsed = urlsplit(docker_host)
    check(parsed.scheme == "unix" and bool(parsed.path) and os.path.realpath(parsed.path) not in (
        "/var/run/docker.sock", "/run/docker.sock"),
        "container override E2E requires an explicit dedicated DOCKER_HOST (never the default socket)")
    check(not os.environ.get("DOCKER_CONTEXT", ""),
          "container override E2E requires empty DOCKER_CONTEXT so it cannot override the dedicated DOCKER_HOST")


def main():
    require_dedicated_docker()
    return wizard.main(manifest_path=ROOT / "test/e2e/coverage/deployment-container-overrides.json",
                       exercise=test_deployment_container_overrides,
                       fixture_module=fixture_tools)


if __name__ == "__main__":
    raise SystemExit(main())
