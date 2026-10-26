#!/usr/bin/env python3
"""Real-browser template clone/edit and YAML failure recovery.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-template-edit E2E_LOCALE=en (or zh).
CLI after make build:
  python3 test/e2e/template_edit.py --locale en --case clone
Choices: --locale en|zh; --case clone|syntax|schema|all.
Set NO_PROXY and no_proxy to 127.0.0.1,localhost.
Requires Python Playwright/system Chrome. Each case owns a server/SQLite and
catalog package. Invalid YAML is rejected by the real server; the draft survives.
Artifacts: secrets/e2e/<runId>/report.json, trace and saved YAML. Failed private
state is retained for diagnosis and the owned server stops.
Coverage is PARTIAL: edited spec runtime consumption remains MISSING.
"""
from __future__ import annotations

import argparse
import copy
from datetime import datetime, timezone
import hashlib
import json
import re
import subprocess
import traceback
from urllib.parse import parse_qs, quote, urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest
from dialog_checks import assert_drawer_identity, assert_yaml_drawer_focus
from platform_token_read_revoke import button, check, modal_geometry
from template_catalog_fixture import catalog_package
from template_spec_delete_blocks import exercise_spec_delete_blocks

API = "/api/v1/inference-templates"
CASES = {
    "clone": ("gpuworks/templates/clone-edit", "推理模板 / 克隆后编辑规格 / 副本刷新回读且原始 YAML 与规格不变"),
    "syntax": ("gpuworks/templates/yaml-syntax-recovery", "推理模板 / YAML 语法错误 / 服务端拒绝且草稿保留，修正后保存"),
    "schema": ("gpuworks/templates/yaml-schema-recovery", "推理模板 / YAML 未知字段 / 服务端拒绝且原记录不变，修正后保存"),
}


def action(scope, text):
    # Icons may contribute to the accessible name. Use one exact visible label.
    label = r"\s*".join(text) if re.fullmatch(r"[\u3400-\u9fff]{2}", text) else re.escape(text)
    return scope.get_by_role("button").filter(has_text=re.compile("^" + label + "$"))


def yaml_hash(text):
    return hashlib.sha256(text.encode()).hexdigest()


def read_model(page, base, model_id):
    response = page.request.get(base + API + "/model/" + quote(model_id, safe="/"))
    check(response.status == 200, f"independent model read HTTP {response.status}")
    model = response.json()
    check(model["model_id"] == model_id and model["source"] == "user", "model identity/source drift")
    check(len(model["specs"]) == 1 and model["specs"][0]["source"] == "user", "unexpected spec set")
    return model


def read_yaml(page, base, model_id):
    response = page.request.get(base + API + "/yaml", params={"model_id": model_id})
    check(response.status == 200, f"independent YAML read HTTP {response.status}")
    return response.text()


def replace_editor(editor, value):
    expect(editor).to_have_attribute("contenteditable", "true")
    editor.fill(value)
    # Rendered innerText preserves CodeMirror line boundaries (textContent joins divs).
    expect(editor).to_have_text(value, use_inner_text=True)


def editor_ready(page, text):
    dialog = page.get_by_role("dialog", name=text, exact=True)
    editor = dialog.get_by_role("textbox", name=text, exact=True)
    expect(editor).to_have_attribute("contenteditable", "true")
    return dialog, editor


# Mainline: 独立 SQLite 导入唯一用户模板 → UI 克隆/编辑规格或分别提交语法/Schema错误
# → 精确请求与独立模型/YAML回读、刷新 → 原始hash/规格守恒 → UI取消删除及删除本次规格/模板。
# Contracts: gpuworks/templates/clone-edit, gpuworks/templates/yaml-syntax-recovery,
# gpuworks/templates/yaml-schema-recovery. Each parameter gets an independent server/report.
def test_template_edit(page, context, server, artifacts, report, locale, case):
    directory = ROOT / "ui/src/i18n/locales" / locale
    t = json.loads((directory / "inferenceTemplates.json").read_text())
    auth = json.loads((directory / "auth.json").read_text())
    model_id = "e2e-" + report["run_id"].lower() + "/template"
    spec_name = "e2e-" + report["run_id"] + "-spec"
    title = "e2e-" + report["run_id"]
    # Explicit size avoids metadata network access; this fixture is never deployed.
    source = catalog_package(model_id, spec_name, title)
    page.goto(server.base + "/inference-templates")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "browser login failed")
    expect(page.get_by_role("heading", name=t["page"]["title"], exact=True)).to_be_visible()
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    report["preflight"] = "PASS"
    report["evidence_layer"] = "fixture"
    imported = page.request.post(server.base + API + "/import", data=source, headers={"Content-Type": "application/gzip"})
    check(imported.status == 200 and imported.json()["models"] == 1, "fixture package import failed")
    package_digest = imported.json()["package_digest"]
    check(re.fullmatch("[0-9a-f]{64}", package_digest), "fixture package has no content identity")
    package_url = server.base + API + "/export/" + package_digest
    exported = page.request.get(package_url)
    check(exported.status == 200 and exported.headers["etag"] == '"' + package_digest + '"', "fixture export failed")
    original_package = exported.body()
    report["original_package_sha256"] = hashlib.sha256(original_package).hexdigest()
    report["owned_models"] = [model_id]
    original = read_model(page, server.base, model_id)
    original_yaml = read_yaml(page, server.base, model_id)
    check(original["specs"][0]["args"] == ["--max-model-len", "4096"], "fixture args differ")
    report["original_yaml_sha256"] = yaml_hash(original_yaml)
    errors, bad, mutations, console_errors = [], [], [], []
    page.on("console", lambda message: console_errors.append(message.text) if message.type == "error" else None)
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("response", lambda r: bad.append({"path": urlsplit(r.url).path, "status": r.status}) if r.status >= 400 else None)
    page.on("request", lambda r: mutations.append({"method": r.method, "path": urlsplit(r.url).path})
            if urlsplit(r.url).path.startswith(API) and r.method in {"POST", "DELETE"} else None)
    # fetch(File) omits its body from Playwright's request event. Chrome's
    # read-only network API exposes the actual transmitted bytes; no interception.
    cdp = context.new_cdp_session(page)
    cdp.send("Network.enable")
    imports = []
    cdp.on("Network.requestWillBeSent", lambda event: imports.append(event["requestId"])
           if event["request"]["method"] == "POST" and urlsplit(event["request"]["url"]).path == API + "/import" else None)
    artifacts.start_trace(context)
    page.reload()
    page.get_by_placeholder(t["flat"]["search"], exact=True).fill(title)
    action(page, title).click()
    expect(page.get_by_role("heading", name=title, exact=True)).to_be_visible()
    expect(page.get_by_role("rowheader", name=t["detail"]["operations"], exact=True)).to_be_visible()
    expect(page.get_by_role("rowheader", name=t["detail"]["inputModalities"], exact=True)).to_be_visible()
    source_row = page.get_by_role("row").filter(has=page.get_by_role("rowheader", name=t["detail"]["source"], exact=True))
    expect(source_row.get_by_text(t["flat"]["userSource"], exact=True)).to_be_visible()
    report["evidence_layer"] = "ui"
    edit_trigger = action(page, t["flat"]["edit"])
    edit_trigger.click()
    dialog, editor = editor_ready(page, t["flat"]["editYAML"])
    save = action(dialog, t["flat"]["save"])
    modal_geometry(dialog, save)
    report["evidence"].append(assert_drawer_identity(
        page, dialog, t["flat"]["editYAML"], editor,
        page.get_by_role("dialog", name=t["editor"]["discardTitle"], exact=True)))
    report["evidence"].append(assert_yaml_drawer_focus(page, dialog, save, editor))
    expect(editor).to_have_text(original_yaml, use_inner_text=True)
    expect(action(dialog, t["editor"]["reset"])).to_be_disabled()
    save.press("Tab")
    expect(editor).to_be_focused()
    check(dialog.evaluate("el => el.contains(document.activeElement)"), "focus left YAML drawer")
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(edit_trigger).to_be_focused()
    check(not mutations, "opening or cancelling YAML editor mutated data")

    if case == "clone":
        report["evidence_layer"] = "clone request"
        with page.expect_response(lambda r: urlsplit(r.url).path == API + "/clone" and r.request.method == "POST") as cloned:
            action(page, t["detail"]["clone"]).click()
        response = cloned.value
        check(response.status == 201, f"clone HTTP {response.status}")
        check(response.request.post_data_json == {"source_model_id": model_id} and not urlsplit(response.url).query, "clone request drift")
        target = response.json()["model_id"]
        check(target == model_id + "-copy", "clone identity differs")
        report["owned_models"].append(target)
        cloned_model = read_model(page, server.base, target)
        check(cloned_model["display_title"] == title + " Copy", "clone title differs")
        expected_spec = dict(original["specs"][0], id=cloned_model["specs"][0]["id"])
        check(cloned_model["specs"] == [expected_spec], "clone did not preserve full original spec")
        report["evidence"].append({"layer": "request", "clone": {"method": "POST", "path": API + "/clone", "body": response.request.post_data_json, "status": 201}})
        baseline = cloned_model
    else:
        target, baseline = model_id, original
        edit_trigger.click()
    dialog, editor = editor_ready(page, t["flat"]["editYAML"])
    save = action(dialog, t["flat"]["save"])
    baseline_yaml = read_yaml(page, server.base, target)
    expect(editor).to_have_text(baseline_yaml, use_inner_text=True)
    # Change exactly one spec argument; model-level context and all other fields stay unchanged.
    check(baseline_yaml.count('- "4096"') == 1, "canonical YAML argument shape changed")
    updated = baseline_yaml.replace('- "4096"', '- "2048"')
    replace_editor(editor, updated)
    expect(action(dialog, t["editor"]["reset"])).to_be_enabled()
    def open_discard():
        page.keyboard.press("Escape")
        opened = page.get_by_role("dialog", name=t["editor"]["discardTitle"], exact=True)
        expect(opened).to_be_visible()
        return opened

    def escape_discard(child):
        # Confirm has no close icon. Focus its cancel control so Escape hits
        # the child dialog and not the YAML editor behind it.
        action(child, t["editor"]["keepEditing"]).focus()
        child.page.keyboard.press("Escape")

    from dialog_checks import assert_nested_dismissals
    discard = assert_nested_dismissals(
        open_discard(), dialog, open_discard,
        [("keep editing", lambda child: action(child, t["editor"]["keepEditing"]).click()),
         ("escape", escape_discard)],
        lambda: len(mutations))
    action(discard, t["editor"]["keepEditing"]).click()
    expect(discard).not_to_be_visible()
    expect(dialog).to_be_visible()
    expect(editor).to_have_text(updated, use_inner_text=True)
    check(read_yaml(page, server.base, target) == baseline_yaml, "cancel dialog modified persisted YAML")
    expected_before = 1 if case == "clone" else 0
    check(len(mutations) == expected_before, "cancel/keep editing sent mutation")
    report["evidence"].append({
        "layer": "ui", "nested_dialog": t["editor"]["discardTitle"],
        "parent": t["flat"]["editYAML"], "dismissals": ["keep editing", "escape"],
        "draft_kept": True})

    if case != "clone":
        report["evidence_layer"] = "error recovery"
        invalid = updated + ("\ninvalid: [\n" if case == "syntax" else "\nunknown_e2e_field: true\n")
        replace_editor(editor, invalid)
        with page.expect_response(lambda r: urlsplit(r.url).path == API + "/import" and r.request.method == "POST") as rejected:
            save.click()
        response = rejected.value
        check(response.status == 400, f"invalid {case} YAML HTTP {response.status}")
        check(parse_qs(urlsplit(response.url).query) == {"overwrite": ["true"]} and cdp.send("Network.getRequestPostData", {"requestId": imports[-1]})["postData"] == invalid, "invalid YAML request changed")
        payload = response.json()
        check(isinstance(payload.get("error"), str) and payload["error"], "server failure has no reason")
        alert = dialog.get_by_role("alert")
        expect(alert).to_contain_text(t["editor"]["saveFailed"])
        expect(alert).to_contain_text("expected" if case == "syntax" else "unknown_e2e_field")
        line_button = alert.get_by_role("button")
        expect(line_button).to_have_text(re.compile(r"\d+"))
        line_button.click()
        expect(editor).to_be_focused()
        expect(editor).to_have_text(invalid, use_inner_text=True)
        expect(save).to_be_enabled()
        check(read_model(page, server.base, target) == baseline and read_yaml(page, server.base, target) == baseline_yaml, "rejected YAML partially changed model/specs")
        unchanged_package = page.request.get(package_url)
        check(unchanged_package.status == 200 and unchanged_package.body() == original_package, "rejected YAML changed package bytes")
        artifacts.write_json("rejected-request.json", {"case": case, "status": response.status, "query": parse_qs(urlsplit(response.url).query), "body": invalid, "response": payload})
        page.screenshot(path=artifacts.directory / "validation-error.png")
        report["evidence"].append({"layer": "recovery", "status": 400, "case": case, "draft_retained": True, "baseline_unchanged": True})
        replace_editor(editor, updated)

    # Hold the corrected import. Save shows the loading state, a second click
    # sends nothing, and a 502 keeps the draft and the stored YAML.
    report["evidence_layer"] = "inflight save"
    reason = "upstream fixture is unreachable"
    held = []
    import_path = API + "/import"

    def hold_import(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != import_path:
            route.continue_()
            return
        held.append(route)

    import_route = re.compile(re.escape(import_path) + r"(?:\?|$)")
    page.route(import_route, hold_import)
    save.click()
    report["last_step"] = "yaml-inflight-loading"
    expect(save).to_have_class(re.compile(r"\bant-btn-loading\b"))
    save.click()
    check(len(held) == 1, "duplicate YAML import while the first request is held")
    check(parse_qs(urlsplit(held[0].request.url).query) == {"overwrite": ["true"]}, "held import query differs")
    post = held[0].request.post_data
    if not post:
        check(imports, "held import was not observed on the network")
        post = cdp.send("Network.getRequestPostData", {"requestId": imports[-1]})["postData"]
    check(post == updated, "held import body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == import_path and r.request.method == "POST") as failed_save:
        held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
    page.unroute(import_route, hold_import)
    check(failed_save.value.status == 502, "forced YAML import failure did not surface")
    expect(dialog).to_be_visible()
    expect(editor).to_have_text(updated, use_inner_text=True)
    alert = dialog.get_by_role("alert")
    expect(alert).to_contain_text(t["editor"]["saveFailed"])
    expect(alert).to_contain_text(reason)
    check(read_model(page, server.base, target) == baseline, "refused import changed the model")
    check(read_yaml(page, server.base, target) == baseline_yaml, "refused import changed YAML")
    check(any(item.get("path") == import_path and item.get("status") == 502 for item in bad), "502 was not recorded")
    check(any("502" in text for text in console_errors), "502 console line absent: " + str(console_errors))
    bad[:] = [item for item in bad if item.get("status") != 502]
    console_errors[:] = [text for text in console_errors if "502" not in text]
    expect(save).to_be_enabled()
    expect(save).not_to_have_class(re.compile(r"\bant-btn-loading\b"))
    report["last_step"] = "yaml-inflight-rejected"
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True,
                               "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})

    report["evidence_layer"] = "save request"
    modal_geometry(dialog, save)
    with page.expect_response(lambda r: urlsplit(r.url).path == API + "/import" and r.request.method == "POST") as saved:
        save.click()
    response = saved.value
    check(response.status == 200 and response.json()["models"] == 1 and re.fullmatch("[0-9a-f]{64}", response.json()["package_digest"]), "save envelope/status drift")
    check(parse_qs(urlsplit(response.url).query) == {"overwrite": ["true"]}, "save overwrite query drift")
    check(cdp.send("Network.getRequestPostData", {"requestId": imports[-1]})["postData"] == updated and response.request.headers["content-type"] == "application/yaml", "save request did not preserve editor YAML")
    expect(dialog).not_to_be_visible()
    report["evidence_layer"] = "persistence"
    expected = copy.deepcopy(baseline)
    expected["specs"][0]["args"] = ["--max-model-len", "2048"]
    actual = read_model(page, server.base, target)
    check(actual == expected, "saving one argument changed other model/spec fields")
    saved_yaml = read_yaml(page, server.base, target)
    check(saved_yaml == updated, "canonical YAML differs after save")
    page.reload()
    expect(page.get_by_role("heading", name=expected["display_title"], exact=True)).to_be_visible()
    action(page, t["flat"]["edit"]).click()
    dialog, editor = editor_ready(page, t["flat"]["editYAML"])
    expect(editor).to_have_text(saved_yaml, use_inner_text=True)
    editor.click()
    page.keyboard.press("Escape")
    expect(dialog).not_to_be_visible()
    if case == "clone":
        check(read_model(page, server.base, model_id) == original, "clone edit changed original model/spec set")
        check(read_yaml(page, server.base, model_id) == original_yaml, "clone edit changed original YAML/hash")
        exported_after = page.request.get(package_url)
        check(exported_after.status == 200 and exported_after.body() == original_package, "clone edit changed original package bytes")
    report["evidence"].extend([
        {"layer": "ui", "checks": ["list search/detail", "named real CodeMirror", "drawer viewport/focus containment/Esc return", "dirty cancel keeps draft", "refresh/reopen"]},
        {"layer": "request", "save": {"method": "POST", "path": API + "/import", "query": {"overwrite": "true"}, "body_sha256": yaml_hash(updated), "status": 200}},
        {"layer": "persistence", "model": actual, "yaml_sha256": yaml_hash(saved_yaml), "original_unchanged": case == "clone"},
    ])
    artifacts.write_json("saved-model.json", actual)
    (artifacts.directory / "saved-template.yaml").write_text(updated)
    page.screenshot(path=artifacts.directory / "saved-detail.png")

    report["evidence_layer"] = "cleanup"
    blocks_done = False
    for owned in list(reversed(report["owned_models"])):
        page.goto(server.base + "/inference-templates/" + owned)
        delete = page.get_by_role("button", name=t["editor"]["deleteSpec"].replace("{{name}}", spec_name), exact=True)

        def open_confirm():
            delete.click()
            opened = page.get_by_role("tooltip")
            expect(opened).to_contain_text(t["editor"]["deleteSpecTitle"])
            expect(opened).to_contain_text(t["editor"]["deleteSpecBody"].replace("{{name}}", spec_name))
            expect(opened).to_be_in_viewport(ratio=1)
            return opened

        confirm = open_confirm()
        before = len(mutations)
        button(confirm, t["flat"]["cancel"]).click()
        expect(confirm).not_to_be_visible()
        check(len(mutations) == before, "cancel delete sent mutation")
        check(read_model(page, server.base, owned)["model_id"] == owned, "cancel delete lost model")
        if not blocks_done:
            report["evidence"].append(exercise_spec_delete_blocks(
                page, server, t["editor"], spec_name, lambda: read_model(page, server.base, owned), open_confirm))
            blocks_done = True
        confirm = open_confirm()
        with page.expect_response(lambda r: urlsplit(r.url).path == API + "/spec" and r.request.method == "DELETE") as deleted:
            button(confirm, t["editor"]["deleteSpecOk"]).click()
        response = deleted.value
        check(response.status == 200 and response.json() == {"deleted": True, "model_deleted": True}, "last-spec deletion failed")
        check(parse_qs(urlsplit(response.url).query) == {"model_id": [owned], "spec": [spec_name]}, "delete targeted other resource")
        check(response.request.post_data is None, "delete unexpectedly carried a body")
        check(page.request.get(server.base + API + "/model/" + owned).status == 404, "deleted model still readable")
        page.goto(server.base + "/inference-templates")
        page.get_by_placeholder(t["flat"]["search"], exact=True).fill(owned)
        expect(page.get_by_text(t["flat"]["empty"], exact=True)).to_be_visible()
        report["owned_models"].remove(owned)
    refused = [{"path": API + "/spec", "status": 409}, {"path": API + "/spec", "status": 409}]
    expected_bad = refused if case == "clone" else [{"path": API + "/import", "status": 400}] + refused
    check(bad == expected_bad and not errors, "unexpected browser request failure or exception")
    console_errors[:] = [text for text in console_errors if "409" not in text]
    check(not console_errors if case == "clone" else len(console_errors) == 1 and "400" in console_errors[0], "unexpected console errors")
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "horizontal overflow")
    expected_mutations = ([{"method": "POST", "path": API + "/clone"}] if case == "clone" else [])
    expected_mutations += [{"method": "POST", "path": API + "/import"}] * (2 if case == "clone" else 3)
    expected_mutations += [{"method": "DELETE", "path": API + "/spec"}] * (4 if case == "clone" else 3)
    check(mutations == expected_mutations, "duplicate/unexpected mutation")
    report["evidence"].append({"layer": "cleanup", "owned_models": [], "ui_delete": True, "independent_404": True, "mutations": mutations})


def run_case(args, case):
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(ROOT / "secrets/e2e" / run_id)
    manifest = ROOT / "test/e2e/coverage/template-edit.json"
    report = {"run_id": run_id, "contract": CASES[case][0], "case": CASES[case][1], "parameter": case,
              "locale": args.locale, "status": "NOT_RUN", "evidence": [], "owned_models": [],
              "git_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
              "source_sha256": {str(path.relative_to(ROOT)): digest(path) for path in [
                  ROOT / "test/e2e/template_edit.py", manifest, ROOT / "test/e2e/local_browser_fixture.py",
                  ROOT / "test/e2e/platform_token_read_revoke.py", ROOT / "test/e2e/template_catalog_fixture.py",
                  ROOT / "test/e2e/template_spec_delete_blocks.py",
                  ROOT / "ui/src/pages/yamlEditor.tsx", ROOT / "ui/src/pages/InferenceTemplateDetailPage.tsx",
                  ROOT / "ui/src/i18n/locales/en/inferenceTemplates.json", ROOT / "ui/src/i18n/locales/zh/inferenceTemplates.json"]},
              "binary_sha256": digest(ROOT / "bin/gpuworks"), "viewport": {"width": 1440, "height": 900},
              "execution": "MISSING: edited spec runtime argv/env/mount/role/rank consumption is unverified"}
    server = LocalServer(ROOT / "bin/gpuworks", artifacts.private)
    try:
        report["health"] = server.start()
        with sync_playwright() as p:
            browser = p.chromium.launch(channel="chrome")
            context = browser.new_context(viewport=report["viewport"], locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_template_edit(page, context, server, artifacts, report, args.locale, case)
                required = set(json.loads(manifest.read_text())["required_evidence"])
                check(required <= {e["layer"] for e in report["evidence"]}, "required evidence missing")
                if case != "clone":
                    check("recovery" in {e["layer"] for e in report["evidence"]}, "recovery evidence missing")
                check(all(digest(ROOT / path) == value for path, value in report["source_sha256"].items()), "source changed during execution")
                check(digest(ROOT / "bin/gpuworks") == report["binary_sha256"], "binary changed during execution")
                report["status"] = "PASS"
            except Exception as error:
                report.update(status="FAIL_TEST", failure=traceback.format_exc(), exception_type=type(error).__name__,
                              failure_attribution="UNTRIAGED: assertion/execution failure; not automatically a product defect")
                if report.get("preflight") == "PASS":
                    page.screenshot(path=artifacts.directory / "failure.png")
                    (artifacts.directory / "failure-aria.txt").write_text(artifacts.redact(page.locator("body").aria_snapshot()))
                else:
                    report["failure_capture"] = "omitted before authenticated preflight; credentials may be visible"
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report.update(status="FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA", failure=traceback.format_exc())
    finally:
        report["server_stopped"] = server.stop()
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        log = artifacts.private / "server.log"
        if log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))
        report["private_state"] = "Owned isolated SQLite preserved for diagnosis (7 days); server stopped; no shared cleanup"
        artifacts.write_json("report.json", report)
        artifacts.scan()
    print(json.dumps({"status": report["status"], "case": case, "locale": args.locale, "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return report["status"] == "PASS"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--case", choices=["all", *CASES], default="all")
    args = parser.parse_args()
    results = [run_case(args, case) for case in (CASES if args.case == "all" else [args.case])]
    return 0 if all(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
