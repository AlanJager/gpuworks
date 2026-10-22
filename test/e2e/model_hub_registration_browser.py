"""Three independent browser metadata registration contracts, no downloads/GPU.

E2E standard: test/e2e/browser-e2e-testing-standard.md.
CLI: model_hub_registration.py --case huggingface|modelscope|quick --locale en|zh.
"""
import hashlib
import json
import re
import time
from urllib.parse import quote, urlsplit
from playwright.sync_api import expect
from local_browser_fixture import ROOT, http
from platform_token_read_revoke import button, check, modal_geometry
from model_hub_fixture import CONFIG, INDEX, README, SHA, WEIGHT_BYTES
from model_hub_fixture_setup import MODELS, SPEC


def strict(actual, expected, label):
    check(json.dumps(actual, sort_keys=True, ensure_ascii=False) == json.dumps(expected, sort_keys=True, ensure_ascii=False), label)


def texts(locale, namespace):
    return json.loads((ROOT / f"ui/src/i18n/locales/{locale}/{namespace}.json").read_text())


def poll(page, read, label, timeout=20):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = read()
        if value:
            return value
        page.wait_for_timeout(100)
    raise AssertionError("timed out: " + label)


def begin(page, context, server, artifacts, report, locale):
    report["sensitive_phase"] = True
    auth = texts(locale, "auth")
    page.goto(server.base + "/models")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    labels = texts(locale, "models")
    expect(page.get_by_role("heading", name=labels["page"]["title"], exact=True)).to_be_visible()
    page.get_by_role("img", name=labels["view"]["list"], exact=True).click()
    mutations, failures, errors, consoles = [], [], [], []
    report.update({"requests": mutations, "browser_failures": failures, "page_errors": errors, "console_errors": consoles, "evaluations": []})
    def requested(request):
        parts = urlsplit(request.url)
        if request.method in ("POST", "PUT", "PATCH", "DELETE") and (parts.path == MODELS or parts.path.startswith(MODELS + "/") or parts.path.startswith("/api/v1/deployments")):
            mutations.append({"method": request.method, "path": parts.path, "query": parts.query,
                              "body": request.post_data_json if request.post_data else None})
    page.on("request", requested)
    page.on("request", lambda request: report["evaluations"].append(request.post_data_json)
            if urlsplit(request.url).path == "/api/v1/evaluate" and request.method == "POST" else None)
    page.on("response", lambda r: failures.append({"method": r.request.method, "url": r.url, "status": r.status}) if r.status >= 400 else None)
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("console", lambda message: consoles.append({"url": message.location.get("url", ""), "text": message.text}) if message.type == "error" else None)
    return labels


def listed(server, token, data):
    code, result = http(server.base, "GET", MODELS + "?source_id=" + quote(data["repo"], safe=""), token)
    check(code == 200 and result["total"] == len(result["items"]), "independent model list failed")
    check(all(m["source_id"] == data["repo"] for m in result["items"]), "lookup included foreign repository")
    return result["items"]


def read(server, token, ident):
    code, document = http(server.base, "GET", MODELS + "/" + str(ident), token)
    check(code == 200, "independent model GET failed")
    return document


def facts(document, data, source, revision, known=True, name=None):
    expected = {"source": source, "source_id": data["repo"], "revision": revision,
        "name": name or data["name"], "arch": "Qwen2ForCausalLM" if known else "",
        "weight_gib": WEIGHT_BYTES / 1024 ** 3 if known else 0}
    if known:
        expected.update({"context_length": 4096, "precision": "float16", "readme": README})
    else:
        check(document.get("context_length", 0) == 0 and document.get("precision", "") == ""
              and document.get("readme", "") == "", "outage fabricated known metadata")
    for key, value in expected.items():
        strict(document.get(key), value, "independent metadata mismatch: " + key)
    check(not document.get("nodes") and not document.get("storages"), "metadata registration invented a file copy")


def visible_action(scope, label):
    text = r"\s*".join(label) if re.fullmatch(r"[\u3400-\u9fff]{2}", label) else re.escape(label)
    return scope.get_by_role("button").filter(has_text=re.compile("^" + text + "$"))


def choose_option(page, control, label):
    # AntD's accessibility options are a hidden virtual list. Click the
    # visible option in this control's actual popup, as the user does.
    control.click()
    popup_id = control.get_attribute("aria-controls")
    check(bool(popup_id), "source selector has no controlled popup")
    popup = page.locator("[id=" + json.dumps(popup_id) + "]").locator("..")
    popup.get_by_text(label, exact=True).filter(visible=True).click()


def registration(page, labels, source, repo, name, revision=""):
    visible_action(page, labels["action"]["register"]).first.click()
    dialog = page.get_by_role("dialog", name=labels["action"]["register"], exact=True)
    choose_option(page, dialog.get_by_label(labels["column"]["source"], exact=True),
                  "Hugging Face" if source == "huggingface" else "ModelScope")
    dialog.get_by_label(labels["column"]["sourceID"], exact=True).fill(repo)
    dialog.get_by_label(labels["column"]["name"], exact=True).fill(name)
    dialog.get_by_text(labels["register"]["advanced"], exact=True).click()
    revision_input = dialog.get_by_label(labels["detail"]["revision"], exact=True)
    expect(revision_input).to_have_value("")
    if revision:
        revision_input.fill(revision)
    submit = dialog.get_by_role("button").filter(has_text=re.compile("^" + re.escape(labels["register"]["confirm"]) + "$"))
    modal_geometry(dialog, submit)
    return dialog, submit


def submit_create(page, submit, report):
    with page.expect_response(lambda r: urlsplit(r.url).path == MODELS and r.request.method == "POST") as response:
        submit.click()
    document = response.value.json()
    if isinstance(document, dict) and type(document.get("id")) is int:
        report.setdefault("owned_model_ids", []).append(document["id"])
    return response.value, document


def saved_ui(page, server, labels, data, ident, source, revision, token):
    page.goto(server.base + "/models?detail=" + str(ident))
    dialog = page.get_by_role("dialog", name=data["name"], exact=True)
    expect(dialog).to_be_in_viewport(ratio=1)
    heading_repo = dialog.locator(".model-detail-heading span.ant-typography-secondary")
    expect(heading_repo).to_have_text(data["repo"])
    expect(heading_repo).to_be_visible()
    expect(dialog.get_by_text(revision, exact=True)).to_be_visible()
    expect(dialog.get_by_text("Qwen2ForCausalLM", exact=True)).to_be_visible()
    expect(dialog.get_by_text("float16", exact=True)).to_be_visible()
    expect(dialog.get_by_text("4,096", exact=True)).to_be_visible()
    facts(read(server, token, ident), data, source, revision)
    visible_action(dialog, labels["action"]["close"]).click()
    expect(dialog).not_to_be_visible()
    row = page.get_by_role("row").filter(has_text=data["name"])
    expect(row).to_have_count(1)
    return row


def delete_ui(page, server, labels, data, ident, token, report):
    row = page.get_by_role("row").filter(has_text=data["name"])
    button(row, labels["action"]["more"]).click()
    page.get_by_role("menu").get_by_text(labels["action"]["deleteModel"], exact=True).click()
    modal = page.get_by_role("dialog", name=labels["delete"]["confirm"].replace("{{name}}", data["name"]), exact=True)
    expect(modal.get_by_text(labels["delete"]["listDescription"], exact=True)).to_be_visible()
    with page.expect_response(lambda r: urlsplit(r.url).path == MODELS + "/" + str(ident) and r.request.method == "DELETE") as deleted:
        button(modal, labels["action"]["delete"]).click()
    check(deleted.value.status == 204, "model UI deletion failed")
    code, _ = http(server.base, "GET", MODELS + "/" + str(ident), token)
    check(code == 404 and listed(server, token, data) == [], "deleted model remains in authority")
    expect(row).to_have_count(0)
    report["last_step"] = "browser deletion independently observed"



def applied_model(page, report, ident, repo, label, offset):
    # ModelPicker's selected label proves what the user sees. The actual
    # evaluate wire binds that selection to the exact numeric model ID.
    control = page.locator("#model_id")
    selection = control.locator("xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-select ')][1]").locator(".ant-select-selection-item")
    expect(selection).to_have_count(1)
    expect(selection).to_contain_text(repo)
    expect(selection.locator(".ant-tag")).to_have_text("MS")
    actual = poll(page, lambda: next((request for request in reversed(report["evaluations"][offset:])
        if request.get("model_id") == ident), None), label + " evaluate exact selected model_id")
    strict(actual["model_id"], ident, label + " model identity")
    report.setdefault("selected_model_evidence", []).append({"action": label,
        "model_id": ident, "selected_label": selection.inner_text(), "evaluate_request": actual})


def finish(report, server, data, allowed_management):
    origin = data["hub"].base
    allowed_urls = {server.base + path: codes for path, codes in allowed_management.items()}
    for item in report["browser_failures"]:
        if item["url"].startswith(origin + "/") and item["status"] == 503:
            continue
        check(item["status"] in allowed_urls.get(item["url"], []), "unexpected browser HTTP error")
    for error in report["console_errors"]:
        url = error["url"]
        allowed = ([503] if url.startswith(origin + "/") else allowed_urls.get(url, []))
        check(any(str(code) in error["text"] for code in allowed), "unexpected browser console error")
    check(report["page_errors"] == [], "unhandled browser page error")
    report["evidence"].extend([{"layer": "ui", "assertions": "named controls, draft/persisted values, real source branch, exact metadata, deletion"},
        {"layer": "request", "mutations": report["requests"]},
        {"layer": "persistence", "observations": report["observations"]},
        {"layer": "source", "audit": data["hub"].audit(), "scope": "actual metadata HTTP; no weight download, agent or GPU execution"}])


def wire(body, path=MODELS):
    return {"method": "POST", "path": path, "query": "", "body": body}


# Contract: gpuworks/models/huggingface-browser-registration
# Mainline: source rules select real local HF -> blank revision reads browser SHA/config/index ->
# real 257-char name400, draft retained and zero records -> corrected201 -> same-artifact
# duplicate201 reuses exactID -> refresh/detail independent exact metadata -> UI delete404.
def test_huggingface(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale)
    report["step"] = "Hugging Face invalid name and exact browser metadata"
    check(listed(server, token, data) == [], "fixture models not empty")
    dialog, submit = registration(page, labels, "huggingface", data["repo"], "x" * 257)
    offset = len(data["hub"].audit())
    body = {"source": "huggingface", "source_id": data["repo"], "revision": SHA, "name": "x" * 257,
            "readme": README, "config_raw": CONFIG, "weight_bytes": WEIGHT_BYTES, "logo_data": "", "browser_metadata": True}
    rejected, error = submit_create(page, submit, report)
    check(rejected.status == 400 and error["code"] == "FIELD_INVALID" and error["params"]["field"] == "name", "expected actual backend name validation")
    strict(report["requests"], [wire(body)], "HF rejected full body/count/order differs")
    expect(dialog.get_by_label(labels["column"]["name"], exact=True)).to_have_value("x" * 257)
    expect(dialog.get_by_label(labels["column"]["sourceID"], exact=True)).to_have_value(data["repo"])
    expect(dialog.get_by_label(labels["detail"]["revision"], exact=True)).to_have_value("")
    expect(page.get_by_text(re.compile(re.escape(labels["register"]["failed"].split("{{detail}}")[0])))).to_be_visible()
    check(listed(server, token, data) == [], "invalid name partially persisted a model")
    dialog.get_by_label(labels["column"]["name"], exact=True).fill(data["name"])
    response, created = submit_create(page, submit, report)
    check(response.status == 201, "HF registration failed")
    body = dict(body, name=data["name"])
    strict(report["requests"], [wire(dict(body, name="x" * 257)), wire(body)], "HF successful full body/count differs")
    ident = created["id"]
    facts(read(server, token, ident), data, "huggingface", SHA)
    dialog, submit = registration(page, labels, "huggingface", data["repo"], data["name"])
    response, duplicate = submit_create(page, submit, report)
    check(response.status == 201 and duplicate["id"] == ident and len(listed(server, token, data)) == 1, "HF duplicate changed artifact identity")
    audit = data["hub"].audit()[offset:]
    expected_paths = {"/hf/" + data["repo"] + "/raw/main/" + f: hashlib.sha256(v.encode()).hexdigest()
        for f, v in (("README.md", README), ("config.json", CONFIG), ("model.safetensors.index.json", INDEX))}
    expected_paths["/hf/api/models/" + data["repo"] + "?blobs=true"] = None
    strict(sorted(r["target"] for r in audit), sorted(list(expected_paths) * 3), "HF browser did not make exact per-submit reads")
    for request in audit:
        check(request["origin"] == server.base and request["status"] == 200, "HF metadata did not come from real browser source")
        if expected_paths[request["target"]]:
            strict(request["sha256"], expected_paths[request["target"]], "HF fixture sent different authoritative bytes")
    report["observations"] = {"created_id": ident, "duplicate_id": duplicate["id"], "saved": read(server, token, ident), "rejected": error}
    saved_ui(page, server, labels, data, ident, "huggingface", SHA, token)
    delete_ui(page, server, labels, data, ident, token, report)
    strict(report["requests"], [wire(dict(body, name="x" * 257)), wire(body), wire(body),
        {"method": "DELETE", "path": MODELS + "/" + str(ident), "query": "", "body": None}], "HF final mutation sequence differs")
    finish(report, server, data, {MODELS: [400]})


# Contract: gpuworks/models/modelscope-registration-recovery
# Mainline: explicit main -> real server converts master; upstream503 still201 with
# notice and unknown metadata -> UI refresh502 preserves same row -> source recovery
# refresh200 exact bytes/sameID -> duplicate201 same artifact -> reload/delete404.
def test_modelscope(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale)
    data["hub"].available(False)
    dialog, submit = registration(page, labels, "modelscope", data["repo"], data["name"], "main")
    report["step"] = "ModelScope metadata unavailable registration remains valid"
    offset = len(data["hub"].audit())
    response, created = submit_create(page, submit, report)
    check(response.status == 201, "best-effort MS registration should succeed")
    ident = created["id"]
    expected = {"source": "modelscope", "source_id": data["repo"], "revision": "main", "name": data["name"]}
    strict(report["requests"], [wire(expected)], "MS create body/count differs")
    facts(read(server, token, ident), data, "modelscope", "master", known=False)
    notice = created["metadata_notice"]
    strict({k: notice[k] for k in ("code", "params")}, {"code": "MODEL_METADATA_UNREACHABLE", "params": {
        "source": "modelscope", "hosts": urlsplit(data["hub"].base).netloc, "route": "/api/v1/pull/source-rules"}}, "MS missing-metadata notice differs")
    common = texts(locale, "common")
    notice_text = common["apiError"]["MODEL_METADATA_UNREACHABLE"].replace("{{hosts}}", urlsplit(data["hub"].base).netloc)
    expect(page.get_by_text(notice_text, exact=True)).to_be_visible()
    initial = read(server, token, ident)
    failed_targets = ["/ms/api/v1/models/" + data["repo"],
        *[f"/ms/models/{data['repo']}/resolve/master/{file}" for file in
          ("config.json", "README.md", "model.safetensors.index.json")],
        "/ms/api/v1/models/" + data["repo"] + "/repo/files?Recursive=true&Revision=master"]
    outage_reads = data["hub"].audit()[offset:]
    strict(sorted(r["target"] for r in outage_reads), sorted(failed_targets), "MS create outage exact nonempty source reads")
    check(all(r["status"] == 503 and r["origin"] == "" for r in outage_reads), "MS outage did not reach real server-side source")
    page.goto(server.base + "/models?detail=" + str(ident))
    detail = page.get_by_role("dialog", name=data["name"], exact=True)
    refresh = visible_action(detail, labels["detail"]["refreshMetadata"])
    path = MODELS + f"/{ident}/metadata/refresh"
    failure_offset = len(data["hub"].audit())
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as failed:
        refresh.click()
    check(failed.value.status == 502 and failed.value.json()["code"] == notice["code"], "MS refresh outage not reported")
    strict(read(server, token, ident), initial, "failed refresh partially modified model")
    refresh_failure_reads = data["hub"].audit()[failure_offset:]
    strict(sorted(r["target"] for r in refresh_failure_reads), sorted(failed_targets), "MS refresh outage exact nonempty source reads")
    check(all(r["status"] == 503 and r["origin"] == "" for r in refresh_failure_reads), "MS failed refresh bypassed configured source")
    report["outage_source_evidence"] = {"create": outage_reads, "refresh": refresh_failure_reads}
    expect(page.get_by_text(re.compile(re.escape(labels["detail"]["message"]["refreshFailed"].split("{{detail}}")[0])))).to_be_visible()
    data["hub"].available(True)
    recovery_offset = len(data["hub"].audit())
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as refreshed:
        refresh.click()
    check(refreshed.value.status == 200, "MS metadata refresh recovery failed")
    facts(read(server, token, ident), data, "modelscope", "master")
    visible_action(detail, labels["action"]["close"]).click()
    expect(detail).not_to_be_visible()
    dialog, submit = registration(page, labels, "modelscope", data["repo"], data["name"], "main")
    response, duplicate = submit_create(page, submit, report)
    check(response.status == 201 and duplicate["id"] == ident and len(listed(server, token, data)) == 1, "MS duplicate did not reuse normalized artifact")
    reads = data["hub"].audit()[recovery_offset:]
    files = {f"/ms/models/{data['repo']}/resolve/master/{file}": value for file, value in
             (("config.json", CONFIG), ("README.md", README), ("model.safetensors.index.json", INDEX))}
    expected_targets = ["/ms/api/v1/models/" + data["repo"], *files]
    strict(sorted(r["target"] for r in reads), sorted(expected_targets * 2), "MS exact recovery/duplicate protocol reads differ")
    for request in reads:
        check(request["origin"] == "" and request["status"] == 200, "MS metadata incorrectly fetched in browser")
        if request["target"] in files:
            strict(request["sha256"], hashlib.sha256(files[request["target"]].encode()).hexdigest(), "MS authority bytes drift")
    report["observations"] = {"degraded": initial, "notice": notice, "refresh_error": failed.value.json(), "saved": read(server, token, ident), "duplicate_id": duplicate["id"]}
    saved_ui(page, server, labels, data, ident, "modelscope", "master", token)
    delete_ui(page, server, labels, data, ident, token, report)
    strict(report["requests"], [wire(expected), wire(None, path), wire(None, path), wire(expected),
        {"method": "DELETE", "path": MODELS + "/" + str(ident), "query": "", "body": None}], "MS final full mutation sequence differs")
    finish(report, server, data, {path: [502]})


# Contract: gpuworks/models/template-quick-registration
# Mainline: owned two-source template -> real Deploy action -> explicitly choose MS
# -> quick201 + background source reads -> sameID metadata -> reopen template and
# confirm matching registered model with zero newPOST -> reload model -> UI delete404.
def test_quick(page, context, server, artifacts, report, locale, data, token, holder):
    labels = begin(page, context, server, artifacts, report, locale)
    templates, deploy = texts(locale, "inferenceTemplates"), texts(locale, "deploy")
    page.goto(server.base + "/inference-templates/" + data["template_model_id"])
    expect(page.get_by_role("heading", name=data["template"]["display_title"], exact=True)).to_be_visible()
    visible_action(page, templates["detail"]["deploy"]).click()
    modal = page.get_by_role("dialog", name=deploy["action"]["selectWeightSource"], exact=True)
    choose = modal.get_by_role("combobox", name=deploy["action"]["selectSourcePlaceholder"], exact=True)
    expect(choose).to_have_value("")
    expect(button(modal, deploy["action"]["registerAndDeploy"])).to_be_disabled()
    choose_option(page, choose, deploy["action"]["modelScope"] + " · " + data["repo"])
    path = MODELS + "/quick-register"
    first_evaluate_offset = len(report["evaluations"])
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as registered:
        button(modal, deploy["action"]["registerAndDeploy"]).click()
    check(registered.value.status == 201, "template quick registration failed")
    created = registered.value.json()
    ident = created["id"]
    report.setdefault("owned_model_ids", []).append(ident)
    expected = {"source": "modelscope", "source_id": data["repo"], "revision": "main", "name": data["repo"],
                "weight_bytes": WEIGHT_BYTES, "quantization": "fp16", "architecture": "Qwen2ForCausalLM", "context_length": 4096}
    strict(report["requests"], [wire(expected, path)], "quick complete body/count differs")
    report["step"] = "quick background metadata reaches independent source"
    saved = poll(page, lambda: (lambda doc: doc if doc.get("readme") == README else None)(read(server, token, ident)), "quick background metadata")
    data["name"] = data["repo"]
    facts(saved, data, "modelscope", "master")
    applied_model(page, report, ident, data["repo"], "quick initial application", first_evaluate_offset)
    check(len(listed(server, token, data)) == 1, "quick registration duplicated model")
    audit = data["hub"].audit()
    check(any(r["target"] == "/ms/models/" + data["repo"] + "/resolve/master/config.json" and r["sha256"] == hashlib.sha256(CONFIG.encode()).hexdigest() for r in audit), "quick background metadata did not read authoritative config")
    check(all(r["origin"] == "" and r["status"] == 200 for r in audit), "quick source read failed or incorrectly used browser metadata")
    page.goto(server.base + "/inference-templates/" + data["template_model_id"])
    visible_action(page, templates["detail"]["deploy"]).click()
    modal = page.get_by_role("dialog", name=deploy["weightSourceModal"]["confirmTitle"], exact=True)
    matching_radio = modal.locator("label.ant-radio-wrapper").filter(has_text=data["repo"]).get_by_role("radio")
    expect(matching_radio).to_have_count(1)
    expect(matching_radio).to_be_checked()
    reuse_evaluate_offset = len(report["evaluations"])
    button(modal, deploy["weightSourceModal"]["useModel"]).click()
    expect(modal).not_to_be_visible()
    strict(report["requests"], [wire(expected, path)], "reusing registered template model emitted extra mutation")
    applied_model(page, report, ident, data["repo"], "existing model confirmation", reuse_evaluate_offset)
    strict(read(server, token, ident), saved, "template reuse overwrote saved metadata")
    report["observations"] = {"saved": saved, "selected_template_id": data["template"]["id"], "reuse": "same model selected with zero new POST"}
    saved_ui(page, server, labels, data, ident, "modelscope", "master", token)
    delete_ui(page, server, labels, data, ident, token, report)
    strict(report["requests"], [wire(expected, path), {"method": "DELETE", "path": MODELS + "/" + str(ident), "query": "", "body": None}], "quick final mutation sequence differs")
    finish(report, server, data, {})
